// Package report checks a GitLab backup (and optionally compares it with
// the previous one) and renders the result as text, JSON or Prometheus
// metrics, for nightly backup monitoring.
package report

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
	"github.com/movenial/gitlab-backup-browser/internal/index"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

// Severity of a problem. Errors mean the backup cannot be trusted;
// warnings deserve a look (they can be legitimate, e.g. a deleted project).
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Status values, also used as exit codes 0 / 1 / 2.
const (
	StatusOK      = "ok"
	StatusWarning = "warning"
	StatusError   = "error"
)

// Problem is one finding.
type Problem struct {
	Severity Severity `json:"severity"`
	Kind     string   `json:"kind"` // e.g. bundle_unreadable, project_removed, ref_rewound
	Project  string   `json:"project,omitempty"`
	Message  string   `json:"message"`
}

// Summary describes one backup.
type Summary struct {
	Path          string     `json:"path"`
	SizeBytes     int64      `json:"size_bytes"`
	GitLabVersion string     `json:"gitlab_version,omitempty"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	AgeSeconds    float64    `json:"age_seconds,omitempty"`
	Database      string     `json:"database"`
	Projects      int        `json:"projects"`
	WithRepo      int        `json:"projects_with_repository"`
	WithoutRepo   int        `json:"projects_without_repository"`
	Wikis         int        `json:"wikis"`
	Namespaces    int        `json:"namespaces"`
	Bundles       int        `json:"repositories"`
	BundleBytes   int64      `json:"bundle_bytes"`
	Unmatched     int        `json:"unmatched_bundles"`
	Issues        int        `json:"issues"`
	MergeRequests int        `json:"merge_requests"`
	IssuesKnown   bool       `json:"issues_imported"`
	Status        string     `json:"status,omitempty"` // in snapshots: result of that day's check
}

// RepoResult is the outcome of fully restoring one repository (--deep).
type RepoResult struct {
	Project     string  `json:"project"`
	Objects     int     `json:"objects"`
	Seconds     float64 `json:"seconds"`
	PeakHeap    uint64  `json:"peak_heap_bytes"`
	DroppedRefs int     `json:"dropped_refs,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// Rename is a project whose path changed (same database id).
type Rename struct {
	ID   int64  `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// RefChange is a branch or tag that changed between two backups.
type RefChange struct {
	Project string `json:"project"`
	Ref     string `json:"ref"`
	// Change is "deleted", "deleted-merged" (source branch of a merged or
	// closed merge request: normal, not a warning), "added", "moved" (not
	// analysed), "fast-forward",
	// "rewound" (moved back to an ancestor), "diverged" (force-push) or
	// "rewritten" (the old commit is not in the new backup at all).
	Change string `json:"change"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
	// Ignored: matched by --ignore-refs, so not reported as a warning.
	Ignored bool `json:"ignored,omitempty"`
}

// CountDrop is a project whose issue or MR count went down.
type CountDrop struct {
	Project string `json:"project"`
	What    string `json:"what"` // issues | merge_requests
	From    int    `json:"from"`
	To      int    `json:"to"`
}

// Comparison is the difference with the previous backup.
type Comparison struct {
	Previous        Summary     `json:"previous"`
	ProjectsAdded   []string    `json:"projects_added"`
	ProjectsRemoved []string    `json:"projects_removed"`
	ProjectsRenamed []Rename    `json:"projects_renamed"`
	ReposLost       []string    `json:"repositories_lost"`
	Refs            []RefChange `json:"refs"`
	CountDrops      []CountDrop `json:"count_drops"`
	IssuesDelta     int         `json:"issues_delta"`
	MRsDelta        int         `json:"merge_requests_delta"`
	RefsAnalysed    bool        `json:"refs_analysed"` // true with --deep
}

// Report is the full result.
type Report struct {
	Status        string        `json:"status"`
	CheckedAt     time.Time     `json:"checked_at"`
	Seconds       float64       `json:"duration_seconds"` // whole command, import included
	ImportSeconds float64       `json:"import_seconds"`
	Deep          bool          `json:"deep"`
	Backup        Summary       `json:"backup"`
	ReposChecked  int           `json:"repositories_checked"`
	Problems      []Problem     `json:"problems"`
	Heaviest      []RepoResult  `json:"heaviest,omitempty"`
	NoBundle      []string      `json:"projects_without_bundle,omitempty"`
	Comparison    *Comparison   `json:"comparison,omitempty"`
	Assets        []*AssetCheck `json:"assets,omitempty"` // with --deep
	// Schema: tables and columns of the database gbb could not find, or
	// found under an older name (see index.Schema).
	Schema       []index.SchemaNote `json:"schema,omitempty"`
	ErrorCount   int                `json:"errors"`
	WarningCount int                `json:"warnings"`
	Jobs         int                `json:"jobs,omitempty"` // repositories restored in parallel
	repoNames    map[string]string
	mu           sync.Mutex
}

// Loaded is an indexed backup.
type Loaded struct {
	Path    string
	Index   *index.Index
	Src     backup.Source
	Tracker *tracker.Store // issues/MRs, nil when not imported
	// Expected: LFS objects and attachments the database lists (nil when
	// unknown); checked against the archives with Deep.
	Expected *index.Expected
}

// Options control Run.
type Options struct {
	Deep     bool
	MaxAge   time.Duration // error when the backup is older (0 = no check)
	TempDir  string        // where --deep restores repositories
	Progress func(done, total int, name string)
	OnRepo   func(RepoResult)
	Sample   func() (stop func() uint64) // peak heap sampler for --deep
	Jobs     int                         // --deep repositories restored at once (0 = all CPUs)
	Now      func() time.Time
	Previous *Snapshot // compare with this earlier backup
	// IgnoreRefs: changes of these refs are listed but not warned about
	// ("renovate/*", or "group/project:*" for a mirror rewritten upstream).
	IgnoreRefs []string
	Started    time.Time // when the whole command started (import included)
}

func (r *Report) add(sev Severity, kind, project, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Problems = append(r.Problems, Problem{Severity: sev, Kind: kind, Project: project, Message: fmt.Sprintf(format, args...)})
}

var leadingUnix = regexp.MustCompile(`^(\d{9,11})_`)

// createdAt finds when a backup was taken: backup_information.yml, or the
// Unix timestamp GitLab puts at the start of the archive name.
func createdAt(ix *index.Index, path string) *time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, ix.Info["backup_created_at"]); err == nil {
			t = t.UTC()
			return &t
		}
	}
	if m := leadingUnix.FindStringSubmatch(filepath.Base(path)); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			t := time.Unix(sec, 0).UTC()
			return &t
		}
	}
	return nil
}

// Summarize describes a loaded backup.
func Summarize(l *Loaded, now time.Time) Summary {
	ix := l.Index
	st := ix.Stats()
	s := Summary{
		Path: l.Path, GitLabVersion: ix.Info["gitlab_version"], Database: ix.DBStatus,
		Projects: st.Projects, WithRepo: st.WithRepo, WithoutRepo: st.WithoutRepo, Wikis: st.Wikis,
		Namespaces: st.Namespaces, Bundles: len(ix.Repos), BundleBytes: st.BundleBytes, Unmatched: st.Unmatched,
		IssuesKnown: ix.Tracker,
	}
	if fi, err := os.Stat(l.Path); err == nil && !fi.IsDir() {
		s.SizeBytes = fi.Size()
	}
	if t := createdAt(ix, l.Path); t != nil {
		s.CreatedAt = t
		s.AgeSeconds = now.Sub(*t).Seconds()
	}
	for _, p := range ix.Projects {
		s.Issues += p.Issues
		s.MergeRequests += p.MRs
	}
	return s
}

func repoNames(ix *index.Index) map[string]string {
	names := map[string]string{}
	for _, p := range ix.Projects {
		if p.Repo != "" {
			names[p.Repo] = p.FullPath
		}
		if p.Wiki != "" {
			names[p.Wiki] = p.FullPath + " (wiki)"
		}
		if p.Design != "" {
			names[p.Design] = p.FullPath + " (designs)"
		}
	}
	for _, s := range ix.Snippets {
		if s.Repo != "" {
			names[s.Repo] = fmt.Sprintf("snippet $%d", s.ID)
		}
	}
	return names
}

// Run checks cur and, when opts.Previous is set, compares with it (the
// snapshot of cur used for that comparison is returned too).
func Run(cur *Loaded, opts Options) (*Report, *Snapshot) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	start := now()
	r := &Report{CheckedAt: start.UTC(), Deep: opts.Deep, Backup: Summarize(cur, start), repoNames: repoNames(cur.Index),
		Problems: []Problem{}}
	ix, src := cur.Index, cur.Src

	switch {
	case ix.DBStatus == "missing":
		r.add(Error, "database_missing", "", "the backup has no db/database.sql.gz: project names, issues and merge requests are lost")
	case ix.DBStatus != "ok":
		r.add(Error, "database_unreadable", "", "the database dump could not be read: %s", ix.DBStatus)
	}
	// A GitLab version that renamed or moved what gbb reads must be
	// noticed, not shown as wrong data.
	r.Schema = ix.Schema
	for _, n := range ix.Schema {
		if n.Severity == "warning" {
			r.add(Warning, "database_schema", "", "%s", n.String())
		}
	}
	if opts.MaxAge > 0 {
		if r.Backup.CreatedAt == nil {
			r.add(Warning, "backup_age_unknown", "", "the backup date could not be determined")
		} else if age := start.Sub(*r.Backup.CreatedAt); age > opts.MaxAge {
			r.add(Error, "backup_too_old", "", "the backup is %s old (limit %s)", age.Round(time.Minute), opts.MaxAge)
		}
	}
	for _, p := range ix.Projects {
		if !p.Unmatched && p.Repo == "" {
			r.NoBundle = append(r.NoBundle, p.FullPath)
		}
	}

	var cmp *comparer
	var snap *Snapshot
	if opts.Previous != nil {
		snap = TakeSnapshot(cur, r.Backup)
		cmp = newComparer(r, opts.Previous, snap, opts.IgnoreRefs)
		if p, c := opts.Previous.Summary.CreatedAt, r.Backup.CreatedAt; p != nil && c != nil && !c.After(*p) {
			r.add(Warning, "backup_not_newer", "", "this backup is not newer than the one it is compared with (%s vs %s)",
				c.Format(time.RFC3339), p.Format(time.RFC3339))
		}
	}

	keys := make([]string, 0, len(ix.Repos))
	for k := range ix.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Phase 1 (sequential, cheap): every bundle chain must be readable and
	// start with a full bundle.
	var deep []string
	for i, k := range keys {
		rb := ix.Repos[k]
		name := r.name(k)
		if opts.Progress != nil && !opts.Deep {
			opts.Progress(i, len(keys), name)
		}
		startIdx, err := backup.ChainStart(src, rb)
		if err != nil {
			r.add(Error, "bundle_unreadable", name, "%v", err)
			continue
		}
		if h, _ := backup.ReadHeader(src, rb.Steps[startIdx].Bundle); h != nil && len(h.Prereqs) > 0 {
			r.add(Error, "full_bundle_missing", name, "the oldest bundle is incremental; its full backup is missing")
			continue
		}
		r.ReposChecked++
		deep = append(deep, k)
	}

	// Phase 2 (--deep): restore repositories, opts.Jobs at a time. Memory
	// grows with the number of jobs, so the default is one.
	var results []RepoResult
	if opts.Deep {
		jobs := opts.Jobs
		if jobs <= 0 {
			jobs = runtime.NumCPU()
		}
		if jobs > len(deep) {
			jobs = len(deep)
		}
		var (
			mu      sync.Mutex
			started int
			wg      sync.WaitGroup
			next    = make(chan int)
		)
		worker := func() {
			defer wg.Done()
			for i := range next {
				k := deep[i]
				name := r.name(k)
				mu.Lock()
				started++
				if opts.Progress != nil {
					opts.Progress(started-1, len(deep), name)
				}
				mu.Unlock()
				res := RepoResult{Project: name}
				dst := filepath.Join(opts.TempDir, fmt.Sprintf("r%d.git", i))
				var stop func() uint64
				if opts.Sample != nil && jobs == 1 { // the heap is shared: per-repo peaks only make sense alone
					stop = opts.Sample()
				}
				t0 := time.Now()
				bs, err := backup.BuildRepo(src, ix.Repos[k], dst)
				res.Seconds = time.Since(t0).Seconds()
				if stop != nil {
					res.PeakHeap = stop()
				}
				if err != nil {
					res.Error = err.Error()
					r.add(Error, "repository_corrupt", name, "restore failed: %v", err)
				} else {
					res.Objects, res.DroppedRefs = bs.Objects, bs.DroppedRefs
					if bs.DroppedRefs > 0 {
						r.add(Warning, "refs_dangling", name, "%d refs point to objects missing from the bundle", bs.DroppedRefs)
					}
					if cmp != nil {
						cmp.analyse(k, dst)
					}
				}
				os.RemoveAll(dst)
				debug.FreeOSMemory()
				mu.Lock()
				if opts.OnRepo != nil {
					opts.OnRepo(res)
				}
				results = append(results, res)
				mu.Unlock()
			}
		}
		wg.Add(jobs)
		for j := 0; j < jobs; j++ {
			go worker()
		}
		for i := range deep {
			next <- i
		}
		close(next)
		wg.Wait()
		r.Jobs = jobs
	}
	if opts.Progress != nil {
		opts.Progress(len(keys), len(keys), "")
	}
	if cmp != nil {
		cmp.finish(opts.Deep)
	}

	sort.Slice(results, func(a, b int) bool {
		if results[a].PeakHeap != results[b].PeakHeap {
			return results[a].PeakHeap > results[b].PeakHeap
		}
		return results[a].Seconds > results[b].Seconds // with --jobs > 1: slowest first
	})
	if len(results) > 5 {
		results = results[:5]
	}
	r.Heaviest = results
	if opts.Deep {
		r.checkAssets(cur, func(name string) {
			if opts.Progress != nil {
				opts.Progress(0, 1, name)
			}
		})
		if opts.Progress != nil {
			opts.Progress(1, 1, "")
		}
	}
	sort.SliceStable(r.Problems, func(a, b int) bool {
		x, y := r.Problems[a], r.Problems[b]
		if x.Severity != y.Severity {
			return x.Severity == Error
		}
		if x.Project != y.Project {
			return x.Project < y.Project
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Message < y.Message
	})
	for _, p := range r.Problems {
		if p.Severity == Error {
			r.ErrorCount++
		} else {
			r.WarningCount++
		}
	}
	switch {
	case r.ErrorCount > 0:
		r.Status = StatusError
	case r.WarningCount > 0:
		r.Status = StatusWarning
	default:
		r.Status = StatusOK
	}
	r.Seconds = now().Sub(start).Seconds()
	if !opts.Started.IsZero() && opts.Started.Before(start) {
		r.ImportSeconds = start.Sub(opts.Started).Seconds()
		r.Seconds += r.ImportSeconds
	}
	if snap != nil {
		snap.Summary.Status = r.Status
	}
	return r, snap
}

func (r *Report) name(key string) string {
	if n := r.repoNames[key]; n != "" {
		return n
	}
	return key
}

// ExitCode maps the status to 0 (ok), 1 (warnings) or 2 (errors).
func (r *Report) ExitCode() int {
	switch r.Status {
	case StatusError:
		return 2
	case StatusWarning:
		return 1
	}
	return 0
}

// --- comparison ---

type comparer struct {
	mu     sync.Mutex // analyse runs from several workers
	r      *Report
	c      *Comparison
	moved  map[string][]int // repo key (current backup) -> indexes in c.Refs needing analysis
	ignore []string         // --ignore-refs
}

func newComparer(r *Report, prev, cur *Snapshot, ignore []string) *comparer {
	c := &Comparison{Previous: prev.Summary,
		ProjectsAdded: []string{}, ProjectsRemoved: []string{}, ProjectsRenamed: []Rename{},
		ReposLost: []string{}, Refs: []RefChange{}, CountDrops: []CountDrop{}}
	cmp := &comparer{r: r, c: c, moved: map[string][]int{}, ignore: ignore}
	r.Comparison = c

	type pair struct{ old, new *SnapProject }
	useIDs := prev.Summary.Database == "ok" && cur.Summary.Database == "ok"
	key := func(p *SnapProject) string {
		if useIDs && !p.Unmatched {
			return "id:" + strconv.FormatInt(p.ID, 10)
		}
		return "repo:" + p.Repo
	}
	pairs := map[string]*pair{}
	for i := range prev.Projects {
		p := &prev.Projects[i]
		pairs[key(p)] = &pair{old: p}
	}
	for i := range cur.Projects {
		p := &cur.Projects[i]
		k := key(p)
		if pr, ok := pairs[k]; ok {
			pr.new = p
		} else {
			pairs[k] = &pair{new: p}
		}
	}
	ks := make([]string, 0, len(pairs))
	for k := range pairs {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		pr := pairs[k]
		switch {
		case pr.old == nil:
			if !pr.new.Unmatched {
				c.ProjectsAdded = append(c.ProjectsAdded, pr.new.Path)
			}
		case pr.new == nil:
			if !pr.old.Unmatched {
				c.ProjectsRemoved = append(c.ProjectsRemoved, pr.old.Path)
				r.add(Warning, "project_removed", pr.old.Path, "present in the previous backup, missing now")
			}
		default:
			cmp.compareProject(pr.old, pr.new)
		}
	}
	c.IssuesDelta = cur.Summary.Issues - prev.Summary.Issues
	c.MRsDelta = cur.Summary.MergeRequests - prev.Summary.MergeRequests
	if prev.Summary.IssuesKnown && cur.Summary.IssuesKnown {
		if c.IssuesDelta < 0 {
			r.add(Warning, "issues_decreased", "", "%d fewer issues than in the previous backup", -c.IssuesDelta)
		}
		if c.MRsDelta < 0 {
			r.add(Warning, "merge_requests_decreased", "", "%d fewer merge requests than in the previous backup", -c.MRsDelta)
		}
	}
	return cmp
}

func (cmp *comparer) compareProject(old, cur *SnapProject) {
	r, c := cmp.r, cmp.c
	if old.Path != cur.Path && !old.Unmatched {
		c.ProjectsRenamed = append(c.ProjectsRenamed, Rename{ID: cur.ID, From: old.Path, To: cur.Path})
	}
	if old.Repo != "" && cur.Repo == "" {
		c.ReposLost = append(c.ReposLost, cur.Path)
		r.add(Warning, "repository_lost", cur.Path, "had a repository bundle in the previous backup, has none now")
	}
	if old.Issues > cur.Issues {
		c.CountDrops = append(c.CountDrops, CountDrop{cur.Path, "issues", old.Issues, cur.Issues})
	}
	if old.MRs > cur.MRs {
		c.CountDrops = append(c.CountDrops, CountDrop{cur.Path, "merge_requests", old.MRs, cur.MRs})
	}
	if old.Repo == "" || cur.Repo == "" || old.Refs == nil || cur.Refs == nil {
		return
	}
	names := make([]string, 0, len(old.Refs))
	for n := range old.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		o, nw := old.Refs[n], cur.Refs[n]
		switch {
		case nw == "" && cur.mergedSource(n):
			// GitLab deletes a merge request's source branch after merging:
			// expected every night, so not a warning.
			c.Refs = append(c.Refs, RefChange{Project: cur.Path, Ref: n, Change: "deleted-merged", Old: o})
		case nw == "":
			c.Refs = append(c.Refs, RefChange{Project: cur.Path, Ref: n, Change: "deleted", Old: o})
			cmp.warnRef(&c.Refs[len(c.Refs)-1], "ref_deleted", "%s was deleted (was %s)", n, short(o))
		case nw != o && strings.HasPrefix(n, "refs/tags/"):
			c.Refs = append(c.Refs, RefChange{Project: cur.Path, Ref: n, Change: "moved", Old: o, New: nw})
			cmp.warnRef(&c.Refs[len(c.Refs)-1], "tag_moved", "tag %s now points to %s (was %s)", strings.TrimPrefix(n, "refs/tags/"), short(nw), short(o))
		case nw != o:
			c.Refs = append(c.Refs, RefChange{Project: cur.Path, Ref: n, Change: "moved", Old: o, New: nw})
			cmp.moved[cur.Repo] = append(cmp.moved[cur.Repo], len(c.Refs)-1)
		}
	}
	for n, nw := range cur.Refs {
		if _, ok := old.Refs[n]; !ok {
			c.Refs = append(c.Refs, RefChange{Project: cur.Path, Ref: n, Change: "added", New: nw})
		}
	}
}

// warnRef reports a ref change as a warning, unless --ignore-refs matches it.
func (cmp *comparer) warnRef(rc *RefChange, kind, format string, args ...any) {
	if refIgnored(cmp.ignore, rc.Project, rc.Ref) {
		rc.Ignored = true
		return
	}
	cmp.r.add(Warning, kind, rc.Project, format, args...)
}

// refIgnored matches a ref against --ignore-refs patterns: "renovate/*"
// (branch or tag name, any project) or "group/project:pattern". "*"
// matches anything, slashes included.
func refIgnored(patterns []string, project, ref string) bool {
	name := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	for _, p := range patterns {
		proj, refPat, ok := strings.Cut(p, ":")
		if !ok {
			proj, refPat = "*", p
		}
		if wildcard(proj, project) && (wildcard(refPat, name) || wildcard(refPat, ref)) {
			return true
		}
	}
	return false
}

// wildcard matches s against a pattern where * stands for any text.
func wildcard(pat, s string) bool {
	parts := strings.Split(pat, "*")
	if len(parts) == 1 {
		return pat == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// analyse classifies moved branches using the freshly restored repository.
func (cmp *comparer) analyse(key, dir string) {
	cmp.mu.Lock()
	idxs := cmp.moved[key]
	delete(cmp.moved, key)
	cmp.mu.Unlock()
	if len(idxs) == 0 {
		return
	}
	repo, err := gitstore.Open(dir)
	if err != nil {
		return
	}
	defer repo.Close()
	for _, i := range idxs {
		rc := &cmp.c.Refs[i]
		oldH, e1 := gitstore.ParseHash(rc.Old)
		newH, e2 := gitstore.ParseHash(rc.New)
		if e1 != nil || e2 != nil {
			continue
		}
		branch := strings.TrimPrefix(rc.Ref, "refs/heads/")
		if !repo.Has(oldH) {
			rc.Change = "rewritten"
			cmp.warnRef(rc, "ref_rewritten", "branch %s: its previous commit %s is no longer in the backup (history rewritten or backup truncated)", branch, short(rc.Old))
			continue
		}
		ok, done := repo.IsAncestor(oldH, newH, 500000)
		switch {
		case ok:
			rc.Change = "fast-forward"
		default:
			if back, _ := repo.IsAncestor(newH, oldH, 500000); back {
				rc.Change = "rewound"
				cmp.warnRef(rc, "ref_rewound", "branch %s went back from %s to the older %s", branch, short(rc.Old), short(rc.New))
			} else if done {
				rc.Change = "diverged"
				cmp.warnRef(rc, "ref_diverged", "branch %s was force-pushed (%s is not an ancestor of %s)", branch, short(rc.Old), short(rc.New))
			}
		}
	}
}

func (cmp *comparer) finish(deep bool) {
	cmp.c.RefsAnalysed = deep
	sort.Slice(cmp.c.Refs, func(a, b int) bool {
		x, y := cmp.c.Refs[a], cmp.c.Refs[b]
		if x.Project != y.Project {
			return x.Project < y.Project
		}
		return x.Ref < y.Ref
	})
	for _, d := range cmp.c.CountDrops {
		cmp.r.add(Warning, d.What+"_decreased", d.Project, "%d %s in the previous backup, %d now", d.From, strings.ReplaceAll(d.What, "_", " "), d.To)
	}
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

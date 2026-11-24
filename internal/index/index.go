// Package index builds and persists the catalogue of a GitLab backup:
// groups, projects and the repository bundles that belong to them.
package index

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/pgdump"
	"github.com/movenial/gitlab-backup-browser/internal/readahead"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

// FormatVersion is bumped whenever the on-disk index changes shape.
const FormatVersion = 2

// Namespace is a GitLab group or user namespace.
type Namespace struct {
	ID          int64  `json:"id"`
	ParentID    int64  `json:"parent_id,omitempty"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	FullPath    string `json:"full_path"`
	Type        string `json:"type,omitempty"` // Group, User, Project
	OwnerID     int64  `json:"owner_id,omitempty"`
	Kind        string `json:"kind,omitempty"` // group, user, bot
	Description string `json:"description,omitempty"`
}

// Project is a GitLab project with its backed-up repositories.
type Project struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	FullPath     string `json:"full_path"`
	NamespaceID  int64  `json:"namespace_id,omitempty"`
	Description  string `json:"description,omitempty"`
	Archived     bool   `json:"archived,omitempty"`
	Visibility   int    `json:"visibility"`
	PendingDel   bool   `json:"pending_delete,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	LastActivity string `json:"last_activity_at,omitempty"`
	DiskPath     string `json:"disk_path,omitempty"`
	Repo         string `json:"repo,omitempty"`   // RepoBackup key of the code repo
	Wiki         string `json:"wiki,omitempty"`   // key of the wiki repo
	Design       string `json:"design,omitempty"` // key of the design repo
	Unmatched    bool   `json:"unmatched,omitempty"`
	Issues       int    `json:"issues,omitempty"`
	OpenIssues   int    `json:"open_issues,omitempty"`
	// Confidential issues, also counted in Issues and OpenIssues.
	ConfidentialIssues     int `json:"confidential_issues,omitempty"`
	OpenConfidentialIssues int `json:"open_confidential_issues,omitempty"`
	MRs                    int `json:"mrs,omitempty"`
	OpenMRs                int `json:"open_mrs,omitempty"`
	Releases               int `json:"releases,omitempty"`
	// Kind is "group" (in a group), "personal" (a person's namespace), "bot"
	// (a bot or service account's namespace) or "unmatched".
	Kind     string   `json:"kind,omitempty"`
	Topics   []string `json:"topics,omitempty"`
	Size     int64    `json:"size,omitempty"` // bytes of the code repository's bundles
	HasLFS   bool     `json:"lfs,omitempty"`  // the database links LFS objects to it
	Snippets int      `json:"snippets,omitempty"`
}

// Snippet is a project or personal snippet. Its files live in a
// repository (GitLab 13+), found under @snippets/ in the backup.
type Snippet struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	FileName    string `json:"file_name,omitempty"`
	Author      int64  `json:"author,omitempty"`
	ProjectID   int64  `json:"project_id,omitempty"`
	Project     string `json:"project,omitempty"` // full path, "" for personal snippets
	Visibility  int    `json:"visibility"`
	Created     string `json:"created,omitempty"`
	Updated     string `json:"updated,omitempty"`
	Repo        string `json:"repo,omitempty"` // repository key, "" when not in the backup
}

// VisibilityName is private, internal or public.
func (s *Snippet) VisibilityName() string { return visibilityName(s.Visibility) }

// VisibilityName returns private/internal/public.
func (p *Project) VisibilityName() string { return visibilityName(p.Visibility) }

func visibilityName(level int) string {
	switch level {
	case 20:
		return "public"
	case 10:
		return "internal"
	}
	return "private"
}

// Snippet returns a snippet by id, or nil.
func (ix *Index) Snippet(id int64) *Snippet {
	for _, s := range ix.Snippets {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// snippetPath is where GitLab stores a snippet's repository.
func snippetPath(id int64) string {
	return "@snippets/" + strings.TrimPrefix(hashedPath(id), "@hashed/")
}

// Index is the complete catalogue.
type Index struct {
	Snippets []*Snippet `json:"snippets,omitempty"`
	// Schema: tables and columns gbb reads that the dump lacks (or has
	// under an older name), see Schema.
	Schema []SchemaNote `json:"schema,omitempty"`
	// LFSKnown: the dump had lfs_objects_projects, so Project.HasLFS is
	// meaningful.
	LFSKnown    bool                          `json:"lfs_known,omitempty"`
	Version     int                           `json:"version"`
	Fingerprint string                        `json:"fingerprint"`
	BuiltAt     time.Time                     `json:"built_at"`
	Info        map[string]string             `json:"info,omitempty"` // backup_information.yml
	DBStatus    string                        `json:"db_status"`      // "ok", "missing", or an error
	Tracker     bool                          `json:"tracker"`        // issues and MRs were imported
	Namespaces  []*Namespace                  `json:"namespaces"`
	Projects    []*Project                    `json:"projects"`
	Repos       map[string]*backup.RepoBackup `json:"repos"`

	byPath   map[string]*Project
	nsByPath map[string]*Namespace
	nsByID   map[int64]*Namespace
}

// Finish builds lookup tables; call after loading or building.
func (ix *Index) Finish() {
	ix.byPath = map[string]*Project{}
	ix.nsByPath = map[string]*Namespace{}
	ix.nsByID = map[int64]*Namespace{}
	for _, p := range ix.Projects {
		ix.byPath[strings.ToLower(p.FullPath)] = p
		if rb := ix.Repos[p.Repo]; rb != nil && p.Repo != "" {
			p.Size = rb.Size
		}
	}
	for _, n := range ix.Namespaces {
		ix.nsByPath[strings.ToLower(n.FullPath)] = n
		ix.nsByID[n.ID] = n
	}
	sort.Slice(ix.Projects, func(i, j int) bool { return ix.Projects[i].FullPath < ix.Projects[j].FullPath })
	sort.Slice(ix.Namespaces, func(i, j int) bool {
		a, b := ix.Namespaces[i], ix.Namespaces[j]
		if (a.ID == -1) != (b.ID == -1) { // the synthetic _unmatched group goes last
			return b.ID == -1
		}
		return strings.ToLower(a.FullPath) < strings.ToLower(b.FullPath)
	})
}

// Project looks a project up by full path (case-insensitive).
func (ix *Index) Project(fullPath string) *Project {
	return ix.byPath[strings.ToLower(strings.Trim(fullPath, "/"))]
}

// Namespace looks a namespace up by full path.
// NamespaceByID returns a namespace by database id, or nil.
func (ix *Index) NamespaceByID(id int64) *Namespace { return ix.nsByID[id] }

func (ix *Index) Namespace(fullPath string) *Namespace {
	return ix.nsByPath[strings.ToLower(strings.Trim(fullPath, "/"))]
}

// Children returns direct sub-namespaces and projects of a namespace
// path ("" for the top level).
func (ix *Index) Children(fullPath string) ([]*Namespace, []*Project) {
	fullPath = strings.ToLower(strings.Trim(fullPath, "/"))
	var ns []*Namespace
	var ps []*Project
	parentOf := func(p string) string {
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			return strings.ToLower(p[:i])
		}
		return ""
	}
	for _, n := range ix.Namespaces {
		if parentOf(n.FullPath) == fullPath && n.FullPath != "" {
			ns = append(ns, n)
		}
	}
	for _, p := range ix.Projects {
		if parentOf(p.FullPath) == fullPath {
			ps = append(ps, p)
		}
	}
	return ns, ps
}

// Stats summarises the index.
type Stats struct {
	Projects, WithRepo, WithoutRepo, Unmatched, Wikis, Namespaces int
	BundleBytes                                                   int64
}

// Stats computes summary numbers.
func (ix *Index) Stats() Stats {
	var s Stats
	s.Namespaces = len(ix.Namespaces)
	for _, p := range ix.Projects {
		switch {
		case p.Unmatched:
			s.Unmatched++
		case p.Repo != "":
			s.Projects++
			s.WithRepo++
		default:
			s.Projects++
			s.WithoutRepo++
		}
		if p.Wiki != "" {
			s.Wikis++
		}
	}
	for _, r := range ix.Repos {
		s.BundleBytes += r.Size
	}
	return s
}

// Progress reports import progress.
type Progress func(phase string, done, total int64)

// Throttle is called periodically during long phases; it may block (for
// example while a phone is running on battery).
type Throttle func()

// Options tune Build.
type Options struct {
	// TrackerDir, when set, receives issues, merge requests, comments and
	// user names (see package tracker).
	TrackerDir string
	// ExpectedFile, when set, receives the LFS objects and attachments the
	// database lists (see Expected), for backup checks.
	ExpectedFile string
}

// Build scans src and parses its database dump.
func Build(src backup.Source, fingerprint string, opts Options, progress Progress, throttle Throttle) (*Index, error) {
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	ix := &Index{Version: FormatVersion, Fingerprint: fingerprint, BuiltAt: time.Now().UTC()}
	progress("scanning repositories", 0, 0)
	ix.Repos = backup.Layout(src.Entries())

	if rc, err := src.Open("backup_information.yml"); err == nil {
		ix.Info = backup.BackupInfo(rc)
		rc.Close()
	}

	dbName := "db/database.sql.gz"
	var dbSize int64
	for _, e := range src.Entries() {
		if e.Name == dbName {
			dbSize = e.Size
		}
	}
	var h *handler
	if !src.Has(dbName) {
		ix.DBStatus = "missing"
	} else {
		h = newHandler()
		var tmp string
		if opts.TrackerDir != "" {
			tmp = opts.TrackerDir + ".partial"
			os.RemoveAll(tmp)
			tw, err := tracker.NewWriter(tmp)
			if err != nil {
				return nil, err
			}
			tw.Async() // encode and write in parallel with parsing
			h.tw = tw
		}
		err := parseDump(src, dbName, dbSize, h, progress, throttle)
		var counts map[int64]tracker.Counts
		if h.tw != nil {
			c, cerr := h.tw.Close()
			if err == nil && cerr == nil {
				os.RemoveAll(opts.TrackerDir)
				cerr = os.Rename(tmp, opts.TrackerDir)
			}
			if cerr != nil {
				os.RemoveAll(tmp)
				if err == nil {
					err = fmt.Errorf("writing issues: %w", cerr)
				}
			} else if err == nil {
				counts, ix.Tracker = c, true
			}
		}
		if err != nil {
			ix.DBStatus = err.Error()
		} else {
			ix.DBStatus = "ok"
			ix.Schema = schemaNotes(h.seen, h.tw != nil)
		}
		defer func() {
			for _, p := range ix.Projects {
				if c, ok := counts[p.ID]; ok && p.ID != 0 {
					p.Issues, p.OpenIssues, p.MRs, p.OpenMRs, p.Releases = c.Issues, c.OpenIssues, c.MRs, c.OpenMRs, c.Releases
					p.ConfidentialIssues, p.OpenConfidentialIssues = c.ConfidentialIssues, c.OpenConfidentialIssues
				}
			}
		}()
	}
	ix.join(h)
	ix.Finish()
	if opts.ExpectedFile != "" && h != nil {
		if err := WriteExpected(opts.ExpectedFile, h.expected(ix)); err != nil {
			return nil, err
		}
	}
	return ix, nil
}

// Partial returns an index with only the repositories (all unmatched),
// used while the database is still being read.
func Partial(src backup.Source, fingerprint string) *Index {
	ix := &Index{Version: FormatVersion, Fingerprint: fingerprint, BuiltAt: time.Now().UTC(), DBStatus: "pending"}
	ix.Repos = backup.Layout(src.Entries())
	ix.join(nil)
	ix.Finish()
	return ix
}

func parseDump(src backup.Source, name string, size int64, h *handler, progress Progress, throttle Throttle) error {
	rc, err := src.Open(name)
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := &progressReader{r: rc, total: size, cb: func(done int64) {
		progress("reading database", done, size)
		if throttle != nil {
			throttle()
		}
	}}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		return fmt.Errorf("database dump: %w", err)
	}
	defer gz.Close()
	// Decompress in its own goroutine while this one parses: three stages
	// (inflate, parse, encode issues) run in parallel on a multi-core CPU.
	ra := readahead.New(gz, 256<<10, 4)
	defer ra.Close()
	if err := pgdump.Parse(ra, h); err != nil {
		return err
	}
	progress("reading database", size, size)
	return nil
}

type progressReader struct {
	r           io.Reader
	done, total int64
	last        int64
	cb          func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.done-p.last >= 4<<20 {
		p.last = p.done
		p.cb(p.done)
	}
	return n, err
}

// hashedPath is GitLab's hashed storage path for a project id.
func hashedPath(id int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(id)))
	h := hex.EncodeToString(sum[:])
	return "@hashed/" + h[:2] + "/" + h[2:4] + "/" + h
}

// join links database rows with repository bundles.
func (ix *Index) join(h *handler) {
	used := map[string]bool{}
	if h != nil {
		ix.LFSKnown = h.sawLFS
		nsPaths := map[int64]string{}
		for _, r := range h.routes {
			if r.sourceType == "Namespace" {
				nsPaths[r.sourceID] = r.path
			}
		}
		projPaths := map[int64]string{}
		for _, r := range h.routes {
			if r.sourceType == "Project" {
				projPaths[r.sourceID] = r.path
			}
		}
		for _, n := range h.namespaces {
			n.FullPath = nsPaths[n.ID]
			if n.FullPath == "" {
				n.FullPath = n.Path
			}
			if d := h.nsDescriptions[n.ID]; n.Description == "" && d != "" { // GitLab 19: in namespace_details
				n.Description = d
			}
			if n.Type == "Project" { // project namespaces (GitLab 15+) mirror projects
				continue
			}
			switch {
			case n.Type == "Group":
				n.Kind = "group"
			case h.botUsers[n.OwnerID]:
				n.Kind = "bot"
			default: // "User", or NULL on old GitLab versions
				n.Kind = "user"
			}
			ix.Namespaces = append(ix.Namespaces, n)
		}
		nsByID := map[int64]*Namespace{}
		for _, n := range h.namespaces {
			nsByID[n.ID] = n
		}
		for _, p := range h.projects {
			p.FullPath = projPaths[p.ID]
			if p.FullPath == "" {
				if ns := nsByID[p.NamespaceID]; ns != nil {
					p.FullPath = ns.FullPath + "/" + p.Path
				} else {
					p.FullPath = p.Path
				}
			}
			if dp, ok := h.diskPaths[p.ID]; ok {
				p.DiskPath = dp
			}
			p.Kind = "group"
			if ns := nsByID[p.NamespaceID]; ns != nil {
				switch ns.Kind {
				case "user":
					p.Kind = "personal"
				case "bot":
					p.Kind = "bot"
				}
			}
			for _, tid := range h.projTopics[p.ID] {
				if name := h.topics[tid]; name != "" {
					p.Topics = append(p.Topics, name)
				}
			}
			sort.Strings(p.Topics)
			p.HasLFS = h.lfsProjects[p.ID]
			candidates := []string{p.DiskPath, hashedPath(p.ID), p.FullPath}
			for _, c := range candidates {
				if c == "" {
					continue
				}
				if _, ok := ix.Repos[c]; ok && p.Repo == "" {
					p.Repo = c
				}
				if _, ok := ix.Repos[c+".wiki"]; ok && p.Wiki == "" {
					p.Wiki = c + ".wiki"
				}
				if _, ok := ix.Repos[c+".design"]; ok && p.Design == "" {
					p.Design = c + ".design"
				}
			}
			for _, k := range []string{p.Repo, p.Wiki, p.Design} {
				if k != "" {
					used[k] = true
				}
			}
			ix.Projects = append(ix.Projects, p)
		}
	}
	if h != nil {
		byID := map[int64]*Project{}
		for _, p := range ix.Projects {
			if p.ID != 0 {
				byID[p.ID] = p
			}
		}
		for _, sn := range h.snippets {
			for _, k := range []string{h.snippetDisk[sn.ID], snippetPath(sn.ID)} {
				if _, ok := ix.Repos[k]; ok && k != "" && sn.Repo == "" {
					sn.Repo, used[k] = k, true
				}
			}
			if p := byID[sn.ProjectID]; p != nil && sn.ProjectID != 0 {
				sn.Project = p.FullPath
				p.Snippets++
			}
			ix.Snippets = append(ix.Snippets, sn)
		}
		sort.Slice(ix.Snippets, func(i, j int) bool { return ix.Snippets[i].ID > ix.Snippets[j].ID })
	}
	// Bundles nobody claimed are still browsable under _unmatched/.
	var keys []string
	for k := range ix.Repos {
		if !used[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasSuffix(k, ".design") {
			continue
		}
		name := k
		if i := strings.LastIndexByte(k, '/'); i >= 0 {
			name = k[i+1:]
		}
		short := name
		if len(short) > 16 && !strings.Contains(short, ".") {
			short = short[:16]
		}
		ix.Projects = append(ix.Projects, &Project{
			Name: name, Path: short, FullPath: "_unmatched/" + short, Repo: k, Unmatched: true, Kind: "unmatched",
		})
	}
	if len(keys) > 0 {
		ix.Namespaces = append(ix.Namespaces, &Namespace{ID: -1, Name: "Unmatched repositories", Path: "_unmatched", FullPath: "_unmatched",
			Description: "Bundles found in the backup that no project in the database points to (or the database could not be read)."})
	}
}

// Save writes the index atomically.
func (ix *Index) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(ix); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a saved index; it returns nil, nil when missing or outdated.
func Load(path, fingerprint string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var ix Index
	if err := json.NewDecoder(f).Decode(&ix); err != nil {
		return nil, nil // corrupt: rebuild
	}
	if ix.Version != FormatVersion || ix.Fingerprint != fingerprint || !ix.complete() {
		return nil, nil
	}
	ix.Finish()
	return &ix, nil
}

// complete reports whether a loaded index has no null entries, which a
// damaged file could hold and lookups would trip on.
func (ix *Index) complete() bool {
	for _, p := range ix.Projects {
		if p == nil {
			return false
		}
	}
	for _, n := range ix.Namespaces {
		if n == nil {
			return false
		}
	}
	for _, r := range ix.Repos {
		if r == nil {
			return false
		}
	}
	for _, s := range ix.Snippets {
		if s == nil {
			return false
		}
	}
	return true
}

// Fingerprint identifies a backup path by size and modification time.
func Fingerprint(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s|%d|%d", abs, st.Size(), st.ModTime().UnixNano()), nil
}

// Topics returns every project topic with its number of projects.
func (ix *Index) Topics() []TopicCount {
	counts := map[string]int{}
	for _, p := range ix.Projects {
		for _, t := range p.Topics {
			counts[t]++
		}
	}
	out := make([]TopicCount, 0, len(counts))
	for t, n := range counts {
		out = append(out, TopicCount{t, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// TopicCount is a topic and how many projects carry it.
type TopicCount struct {
	Name  string
	Count int
}

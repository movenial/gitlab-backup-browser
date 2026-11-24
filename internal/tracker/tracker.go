// Package tracker stores the issues, merge requests and comments of a
// backup on disk, one directory per project, so they can be shown without
// keeping them in memory.
//
// Layout:
//
//	<dir>/users.json
//	<dir>/labels.json                   all labels (project and group)
//	<dir>/milestones.json               all milestones (project and group)
//	<dir>/<project_id>/labels.json      label ids per issue and MR iid
//	<dir>/<project_id>/releases.jsonl
//	<dir>/<project_id>/release_links.json  asset links per release id
//	<dir>/<project_id>/issues.jsonl
//	<dir>/<project_id>/merge_requests.jsonl
//	<dir>/<project_id>/notes.jsonl
package tracker

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Issue is a project issue.
type Issue struct {
	ID           int64   `json:"id"`
	IID          int64   `json:"iid"`
	Title        string  `json:"title"`
	Description  string  `json:"description,omitempty"`
	State        string  `json:"state"` // opened, closed
	Author       int64   `json:"author,omitempty"`
	Created      string  `json:"created,omitempty"`
	Updated      string  `json:"updated,omitempty"`
	Closed       string  `json:"closed,omitempty"`
	Confidential bool    `json:"confidential,omitempty"`
	Milestone    int64   `json:"milestone,omitempty"`
	Labels       []int64 `json:"-"` // filled when read
}

// MergeRequest is a project merge request.
type MergeRequest struct {
	ID            int64   `json:"id"`
	IID           int64   `json:"iid"`
	Title         string  `json:"title"`
	Description   string  `json:"description,omitempty"`
	State         string  `json:"state"` // opened, closed, merged, locked
	Author        int64   `json:"author,omitempty"`
	Source        string  `json:"source"`
	Target        string  `json:"target"`
	SourceProject int64   `json:"source_project,omitempty"`
	Created       string  `json:"created,omitempty"`
	Updated       string  `json:"updated,omitempty"`
	Draft         bool    `json:"draft,omitempty"`
	MergeSHA      string  `json:"merge_sha,omitempty"`
	BaseSHA       string  `json:"base_sha,omitempty"` // from the latest merge_request_diffs row
	HeadSHA       string  `json:"head_sha,omitempty"`
	Milestone     int64   `json:"milestone,omitempty"`
	Labels        []int64 `json:"-"` // filled when read
}

// Release is a project release (attached to a tag).
type Release struct {
	ID          int64         `json:"id"`
	Tag         string        `json:"tag"`
	Name        string        `json:"name,omitempty"`
	Description string        `json:"description,omitempty"`
	Author      int64         `json:"author,omitempty"`
	Created     string        `json:"created,omitempty"`
	Released    string        `json:"released,omitempty"`
	SHA         string        `json:"sha,omitempty"`
	Links       []ReleaseLink `json:"-"` // filled when read
}

// ReleaseLink is an asset link of a release.
type ReleaseLink struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Filepath string `json:"filepath,omitempty"`
	Type     string `json:"type,omitempty"` // other, runbook, image, package
}

// Label is a project or group label.
type Label struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
	ProjectID   int64  `json:"project,omitempty"`
	GroupID     int64  `json:"group,omitempty"`
}

// Milestone is a project or group milestone.
type Milestone struct {
	ID          int64  `json:"id"`
	IID         int64  `json:"iid,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"` // active, closed
	Start       string `json:"start,omitempty"`
	Due         string `json:"due,omitempty"`
	ProjectID   int64  `json:"project,omitempty"`
	GroupID     int64  `json:"group,omitempty"`
}

// Note is a comment (or a system note) on an issue, MR or commit.
type Note struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`             // Issue, MergeRequest, Commit, ...
	Target   int64  `json:"target,omitempty"` // noteable_id (issue/MR id, not iid)
	CommitID string `json:"commit,omitempty"`
	Author   int64  `json:"author,omitempty"`
	Body     string `json:"body"`
	Created  string `json:"created,omitempty"`
	System   bool   `json:"system,omitempty"`
	Internal bool   `json:"internal,omitempty"`
}

// User is the public part of a user row (no e-mail, no credentials).
type User struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

// Counts summarises one project. Confidential issues are counted in
// Issues and OpenIssues too.
type Counts struct {
	Issues, OpenIssues, MRs, OpenMRs, Releases int
	ConfidentialIssues, OpenConfidentialIssues int
}

// IssueState maps issues.state_id.
func IssueState(id int64) string {
	if id == 2 {
		return "closed"
	}
	return "opened"
}

// MRState maps merge_requests.state_id.
func MRState(id int64) string {
	switch id {
	case 2:
		return "closed"
	case 3:
		return "merged"
	case 4:
		return "locked"
	}
	return "opened"
}

// --- writing ---

// Writer streams records into per-project files while the dump is read.
//
// Records of many projects arrive interleaved (comments are dumped in id
// order), so writing each one straight to its file would open and close
// files constantly. Instead records are accumulated per file in memory,
// within a fixed budget; when the budget is exceeded the largest buffers
// are appended to their files in one write each.
type Writer struct {
	dir     string
	pending map[string]*bytes.Buffer // "<pid>/<kind>" -> JSON lines not yet written
	total   int                      // bytes in pending
	budget  int
	mrOwner map[int64]mrRef  // MR id -> project and iid (to file its diff)
	diffs   map[int64]mrDiff // latest diff per MR id (compact: no strings)
	// Labels are linked to issues and MRs by database id in label_links,
	// which may come before or after them in the dump: the links are kept
	// (two ints each) and filed per project on Close.
	issueOwner map[int64]mrRef
	issueLinks map[int64][]int64 // issue id -> label ids
	mrLinks    map[int64][]int64 // MR id -> label ids
	labels     []Label
	milestones []Milestone
	// Release links may come before their releases (release_links sorts
	// first): kept per release id and filed per project on Close.
	releaseOwner map[int64]int64 // release id -> project id
	releaseLinks map[int64][]ReleaseLink
	users        map[int64]User
	counts       map[int64]*Counts
	err          error
	// Stats: how often buffers were written out (for tests/benchmarks).
	Flushes int

	// Background encoding (see Async).
	queue chan []record
	batch []record
	done  chan struct{}
}

type record struct {
	pid  int64
	kind string
	v    any
}

const batchSize = 256

// Async moves JSON encoding and file writes to a background goroutine, so
// they overlap with reading the dump. Records keep their order. Only the
// batches in flight (a few hundred records) are added to memory.
func (w *Writer) Async() {
	w.queue = make(chan []record, 4)
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		for b := range w.queue {
			for _, r := range b {
				w.encode(r.pid, r.kind, r.v)
			}
		}
	}()
}

// DefaultBudget is the memory used to batch writes.
const DefaultBudget = 8 << 20

type mrRef struct{ pid, iid int64 }

type mrDiff struct {
	id         int64
	base, head [20]byte
	ok         bool
}

func shaBytes(s string) ([20]byte, bool) {
	var b [20]byte
	if len(s) != 40 {
		return b, false
	}
	_, err := hex.Decode(b[:], []byte(s))
	return b, err == nil
}

// NewWriter writes into dir (which must not exist yet).
func NewWriter(dir string) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Writer{
		dir: dir, pending: map[string]*bytes.Buffer{}, budget: DefaultBudget,
		mrOwner: map[int64]mrRef{}, diffs: map[int64]mrDiff{},
		issueOwner: map[int64]mrRef{}, issueLinks: map[int64][]int64{}, mrLinks: map[int64][]int64{},
		releaseOwner: map[int64]int64{}, releaseLinks: map[int64][]ReleaseLink{},
		users: map[int64]User{}, counts: map[int64]*Counts{},
	}, nil
}

func (w *Writer) count(pid int64) *Counts {
	c := w.counts[pid]
	if c == nil {
		c = &Counts{}
		w.counts[pid] = c
	}
	return c
}

func (w *Writer) append(pid int64, kind string, v any) {
	if w.queue != nil {
		w.batch = append(w.batch, record{pid, kind, v})
		if len(w.batch) >= batchSize {
			w.queue <- w.batch
			w.batch = make([]record, 0, batchSize)
		}
		return
	}
	w.encode(pid, kind, v)
}

// encode appends one record to its pending buffer (only ever called from
// one goroutine at a time).
func (w *Writer) encode(pid int64, kind string, v any) {
	if w.err != nil {
		return
	}
	name := key(pid) + "/" + kind
	buf := w.pending[name]
	if buf == nil {
		buf = &bytes.Buffer{}
		w.pending[name] = buf
	}
	before := buf.Len()
	enc := json.NewEncoder(buf) // Encode appends the newline
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		w.err = err
		return
	}
	w.total += buf.Len() - before
	if w.total > w.budget {
		w.spill()
	}
}

// spill writes the largest buffers until half the budget is free.
func (w *Writer) spill() {
	type kv struct {
		key string
		n   int
	}
	var all []kv
	for k, b := range w.pending {
		all = append(all, kv{k, b.Len()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
	for _, e := range all {
		if w.total <= w.budget/2 || w.err != nil {
			break
		}
		w.writeOut(e.key)
	}
}

// writeOut appends one pending buffer to its file and forgets it.
func (w *Writer) writeOut(name string) {
	buf := w.pending[name]
	delete(w.pending, name)
	if buf == nil || buf.Len() == 0 {
		return
	}
	w.total -= buf.Len()
	w.Flushes++
	p := filepath.Join(w.dir, name+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.err = err
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		w.err = err
		return
	}
	_, err = f.Write(buf.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		w.err = err
	}
}

// AddIssue records an issue of project pid.
func (w *Writer) AddIssue(pid int64, is *Issue) {
	c := w.count(pid)
	c.Issues++
	if is.State == "opened" {
		c.OpenIssues++
	}
	if is.Confidential {
		c.ConfidentialIssues++
		if is.State == "opened" {
			c.OpenConfidentialIssues++
		}
	}
	w.issueOwner[is.ID] = mrRef{pid, is.IID}
	w.append(pid, "issues", is)
}

// AddRelease records a release of project pid.
func (w *Writer) AddRelease(pid int64, r *Release) {
	w.count(pid).Releases++
	w.releaseOwner[r.ID] = pid
	w.append(pid, "releases", r)
}

// AddReleaseLink records an asset link of a release.
func (w *Writer) AddReleaseLink(releaseID int64, l ReleaseLink) {
	w.releaseLinks[releaseID] = append(w.releaseLinks[releaseID], l)
}

// AddLabel records a label definition.
func (w *Writer) AddLabel(l Label) { w.labels = append(w.labels, l) }

// AddMilestone records a milestone.
func (w *Writer) AddMilestone(m Milestone) { w.milestones = append(w.milestones, m) }

// AddLabelLink records that a label is set on an issue or merge request
// (targetType as in label_links: "Issue", "MergeRequest").
func (w *Writer) AddLabelLink(labelID int64, targetType string, targetID int64) {
	switch targetType {
	case "Issue":
		w.issueLinks[targetID] = append(w.issueLinks[targetID], labelID)
	case "MergeRequest":
		w.mrLinks[targetID] = append(w.mrLinks[targetID], labelID)
	}
}

// AddMR records a merge request. Its base/head commits are filled in on
// Close, because merge_request_diffs may come before or after it.
func (w *Writer) AddMR(pid int64, mr *MergeRequest) {
	c := w.count(pid)
	c.MRs++
	if mr.State == "opened" {
		c.OpenMRs++
	}
	w.mrOwner[mr.ID] = mrRef{pid, mr.IID}
	w.append(pid, "merge_requests", mr)
}

// AddMRDiff remembers the newest diff (base/head commits) of an MR.
func (w *Writer) AddMRDiff(diffID, mrID int64, base, head string) {
	if d, ok := w.diffs[mrID]; ok && diffID <= d.id {
		return
	}
	b, ok1 := shaBytes(base)
	h, ok2 := shaBytes(head)
	w.diffs[mrID] = mrDiff{id: diffID, base: b, head: h, ok: ok1 && ok2}
}

// AddNote records a comment of project pid.
func (w *Writer) AddNote(pid int64, n *Note) { w.append(pid, "notes", n) }

// AddUser records a user's public name.
func (w *Writer) AddUser(id int64, u User) { w.users[id] = u }

// Close flushes everything and returns the per-project counts.
func (w *Writer) Close() (map[int64]Counts, error) {
	if w.queue != nil {
		if len(w.batch) > 0 {
			w.queue <- w.batch
		}
		close(w.queue)
		<-w.done
		w.queue, w.batch = nil, nil
	}
	keys := make([]string, 0, len(w.pending))
	for k := range w.pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w.writeOut(k)
	}
	// Diff commits go to a small per-project file (iid -> [base, head])
	// rather than rewriting every merge request record.
	if w.err == nil {
		w.err = w.writeDiffs()
	}
	if w.err == nil {
		w.err = w.writeLabels()
	}
	if w.err == nil {
		w.err = w.writeReleaseLinks()
	}
	if w.err == nil {
		w.err = writeJSON(filepath.Join(w.dir, "users.json"), w.users)
	}
	out := map[int64]Counts{}
	for pid, c := range w.counts {
		out[pid] = *c
	}
	w.mrOwner, w.diffs, w.users = nil, nil, nil
	w.issueOwner, w.issueLinks, w.mrLinks, w.labels, w.milestones = nil, nil, nil, nil, nil
	w.releaseOwner, w.releaseLinks = nil, nil
	return out, w.err
}

// projectLabels is <pid>/labels.json: label ids per iid.
type projectLabels struct {
	Issues map[string][]int64 `json:"issues,omitempty"`
	MRs    map[string][]int64 `json:"merge_requests,omitempty"`
}

func (w *Writer) writeLabels() error {
	if err := writeJSON(filepath.Join(w.dir, "labels.json"), w.labels); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(w.dir, "milestones.json"), w.milestones); err != nil {
		return err
	}
	byProject := map[int64]*projectLabels{}
	get := func(pid int64) *projectLabels {
		pl := byProject[pid]
		if pl == nil {
			pl = &projectLabels{Issues: map[string][]int64{}, MRs: map[string][]int64{}}
			byProject[pid] = pl
		}
		return pl
	}
	for id, ls := range w.issueLinks {
		if ref, ok := w.issueOwner[id]; ok {
			get(ref.pid).Issues[key(ref.iid)] = ls
		}
	}
	for id, ls := range w.mrLinks {
		if ref, ok := w.mrOwner[id]; ok {
			get(ref.pid).MRs[key(ref.iid)] = ls
		}
	}
	return writePerProject(w.dir, "labels.json", byProject)
}

func (w *Writer) writeReleaseLinks() error {
	byProject := map[int64]map[string][]ReleaseLink{}
	for id, ls := range w.releaseLinks {
		pid, ok := w.releaseOwner[id]
		if !ok {
			continue
		}
		if byProject[pid] == nil {
			byProject[pid] = map[string][]ReleaseLink{}
		}
		byProject[pid][key(id)] = ls
	}
	return writePerProject(w.dir, "release_links.json", byProject)
}

// writePerProject writes one small JSON file per project: <dir>/<pid>/<name>.
// They are side tables (label links, MR commits...) filled from rows
// that may come before or after the records they belong to.
func writePerProject[V any](dir, name string, byProject map[int64]V) error {
	for pid, v := range byProject {
		d := filepath.Join(dir, key(pid))
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(d, name), v); err != nil {
			return err
		}
	}
	return nil
}

// readJSON decodes the file at p into v; false when missing or invalid
// (older caches lack the newer side files).
func readJSON(p string, v any) bool {
	b, err := os.ReadFile(p)
	return err == nil && json.Unmarshal(b, v) == nil
}

// projectFile is the path of a per-project file.
func (s *Store) projectFile(pid int64, name string) string {
	return filepath.Join(s.Dir, key(pid), name)
}

// key formats an id as a file or map key.
func key(id int64) string { return strconv.FormatInt(id, 10) }

func writeJSON(p string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func (w *Writer) writeDiffs() error {
	byProject := map[int64]map[string][2]string{}
	for mrID, d := range w.diffs {
		ref, ok := w.mrOwner[mrID]
		if !ok || !d.ok {
			continue
		}
		m := byProject[ref.pid]
		if m == nil {
			m = map[string][2]string{}
			byProject[ref.pid] = m
		}
		m[key(ref.iid)] = [2]string{hex.EncodeToString(d.base[:]), hex.EncodeToString(d.head[:])}
	}
	return writePerProject(w.dir, "mr_diffs.json", byProject)
}

// --- reading ---

// Store reads a tracker directory.
type Store struct {
	Dir        string
	Users      map[int64]User
	Labels     map[int64]*Label
	Milestones map[int64]*Milestone
}

// Open loads users.json; a missing directory yields an empty store.
func Open(dir string) (*Store, error) {
	s := &Store{Dir: dir, Users: map[int64]User{}, Labels: map[int64]*Label{}, Milestones: map[int64]*Milestone{}}
	// Labels and milestones are small (one line each); older caches lack them.
	var labels []*Label
	readJSON(filepath.Join(dir, "labels.json"), &labels)
	for _, l := range labels {
		s.Labels[l.ID] = l
	}
	var ms []*Milestone
	readJSON(filepath.Join(dir, "milestones.json"), &ms)
	for _, m := range ms {
		s.Milestones[m.ID] = m
	}
	b, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.Users); err != nil {
		return nil, fmt.Errorf("tracker: users.json: %w", err)
	}
	return s, nil
}

// UserName returns "Name (@username)" or a placeholder.
func (s *Store) UserName(id int64) string {
	if u, ok := s.Users[id]; ok {
		if u.Name != "" && u.Name != u.Username {
			return u.Name
		}
		return "@" + u.Username
	}
	if id == 0 {
		return "unknown"
	}
	return fmt.Sprintf("user #%d", id)
}

// each streams the JSON lines of a project file into fn.
func (s *Store) each(pid int64, kind string, fn func(line []byte) error) error {
	f, err := os.Open(filepath.Join(s.Dir, key(pid), kind+".jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 1 {
			if ferr := fn(line); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

var errStop = errors.New("stop")

// Query filters a list.
type Query struct {
	State     string // "", "opened", "closed", "merged", "all"
	Search    string
	Label     string // label title (case-insensitive); "none" = without labels
	Milestone string // milestone title; "none" = without milestone
	Offset    int
	Limit     int
	// HideConfidential leaves confidential issues out of the list and the
	// total.
	HideConfidential bool
}

func (q Query) match(state, title, desc string) bool {
	if q.State != "" && q.State != "all" && state != q.State {
		return false
	}
	if q.Search == "" {
		return true
	}
	hay := strings.ToLower(title + " " + desc)
	for _, t := range strings.Fields(strings.ToLower(q.Search)) {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// projectLabels reads <pid>/labels.json (empty when missing).
func (s *Store) projectLabels(pid int64) *projectLabels {
	pl := &projectLabels{}
	readJSON(s.projectFile(pid, "labels.json"), pl)
	return pl
}

// matchMeta applies the label and milestone filters.
func (s *Store) matchMeta(q Query, labels []int64, milestone int64) bool {
	if q.Label != "" {
		if strings.EqualFold(q.Label, "none") {
			if len(labels) > 0 {
				return false
			}
		} else {
			found := false
			for _, id := range labels {
				if l := s.Labels[id]; l != nil && strings.EqualFold(l.Title, q.Label) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	if q.Milestone != "" {
		if strings.EqualFold(q.Milestone, "none") {
			return milestone == 0
		}
		m := s.Milestones[milestone]
		return m != nil && strings.EqualFold(m.Title, q.Milestone)
	}
	return true
}

// SortLabels returns the known labels among ids, sorted by title.
func (s *Store) SortLabels(ids []int64) []*Label {
	var out []*Label
	for _, id := range ids {
		if l := s.Labels[id]; l != nil {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}

// MilestoneOf returns a milestone by id, or nil.
func (s *Store) MilestoneOf(id int64) *Milestone {
	if id == 0 {
		return nil
	}
	return s.Milestones[id]
}

// item is what lists need from an issue or a merge request.
type item interface {
	head() (iid int64, state, title, desc string, milestone int64)
	confidential() bool
	setLabels([]int64)
	dropDescription()
}

func (is *Issue) head() (int64, string, string, string, int64) {
	return is.IID, is.State, is.Title, is.Description, is.Milestone
}
func (is *Issue) confidential() bool  { return is.Confidential }
func (is *Issue) setLabels(l []int64) { is.Labels = l }
func (is *Issue) dropDescription()    { is.Description = "" }

func (mr *MergeRequest) head() (int64, string, string, string, int64) {
	return mr.IID, mr.State, mr.Title, mr.Description, mr.Milestone
}
func (mr *MergeRequest) confidential() bool  { return false }
func (mr *MergeRequest) setLabels(l []int64) { mr.Labels = l }
func (mr *MergeRequest) dropDescription()    { mr.Description = "" }

// of picks the label map of a kind ("issues" or "merge_requests").
func (pl *projectLabels) of(kind string) map[string][]int64 {
	if kind == "issues" {
		return pl.Issues
	}
	return pl.MRs
}

// eachItem decodes every record of a project file.
func eachItem[T any, P interface {
	*T
	item
}](s *Store, pid int64, kind string, fn func(P) error) error {
	return s.each(pid, kind, func(line []byte) error {
		v := P(new(T))
		if err := json.Unmarshal(line, v); err != nil {
			return err
		}
		return fn(v)
	})
}

// list returns one page of a project's issues or MRs, newest first. The
// file is read twice so only the page is kept in memory (plus one int per
// matching record for sorting).
func list[T any, P interface {
	*T
	item
}](s *Store, pid int64, kind string, q Query) (page []P, total int, err error) {
	labels := s.projectLabels(pid).of(kind)
	var iids []int64
	err = eachItem[T, P](s, pid, kind, func(v P) error {
		iid, state, title, desc, ms := v.head()
		if q.HideConfidential && v.confidential() {
			return nil
		}
		if q.match(state, title, desc) && s.matchMeta(q, labels[key(iid)], ms) {
			iids = append(iids, iid)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	want := pageOf(iids, q)
	err = eachItem[T, P](s, pid, kind, func(v P) error {
		iid, _, _, _, _ := v.head()
		if _, ok := want[iid]; ok {
			v.dropDescription()
			v.setLabels(labels[key(iid)])
			page = append(page, v)
		}
		return nil
	})
	sort.Slice(page, func(i, j int) bool {
		a, _, _, _, _ := page[i].head()
		b, _, _, _, _ := page[j].head()
		return a > b
	})
	return page, len(iids), err
}

// get returns one issue or MR by iid.
func get[T any, P interface {
	*T
	item
}](s *Store, pid int64, kind string, iid int64) (P, error) {
	var out P
	err := eachItem[T, P](s, pid, kind, func(v P) error {
		if id, _, _, _, _ := v.head(); id == iid {
			out = v
			return errStop
		}
		return nil
	})
	if err != nil && err != errStop {
		return nil, err
	}
	if out == nil {
		return nil, os.ErrNotExist
	}
	out.setLabels(s.projectLabels(pid).of(kind)[key(iid)])
	return out, nil
}

// Issues lists issues, newest first.
func (s *Store) Issues(pid int64, q Query) ([]*Issue, int, error) {
	return list[Issue](s, pid, "issues", q)
}

// MergeRequests lists merge requests, newest first.
func (s *Store) MergeRequests(pid int64, q Query) ([]*MergeRequest, int, error) {
	return list[MergeRequest](s, pid, "merge_requests", q)
}

// Issue returns one issue by iid.
func (s *Store) Issue(pid, iid int64) (*Issue, error) {
	return get[Issue](s, pid, "issues", iid)
}

// MergeRequest returns one MR by iid, with its base and head commits.
func (s *Store) MergeRequest(pid, iid int64) (*MergeRequest, error) {
	mr, err := get[MergeRequest](s, pid, "merge_requests", iid)
	if err != nil {
		return nil, err
	}
	var m map[string][2]string
	if readJSON(s.projectFile(pid, "mr_diffs.json"), &m) {
		if d, ok := m[key(iid)]; ok {
			mr.BaseSHA, mr.HeadSHA = d[0], d[1]
		}
	}
	return mr, nil
}

func pageOf(iids []int64, q Query) map[int64]struct{} {
	sort.Slice(iids, func(i, j int) bool { return iids[i] > iids[j] })
	if q.Limit <= 0 {
		q.Limit = 50
	}
	lo := min(max(q.Offset, 0), len(iids)) // Offset comes from a URL
	hi := len(iids)
	if q.Limit < hi-lo {
		hi = lo + q.Limit
	}
	want := map[int64]struct{}{}
	for _, id := range iids[lo:hi] {
		want[id] = struct{}{}
	}
	return want
}

// Notes returns the notes on one issue or MR (by its database id), oldest
// first. At most limit notes are returned. Internal notes are left out
// unless internal is set.
func (s *Store) Notes(pid int64, typ string, target int64, limit int, internal bool) ([]*Note, bool, error) {
	var out []*Note
	more := false
	needle := []byte(`"target":` + key(target) + `,`)
	err := s.each(pid, "notes", func(line []byte) error {
		if !containsBytes(line, needle) { // cheap pre-filter before decoding
			return nil
		}
		var n Note
		if err := json.Unmarshal(line, &n); err != nil {
			return err
		}
		if n.Type == typ && n.Target == target && (internal || !n.Internal) {
			if len(out) == limit {
				more = true
				return nil
			}
			out = append(out, &n)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, more, err
}

func containsBytes(b, sub []byte) bool { return bytes.Contains(b, sub) }

// Usage counts the issues and merge requests of a project that use a
// label or a milestone.
type Usage struct {
	Issues, OpenIssues, ClosedIssues int
	MRs, OpenMRs                     int
}

// usage scans a project's issues and MRs, calling keys for each record
// to know which ids (labels or milestone) it counts towards. Confidential
// issues are skipped when hideConfidential is set.
func (s *Store) usage(pid int64, hideConfidential bool, keys func(kind string, iid, milestone int64) []int64) (map[int64]*Usage, error) {
	out := map[int64]*Usage{}
	for _, kind := range []string{"issues", "merge_requests"} {
		err := s.each(pid, kind, func(line []byte) error {
			var r struct {
				IID          int64  `json:"iid"`
				State        string `json:"state"`
				Milestone    int64  `json:"milestone"`
				Confidential bool   `json:"confidential"`
			}
			if err := json.Unmarshal(line, &r); err != nil {
				return err
			}
			if hideConfidential && r.Confidential {
				return nil
			}
			for _, id := range keys(kind, r.IID, r.Milestone) {
				u := out[id]
				if u == nil {
					u = &Usage{}
					out[id] = u
				}
				open := r.State == "opened"
				switch {
				case kind == "merge_requests":
					u.MRs++
					if open {
						u.OpenMRs++
					}
				case open:
					u.Issues++
					u.OpenIssues++
				default:
					u.Issues++
					u.ClosedIssues++
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LabelUsage counts the issues and MRs of a project per label id.
func (s *Store) LabelUsage(pid int64, hideConfidential bool) (map[int64]*Usage, error) {
	pl := s.projectLabels(pid)
	return s.usage(pid, hideConfidential, func(kind string, iid, _ int64) []int64 {
		return pl.of(kind)[key(iid)]
	})
}

// MilestoneUsage counts the issues and MRs of a project per milestone id.
func (s *Store) MilestoneUsage(pid int64, hideConfidential bool) (map[int64]*Usage, error) {
	return s.usage(pid, hideConfidential, func(_ string, _, milestone int64) []int64 {
		if milestone == 0 {
			return nil
		}
		return []int64{milestone}
	})
}

// Releases returns the releases of a project with their links, newest
// first (by release date). Projects have few releases: all are returned.
func (s *Store) Releases(pid int64) ([]*Release, error) {
	var out []*Release
	err := s.each(pid, "releases", func(line []byte) error {
		var r Release
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		out = append(out, &r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var links map[string][]ReleaseLink
	readJSON(s.projectFile(pid, "release_links.json"), &links)
	for _, r := range out {
		r.Links = links[key(r.ID)]
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Released, out[j].Released
		if a == "" {
			a = out[i].Created
		}
		if b == "" {
			b = out[j].Created
		}
		return a > b
	})
	return out, nil
}

package web

// History: commit lists, single commits and their diffs.

import (
	"container/heap"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
	"github.com/movenial/gitlab-backup-browser/internal/highlight"
	"github.com/movenial/gitlab-backup-browser/internal/human"
)

// --- history ---

type commitHeap []*gitstore.Commit

func (h commitHeap) Len() int { return len(h) }
func (h commitHeap) Less(i, j int) bool {
	return h[i].Committer.When.After(h[j].Committer.When)
}
func (h commitHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *commitHeap) Push(x any)   { *h = append(*h, x.(*gitstore.Commit)) }
func (h *commitHeap) Pop() any {
	old := *h
	c := old[len(old)-1]
	*h = old[:len(old)-1]
	return c
}

// walk returns up to limit commits after skipping skip, newest first.
// When p is set, only commits that change p are returned.
func walk(repo *gitstore.Repo, start gitstore.Hash, skip, limit int, p string) ([]*gitstore.Commit, bool, error) {
	first, err := repo.Commit(start)
	if err != nil {
		return nil, false, err
	}
	h := &commitHeap{first}
	seen := map[gitstore.Hash]bool{first.Hash: true}
	var out []*gitstore.Commit
	visited := 0
	for h.Len() > 0 {
		c := heap.Pop(h).(*gitstore.Commit)
		for _, ph := range c.Parents {
			if seen[ph] {
				continue
			}
			seen[ph] = true
			pc, err := repo.Commit(ph)
			if err != nil {
				continue // shallow or missing history
			}
			heap.Push(h, pc)
		}
		if p != "" && !touches(repo, c, p) {
			continue
		}
		visited++
		if visited <= skip {
			continue
		}
		if len(out) == limit {
			return out, true, nil
		}
		out = append(out, c)
		if visited > 200000 {
			break
		}
	}
	return out, false, nil
}

func touches(repo *gitstore.Repo, c *gitstore.Commit, p string) bool {
	cur, err := repo.Lookup(c.Tree, p)
	if len(c.Parents) == 0 {
		return err == nil
	}
	pc, perr := repo.Commit(c.Parents[0])
	if perr != nil {
		return true
	}
	prev, err2 := repo.Lookup(pc.Tree, p)
	if (err == nil) != (err2 == nil) {
		return true
	}
	return err == nil && (cur.Hash != prev.Hash || cur.Mode != prev.Mode)
}

type commitsData struct {
	RepoView
	Commits []*gitstore.Commit
	Page    int
	More    bool
}

const pageSize = 30

func (s *Server) commits(w http.ResponseWriter, r *http.Request, t *target, repo *gitstore.Repo, ref string, h gitstore.Hash, p string) {
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if pg < 1 {
		pg = 1
	}
	cs, more, err := walk(repo, h, (pg-1)*pageSize, pageSize, p)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	v := s.baseView(r, t, repo)
	v.Ref, v.Path = ref, p
	s.render(w, r, http.StatusOK, "commits.html", "Commits · "+t.Name, s.crumbsFor(t), commitsData{v, cs, pg, more})
}

type fileDiff struct {
	FileChange
	Lines    []DiffLine
	Note     string
	Added    int
	Removed  int
	Collapse bool
}

type commitData struct {
	RepoView
	Files     []fileDiff
	Truncated bool
}

const maxFiles = 300

func (s *Server) commit(w http.ResponseWriter, r *http.Request, t *target, repo *gitstore.Repo, id string) {
	// A full or short id, or any name git accepts (main~3, v1.0^).
	h, _, err := repo.Resolve(strings.Trim(id, "/"))
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "No such commit in this backup.")
		return
	}
	c, err := repo.Commit(h)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "Commit not found in this backup.")
		return
	}
	var parentTree gitstore.Hash
	if len(c.Parents) > 0 {
		if pc, err := repo.Commit(c.Parents[0]); err == nil {
			parentTree = pc.Tree
		}
	}
	d := commitData{RepoView: s.baseView(r, t, repo)}
	d.Commit, d.Ref = c, c.Hash.String()
	var err2 error
	if d.Files, d.Truncated, err2 = buildDiffs(repo, parentTree, c.Tree); err2 != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err2.Error())
		return
	}
	s.render(w, r, http.StatusOK, "commit.html", c.Subject(), s.crumbsFor(t), d)
}

// buildDiffs renders the changes between two trees, within a line budget.
func buildDiffs(repo *gitstore.Repo, oldTree, newTree gitstore.Hash) ([]fileDiff, bool, error) {
	var changes []FileChange
	if err := diffTrees(repo, oldTree, newTree, "", &changes, maxFiles+1); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(changes) > maxFiles {
		changes, truncated = changes[:maxFiles], true
	}
	var files []fileDiff
	budget := 3000 // total diff lines rendered
	for _, fc := range changes {
		fd := fileDiff{FileChange: fc}
		var a, b []byte
		if !fc.Old.IsZero() {
			_, a, _ = repo.Object(fc.Old)
		}
		if !fc.New.IsZero() {
			_, b, _ = repo.Object(fc.New)
		}
		switch {
		case fc.NewMode == 0o160000 || fc.OldMode == 0o160000:
			fd.Note = "Submodule changed"
		case isBinary(a) || isBinary(b):
			fd.Note = fmt.Sprintf("Binary file (%s → %s)", human.Bytes(int64(len(a))), human.Bytes(int64(len(b))))
		case len(a) > 512<<10 || len(b) > 512<<10:
			fd.Note = "File too large to diff"
		case budget <= 0:
			fd.Note = "Diff not shown (change too large)"
		default:
			lines, ok := unified(splitLines(a), splitLines(b), 3)
			if !ok {
				fd.Note = "Change too large to display"
				break
			}
			for _, l := range lines {
				switch l.Kind {
				case '+':
					fd.Added++
				case '-':
					fd.Removed++
				}
			}
			budget -= len(lines)
			highlightDiff(fc.Path, a, b, lines)
			fd.Lines = lines
			fd.Collapse = len(lines) > 400
		}
		files = append(files, fd)
	}
	return files, truncated, nil
}

// codeLines returns the lines of a file as HTML, highlighted when the
// language of name is known (with its name), escaped otherwise.
func codeLines(name string, data []byte) ([]template.HTML, string) {
	if hl := highlight.Lines(name, data); hl != nil {
		return hl, highlight.Name(name, data)
	}
	lines := splitLines(data)
	out := make([]template.HTML, len(lines))
	for i, l := range lines {
		out[i] = template.HTML(template.HTMLEscapeString(l))
	}
	return out, ""
}

// highlightDiff colours diff lines by highlighting both versions of the
// file whole (so multi-line strings and comments are right) and picking
// each line from the side it belongs to.
func highlightDiff(name string, a, b []byte, lines []DiffLine) {
	ha, hb := highlight.Lines(name, a), highlight.Lines(name, b)
	if ha == nil && hb == nil {
		return
	}
	pick := func(h []template.HTML, n int) template.HTML {
		if n >= 1 && n <= len(h) {
			return h[n-1]
		}
		return ""
	}
	for i := range lines {
		switch lines[i].Kind {
		case '-':
			lines[i].HTML = pick(ha, lines[i].Old)
		case '+', ' ':
			lines[i].HTML = pick(hb, lines[i].New)
		}
	}
}

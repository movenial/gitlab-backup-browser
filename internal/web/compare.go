package web

import (
	"container/heap"
	"net/http"
	"net/url"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

const (
	flagFrom = 1 << iota
	flagTo
	flagStale
)

// compareCommits returns the commits reachable from to but not from from
// (newest first, at most limit, with their total), and their merge base.
// It walks both histories by date, painting commits with where they are
// reachable from, as git merge-base does; it stops once only commits
// reachable from both remain.
func compareCommits(repo *gitstore.Repo, from, to gitstore.Hash, limit int) (ahead []*gitstore.Commit, total int, base gitstore.Hash, err error) {
	fc, err := repo.Commit(from)
	if err != nil {
		return nil, 0, base, err
	}
	tc, err := repo.Commit(to)
	if err != nil {
		return nil, 0, base, err
	}
	flags := map[gitstore.Hash]uint8{fc.Hash: flagFrom}
	flags[tc.Hash] |= flagTo
	h := &commitHeap{fc}
	if tc.Hash != fc.Hash {
		heap.Push(h, tc)
	}
	live := func() bool { // anything not yet known to be common?
		for _, c := range *h {
			if flags[c.Hash]&flagStale == 0 {
				return true
			}
		}
		return false
	}
	for visited := 0; h.Len() > 0 && visited < 200000 && live(); visited++ {
		c := heap.Pop(h).(*gitstore.Commit)
		f := flags[c.Hash]
		switch {
		case f&(flagFrom|flagTo) == flagFrom|flagTo:
			if base.IsZero() {
				base = c.Hash
			}
			f |= flagStale
			flags[c.Hash] = f
		case f&flagTo != 0:
			total++
			if len(ahead) < limit {
				ahead = append(ahead, c)
			}
		}
		for _, ph := range c.Parents {
			old, seen := flags[ph]
			nf := old | f
			if seen && nf == old {
				continue
			}
			flags[ph] = nf
			if pc, err := repo.Commit(ph); err == nil {
				heap.Push(h, pc)
			}
		}
	}
	return ahead, total, base, nil
}

type compareData struct {
	RepoView
	From, To  string
	Commits   []*gitstore.Commit
	Total     int
	Base      string
	Files     []fileDiff
	Truncated bool
	Error     string
}

// compare is /<project>/-/compare[/<from>...<to>].
func (s *Server) compare(w http.ResponseWriter, r *http.Request, t *target, repo *gitstore.Repo, rest string) {
	q := r.URL.Query()
	if f, to := q.Get("from"), q.Get("to"); f != "" && to != "" { // the form
		http.Redirect(w, r, "/"+t.Base+"/-/compare/"+url.PathEscape(f)+"..."+url.PathEscape(to), http.StatusFound)
		return
	}
	d := compareData{RepoView: s.baseView(r, t, repo)}
	from, to, ok := strings.Cut(rest, "...")
	if !ok {
		from, to, ok = strings.Cut(rest, "..")
	}
	if !ok || from == "" || to == "" {
		// Empty form, the default branch preselected as the base.
		if _, ref, err := repo.Resolve("HEAD"); err == nil {
			d.From = strings.TrimPrefix(ref, "refs/heads/")
		}
		cs := append(s.crumbsFor(t), crumb{"Compare", ""})
		s.render(w, r, http.StatusOK, "compare.html", "Compare · "+t.Name, cs, d)
		return
	}
	d.From, d.To = from, to
	fh, _, ferr := repo.Resolve(from)
	th, _, terr := repo.Resolve(to)
	switch {
	case ferr != nil:
		d.Error = "Unknown branch, tag or commit: " + from
	case terr != nil:
		d.Error = "Unknown branch, tag or commit: " + to
	}
	if d.Error == "" {
		var err error
		var base gitstore.Hash
		d.Commits, d.Total, base, err = compareCommits(repo, fh, th, 100)
		if err != nil {
			d.Error = err.Error()
		} else {
			// Like GitLab: the changes since the merge base (three dots).
			var oldTree gitstore.Hash
			if !base.IsZero() {
				d.Base = base.String()
				if bc, err := repo.Commit(base); err == nil {
					oldTree = bc.Tree
				}
			}
			if tc, err := repo.Commit(th); err == nil {
				d.Files, d.Truncated, err = buildDiffs(repo, oldTree, tc.Tree)
				if err != nil {
					d.Error = err.Error()
				}
			}
		}
	}
	cs := append(s.crumbsFor(t), crumb{"Compare", ""})
	s.render(w, r, http.StatusOK, "compare.html", from+"..."+to+" · "+t.Name, cs, d)
}

package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/movenial/gitlab-backup-browser/internal/index"
	"github.com/movenial/gitlab-backup-browser/internal/search"
)

type codeForm struct {
	Q, Path, Project string
	Regex, Case, All bool
	Err              string
	Searching        bool
	Unbuilt          int // repositories not unbundled yet in scope
}

type codeLine struct {
	N              int
	Pre, Mid, Post string
}

type codeHit struct {
	Project *index.Project
	Ref     string
	Path    string
	Lines   []codeLine
	More    int
}

type codeTail struct {
	Stats     search.Stats
	Repos     int
	NotOpened int
	Failed    int
	Waited    bool
	Cancelled bool
	Form      codeForm
}

// code searches file contents. The page streams: the form and the first
// results appear while later repositories are still being searched.
func (s *Server) code(w http.ResponseWriter, r *http.Request) {
	ix := s.mgr().Index()
	if ix == nil {
		s.loading(w, r, http.StatusOK)
		return
	}
	q := r.URL.Query()
	f := codeForm{Q: q.Get("q"), Path: q.Get("path"), Project: strings.Trim(q.Get("p"), "/"),
		Regex: q.Get("re") != "", Case: q.Get("case") != "", All: q.Get("all") != ""}
	var scope []*index.Project
	if f.Project != "" {
		if p := ix.Project(f.Project); p != nil && p.Repo != "" {
			scope = []*index.Project{p}
		}
	} else {
		for _, p := range ix.Projects {
			if p.Repo != "" {
				scope = append(scope, p)
			}
		}
		// Most recently active projects first: likelier to hold the answer.
		sort.SliceStable(scope, func(i, j int) bool { return scope[i].LastActivity > scope[j].LastActivity })
	}
	for _, p := range scope {
		if !s.mgr().Built(p.Repo) {
			f.Unbuilt++
		}
	}
	query := search.Query{Pattern: f.Q, Regex: f.Regex, CaseSensitive: f.Case, PathFilter: f.Path}
	var matcher *search.Matcher
	if f.Q != "" {
		m, err := search.Compile(query)
		if err != nil {
			f.Err = err.Error()
		} else {
			matcher = m
		}
	}
	f.Searching = matcher != nil

	title := "Search code"
	var crumbs []crumb
	if f.Project != "" {
		crumbs = []crumb{{f.Project, "/" + f.Project}, {"Search code", ""}}
	}
	pg := s.pageFor(r, title, crumbs, f)
	pageHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	s.tmpl.ExecuteTemplate(w, "top", pg)
	s.tmpl.ExecuteTemplate(w, "code_head", f)
	flush()
	defer s.tmpl.ExecuteTemplate(w, "bottom", pg)
	if matcher == nil {
		return
	}

	tail := codeTail{Form: f}
	// One search at a time: concurrent searches would multiply memory.
	ctx := r.Context()
	select {
	case s.codeSem <- struct{}{}:
	default:
		tail.Waited = true
		s.tmpl.ExecuteTemplate(w, "code_wait", nil)
		flush()
		select {
		case s.codeSem <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
	defer func() { <-s.codeSem }()

	start := time.Now()
	budget := search.MaxResults
	lastFlush := time.Now()
	for _, p := range scope {
		if ctx.Err() != nil {
			tail.Cancelled = true
			break
		}
		if !s.mgr().Built(p.Repo) {
			if !f.All {
				tail.NotOpened++
				continue
			}
			if err := s.mgr().Ensure(p.Repo); err != nil {
				tail.Failed++
				continue
			}
		}
		err := s.searchProject(ctx, p, matcher, query, &budget, &tail.Stats, func(h codeHit) {
			s.tmpl.ExecuteTemplate(w, "code_hit", h)
			if time.Since(lastFlush) > 150*time.Millisecond || len(h.Lines) > 0 {
				flush()
				lastFlush = time.Now()
			}
		})
		tail.Repos++
		if errors.Is(err, search.ErrLimit) {
			tail.Stats.Truncated = true
			break
		}
		if errors.Is(err, context.Canceled) {
			tail.Cancelled = true
			break
		}
		if err != nil {
			tail.Failed++
		}
	}
	tail.Stats.Elapsed = time.Since(start).Round(time.Millisecond)
	s.tmpl.ExecuteTemplate(w, "code_tail", tail)
}

func (s *Server) searchProject(ctx context.Context, p *index.Project, m *search.Matcher, q search.Query, budget *int, st *search.Stats, emit func(codeHit)) error {
	repo, release, err := s.mgr().Repo(p.Repo)
	if err != nil {
		return err
	}
	defer release()
	short, h, _, err := resolveRefPath(repo, "")
	if err != nil {
		return nil // empty repository
	}
	st.Repos++
	return search.Repo(ctx, repo, h, m, q, budget, st, func(fh search.FileHit) error {
		hit := codeHit{Project: p, Ref: short, Path: fh.Path, More: fh.More}
		for _, mt := range fh.Matches {
			hit.Lines = append(hit.Lines, splitMatch(mt))
		}
		emit(hit)
		return nil
	})
}

func splitMatch(m search.Match) codeLine {
	t := m.Text
	s, e := m.Start, m.End
	if s < 0 || e > len(t) || s > e {
		return codeLine{N: m.Line, Pre: strings.ToValidUTF8(t, "�")}
	}
	valid := func(x string) string {
		if utf8.ValidString(x) {
			return x
		}
		return strings.ToValidUTF8(x, "�")
	}
	return codeLine{N: m.Line, Pre: valid(t[:s]), Mid: valid(t[s:e]), Post: valid(t[e:])}
}

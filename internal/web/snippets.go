package web

import (
	"html/template"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
	"github.com/movenial/gitlab-backup-browser/internal/highlight"
	"github.com/movenial/gitlab-backup-browser/internal/index"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

// snippetTarget resolves "-/snippets/<id>" or "<project>/-/snippets/<id>"
// (the project form must name the snippet's own project).
func snippetTarget(ix *index.Index, full string) *target {
	proj, idStr, ok := strings.Cut(full, "-/snippets/")
	if !ok {
		return nil
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil
	}
	sn := ix.Snippet(id)
	if sn == nil || strings.TrimSuffix(proj, "/") != sn.Project && proj != "" {
		return nil
	}
	p := ix.Project(sn.Project)
	if p == nil {
		p = &index.Project{Name: sn.Title, FullPath: "-/snippets/" + idStr}
	}
	return &target{P: p, Key: sn.Repo, Base: "-/snippets/" + idStr, Name: sn.Title, Snippet: sn}
}

type snippetFile struct {
	Path   string
	Size   int64
	Lines  []template.HTML
	Note   string
	RawURL string
	Lang   string
	Hidden bool // collapsed: too many files
}

type snippetData struct {
	T        *target
	S        *index.Snippet
	Author   string
	Desc     template.HTML
	Files    []snippetFile
	CloneURL string
	More     bool
}

type snippetsData struct {
	Project  *index.Project // nil: all snippets
	Snippets []*index.Snippet
	Users    interface{ UserName(int64) string }
}

// userNames names users from the tracker store, when issues were imported.
type userNames struct{ ts *tracker.Store }

func (u userNames) UserName(id int64) string {
	if u.ts == nil { // imported with --no-issues
		return "user #" + strconv.FormatInt(id, 10)
	}
	return u.ts.UserName(id)
}

// snippetRoute handles /-/snippets, /-/snippets/<id>, its raw files and
// its git repository (/-/snippets/<id>.git/...).
func (s *Server) snippetRoute(w http.ResponseWriter, r *http.Request, full string) {
	ix := s.mgr().Index()
	if ix == nil {
		s.loading(w, r, http.StatusOK)
		return
	}
	rest := strings.TrimPrefix(full, "-/snippets")
	if rest == "" || rest == "/" {
		s.snippetList(w, r, nil)
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	if i := strings.Index(rest+"/", ".git/"); i >= 0 {
		s.git(w, r, "-/snippets/"+rest[:i], strings.TrimPrefix(rest[i+4:], "/"))
		return
	}
	id, file, _ := strings.Cut(rest, "/")
	t := snippetTarget(ix, "-/snippets/"+id)
	if t == nil {
		s.errorPage(w, r, http.StatusNotFound, "No such snippet in this backup.")
		return
	}
	if f, ok := strings.CutPrefix(file, "raw/"); ok && f != "" {
		s.snippetRaw(w, r, t, f)
		return
	}
	s.snippet(w, r, t)
}

// snippetList is /-/snippets (every snippet) or /<project>/-/snippets.
func (s *Server) snippetList(w http.ResponseWriter, r *http.Request, p *index.Project) {
	ix := s.mgr().Index()
	d := snippetsData{Project: p, Users: userNames{s.mgr().Tracker()}}
	for _, sn := range ix.Snippets {
		if p == nil || sn.ProjectID == p.ID {
			d.Snippets = append(d.Snippets, sn)
		}
	}
	title, cs := "Snippets", []crumb{{"Snippets", "/-/snippets"}}
	if p != nil {
		t := &target{P: p, Key: p.Repo, Base: p.FullPath, Name: p.Name}
		cs = append(s.crumbsFor(t), crumb{"Snippets", ""})
		title = "Snippets · " + p.Name
		s.render(w, r, http.StatusOK, "snippets.html", title, cs, map[string]any{"T": t, "D": d})
		return
	}
	s.render(w, r, http.StatusOK, "snippets.html", title, cs, map[string]any{"D": d})
}

// snippet shows the files of a snippet (its repository's default branch).
func (s *Server) snippet(w http.ResponseWriter, r *http.Request, t *target) {
	sn := t.Snippet
	d := snippetData{T: t, S: sn, Author: userNames{s.mgr().Tracker()}.UserName(sn.Author),
		Desc:     renderMarkdown([]byte(sn.Description), "", "", ""),
		CloneURL: s.cloneURL(r, t)}
	if sn.Repo == "" {
		s.render(w, r, http.StatusOK, "snippet.html", sn.Title, s.crumbsFor(t), d)
		return
	}
	repo, release, ok := s.openRepo(w, r, t)
	if !ok {
		return
	}
	defer release()
	files, more := snippetFiles(repo, 50)
	d.More = more
	for i, f := range files {
		f.RawURL = "/" + t.Base + "/raw/" + f.Path
		f.Hidden = i >= 10
		d.Files = append(d.Files, f)
	}
	s.render(w, r, http.StatusOK, "snippet.html", sn.Title, s.crumbsFor(t), d)
}

// snippetFiles reads up to max files of HEAD, highlighted.
func snippetFiles(repo *gitstore.Repo, max int) (out []snippetFile, more bool) {
	h, _, err := repo.Resolve("HEAD")
	if err != nil {
		return nil, false
	}
	c, err := repo.Commit(h)
	if err != nil {
		return nil, false
	}
	var walk func(tree gitstore.Hash, dir string)
	walk = func(tree gitstore.Hash, dir string) {
		es, err := repo.Tree(tree)
		if err != nil {
			return
		}
		for _, e := range es {
			if len(out) >= max {
				more = true
				return
			}
			p := path.Join(dir, e.Name)
			switch {
			case e.IsDir():
				walk(e.Hash, p)
				continue
			case e.IsSubmodule(), e.IsSymlink():
				continue
			}
			f := snippetFile{Path: p}
			_, size, err := repo.Stat(e.Hash)
			f.Size = size
			switch {
			case err != nil:
				f.Note = err.Error()
			case size > highlight.MaxBytes:
				f.Note = "Too large to display: use Raw."
			default:
				_, data, err := repo.Object(e.Hash)
				switch {
				case err != nil:
					f.Note = err.Error()
				case isBinary(data):
					f.Note = "Binary file: use Raw."
				default:
					f.Lines, f.Lang = codeLines(p, data)
				}
			}
			out = append(out, f)
		}
	}
	walk(c.Tree, "")
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, more
}

// snippetRaw serves one file of a snippet, sandboxed like repository files.
func (s *Server) snippetRaw(w http.ResponseWriter, r *http.Request, t *target, file string) {
	if t.Key == "" || !s.mgr().Built(t.Key) {
		http.NotFound(w, r)
		return
	}
	repo, release, err := s.mgr().Repo(t.Key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer release()
	h, _, err := repo.Resolve("HEAD")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.raw(w, r, repo, h, file)
}

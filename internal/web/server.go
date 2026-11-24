// Package web serves the browsing UI and git's dumb HTTP protocol.
package web

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/app"
	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
	"github.com/movenial/gitlab-backup-browser/internal/human"
	"github.com/movenial/gitlab-backup-browser/internal/index"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server is the HTTP handler.
type Server struct {
	codeSem chan struct{} // one code search at a time
	hub     *app.Hub
	// ActionsAllowed enables the action buttons (extract, unbundle, open
	// another backup...). The caller disables them when the server is
	// reachable from the network without a login.
	ActionsAllowed bool
	// ShowConfidential shows confidential issues and internal comments;
	// otherwise they appear as placeholders that ask for a login. The
	// caller sets it only when a login protects the server: without one,
	// anyone who reaches gbb would read them.
	ShowConfidential bool
	csrf             string // token required by every action form
	tmpl             *template.Template
	static           http.Handler
	Version          string
}

// New builds a server for a single, already started backup.
func New(m *app.Manager, version string) (*Server, error) {
	return NewHub(app.HubWith(m), version)
}

// mgr returns the open backup's manager, or nil when none is open.
func (s *Server) mgr() *app.Manager { return s.hub.Active() }

// NewHub builds a server that can switch between backups.
func NewHub(h *app.Hub, version string) (*Server, error) {
	s := &Server{codeSem: make(chan struct{}, 1), hub: h, ActionsAllowed: true, Version: version}
	funcs := template.FuncMap{
		"hasTracker": func() bool { m := h.Active(); return m != nil && m.Tracker() != nil },
		"dict": func(kv ...any) map[string]any {
			out := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				out[kv[i].(string)] = kv[i+1]
			}
			return out
		},
		"ago":       ago,
		"scope":     scope,
		"loadSteps": loadSteps,
		"isRef": func(v RepoView, name string) bool {
			for _, l := range [][]gitstore.RefInfo{v.Branches, v.Tags} {
				for _, ref := range l {
					if ref.ShortName() == name {
						return true
					}
				}
			}
			return false
		},
		"shortdir":  shortDir,
		"date":      func(t time.Time) string { return t.Format("2006-01-02 15:04") },
		"bytes":     human.Bytes,
		"short":     func(h gitstore.Hash) string { return h.Short() },
		"join":      path.Join,
		"pathParts": pathParts,
		"add":       func(a, b int) int { return a + b },
		"lineKind":  func(b byte) string { return string(b) },
		"initial":   initial,
		"shortTime": shortTime,
		"ubytes":    func(n uint64) string { return human.Bytes(int64(n)) },
		"ibytes":    func(n int) string { return human.Bytes(int64(n)) },
	}
	t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	sub, _ := fs.Sub(assets, "static")
	tok := make([]byte, 16)
	rand.Read(tok)
	s.csrf, s.tmpl, s.static = hex.EncodeToString(tok), t, http.StripPrefix("/-/static/", http.FileServer(http.FS(sub)))
	return s, nil
}

// --- page plumbing ---

type crumb struct{ Name, URL string }

type page struct {
	Title     string
	Crumbs    []crumb
	Status    app.Status
	Version   string
	Index     *index.Index
	Data      any
	Actions   bool   // action buttons allowed
	CSRF      string // token for action forms
	Backup    string // name of the open backup
	Picker    bool   // more than one backup can be opened
	Nav       string // current top-level page: projects, code, backups, info
	LabelsCSS string // stylesheet with label colours, when labels exist
	Theme     string // "", light or dark (cookie set from the menu)
	Here      string // path and query of this page, to come back after a menu action
}

// navFor names the top-level page a URL belongs to, for the menu.
func navFor(path string) string {
	switch path {
	case "/-/projects", "/-/search":
		return "projects"
	case "/-/code":
		return "code"
	case "/-/backups":
		return "backups"
	case "/-/snippets":
		return "snippets"
	case "/-/info":
		return "info"
	}
	return ""
}

func (s *Server) pageFor(r *http.Request, title string, crumbs []crumb, data any) page {
	p := page{Title: title, Crumbs: crumbs, Version: s.Version, Data: data, Actions: s.ActionsAllowed, CSRF: s.csrf,
		Nav: navFor(r.URL.Path), Theme: themeOf(r), Here: r.URL.RequestURI(),
		Picker: len(s.hub.Dirs()) > 0, Status: app.Status{Ready: true, Phase: "ready"}}
	if m := s.mgr(); m != nil {
		p.Status, p.Index = m.Status(), m.Index()
		p.Backup = filepath.Base(m.BackupPath())
		if ts := m.Tracker(); ts != nil && len(ts.Labels) > 0 {
			// Versioned by backup, so switching backups refetches it.
			p.LabelsCSS = "/-/labels.css?b=" + filepath.Base(m.Root())
		}
	}
	return p
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, code int, name, title string, crumbs []crumb, data any) {
	p := s.pageFor(r, title, crumbs, data)
	pageHeaders(w)
	w.WriteHeader(code)
	if err := s.tmpl.ExecuteTemplate(w, name, p); err != nil {
		fmt.Fprintf(w, "<pre>template error: %s</pre>", template.HTMLEscapeString(err.Error()))
	}
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.render(w, r, code, "error.html", http.StatusText(code), nil, msg)
}

// ServeHTTP routes requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The only POSTs accepted are reads (Git LFS batch, git's smart fetch)
	// and the action buttons.
	isLFSBatch := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ".git/info/lfs/objects/batch")
	isAction := r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/-/action/")
	isUploadPack := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ".git/git-upload-pack")
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !isLFSBatch && !isAction && !isUploadPack {
		http.Error(w, "read-only", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if isAction {
		s.action(w, r)
		return
	}
	switch {
	case strings.HasPrefix(p, "/-/static/"), p == "/-/backups":
	default:
		if s.mgr() == nil {
			if p == "/-/status.json" {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"phase":"no backup open","ready":false}`))
				return
			}
			http.Redirect(w, r, "/-/backups", http.StatusFound)
			return
		}
	}
	switch {
	case p == "/":
		s.home(w, r)
		return
	case strings.HasPrefix(p, "/-/static/"):
		w.Header().Set("Cache-Control", "public, max-age=3600")
		s.static.ServeHTTP(w, r)
		return
	case p == "/-/status.json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(s.mgr().Status())
		return
	case p == "/-/search":
		s.search(w, r)
		return
	case p == "/-/code":
		s.code(w, r)
		return
	case p == "/-/projects":
		s.projects(w, r)
		return
	case p == "/-/backups":
		s.backups(w, r)
		return
	case p == "/-/info":
		s.info(w, r)
		return
	case p == "/-/snippets" || strings.HasPrefix(p, "/-/snippets/"):
		s.snippetRoute(w, r, strings.TrimPrefix(p, "/"))
		return
	case p == "/-/theme":
		setTheme(w, r)
		return
	case p == "/-/labels.css":
		s.labelsCSS(w, r)
		return
	}
	ix := s.mgr().Index()
	if ix == nil {
		s.loading(w, r, http.StatusServiceUnavailable)
		return
	}
	full := strings.Trim(p, "/")

	// git dumb HTTP: /<project>.git/...
	if i := strings.Index(full+"/", ".git/"); i >= 0 {
		s.git(w, r, full[:i], strings.TrimPrefix(full[i+4:], "/"))
		return
	}
	// project actions: /<project>/-/<action>/...
	if before, after, ok := strings.Cut(full, "/-/"); ok {
		t := s.target(ix, before)
		if t == nil {
			s.errorPage(w, r, http.StatusNotFound, "No such project in this backup.")
			return
		}
		action, rest, _ := strings.Cut(after, "/")
		s.projectAction(w, r, t, action, rest)
		return
	}
	if t := s.target(ix, full); t != nil {
		s.project(w, r, t)
		return
	}
	if ns := ix.Namespace(full); ns != nil {
		s.group(w, r, ns)
		return
	}
	s.errorPage(w, r, http.StatusNotFound, "Nothing at this path. It may belong to a project the backup does not contain.")
}

// target is a browsable repository (a project, or a project's wiki).
type target struct {
	P      *index.Project
	Key    string // repository key, "" when the project has no bundle
	Base   string // URL path without leading slash, e.g. group/proj or group/proj.wiki
	IsWiki bool
	Name   string
	// Snippet is set for snippet repositories (P is then the snippet's
	// project, or a stand-in for personal snippets).
	Snippet *index.Snippet
}

func (s *Server) target(ix *index.Index, full string) *target {
	if p := ix.Project(full); p != nil {
		return &target{P: p, Key: p.Repo, Base: p.FullPath, Name: p.Name}
	}
	if base, ok := strings.CutSuffix(full, ".wiki"); ok {
		if p := ix.Project(base); p != nil && p.Wiki != "" {
			return &target{P: p, Key: p.Wiki, Base: p.FullPath + ".wiki", IsWiki: true, Name: p.Name + " wiki"}
		}
	}
	return snippetTarget(ix, full)
}

func (s *Server) crumbsFor(t *target) []crumb {
	var cs []crumb
	if sn := t.Snippet; sn != nil {
		if sn.Project != "" {
			p := &target{P: t.P, Base: t.P.FullPath}
			return append(s.crumbsFor(p), crumb{"Snippets", "/" + t.P.FullPath + "/-/snippets"}, crumb{sn.Title, "/" + t.Base})
		}
		return []crumb{{"Snippets", "/-/snippets"}, {sn.Title, "/" + t.Base}}
	}
	parts := strings.Split(t.P.FullPath, "/")
	for i := range parts[:len(parts)-1] {
		cs = append(cs, crumb{parts[i], "/" + strings.Join(parts[:i+1], "/")})
	}
	cs = append(cs, crumb{t.P.Name, "/" + t.P.FullPath})
	if t.IsWiki {
		cs = append(cs, crumb{"Wiki", "/" + t.Base})
	}
	return cs
}

// --- top level pages ---

type homeData struct {
	Groups   []*index.Namespace
	Users    []*index.Namespace
	Bots     []*index.Namespace
	Other    []*index.Namespace // the synthetic "Unmatched repositories" group
	Projects []*index.Project
	Stats    index.Stats
	Recent   []*index.Project
	// While the database is read, repositories are only known by hash.
	Pending  bool
	ByHash   []*index.Project // the first of them
	HashOnly int
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ix := s.mgr().Index()
	if ix == nil {
		s.loading(w, r, http.StatusOK)
		return
	}
	s.render(w, r, http.StatusOK, "home.html", "Backup", nil, homeFor(ix))
}

// homeFor groups the top level of the index for the home page.
func homeFor(ix *index.Index) homeData {
	ns, ps := ix.Children("")
	d := homeData{Projects: ps, Stats: ix.Stats()}
	for _, n := range ns {
		switch n.Kind {
		case "group":
			d.Groups = append(d.Groups, n)
		case "user":
			d.Users = append(d.Users, n)
		case "bot":
			d.Bots = append(d.Bots, n)
		default:
			d.Other = append(d.Other, n)
		}
	}
	var recent []*index.Project
	for _, p := range ix.Projects {
		if !p.Unmatched && p.Repo != "" && !p.Archived && p.Kind != "bot" {
			recent = append(recent, p)
		}
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].LastActivity > recent[j].LastActivity })
	if len(recent) > 8 {
		recent = recent[:8]
	}
	d.Recent = recent
	if ix.DBStatus == "pending" {
		d.Pending = true
		for _, p := range ix.Projects {
			if p.Unmatched {
				d.HashOnly++
				if len(d.ByHash) < 30 {
					d.ByHash = append(d.ByHash, p)
				}
			}
		}
	}
	return d
}

type groupData struct {
	NS       *index.Namespace
	Groups   []*index.Namespace
	Projects []*index.Project
}

func (s *Server) group(w http.ResponseWriter, r *http.Request, ns *index.Namespace) {
	ix := s.mgr().Index()
	gs, ps := ix.Children(ns.FullPath)
	var cs []crumb
	parts := strings.Split(ns.FullPath, "/")
	for i := range parts {
		name := parts[i]
		if n := ix.Namespace(strings.Join(parts[:i+1], "/")); n != nil {
			name = n.Name
		}
		cs = append(cs, crumb{name, "/" + strings.Join(parts[:i+1], "/")})
	}
	s.render(w, r, http.StatusOK, "group.html", ns.Name, cs, groupData{ns, gs, ps})
}

type searchData struct {
	Q       string
	Results []*index.Project
	More    bool
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	ix := s.mgr().Index()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	d := searchData{Q: q}
	if ix != nil {
		terms := strings.Fields(strings.ToLower(q))
		for _, p := range ix.Projects {
			hay := strings.ToLower(p.FullPath + " " + p.Name + " " + p.Description)
			ok := true
			for _, t := range terms {
				if !strings.Contains(hay, t) {
					ok = false
					break
				}
			}
			if ok {
				if len(d.Results) == 200 {
					d.More = true
					break
				}
				d.Results = append(d.Results, p)
			}
		}
	}
	s.render(w, r, http.StatusOK, "search.html", "Search", nil, d)
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	ix := s.mgr().Index()
	if ix == nil {
		s.loading(w, r, http.StatusOK)
		return
	}
	var keys []string
	for k := range ix.Info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type kv struct{ K, V string }
	var info []kv
	for _, k := range keys {
		info = append(info, kv{k, ix.Info[k]})
	}
	var missing []*index.Project
	for _, p := range ix.Projects {
		if !p.Unmatched && p.Repo == "" {
			missing = append(missing, p)
		}
	}
	oc, ocSize := otherCaches(s.mgr())
	s.render(w, r, http.StatusOK, "info.html", "About this backup", nil, map[string]any{
		"Info": info, "Stats": ix.Stats(), "DB": ix.DBStatus, "Missing": missing,
		"Source": describe(s.mgr()), "Built": ix.BuiltAt, "Mem": s.mgr().MemStats(),
		"Assets": []app.AssetState{s.mgr().Asset("lfs"), s.mgr().Asset("uploads")},
		"Busy":   s.mgr().Busy(), "Phase": s.mgr().Status().Phase, "PauseOnBattery": s.mgr().PausesOnBattery(),
		"OtherCaches": oc, "OtherCachesSize": ocSize,
	})
}

func describe(m *app.Manager) string {
	if src := m.Source(); src != nil {
		return src.Describe()
	}
	return ""
}

// --- project pages ---

type RepoView struct {
	T        *target
	Ref      string // short ref name for URLs
	FullRef  string
	Commit   *gitstore.Commit
	Path     string
	Branches []gitstore.RefInfo
	Tags     []gitstore.RefInfo
	Clone    string
	Building bool
	BuildErr string
	Progress *gitstore.Progress
}

func (s *Server) cloneURL(r *http.Request, t *target) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/" + t.Base + ".git"
}

// openRepo opens the target's repository or renders a placeholder page.
// The caller must call release when done.
func (s *Server) openRepo(w http.ResponseWriter, r *http.Request, t *target) (repo *gitstore.Repo, release func(), ok bool) {
	if t.Key == "" {
		s.render(w, r, http.StatusOK, "norepo.html", t.Name, s.crumbsFor(t), t)
		return nil, nil, false
	}
	if !s.mgr().Built(t.Key) {
		if e := s.mgr().BuildError(t.Key); e != "" && !s.mgr().Building(t.Key) && r.URL.Query().Get("retry") == "" {
			s.render(w, r, http.StatusInternalServerError, "building.html", t.Name, s.crumbsFor(t), RepoView{T: t, BuildErr: e})
			return nil, nil, false
		}
		s.mgr().BuildAsync(t.Key)
		v := RepoView{T: t, Building: true}
		if p, ok := s.mgr().BuildProgress(t.Key); ok {
			v.Progress = &p
		}
		s.render(w, r, http.StatusAccepted, "building.html", t.Name, s.crumbsFor(t), v)
		return nil, nil, false
	}
	repo, release, err := s.mgr().Repo(t.Key)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	return repo, release, true
}

func splitRefs(refs []gitstore.RefInfo) (branches, tags []gitstore.RefInfo) {
	for _, ref := range refs {
		switch {
		case strings.HasPrefix(ref.Name, "refs/heads/"):
			branches = append(branches, ref)
		case strings.HasPrefix(ref.Name, "refs/tags/"):
			tags = append(tags, ref)
		}
	}
	return
}

var (
	hexID   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	shortID = regexp.MustCompile(`^[0-9a-f]{4,39}$`)
)

// resolveRefPath splits "<ref>/<path>" where ref may contain slashes.
func resolveRefPath(repo *gitstore.Repo, rest string) (short string, h gitstore.Hash, p string, err error) {
	refs, head, err := repo.Refs()
	if err != nil {
		return "", h, "", err
	}
	parts := strings.Split(rest, "/")
	if rest == "" || parts[0] == "HEAD" {
		for _, ref := range refs {
			if ref.Name == head {
				return ref.ShortName(), ref.Target(), strings.Join(parts[1:], "/"), nil
			}
		}
		return "", h, "", gitstore.ErrNotFound
	}
	for i := len(parts); i >= 1; i-- {
		cand := strings.Join(parts[:i], "/")
		for _, ref := range refs {
			if ref.Name == "refs/heads/"+cand || ref.Name == "refs/tags/"+cand {
				return cand, ref.Target(), strings.Join(parts[i:], "/"), nil
			}
		}
	}
	if hexID.MatchString(parts[0]) {
		hh, _ := gitstore.ParseHash(parts[0])
		if repo.Has(hh) {
			return parts[0], hh, strings.Join(parts[1:], "/"), nil
		}
	}
	if shortID.MatchString(parts[0]) { // e6f7e7da/README.md
		if hh, err := repo.ResolvePrefix(parts[0]); err == nil {
			return parts[0], hh, strings.Join(parts[1:], "/"), nil
		}
	}
	// Ancestor syntax (main~5, v1.0^2, feature/x~1): the longest prefix
	// that resolves wins, as for plain refs.
	for i := len(parts); i >= 1; i-- {
		cand := strings.Join(parts[:i], "/")
		if !strings.ContainsAny(cand, "~^") {
			continue
		}
		if hh, _, err := repo.Resolve(cand); err == nil {
			return cand, hh, strings.Join(parts[i:], "/"), nil
		}
	}
	return "", h, "", gitstore.ErrNotFound
}

func (s *Server) baseView(r *http.Request, t *target, repo *gitstore.Repo) RepoView {
	refs, _, _ := repo.Refs()
	b, tg := splitRefs(refs)
	return RepoView{T: t, Branches: b, Tags: tg, Clone: s.cloneURL(r, t)}
}

type projectData struct {
	RepoView
	Entries []gitstore.TreeEntry
	Readme  template.HTML
	Empty   bool
	Bundle  bool
}

func (s *Server) project(w http.ResponseWriter, r *http.Request, t *target) {
	repo, release, ok := s.openRepo(w, r, t)
	if !ok {
		return
	}
	defer release()
	d := projectData{RepoView: s.baseView(r, t, repo)}
	ix := s.mgr().Index()
	if rb := ix.Repos[t.Key]; rb != nil && len(rb.Steps) == 1 {
		d.Bundle = true
	}
	short, h, _, err := resolveRefPath(repo, "")
	if err != nil {
		d.Empty = true
		s.render(w, r, http.StatusOK, "project.html", t.Name, s.crumbsFor(t), d)
		return
	}
	d.Ref = short
	if d.Commit, err = repo.Commit(h); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	d.Entries, _ = repo.Tree(d.Commit.Tree)
	gitstore.SortTreeForDisplay(d.Entries)
	d.Readme = s.readme(repo, t, short, "", d.Entries)
	s.render(w, r, http.StatusOK, "project.html", t.Name, s.crumbsFor(t), d)
}

func (s *Server) readme(repo *gitstore.Repo, t *target, ref, dir string, es []gitstore.TreeEntry) template.HTML {
	best := -1
	for i, e := range es {
		ln := strings.ToLower(e.Name)
		if e.IsDir() || !strings.HasPrefix(ln, "readme") {
			continue
		}
		if best < 0 || strings.HasSuffix(ln, ".md") {
			best = i
		}
	}
	// Wikis use home.md as their landing page.
	if best < 0 && t.IsWiki {
		for i, e := range es {
			if strings.EqualFold(e.Name, "home.md") {
				best = i
			}
		}
	}
	if best < 0 {
		return ""
	}
	_, data, err := repo.Object(es[best].Hash)
	if err != nil || len(data) > 1<<20 {
		return ""
	}
	name := strings.ToLower(es[best].Name)
	if strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".markdown") {
		return renderMarkdown(data, "/"+t.Base+"/-/blob/"+ref, "/"+t.Base+"/-/raw/"+ref, dir)
	}
	return template.HTML("<pre>" + template.HTMLEscapeString(string(data)) + "</pre>")
}

func (s *Server) projectAction(w http.ResponseWriter, r *http.Request, t *target, action, rest string) {
	switch action {
	case "bundle":
		s.downloadBundle(w, r, t)
		return
	case "raw", "archive":
		// Non-HTML downloads (curl, wget, <img>) wait for the unbundle
		// instead of receiving the "please wait" page.
		if t.Key != "" && !s.mgr().Built(t.Key) {
			if err := s.mgr().Ensure(t.Key); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	case "uploads":
		s.upload(w, r, t, rest)
		return
	case "snippets":
		if t.IsWiki || t.Snippet != nil {
			break
		}
		if rest == "" {
			s.snippetList(w, r, t.P)
		} else {
			http.Redirect(w, r, "/-/snippets/"+rest, http.StatusFound)
		}
		return
	case "labels", "milestones", "releases":
		if t.IsWiki {
			break
		}
		switch action {
		case "labels":
			s.labelsPage(w, r, t)
		case "milestones":
			s.milestonesPage(w, r, t)
		default:
			s.releasesPage(w, r, t)
		}
		return
	case "issues", "merge_requests":
		if t.IsWiki {
			break
		}
		if rest == "" {
			s.trackerList(w, r, t, action)
		} else {
			s.trackerItem(w, r, t, action, rest)
		}
		return
	}
	repo, release, ok := s.openRepo(w, r, t)
	if !ok {
		return
	}
	defer release()
	switch action {
	case "tree", "blob", "raw", "commits", "archive":
		if action == "archive" {
			rest = strings.TrimSuffix(rest, ".zip")
		}
		short, h, p, err := resolveRefPath(repo, rest)
		if err != nil {
			s.errorPage(w, r, http.StatusNotFound, "Unknown branch, tag or commit.")
			return
		}
		p = strings.Trim(path.Clean("/"+p), "/")
		switch action {
		case "tree":
			s.tree(w, r, t, repo, short, h, p)
		case "blob":
			s.blob(w, r, t, repo, short, h, p)
		case "raw":
			s.raw(w, r, repo, h, p)
		case "commits":
			s.commits(w, r, t, repo, short, h, p)
		case "archive":
			s.archive(w, r, t, repo, short, h)
		}
	case "commit":
		s.commit(w, r, t, repo, rest)
	case "compare":
		s.compare(w, r, t, repo, rest)
	case "branches", "tags":
		v := s.baseView(r, t, repo)
		s.render(w, r, http.StatusOK, "refs.html", t.Name, s.crumbsFor(t), map[string]any{"V": v, "Kind": action})
	default:
		s.errorPage(w, r, http.StatusNotFound, "Unknown page.")
	}
}

package web

// Repository files: tree, file view, raw downloads and attachments.

import (
	"html/template"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/app"
	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

type treeData struct {
	RepoView
	Entries []gitstore.TreeEntry
	Readme  template.HTML
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request, t *target, repo *gitstore.Repo, ref string, h gitstore.Hash, p string) {
	c, err := repo.Commit(h)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	e, err := repo.Lookup(c.Tree, p)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "No such path at this revision.")
		return
	}
	if !e.IsDir() {
		http.Redirect(w, r, "/"+t.Base+"/-/blob/"+ref+"/"+p, http.StatusFound)
		return
	}
	es, err := repo.Tree(e.Hash)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	gitstore.SortTreeForDisplay(es)
	v := s.baseView(r, t, repo)
	v.Ref, v.Commit, v.Path = ref, c, p
	s.render(w, r, http.StatusOK, "tree.html", p+" · "+t.Name, s.crumbsFor(t), treeData{v, es, s.readme(repo, t, ref, p, es)})
}

type blobData struct {
	RepoView
	LFSOid   string
	LFSSize  int64
	LFSReal  bool           // the real LFS content is shown
	LFS      app.AssetState // extraction state when it is not
	Size     int64
	Binary   bool
	Image    bool
	TooLarge bool
	Code     []template.HTML // lines, highlighted when the language is known
	Lang     string
	Markdown template.HTML
	Symlink  string
}

var imageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".ico": true, ".svg": true}

func (s *Server) blob(w http.ResponseWriter, r *http.Request, t *target, repo *gitstore.Repo, ref string, h gitstore.Hash, p string) {
	c, err := repo.Commit(h)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	e, err := repo.Lookup(c.Tree, p)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "No such file at this revision.")
		return
	}
	if e.IsDir() {
		http.Redirect(w, r, "/"+t.Base+"/-/tree/"+ref+"/"+p, http.StatusFound)
		return
	}
	v := s.baseView(r, t, repo)
	v.Ref, v.Commit, v.Path = ref, c, p
	d := blobData{RepoView: v}
	if e.IsSubmodule() {
		d.Symlink = "Submodule at commit " + e.Hash.String()
		s.render(w, r, http.StatusOK, "blob.html", p, s.crumbsFor(t), d)
		return
	}
	// Big files are not shown: read their size from the pack header instead
	// of inflating them (a 300 MB blob would not fit a phone's memory).
	if _, size, err := repo.Stat(e.Hash); err == nil && !e.IsSymlink() {
		image := imageExt[strings.ToLower(path.Ext(p))]
		// Images are shown from /raw; anything over 1 KiB is not an LFS pointer.
		if image && size > 1024 || size > 1<<20 {
			d.Size, d.Image, d.TooLarge = size, image, !image
			s.render(w, r, http.StatusOK, "blob.html", path.Base(p)+" · "+t.Name, s.crumbsFor(t), d)
			return
		}
	}
	_, data, err := repo.Object(e.Hash)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	d.Size = int64(len(data))
	ext := strings.ToLower(path.Ext(p))
	if oid, size, ok := backup.LFSPointer(data); ok {
		d.LFSOid, d.LFSSize = oid, size
		obj, found := s.mgr().LFSObject(oid)
		if !found {
			d.LFS = s.mgr().Asset("lfs")
			d.Code, _ = codeLines("", data)
			s.render(w, r, http.StatusOK, "blob.html", path.Base(p)+" · "+t.Name, s.crumbsFor(t), d)
			return
		}
		// Show the real file. Only small ones are read into memory.
		d.LFSReal, d.Size = true, size
		data = nil
		if size <= 1<<20 && !imageExt[ext] {
			if data, err = os.ReadFile(obj); err != nil {
				s.errorPage(w, r, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	switch {
	case e.IsSymlink():
		d.Symlink = "Symbolic link to " + string(data)
	case imageExt[ext]:
		d.Image = true
	case d.LFSReal && data == nil:
		d.TooLarge = true
	case isBinary(data):
		d.Binary = true
	case len(data) > 1<<20:
		d.TooLarge = true
	case (ext == ".md" || ext == ".markdown") && r.URL.Query().Get("plain") == "":
		d.Markdown = renderMarkdown(data, "/"+t.Base+"/-/blob/"+ref, "/"+t.Base+"/-/raw/"+ref, path.Dir(p))
	default:
		d.Code, d.Lang = codeLines(p, data)
	}
	s.render(w, r, http.StatusOK, "blob.html", path.Base(p)+" · "+t.Name, s.crumbsFor(t), d)
}

func (s *Server) raw(w http.ResponseWriter, r *http.Request, repo *gitstore.Repo, h gitstore.Hash, p string) {
	c, err := repo.Commit(h)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	e, err := repo.Lookup(c.Tree, p)
	if err != nil || e.IsDir() || e.IsSubmodule() {
		http.NotFound(w, r)
		return
	}
	// Anything over 1 KiB cannot be an LFS pointer: stream it.
	if _, size, err := repo.Stat(e.Hash); err == nil && size > 1024 {
		s.streamBlob(w, r, repo, e.Hash, size, p)
		return
	}
	_, data, err := repo.Object(e.Hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var ct string
	if oid, _, ok := backup.LFSPointer(data); ok {
		if obj, found := s.mgr().LFSObject(oid); found {
			s.serveUntrustedFile(w, r, obj, path.Base(p))
			return
		}
		// Never pretend a pointer is the real file (e.g. a broken image/png).
		ct = "text/plain; charset=utf-8"
		w.Header().Set("X-Git-LFS-Pointer", oid)
	} else {
		ct = safeType(p, data)
	}
	untrustedHeaders(w, r, ct, path.Base(p))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// streamBlob sends a blob without holding it in memory when the pack stores
// it whole; the type is sniffed from its first bytes.
func (s *Server) streamBlob(w http.ResponseWriter, r *http.Request, repo *gitstore.Repo, h gitstore.Hash, size int64, p string) {
	sw := &sniffWriter{w: w, start: func(head []byte) {
		untrustedHeaders(w, r, safeType(p, head), path.Base(p))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}}
	_, _, materialized, err := repo.WriteObject(sw, h)
	if err == nil {
		err = sw.flush()
	}
	if err != nil && !sw.started {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	if materialized {
		releaseAfterBig(size)
	}
}

// sniffWriter holds the first 512 bytes, calls start with them (to set
// the headers), then passes everything through.
type sniffWriter struct {
	w       io.Writer
	head    []byte
	started bool
	start   func(head []byte)
}

func (s *sniffWriter) Write(p []byte) (int, error) {
	if s.started {
		return s.w.Write(p)
	}
	n := min(len(p), 512-len(s.head))
	s.head = append(s.head, p[:n]...)
	if len(s.head) < 512 {
		return len(p), nil
	}
	if err := s.flush(); err != nil {
		return 0, err
	}
	if _, err := s.w.Write(p[n:]); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *sniffWriter) flush() error {
	if s.started {
		return nil
	}
	s.started = true
	s.start(s.head)
	_, err := s.w.Write(s.head)
	return err
}

// releaseAfterBig gives memory back to the OS after a large object had to
// be rebuilt in memory: the Go runtime would otherwise keep it for a while,
// above the memory limit on a phone.
func releaseAfterBig(size int64) {
	if size >= 8<<20 {
		go debug.FreeOSMemory()
	}
}

// upload serves an attachment of an issue, merge request or comment:
// /<project>/-/uploads/<secret>/<file>, the target of GitLab's
// "/uploads/<secret>/<file>" Markdown links.
func (s *Server) upload(w http.ResponseWriter, r *http.Request, t *target, rest string) {
	secret, file, ok := strings.Cut(rest, "/")
	if !ok || file == "" || strings.Contains(file, "/") {
		http.NotFound(w, r)
		return
	}
	p, found := s.mgr().Upload(t.P.DiskPath, t.P.FullPath, secret, file)
	if !found {
		if !s.mgr().Asset("uploads").Extracted {
			http.Error(w, "attachments are not extracted: start gbb with --uploads", http.StatusNotFound)
			return
		}
		http.NotFound(w, r)
		return
	}
	s.serveUntrustedFile(w, r, p, file)
}

// serveUntrustedFile streams a file from disk (Range requests supported)
// with the same sandboxing as repository content.
func (s *Server) serveUntrustedFile(w http.ResponseWriter, r *http.Request, file, name string) {
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	head := make([]byte, 512)
	n, _ := f.ReadAt(head, 0)
	untrustedHeaders(w, r, safeType(name, head[:n]), name)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// safeType picks the Content-Type of untrusted content: from the file
// name, except for anything a browser could run (HTML, scripts, XML other
// than SVG), which is sniffed from the first bytes and served as plain text.
func safeType(name string, head []byte) string {
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if ct == "" || strings.HasPrefix(ct, "text/html") || strings.Contains(ct, "javascript") || strings.Contains(ct, "xml") && !strings.Contains(ct, "svg") {
		ct = http.DetectContentType(head)
		if strings.HasPrefix(ct, "text/") {
			ct = "text/plain; charset=utf-8"
		}
	}
	return ct
}

// untrustedHeaders sandboxes repository files and attachments: they must
// never script this origin.
func untrustedHeaders(w http.ResponseWriter, r *http.Request, ct, name string) {
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", ct)
	if r.URL.Query().Get("download") != "" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
}

// pageHeaders are the headers of every HTML page.
func pageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
}

// loading renders the "opening the backup" page.
func (s *Server) loading(w http.ResponseWriter, r *http.Request, code int) {
	s.render(w, r, code, "loading.html", "Loading", nil, nil)
}

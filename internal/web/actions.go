package web

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/app"
)

type backupsData struct {
	Backups []app.BackupFile
	Dirs    []string
	Error   string
}

// backups lists the backups that can be opened.
func (s *Server) backups(w http.ResponseWriter, r *http.Request) {
	d := backupsData{Backups: s.hub.Scan(), Dirs: s.hub.Dirs(), Error: r.URL.Query().Get("error")}
	s.render(w, r, http.StatusOK, "backups.html", "Backups", nil, d)
}

// sameOrigin rejects cross-site form posts: browsers send credentials
// (basic auth) with them, so a token alone is the only other guard.
func sameOrigin(r *http.Request) bool {
	for _, h := range []string{"Origin", "Referer"} {
		v := r.Header.Get(h)
		if v == "" || v == "null" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host != r.Host {
			return false
		}
		return true
	}
	return true // no header (some privacy settings): the token still applies
}

// back returns the local path the form came from, or fallback.
func back(r *http.Request, fallback string) string {
	if u, err := url.Parse(r.Header.Get("Referer")); err == nil && u.Host == r.Host && strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//") {
		if u.RawQuery != "" {
			return u.Path + "?" + u.RawQuery
		}
		return u.Path
	}
	return fallback
}

// action handles POST /-/action/<name>.
func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	if !s.ActionsAllowed {
		http.Error(w, "actions are disabled: this server is reachable from the network without --auth", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("token")), []byte(s.csrf)) != 1 || !sameOrigin(r) {
		http.Error(w, "invalid or expired form; reload the page and try again", http.StatusForbidden)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/-/action/")
	if name == "open" {
		if err := s.hub.Open(r.PostForm.Get("path")); err != nil {
			http.Redirect(w, r, "/-/backups?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	m := s.mgr()
	if m == nil {
		http.Redirect(w, r, "/-/backups", http.StatusSeeOther)
		return
	}
	switch name {
	case "extract":
		if err := m.EnableAsset(r.PostForm.Get("what"), r.PostForm.Get("mode")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	case "prefetch":
		m.PrefetchActive(20)
	case "prune":
		s.pruneCaches(m)
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, back(r, "/-/info"), http.StatusSeeOther)
}

// pruneCaches removes the data of every backup but the open one.
func (s *Server) pruneCaches(m *app.Manager) {
	root := m.Root()
	if root == "" {
		return
	}
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(root), "backup-*"))
	for _, d := range dirs {
		if d == root {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, "SOURCE")); err == nil { // ours
			os.RemoveAll(d)
		}
	}
}

// CacheUsage sums the size of other backups' caches (shown next to the
// button that removes them).
func otherCaches(m *app.Manager) (n int, size int64) {
	root := m.Root()
	if root == "" {
		return 0, 0
	}
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(root), "backup-*"))
	for _, d := range dirs {
		if d == root {
			continue
		}
		n++
		filepath.Walk(d, func(_ string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				size += fi.Size()
			}
			return nil
		})
	}
	return n, size
}

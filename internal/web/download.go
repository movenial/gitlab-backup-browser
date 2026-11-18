package web

// Downloads: zip archives, original bundles and git clone (dumb HTTP).

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/app"
	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

// --- downloads ---

func (s *Server) archive(w http.ResponseWriter, r *http.Request, t *target, repo *gitstore.Repo, ref string, h gitstore.Hash) {
	c, err := repo.Commit(h)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := strings.ReplaceAll(path.Base(t.Base)+"-"+ref, "/", "-")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	zw := zip.NewWriter(w)
	var add func(tree gitstore.Hash, prefix string) error
	add = func(tree gitstore.Hash, prefix string) error {
		es, err := repo.Tree(tree)
		if err != nil {
			return err
		}
		for _, e := range es {
			p := prefix + e.Name
			switch {
			case e.IsDir():
				if err := add(e.Hash, p+"/"); err != nil {
					return err
				}
			case e.IsSubmodule():
			default:
				// Small blobs may be LFS pointers; bigger ones are streamed.
				var data []byte
				var lfsFile string
				_, size, err := repo.Stat(e.Hash)
				if err != nil {
					return err
				}
				if size <= 1024 {
					if _, data, err = repo.Object(e.Hash); err != nil {
						return err
					}
					if oid, _, ok := backup.LFSPointer(data); ok {
						if obj, found := s.mgr().LFSObject(oid); found {
							lfsFile = obj
						}
					}
				}
				hdr := &zip.FileHeader{Name: name + "/" + p, Method: zip.Deflate, Modified: c.Committer.When}
				mode := fs.FileMode(0o644)
				if e.Mode == 0o100755 {
					mode = 0o755
				}
				if e.IsSymlink() {
					mode = fs.ModeSymlink | 0o777
				}
				hdr.SetMode(mode)
				fw, err := zw.CreateHeader(hdr)
				if err != nil {
					return err
				}
				if lfsFile != "" { // the real file, streamed, not the pointer
					f, err := os.Open(lfsFile)
					if err != nil {
						return err
					}
					_, err = io.Copy(fw, f)
					f.Close()
					if err != nil {
						return err
					}
					continue
				}
				if data != nil {
					if _, err := fw.Write(data); err != nil {
						return err
					}
					continue
				}
				_, _, materialized, err := repo.WriteObject(fw, e.Hash)
				if err != nil {
					return err
				}
				if materialized {
					releaseAfterBig(size)
				}
			}
		}
		return nil
	}
	if err := add(c.Tree, ""); err != nil {
		return // headers are sent; the client sees a truncated zip
	}
	zw.Close()
}

func (s *Server) downloadBundle(w http.ResponseWriter, r *http.Request, t *target) {
	ix, src := s.mgr().Index(), s.mgr().Source()
	rb := ix.Repos[t.Key]
	if rb == nil || src == nil || len(rb.Steps) != 1 {
		s.errorPage(w, r, http.StatusNotFound, "No single bundle for this repository (it is stored as an incremental chain; clone it instead).")
		return
	}
	rc, err := src.Open(rb.Steps[0].Bundle)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	name := strings.ReplaceAll(t.Base, "/", "-") + ".bundle"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.FormatInt(rb.Steps[0].Size, 10))
	io.Copy(w, rc)
}

// --- git HTTP: the smart protocol (smart.go), dumb files as a fallback ---

var packFile = regexp.MustCompile(`^objects/pack/pack-[0-9a-f]{40}\.(pack|idx)$`)

func (s *Server) git(w http.ResponseWriter, r *http.Request, project, rest string) {
	ix := s.mgr().Index()
	t := s.target(ix, project)
	if t == nil || t.Key == "" {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	if s.lfs(w, r, t, rest) {
		return
	}
	switch {
	case rest == "" || rest == "/":
		http.Redirect(w, r, "/"+t.Base, http.StatusFound)
		return
	case rest == "info/refs" && r.URL.Query().Get("service") == "git-upload-pack":
		s.advertise(w, t)
		return
	case rest == "git-upload-pack":
		s.uploadPack(w, r, t)
		return
	case rest == "git-receive-pack" || rest == "info/refs" && r.URL.Query().Get("service") != "":
		http.Error(w, "read-only: this is a backup", http.StatusForbidden)
		return
	case rest == "info/refs", rest == "HEAD", rest == "objects/info/packs", packFile.MatchString(rest):
	default:
		http.NotFound(w, r)
		return
	}
	// Cloning blocks until the repository is unbundled. Files are then
	// served straight from disk: the repository is not even opened.
	if err := s.mgr().Ensure(t.Key); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, app.ErrNoBundle) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	if rest == "info/refs" {
		// Generated from packed-refs so internal refs stay hidden, also
		// for repositories unbundled by older versions.
		repo, release, err := s.mgr().Repo(t.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer release()
		b, err := repo.InfoRefs()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
		return
	}
	file := path.Join(s.mgr().RepoDir(t.Key), rest)
	switch {
	case strings.HasSuffix(rest, ".pack"):
		w.Header().Set("Content-Type", "application/x-git-packed-objects")
	case strings.HasSuffix(rest, ".idx"):
		w.Header().Set("Content-Type", "application/x-git-packed-objects-toc")
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFile(w, r, file)
}

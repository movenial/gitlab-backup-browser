package web

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Git LFS batch API (download only), so `git clone` with git-lfs installed
// fetches the real files: https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md

var lfsObjectPath = regexp.MustCompile(`^gitlab-lfs/objects/([0-9a-f]{64})$`)

type lfsBatchRequest struct {
	Operation string `json:"operation"`
	Objects   []struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"objects"`
}

type lfsAction struct {
	Href      string `json:"href"`
	ExpiresIn int    `json:"expires_in,omitempty"`
}

type lfsObject struct {
	OID     string               `json:"oid"`
	Size    int64                `json:"size"`
	Actions map[string]lfsAction `json:"actions,omitempty"`
	Error   *lfsError            `json:"error,omitempty"`
}

type lfsError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func lfsJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// lfs handles /<project>.git/info/lfs/objects/batch and
// /<project>.git/gitlab-lfs/objects/<oid>. It reports whether it handled
// the request.
func (s *Server) lfs(w http.ResponseWriter, r *http.Request, t *target, rest string) bool {
	if rest == "info/lfs/objects/batch" {
		if r.Method != http.MethodPost {
			lfsJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "POST only"})
			return true
		}
		var req lfsBatchRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
			lfsJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "bad request"})
			return true
		}
		if req.Operation != "download" {
			lfsJSON(w, http.StatusForbidden, map[string]string{"message": "this is a read-only backup browser"})
			return true
		}
		if !s.mgr().Asset("lfs").Extracted {
			lfsJSON(w, http.StatusNotFound, map[string]string{"message": "Git LFS objects are not extracted; start gbb with --lfs"})
			return true
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base := scheme + "://" + r.Host + "/" + t.Base + ".git/gitlab-lfs/objects/"
		resp := struct {
			Transfer string      `json:"transfer"`
			Objects  []lfsObject `json:"objects"`
		}{Transfer: "basic"}
		for _, o := range req.Objects {
			obj := lfsObject{OID: o.OID, Size: o.Size}
			if _, ok := s.mgr().LFSObject(strings.ToLower(o.OID)); ok {
				obj.Actions = map[string]lfsAction{"download": {Href: base + strings.ToLower(o.OID), ExpiresIn: 86400}}
			} else {
				obj.Error = &lfsError{Code: 404, Message: "object not in the backup"}
			}
			resp.Objects = append(resp.Objects, obj)
		}
		lfsJSON(w, http.StatusOK, resp)
		return true
	}
	if m := lfsObjectPath.FindStringSubmatch(rest); m != nil {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return true
		}
		obj, ok := s.mgr().LFSObject(m[1])
		if !ok {
			http.NotFound(w, r)
			return true
		}
		s.serveUntrustedFile(w, r, obj, m[1])
		return true
	}
	return false
}

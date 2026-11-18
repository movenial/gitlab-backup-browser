package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Credentials is a single user/password pair for HTTP basic auth.
type Credentials struct {
	User, Password string
}

// ParseCredentials parses "user:password".
func ParseCredentials(s string) (*Credentials, error) {
	u, p, ok := strings.Cut(s, ":")
	if !ok || u == "" || p == "" {
		return nil, errors.New(`auth must look like "user:password"`)
	}
	return &Credentials{User: u, Password: p}, nil
}

// RequireAuth protects h with HTTP basic auth. git clients prompt for (or
// reuse stored) credentials, so cloning keeps working.
func RequireAuth(h http.Handler, c *Credentials) http.Handler {
	wantU := sha256.Sum256([]byte(c.User))
	wantP := sha256.Sum256([]byte(c.Password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		gotU := sha256.Sum256([]byte(u))
		gotP := sha256.Sum256([]byte(p))
		okU := subtle.ConstantTimeCompare(gotU[:], wantU[:])
		okP := subtle.ConstantTimeCompare(gotP[:], wantP[:])
		if !ok || okU&okP != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="gitlab-backup-browser", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

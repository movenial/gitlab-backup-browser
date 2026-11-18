package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAllowHosts(t *testing.T) {
	h := AllowHosts(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), []string{"nas.lan", "Gbb.Example.com:8443"})
	for host, want := range map[string]int{
		// An attacker cannot make a browser send these for his own page.
		"127.0.0.1:8080":   200,
		"192.168.1.20":     200,
		"[::1]:8080":       200,
		"[fe80::1]":        200,
		"localhost:8080":   200,
		"LOCALHOST.":       200,
		"nas.lan:8080":     200,
		"NAS.lan.":         200,
		"gbb.example.com":  200,
		"evil.example":     421,
		"evil.example:80":  421,
		"nas.lan.evil.com": 421,
		// Names made to look like an address or like localhost.
		"127.0.0.1.evil.example": 421,
		"localhost.evil.example": 421,
		"2130706433":             421,
		"":                       421,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %q: %d, want %d", host, w.Code, want)
		}
		if want == 421 && !strings.Contains(w.Body.String(), "--host") {
			t.Errorf("Host %q: the refusal does not name --host: %s", host, w.Body)
		}
	}
}

// A rebinding page has the form token (it read the page) and sends an Origin
// that matches its own Host, which the same-origin check of the action
// buttons accepts: only the Host check stops it.
func TestAllowHostsStopsRebinding(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-site", http.StatusForbidden)
		}
	})
	rebound := func(h http.Handler) int {
		r := httptest.NewRequest("POST", "/-/action/prune", nil)
		r.Host = "evil.example:8080"
		r.Header.Set("Origin", "http://evil.example:8080")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := rebound(inner); c != 200 {
		t.Fatalf("without the Host check: %d, the test no longer shows the attack", c)
	}
	if c := rebound(AllowHosts(inner, nil)); c != 421 {
		t.Fatalf("with the Host check: %d", c)
	}
}

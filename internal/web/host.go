package web

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// AllowHosts refuses requests whose Host header is a name gbb was not told
// about. It is what stops DNS rebinding when there is no login: a web page
// whose domain is re-pointed at this server (127.0.0.1 or a LAN address) is
// one site with it for the browser, so it could read every repository and
// use the action buttons. Such a page controls a name, never an IP address
// or "localhost", so those always pass; names are the ones this server is
// really reached by (--host).
func AllowHosts(h http.Handler, names []string) http.Handler {
	allowed := map[string]bool{"localhost": true}
	for _, n := range names {
		allowed[hostName(n)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostName(r.Host)
		if _, err := netip.ParseAddr(host); err != nil && !allowed[host] {
			http.Error(w, fmt.Sprintf("gbb does not know the host name %q: open it by IP address, or start it with --host %s (or with --auth)", host, host), http.StatusMisdirectedRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// hostName returns the name in a Host header or a --host value: lower
// case, without port, IPv6 brackets or trailing dot.
func hostName(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

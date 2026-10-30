package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// ValidateOrigin reports whether s can be trusted as an origin: a scheme
// and a host, with an optional port and nothing after it, such as
// "https://app.example.com".
func ValidateOrigin(s string) error {
	if s == "null" {
		return errors.New(`the opaque origin "null" cannot be trusted`)
	}
	if err := http.NewCrossOriginProtection().AddTrustedOrigin(s); err != nil {
		return fmt.Errorf("not an origin: %w", err)
	}
	return nil
}

// guard keeps browser pages away from the server, as the MCP transport asks
// of servers: a page a browser loads can otherwise reach the port, whether
// across sites or through a name that rebinds to the server's address.
//
// It refuses a request with an Origin header that is not listed, so with
// none listed every browser page; one a browser marks as cross-site without
// an Origin; and, when the request did not arrive on a loopback address and
// hosts are listed, one whose Host is not among them. On loopback the
// transport's own guard insists on a loopback Host instead.
//
// A GET or HEAD of the health path is left open: a liveness probe addresses
// the server as it finds it and its answer says nothing about the account.
// Any other request there, as when the MCP endpoint shares the path, is
// checked.
type guard struct {
	next    http.Handler
	origins map[string]bool
	hosts   []string
	cross   *http.CrossOriginProtection
}

func newGuard(next http.Handler, origins, hosts []string) http.Handler {
	g := &guard{
		next:    next,
		origins: make(map[string]bool, len(origins)),
		hosts:   hosts,
		cross:   http.NewCrossOriginProtection(),
	}
	for _, o := range origins {
		if ValidateOrigin(o) == nil && g.cross.AddTrustedOrigin(o) == nil {
			g.origins[o] = true
		}
	}
	return g
}

func (g *guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == HealthPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		g.next.ServeHTTP(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !g.origins[origin] {
		http.Error(w, "Forbidden: origin not allowed", http.StatusForbidden)
		return
	}
	if err := g.cross.Check(r); err != nil {
		http.Error(w, "Forbidden: cross-origin request", http.StatusForbidden)
		return
	}
	if len(g.hosts) > 0 && !onLoopback(r) && !hostListed(r.Host, g.hosts) {
		http.Error(w, "Forbidden: host not allowed", http.StatusForbidden)
		return
	}
	g.next.ServeHTTP(w, r)
}

// onLoopback reports whether r arrived on a loopback address.
func onLoopback(r *http.Request) bool {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || addr == nil {
		return false
	}
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

// hostListed reports whether host, a Host header, is in hosts. An entry
// with a port matches only that port; one without matches any.
func hostListed(host string, hosts []string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	for _, entry := range hosts {
		if strings.EqualFold(entry, host) {
			return true
		}
		if _, _, err := net.SplitHostPort(entry); err != nil &&
			strings.EqualFold(strings.Trim(entry, "[]"), strings.Trim(name, "[]")) {
			return true
		}
	}
	return false
}

// errHost explains what a Host entry may be.
var errHost = errors.New(
	"a host is a name or an address with an optional port, such as mcp.example.com or mcp.example.com:8443",
)

// ValidateHost reports whether s can be listed as a Host: a name or an
// address, with an optional port from 1 to 65535, and nothing else.
func ValidateHost(s string) error {
	host := s
	if h, port, err := net.SplitHostPort(s); err == nil {
		if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
			return errHost
		}
		host = h
	} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		host = s[1 : len(s)-1]
	}
	if strings.HasPrefix(s, "[") {
		// Brackets hold an IPv6 address and nothing else.
		if ip, err := netip.ParseAddr(host); err != nil || !ip.Is6() {
			return errHost
		}
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil || hostName(host) {
		return nil
	}
	return errHost
}

// hostName reports whether s is a host name: letters, digits, dots,
// hyphens and underscores.
func hostName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune(".-_", r) {
			return false
		}
	}
	return true
}

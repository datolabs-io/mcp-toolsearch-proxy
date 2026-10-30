package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
	`"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// request is an MCP initialize, or a GET when path is the health path, as
// it arrives on the local address local with the headers given.
type request struct {
	path, local, host, origin, fetchSite string
}

// status serves r with a server built with opts and returns the status.
func status(t *testing.T, opts *server.Options, r request) int {
	t.Helper()
	path := cmpOr(r.path, "/mcp")
	method, body := http.MethodPost, strings.NewReader(initialize)
	if path == server.HealthPath {
		method, body = http.MethodGet, strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, body)
	req.Host = r.host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	if r.fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", r.fetchSite)
	}
	addr, err := net.ResolveTCPAddr("tcp", r.local)
	require.NoError(t, err)
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
	rec := httptest.NewRecorder()
	server.Handler(newServer(t, opts, nil), opts).ServeHTTP(rec, req)
	return rec.Code
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

const (
	loopback = "127.0.0.1:8000"
	public   = "192.0.2.10:8000"
)

func TestOriginsAreRefusedUnlessAllowed(t *testing.T) {
	t.Parallel()
	allowed := &server.Options{AllowedOrigins: []string{"https://app.example.com"}}
	for name, tc := range map[string]struct {
		opts *server.Options
		req  request
		want int
	}{
		"no origin": {nil, request{local: loopback, host: loopback}, http.StatusOK},
		"any origin by default": {
			nil, request{local: loopback, host: loopback, origin: "https://app.example.com"}, http.StatusForbidden,
		},
		"a page on the server's own name": {
			nil, request{local: public, host: "rebound.example.net", origin: "http://rebound.example.net"},
			http.StatusForbidden,
		},
		"allowed origin": {
			allowed, request{local: loopback, host: loopback, origin: "https://app.example.com"}, http.StatusOK,
		},
		"another origin": {
			allowed, request{local: loopback, host: loopback, origin: "https://other.example.com"}, http.StatusForbidden,
		},
		"opaque origin": {allowed, request{local: loopback, host: loopback, origin: "null"}, http.StatusForbidden},
		"cross-site fetch without an origin": {
			nil, request{local: loopback, host: loopback, fetchSite: "cross-site"}, http.StatusForbidden,
		},
		"health probe from a page": {
			nil, request{path: server.HealthPath, local: public, host: public, origin: "https://app.example.com"},
			http.StatusOK,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, status(t, tc.opts, tc.req))
		})
	}
}

func TestHostIsCheckedOffLoopback(t *testing.T) {
	t.Parallel()
	hosts := &server.Options{AllowedHosts: []string{"mcp.example.com", "proxy.example.com:9000"}}
	for name, tc := range map[string]struct {
		opts *server.Options
		req  request
		want int
	}{
		"allowed name":              {hosts, request{local: public, host: "mcp.example.com"}, http.StatusOK},
		"allowed name in any case":  {hosts, request{local: public, host: "MCP.example.com"}, http.StatusOK},
		"allowed name on any port":  {hosts, request{local: public, host: "mcp.example.com:8000"}, http.StatusOK},
		"allowed name and port":     {hosts, request{local: public, host: "proxy.example.com:9000"}, http.StatusOK},
		"allowed name, other port":  {hosts, request{local: public, host: "proxy.example.com:9001"}, http.StatusForbidden},
		"another name":              {hosts, request{local: public, host: "rebound.example.net"}, http.StatusForbidden},
		"the bare address":          {hosts, request{local: public, host: public}, http.StatusForbidden},
		"no list, any name":         {nil, request{local: public, host: "rebound.example.net"}, http.StatusOK},
		"loopback ignores the list": {hosts, request{local: loopback, host: "localhost:8000"}, http.StatusOK},
		"loopback keeps its guard":  {hosts, request{local: loopback, host: "mcp.example.com"}, http.StatusForbidden},
		"health probe by address": {
			hosts, request{path: server.HealthPath, local: public, host: public}, http.StatusOK,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, status(t, tc.opts, tc.req))
		})
	}
}

func TestValidateOrigin(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"https://app.example.com", "http://localhost:3000"} {
		require.NoError(t, server.ValidateOrigin(ok), ok)
	}
	for _, bad := range []string{"app.example.com", "https://app.example.com/", "https://app.example.com/x", "null", ""} {
		require.Error(t, server.ValidateOrigin(bad), bad)
	}
}

func TestValidateHost(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{
		"mcp.example.com", "mcp.example.com:8000", "MCP.Example.com", "192.0.2.10", "192.0.2.10:8000",
		"[2001:db8::1]", "[2001:db8::1]:8000", "2001:db8::1", "localhost",
	} {
		require.NoError(t, server.ValidateHost(ok), ok)
	}
	for _, bad := range []string{
		"", "mcp.example.com:not-a-port", "mcp.example.com:8000:9000", "mcp.example.com:", "mcp.example.com:0",
		"mcp.example.com:65536", ":8000", "https://mcp.example.com", "mcp.example.com/mcp", "user@mcp.example.com",
		"mcp example.com", "[mcp.example.com]:8000",
	} {
		require.Error(t, server.ValidateHost(bad), bad)
	}
}

// With the MCP endpoint served at the health path, only the probe's GET is
// left unchecked: a POST reaches the MCP handler and is checked like any.
func TestHealthPathExemptsOnlyProbes(t *testing.T) {
	t.Parallel()
	for _, path := range []string{server.HealthPath, "/"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			opts := &server.Options{Path: path, AllowedHosts: []string{"mcp.example.com"}}
			h := server.Handler(newServer(t, opts, nil), opts)
			addr, err := net.ResolveTCPAddr("tcp", public)
			require.NoError(t, err)
			serveAs := func(method, origin, host string, body io.Reader) int {
				req := httptest.NewRequestWithContext(
					context.WithValue(t.Context(), http.LocalAddrContextKey, addr), method, server.HealthPath, body)
				req.Host = host
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				if origin != "" {
					req.Header.Set("Origin", origin)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec.Code
			}

			assert.Equal(
				t,
				http.StatusForbidden,
				serveAs(
					http.MethodPost,
					"http://rebound.example.net",
					"rebound.example.net",
					strings.NewReader(initialize),
				),
			)
			assert.Equal(t, http.StatusForbidden,
				serveAs(http.MethodPost, "", "rebound.example.net", strings.NewReader(initialize)))
			assert.NotEqual(t, http.StatusForbidden, serveAs(http.MethodGet, "", public, http.NoBody),
				"a probe still reaches the health path")
		})
	}
}

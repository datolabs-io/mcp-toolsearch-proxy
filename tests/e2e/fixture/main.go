// Command fixture is a small MCP server for the end-to-end tests: -set
// weather, -set admin or -set billing picks which made-up tools it serves.
// Each tool answers with its name and arguments, so a test can tell the call
// arrived as sent.
//
// With -forward it is no MCP server but a reverse proxy in front of a
// gateway, refusing every request whose Host is not -require-host, as a
// gateway reached at an internal address that serves only its public name.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	exitUsage         = 2
	readHeaderTimeout = 10 * time.Second
	// maxBody bounds the request bodies the fixture reads itself.
	maxBody = 1 << 20
)

// tool is a made-up tool: a name, what it says it does, and whether it only
// reads.
type tool struct {
	name, description string
	readOnly          bool
}

//nolint:gochecknoglobals // fixed test fixtures
var sets = map[string][]tool{
	"weather": {
		{"get_forecast", "Hourly and daily weather forecast for a place: temperature, rain and wind.", true},
		{"get_alerts", "Severe weather warnings in force for a region.", true},
	},
	"admin": {
		{"list_users", "List the user accounts of the organization, with their roles.", true},
		{"create_user", "Create a user account with a name, an email address and a role.", false},
		{"delete_user", "Delete a user account and revoke its access.", false},
		{"echo", "Answer with the arguments given.", true},
		{countCalls, "Count the calls each tool of this server has received.", true},
		{waitTool, "Wait the given number of seconds, at most a minute, then answer.", true},
	},
	"billing": {
		{"get_invoice", "Fetch an invoice by its number.", true},
	},
}

type answer struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

func main() {
	set := flag.String("set", "weather", "the tools to serve: weather, admin or billing")
	addr := flag.String("addr", "0.0.0.0:8000", "listen address")
	legacy := flag.Bool("legacy", false, "keep sessions, as servers built before MCP 2026-07-28 do")
	discover500 := flag.Bool("discover-500", false,
		"answer server/discover with 500, as some servers built before MCP 2026-07-28 do")
	listOK := flag.Int("list-ok", -1,
		"answer this many tools/list requests, and refuse every later one with a JSON-RPC error; -1 answers all")
	forward := flag.String("forward", "", "serve as a reverse proxy to this URL instead of as an MCP server")
	requireHost := flag.String("require-host", "", "with -forward, refuse requests whose Host is not this")
	flag.Parse()
	target, perr := url.Parse(*forward)
	tools, known := sets[*set]
	var h http.Handler
	switch {
	case *forward != "" && (perr != nil || *requireHost == ""):
		fmt.Fprintln(os.Stderr, "-forward needs a URL and -require-host")
		os.Exit(exitUsage)
	case *forward != "":
		h = forwardTo(target, *requireHost)
	case !known:
		fmt.Fprintf(os.Stderr, "unknown set %q\n", *set)
		os.Exit(exitUsage)
	default:
		h = mcpServer(*set, tools, !*legacy)
		if *discover500 {
			h = refuseDiscover(h)
		}
		if *listOK >= 0 {
			h = refuseListing(h, int64(*listOK))
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/", h)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	hs := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// countCalls is the tool that answers how many calls each tool received,
// so a test can tell a tool never ran.
const countCalls = "count_calls"

// waitTool is the tool that answers after the seconds its arguments give,
// so a test can make a call take longer than a gateway waits.
const waitTool = "wait"

// maxWait bounds how long waitTool waits.
const maxWait = time.Minute

// wait waits the seconds args give, at most maxWait, or until ctx is done.
func wait(ctx context.Context, args map[string]any) error {
	seconds, _ := args["seconds"].(float64)
	timer := time.NewTimer(min(time.Duration(seconds*float64(time.Second)), maxWait))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait: %w", ctx.Err())
	}
}

// counts are the calls each tool received.
type counts struct {
	Calls map[string]int `json:"calls"`
}

// mcpServer serves tools at /mcp, without sessions when stateless.
func mcpServer(set string, tools []tool, stateless bool) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture-" + set, Version: "0"}, nil)
	var mu sync.Mutex
	calls := map[string]int{}
	for _, t := range tools {
		annotations := &mcp.ToolAnnotations{ReadOnlyHint: t.readOnly}
		if t.name == countCalls {
			mcp.AddTool(srv, &mcp.Tool{Name: t.name, Description: t.description, Annotations: annotations},
				func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, counts, error) {
					mu.Lock()
					defer mu.Unlock()
					return nil, counts{Calls: maps.Clone(calls)}, nil
				})
			continue
		}
		mcp.AddTool(
			srv,
			&mcp.Tool{Name: t.name, Description: t.description, Annotations: annotations},
			func(ctx context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, answer, error) {
				mu.Lock()
				calls[t.name]++
				mu.Unlock()
				if t.name == waitTool {
					if err := wait(ctx, args); err != nil {
						return nil, answer{}, err
					}
				}
				return nil, answer{Tool: t.name, Arguments: args}, nil
			},
		)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: stateless, JSONResponse: true}))
	return mux
}

// refuseDiscover answers MCP 2026-07-28's server/discover with 500, and
// passes everything else to next.
func refuseDiscover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Method") == "server/discover" {
			http.Error(w, `{"jsonrpc":"2.0","error":{"code":-32603,"message":"Internal server error"},"id":1}`,
				http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuseListing passes the first ok tools/list requests to next, and
// answers every later one with a JSON-RPC error, as a server whose own
// backend went down does.
func refuseListing(next http.Handler, ok int64) http.Handler {
	var lists atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(body, &msg) != nil || msg.Method != "tools/list" || lists.Add(1) <= ok {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"listing refused"}}`, msg.ID)
	})
}

// forwardTo passes requests to target, refusing those whose Host is not
// host with 404, as a gateway that routes by name does.
func forwardTo(target *url.URL, host string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) },
		// A transport of its own: httptest servers closing in other tests
		// close the default transport's idle connections under it.
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "no route for host "+r.Host, http.StatusNotFound)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

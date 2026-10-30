package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const readHeaderTimeout = 10 * time.Second

// upstream is an MCP server, in the process, that serves a catalog of
// made-up tools. Each tool answers that it was done, and the upstream
// records which tools each caller called, by the caller's bearer token.
type upstream struct {
	URL string

	hs    *http.Server
	mu    sync.Mutex
	calls map[string][]string
}

// startUpstream serves tools on a free loopback port.
func startUpstream(ctx context.Context, tools []Tool) (*upstream, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "bench-upstream", Version: "0"}, nil)
	u := &upstream{calls: map[string][]string{}}
	for _, tl := range tools {
		raw, err := json.Marshal(tl.Schema)
		if err != nil {
			return nil, fmt.Errorf("schema of %s: %w", tl.Name, err)
		}
		name := tl.Name
		// The generic AddTool checks the arguments against the schema before
		// the handler runs, as the proxy does, so a call with arguments that
		// do not fit is refused and not recorded, in every mode.
		mcp.AddTool(srv, &mcp.Tool{
			Name: name, Description: tl.Description, InputSchema: json.RawMessage(raw),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tl.ReadOnly},
		}, func(_ context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			u.record(token(req.Extra), name)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf(`{"status":"done","tool":%q}`, name),
			}}}, nil, nil
		})
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	u.hs = &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	u.URL = "http://" + ln.Addr().String() + "/mcp"
	go func() {
		if serr := u.hs.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			panic(serr)
		}
	}()
	return u, nil
}

// Close stops the server.
func (u *upstream) Close() { _ = u.hs.Close() }

// Calls is the tools the caller with this token called, in order.
func (u *upstream) Calls(tok string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls[tok]...)
}

// Reset forgets the calls of the caller with this token.
func (u *upstream) Reset(tok string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.calls, tok)
}

func (u *upstream) record(tok, name string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls[tok] = append(u.calls[tok], name)
}

// token is the bearer token of a request, or empty.
func token(extra *mcp.RequestExtra) string {
	if extra == nil {
		return ""
	}
	return strings.TrimPrefix(extra.Header.Get("Authorization"), "Bearer ")
}

// connect opens an MCP client session to url as the caller with this token.
func connect(ctx context.Context, url, tok string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "bench", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: bearer{tok: tok, next: http.DefaultTransport}},
	}
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", url, err)
	}
	return cs, nil
}

// bearer sends a bearer token with every request.
type bearer struct {
	tok  string
	next http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return b.next.RoundTrip(r) //nolint:wrapcheck // passed through unchanged
}

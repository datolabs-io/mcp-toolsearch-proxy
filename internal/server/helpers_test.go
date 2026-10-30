package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// gate is an upstream whose tool "hold" holds its call until let go, whose
// tool "slow" takes a while and whose tool "echo" answers with its text.
type gate struct {
	entered chan struct{}
	open    chan struct{}
	slow    time.Duration
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}), open: make(chan struct{})}
}

func (g *gate) CallTool(_ context.Context, _, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	switch name {
	case "hold":
		close(g.entered)
		<-g.open
	case "slow":
		time.Sleep(g.slow)
	case "echo":
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(args, &in)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil
	}
	return &mcp.CallToolResult{}, nil
}

func (g *gate) list(context.Context, string) ([]*mcp.Tool, error) {
	return []*mcp.Tool{{Name: "hold"}, {Name: "slow"}, {Name: "echo"}}, nil
}

// newServer returns a server over g, or over a fresh gate when g is nil.
func newServer(t *testing.T, opts *server.Options, g *gate) *mcp.Server {
	t.Helper()
	if g == nil {
		g = newGate()
	}
	return server.New(opts, catalog.NewStore(catalog.Tools(g.list), nil), g)
}

func callParams(name string, args map[string]any) *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: server.ToolCallWrite, Arguments: map[string]any{"name": name, "arguments": args}}
}

func connect(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	return connectWith(t, endpoint, "")
}

// connectWith connects to endpoint sending authorization, when not empty,
// as the Authorization header of every request.
func connectWith(t *testing.T, endpoint, authorization string) *mcp.ClientSession {
	t.Helper()
	cs, err := dial(t, endpoint, authorization)
	require.NoError(t, err)
	return cs
}

// dial connects as connectWith does, returning the error, for a goroutine
// other than the test's.
func dial(t *testing.T, endpoint, authorization string) (*mcp.ClientSession, error) {
	t.Helper()
	hc := &http.Client{Transport: bearer{value: authorization}}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(
		t.Context(),
		&mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: true},
		nil,
	)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, nil
}

type bearer struct{ value string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.value != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", b.value)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// get returns the status and body of a GET to path.
func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// listing makes a catalog lister of an upstream client, with the
// upstream's instructions.
func listing(c *upstream.Client) catalog.Lister {
	return func(ctx context.Context, authorization string) (catalog.Listing, error) {
		l, err := c.List(ctx, authorization)
		return catalog.Listing{Tools: l.Tools, Instructions: l.Instructions}, err
	}
}

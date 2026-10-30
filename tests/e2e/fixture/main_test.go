package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func connect(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: endpoint, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestRefuseListingAnswersTheFirstListsOnly(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(refuseListing(mcpServer("billing", sets["billing"], true), 1))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")

	first, err := cs.ListTools(t.Context(), nil)
	require.NoError(t, err)
	_, err = cs.ListTools(t.Context(), nil)
	res, cerr := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_invoice"})

	assert.Len(t, first.Tools, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing refused")
	require.NoError(t, cerr, "only the listing is refused")
	assert.False(t, res.IsError)
}

func TestForwardToRefusesOtherHosts(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Reached", r.Host+r.URL.Path)
	}))
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	front := httptest.NewServer(forwardTo(target, "gateway.example.com"))
	t.Cleanup(front.Close)

	status := func(host string) (int, string) {
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/mcp/all", nil)
		require.NoError(t, reqErr)
		if host != "" {
			req.Host = host
		}
		resp, reqErr := front.Client().Do(req)
		require.NoError(t, reqErr)
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("X-Reached")
	}

	code, reached := status("gateway.example.com")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, target.Host+"/mcp/all", reached, "with the backend's own Host")
	code, _ = status("")
	assert.Equal(t, http.StatusNotFound, code, "the URL's own host is refused")
}

func TestCountCallsCountsEveryOtherTool(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(mcpServer("admin", sets["admin"], true))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")
	count := func() map[string]any {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: countCalls})
		require.NoError(t, err)
		out, ok := res.StructuredContent.(map[string]any)
		require.True(t, ok)
		calls, _ := out["calls"].(map[string]any)
		return calls
	}

	before := count()
	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}})
	require.NoError(t, err)
	after := count()

	assert.Empty(t, before)
	assert.Equal(t, map[string]any{"echo": float64(1)}, after)
}

func TestWaitAnswersAfterTheSecondsGiven(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(mcpServer("admin", sets["admin"], true))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")
	start := time.Now()

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: waitTool, Arguments: map[string]any{"seconds": 0.2},
	})

	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
}

package metrics_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/metrics"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	ts := httptest.NewServer(m.Handler())
	t.Cleanup(ts.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL, nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func TestBuildsAndSearches(t *testing.T) {
	t.Parallel()
	m := metrics.New(&metrics.Options{Catalogs: func() int { return 2 }, MaxTools: func() int { return 41 }})
	m.BuildInfo("1.2.3")

	m.Build(40, 2*time.Second, nil)
	m.Build(41, time.Second, errors.New("embed failed"))
	m.Build(0, 15*time.Second, errors.New("unauthorized"))
	m.Search("hybrid")

	out := scrape(t, m)
	assert.Contains(t, out, `mcp_toolsearch_catalog_builds_total{outcome="ok"} 1`)
	assert.Contains(t, out, `mcp_toolsearch_catalog_builds_total{outcome="lexical_only"} 1`)
	assert.Contains(t, out, `mcp_toolsearch_catalog_builds_total{outcome="error"} 1`)
	assert.Contains(t, out, "mcp_toolsearch_catalog_build_duration_seconds_count 3")
	assert.Contains(t, out, `mcp_toolsearch_catalog_build_duration_seconds_bucket{le="10"} 2`)
	assert.Contains(t, out, "mcp_toolsearch_catalog_tools 41", "the store's largest, not the last build's")
	assert.Contains(t, out, "mcp_toolsearch_catalogs 2")
	assert.Contains(t, out, `mcp_toolsearch_searches_total{mode="hybrid"} 1`)
	assert.Contains(t, out, `mcp_toolsearch_build_info{version="1.2.3"} 1`)
}

func TestUpstreamAndLimits(t *testing.T) {
	t.Parallel()
	m := metrics.New(nil)

	m.Upstream(upstream.OpList, upstream.OutcomeOK, time.Second)
	m.Upstream(upstream.OpCall, upstream.ClassTimeout, 2*time.Minute)
	m.Rejected()
	m.InFlight(1)
	m.InFlight(1)
	m.InFlight(-1)
	m.MissedInstructions()

	out := scrape(t, m)
	assert.Contains(t, out, `mcp_toolsearch_upstream_requests_total{op="list",outcome="ok"} 1`)
	assert.Contains(t, out, `mcp_toolsearch_upstream_requests_total{op="call",outcome="timeout"} 1`)
	assert.Contains(t, out, `mcp_toolsearch_upstream_request_duration_seconds_count{op="call"} 1`)
	assert.Contains(t, out, "mcp_toolsearch_requests_rejected_total 1")
	assert.Contains(t, out, "mcp_toolsearch_in_flight 1")
	assert.Contains(t, out, "mcp_toolsearch_handshakes_without_instructions_total 1")
	assert.Contains(t, out, "mcp_toolsearch_catalog_tools 0")
	assert.Contains(t, out, `mcp_toolsearch_upstream_requests_total{op="list",outcome="refused"} 0`,
		"every outcome starts at zero, so the first of a kind shows in rate")
	assert.Contains(t, out, `mcp_toolsearch_catalog_builds_total{outcome="error"} 0`)
}

// clock is a test clock that moves only when told to.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestServedCatalogs(t *testing.T) {
	t.Parallel()
	clk := &clock{now: time.Unix(1_000_000, 0)}
	m := metrics.New(&metrics.Options{TTL: 10 * time.Minute, Now: clk.Now})

	m.Serve(0)
	m.Serve(9 * time.Minute)
	assert.Contains(t, scrape(t, m), "mcp_toolsearch_catalog_stale_served_total 0")
	assert.Contains(t, scrape(t, m), "mcp_toolsearch_catalog_served_age_seconds 540")

	m.Serve(40 * time.Minute)
	m.Serve(10 * time.Minute)
	out := scrape(t, m)
	assert.Contains(t, out, "mcp_toolsearch_catalog_stale_served_total 2")
	assert.Contains(t, out, "mcp_toolsearch_catalog_served_age_seconds 2400")

	clk.advance(90 * time.Second)
	m.Serve(time.Second)
	assert.Contains(t, scrape(t, m), "mcp_toolsearch_catalog_served_age_seconds 2400", "a minute or two back")
	clk.advance(time.Minute)
	assert.Contains(t, scrape(t, m), "mcp_toolsearch_catalog_served_age_seconds 1", "older serves are forgotten")
	clk.advance(3 * time.Minute)
	assert.Contains(t, scrape(t, m), "mcp_toolsearch_catalog_served_age_seconds 0", "nothing served lately")
}

type caller struct{}

func (caller) CallTool(context.Context, string, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{}, nil
}

func TestMiddlewareCountsToolCalls(t *testing.T) {
	t.Parallel()
	m := metrics.New(nil)
	list := func(context.Context, string) ([]*mcp.Tool, error) { return []*mcp.Tool{{Name: "echo"}}, nil }
	opts := &server.Options{Middleware: []mcp.Middleware{m.Middleware()}, OnSearch: m.Search}
	srv := server.New(opts, catalog.NewStore(catalog.Tools(list), &catalog.StoreOptions{OnBuild: m.Build}), caller{})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	for _, p := range []*mcp.CallToolParams{
		{Name: server.ToolSearch, Arguments: map[string]any{"query": "echo"}},
		{Name: server.ToolCallWrite, Arguments: map[string]any{"name": "nope"}},
		{Name: "made_up"},
	} {
		_, _ = cs.CallTool(t.Context(), p)
	}

	out := scrape(t, m)
	assert.Contains(t, out, `mcp_toolsearch_tool_calls_total{outcome="ok",tool="search_tools"} 1`)
	assert.Contains(t, out, `mcp_toolsearch_tool_calls_total{outcome="tool_error",tool="call_write_tool"} 1`)
	assert.Contains(t, out, `tool="unknown"`)
	assert.NotContains(t, out, "made_up")
	assert.Contains(t, out, `mcp_toolsearch_searches_total{mode="lexical"} 1`)
}

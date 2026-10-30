package server_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

// search_tools returns the input schemas of its best matches, so the
// guidance must not send every call through describe_tool first.
func TestGuidanceTakesSchemasFromSearch(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	instructions := cs.InitializeResult().Instructions
	res, err := cs.ListTools(t.Context(), nil)

	require.NoError(t, err)
	assert.NotContains(t, instructions, "call describe_tool with the names you picked")
	assert.Contains(t, instructions, "describe_tool only for a tool returned without one")
	for _, tl := range res.Tools {
		if tl.Name == server.ToolCallRead || tl.Name == server.ToolCallWrite {
			assert.Contains(t, tl.Description, "the input schema from search_tools or describe_tool", tl.Name)
		}
	}
}

func TestDescribeSuggestsSimilarTools(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	out := structured[server.DescribeOut](t, call(t, cs, server.ToolDescribe, map[string]any{
		"names": []string{"weather_forecast", "weather_get_alerts"},
	}))

	assert.Equal(t, []string{"weather_forecast"}, out.Unknown)
	assert.Contains(t, out.Similar["weather_forecast"], "weather_get_forecast")
	assert.NotContains(t, out.Similar, "weather_get_alerts", "only unknown names get suggestions")
}

func TestAnEmptyCatalogSaysTheUpstreamListsNoTools(t *testing.T) {
	t.Parallel()
	none := func(context.Context, string) ([]*mcp.Tool, error) { return nil, nil }
	store := catalog.NewStore(catalog.Tools(none), nil)
	ts := httptest.NewServer(server.Handler(server.New(nil, store, newGate()), nil))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")

	search := call(t, cs, server.ToolSearch, map[string]any{"query": "weather"})
	describe := call(t, cs, server.ToolDescribe, map[string]any{"names": []string{"weather"}})
	callRead := call(t, cs, server.ToolCallRead, map[string]any{"name": "weather"})

	for _, res := range []*mcp.CallToolResult{search, describe, callRead} {
		assert.True(t, res.IsError)
		assert.Contains(t, text(res), "lists no tools for this account")
	}
}

func TestWriteToolsAreNeverCalledAsReads(t *testing.T) {
	t.Parallel()
	url := proxyWith(t, &server.Options{WriteTools: []string{"weather_get_f*"}}, false)
	cs := connect(t, url)

	found := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{"query": "weather"}))
	described := structured[server.DescribeOut](t, call(t, cs, server.ToolDescribe, map[string]any{
		"names": []string{"weather_get_forecast"},
	}))
	read := call(t, cs, server.ToolCallRead, map[string]any{
		"name": "weather_get_forecast", "arguments": map[string]any{"place": "Vilnius"},
	})
	write := call(t, cs, server.ToolCallWrite, map[string]any{
		"name": "weather_get_forecast", "arguments": map[string]any{"place": "Vilnius"},
	})
	alerts := call(t, cs, server.ToolCallRead, map[string]any{"name": "weather_get_alerts"})

	readOnly := map[string]bool{}
	for _, tl := range found.Tools {
		readOnly[tl.Name] = tl.ReadOnly
	}
	assert.False(t, readOnly["weather_get_forecast"], "listed as a write")
	assert.True(t, readOnly["weather_get_alerts"], "not listed, so still a read")
	require.Len(t, described.Tools, 1)
	assert.False(t, described.Tools[0].Annotations.ReadOnlyHint)
	assert.True(t, read.IsError)
	assert.Contains(t, text(read), server.ToolCallWrite)
	assert.False(t, write.IsError, text(write))
	assert.False(t, alerts.IsError, text(alerts))
}

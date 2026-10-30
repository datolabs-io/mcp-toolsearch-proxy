package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

// queries finds several things in one call: the results of each, in the
// order asked.
func TestSearchSeveralQueries(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{
		"queries": []string{"weather forecast for a place", "severe weather warnings"},
	}))

	require.Len(t, out.Results, 2)
	assert.Empty(t, out.Tools, "results come per query")
	assert.Equal(t, "weather forecast for a place", out.Results[0].Query)
	assert.Equal(t, "weather_get_forecast", out.Results[0].Tools[0].Name)
	assert.Equal(t, "severe weather warnings", out.Results[1].Query)
	assert.Equal(t, "weather_get_alerts", out.Results[1].Tools[0].Name)
	assert.Positive(t, out.Total)
}

// A tool found by more than one query brings its schema once, with the
// first.
func TestSearchQueriesShowEachSchemaOnce(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{
		"queries": []string{"weather forecast", "weather forecast for tomorrow"},
	}))

	schemas := map[string]int{}
	for _, r := range out.Results {
		for _, tl := range r.Tools {
			if tl.InputSchema != nil {
				schemas[tl.Name]++
			}
		}
	}
	assert.Equal(t, 1, schemas["weather_get_forecast"])
	require.NotNil(t, out.Results[0].Tools[0].InputSchema, "the first time it is found")
}

// limit is per query.
func TestSearchQueriesLimitEach(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{
		"queries": []string{"weather", "warnings", "forecast"}, "limit": 1,
	}))

	require.Len(t, out.Results, 3)
	for _, r := range out.Results {
		assert.Len(t, r.Tools, 1, r.Query)
	}
}

func TestSearchQueriesRefused(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	for name, args := range map[string]map[string]any{
		"both":      {"query": "weather", "queries": []string{"warnings"}},
		"none":      {"queries": []string{}},
		"blank":     {"queries": []string{"weather", "  "}},
		"too many":  {"queries": []string{"a", "b", "c", "d", "e", "f"}},
		"too long":  {"queries": []string{"weather", strings.Repeat("x", 1001)}},
		"no fields": {},
	} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: server.ToolSearch, Arguments: args})
		if err == nil {
			assert.True(t, res.IsError, name)
		}
	}
}

// The tool says what queries takes: up to five, instead of query.
func TestSearchInputSchemaOffersQueries(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	list, err := cs.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var schema map[string]any
	for _, tl := range list.Tools {
		if tl.Name == server.ToolSearch {
			raw, merr := json.Marshal(tl.InputSchema)
			require.NoError(t, merr)
			require.NoError(t, json.Unmarshal(raw, &schema))
		}
	}
	require.NotNil(t, schema)
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	queries, ok := props["queries"].(map[string]any)
	require.True(t, ok, "queries is offered")
	assert.InDelta(t, 5, queries["maxItems"], 0)
	required, _ := schema["required"].([]any)
	assert.NotContains(t, required, "query", "query or queries")
}

// A single query keeps its result as it was: tools is there even when
// nothing matches, and schemas may be null.
func TestSearchQueryResultUnchanged(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxyWith(t, &server.Options{SearchSchemas: 1}, false))

	none := call(t, cs, server.ToolSearch, map[string]any{"query": "xyzzy"})
	nullSchemas := call(t, cs, server.ToolSearch, map[string]any{"query": "weather", "schemas": nil})

	require.False(t, none.IsError, text(none))
	raw, err := json.Marshal(none.StructuredContent)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	assert.JSONEq(t, `[]`, string(fields["tools"]), "an empty list, not left out")
	assert.NotContains(t, fields, "results")
	assert.False(t, nullSchemas.IsError, text(nullSchemas))
}

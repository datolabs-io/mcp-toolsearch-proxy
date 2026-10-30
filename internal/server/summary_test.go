package server_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

// search_tools shows the first paragraph of a description, cut at 200
// characters, at the last space past the hundredth. Characters, not bytes:
// a Lithuanian or Chinese description is cut no shorter than an English one.
func TestSearchShortensDescriptionsByCharacters(t *testing.T) {
	t.Parallel()
	prefix := func(s string, n int) string { return string([]rune(s)[:n]) }
	english := strings.Repeat("word ", 60)
	lithuanian := strings.Repeat("ąž", 30) + " " + strings.Repeat("ėš", 150)
	chinese := strings.Repeat("天气", 20) + " " + strings.Repeat("预报", 150)
	short := "Forecast for a place.\n\nThe second paragraph is left out."
	for _, tc := range []struct{ name, description, want string }{
		{"weather_en", english, strings.TrimSpace(strings.Repeat("word ", 40)) + "…"},
		{"weather_lt", lithuanian, prefix(lithuanian, 200) + "…"},
		{"weather_zh", chinese, prefix(chinese, 200) + "…"},
		{"weather_short", short, "Forecast for a place."},
		{"weather_spaces", "  Forecast\tfor   a\nplace.  ", "Forecast for a place."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			list := func(context.Context, string) ([]*mcp.Tool, error) {
				return []*mcp.Tool{{Name: tc.name, Description: tc.description}}, nil
			}
			ts := httptest.NewServer(server.Handler(
				server.New(nil, catalog.NewStore(catalog.Tools(list), nil), newGate()), nil))
			t.Cleanup(ts.Close)
			cs := connect(t, ts.URL+"/mcp")

			out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{"query": "weather"}))

			require.Len(t, out.Tools, 1)
			assert.Equal(t, tc.want, out.Tools[0].Description)
		})
	}
}

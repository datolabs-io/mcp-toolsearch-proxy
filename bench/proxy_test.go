package bench

import (
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real proxy, built from this repository, serves its tools in front of
// the upstream and passes each caller's token on.
func TestProxyInFrontOfUpstream(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "mcp-toolsearch-proxy")
	require.NoError(t, buildProxy(t.Context(), bin))
	up, err := startUpstream(t.Context(), testTools())
	require.NoError(t, err)
	t.Cleanup(up.Close)
	p, err := startProxy(t.Context(), bin, up.URL, "-embedder", "none")
	require.NoError(t, err)
	t.Cleanup(p.Close)

	cs, err := connect(t.Context(), p.URL, "carol")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	list, err := cs.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(list.Tools))
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	assert.Contains(t, names, "search_tools")
	assert.Contains(t, names, "call_write_tool")
	assert.NotEmpty(t, cs.InitializeResult().Instructions)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "call_read_tool",
		Arguments: map[string]any{"name": "weather_get_forecast", "arguments": map[string]any{"location": "Kaunas"}},
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, []string{"weather_get_forecast"}, up.Calls("carol"))
}

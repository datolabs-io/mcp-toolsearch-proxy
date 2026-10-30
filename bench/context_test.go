package bench

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// byteCounter counts a token per four bytes of what it is given, and a
// deferred tool as nothing, as the real count does.
type byteCounter struct{}

func (byteCounter) Count(
	_ context.Context,
	system, user string,
	tools []anthropic.ToolParam,
	toolSearch bool,
) (int64, error) {
	n := len(system) + len(user)
	if toolSearch {
		n += 100
	} else {
		raw, _ := json.Marshal(tools)
		n += len(raw)
	}
	return int64(n / 4), nil
}

// An MCP tool becomes the Anthropic tool a client would send: the same
// name, description and schema.
func TestToolParam(t *testing.T) {
	t.Parallel()
	tp, err := toolParam(&mcp.Tool{
		Name:        "fleet_get_vehicle",
		Description: "Get one vehicle.",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`,
		),
	})
	require.NoError(t, err)

	raw, err := json.Marshal(tp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"fleet_get_vehicle","description":"Get one vehicle.","input_schema":{"type":"object",`+
		`"properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}}`, string(raw))
}

// The direct listing grows with the catalog; the proxy's does not.
func TestMeasureContext(t *testing.T) {
	t.Parallel()
	core, tasks, err := loadEval()
	require.NoError(t, err)
	domains, err := loadDomains()
	require.NoError(t, err)
	bin := filepath.Join(t.TempDir(), "mcp-toolsearch-proxy")
	require.NoError(t, buildProxy(t.Context(), bin))

	rows, err := measureContext(t.Context(), contextSetup{
		bin: bin, flags: []string{"-embedder", "none"}, core: core, domains: domains, tasks: tasks[:3],
	}, []int{50, 500}, byteCounter{})
	require.NoError(t, err)

	require.Len(t, rows, 2)
	small, large := rows[0], rows[1]
	assert.Equal(t, 50, small.Tools)
	assert.Greater(t, large.Direct.Bytes, 5*small.Direct.Bytes)
	assert.InDelta(t, small.Proxy.Bytes, large.Proxy.Bytes, float64(small.Proxy.Bytes)/10)
	assert.Less(t, large.Proxy.Tokens, large.Direct.Tokens/5)
	assert.Positive(t, large.Search.Bytes)
	assert.Positive(t, large.ToolSearch.Tokens)
}

// A listing holds every page of tools, as a client loads them.
func TestListReadsEveryPage(t *testing.T) {
	t.Parallel()
	core, _, err := loadEval()
	require.NoError(t, err)
	domains, err := loadDomains()
	require.NoError(t, err)
	tools, err := catalog(core, domains, 1490)
	require.NoError(t, err)
	up, err := startUpstream(t.Context(), tools)
	require.NoError(t, err)
	t.Cleanup(up.Close)
	cs, err := connect(t.Context(), up.URL, "pages")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	l, err := list(t.Context(), cs)
	require.NoError(t, err)
	require.Len(t, l.tools, 1490)
}

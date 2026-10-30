package bench

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testTools() []Tool {
	return []Tool{
		{Name: "weather_get_forecast", Description: "Forecast.", ReadOnly: true,
			Schema: schema(map[string]any{"location": map[string]any{"type": "string"}}, []string{"location"})},
		{Name: "mail_send_message", Description: "Send mail.", Schema: schema(map[string]any{}, nil)},
	}
}

// The upstream serves the catalog as it is, and records each caller's
// calls apart, by their token.
func TestUpstreamServesAndRecords(t *testing.T) {
	t.Parallel()
	up, err := startUpstream(t.Context(), testTools())
	require.NoError(t, err)
	t.Cleanup(up.Close)

	alice, err := connect(t.Context(), up.URL, "alice")
	require.NoError(t, err)
	t.Cleanup(func() { _ = alice.Close() })
	bob, err := connect(t.Context(), up.URL, "bob")
	require.NoError(t, err)
	t.Cleanup(func() { _ = bob.Close() })

	list, err := alice.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, list.Tools, 2)
	assert.Equal(t, "mail_send_message", list.Tools[0].Name)
	assert.False(t, list.Tools[0].Annotations.ReadOnlyHint)
	assert.True(t, list.Tools[1].Annotations.ReadOnlyHint)

	res, err := alice.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "weather_get_forecast", Arguments: map[string]any{"location": "Vilnius"},
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	_, err = bob.CallTool(t.Context(), &mcp.CallToolParams{Name: "mail_send_message", Arguments: map[string]any{}})
	require.NoError(t, err)

	assert.Equal(t, []string{"weather_get_forecast"}, up.Calls("alice"))
	assert.Equal(t, []string{"mail_send_message"}, up.Calls("bob"))
	up.Reset("alice")
	assert.Empty(t, up.Calls("alice"))
	assert.Equal(t, []string{"mail_send_message"}, up.Calls("bob"))
}

// A call whose arguments do not fit the tool's schema is refused, as the
// proxy refuses it, and not recorded: every mode succeeds on the same calls.
func TestUpstreamRefusesInvalidArguments(t *testing.T) {
	t.Parallel()
	up, err := startUpstream(t.Context(), testTools())
	require.NoError(t, err)
	t.Cleanup(up.Close)
	cs, err := connect(t.Context(), up.URL, "dave")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "weather_get_forecast", Arguments: map[string]any{}})

	if err == nil {
		assert.True(t, res.IsError, "location is required")
	}
	assert.Empty(t, up.Calls("dave"))
}

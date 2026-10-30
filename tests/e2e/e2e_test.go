//go:build e2e

// Package e2e tests the proxy deployed behind a real gateway, as in
// docker-compose.yaml: the endpoints come from the environment, so the same tests
// run against the local stack or any other deployment of that topology.
//
//	E2E_SEARCH_URL  the proxy, through the gateway (required)
//	E2E_ALL_URL     the gateway's aggregated endpoint the proxy searches (required)
//	E2E_TOKEN_ALICE a token the gateway lets use every tool; default testdata/alice.jwt
//	E2E_TOKEN_BOB   a token the gateway lets use the weather tools only; default testdata/bob.jwt
//	E2E_SEMANTIC    "0" when the proxy runs without the built-in model
//	E2E_CLOSED_URL  a second proxy, through the gateway, in front of E2E_CLOSED_ALL_URL; tests of a
//	                server that stops listing are skipped without it
//	E2E_CLOSED_ALL_URL the gateway's failClosed route the second proxy searches, where one server answers
//	                its first tools/list and refuses every later one
//	E2E_CLOSED_METRICS_URL the second proxy's /metrics
//
// In the local stack the proxy reaches the gateway through a front that
// serves only the gateway's public name, so every test fails unless the
// proxy sends TOOLSEARCH_UPSTREAM_HOST.
package e2e_test

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set; see the package comment", name)
	}
	return v
}

// token is the token of user, from E2E_TOKEN_<USER> or testdata/<user>.jwt.
func token(t *testing.T, user string) string {
	t.Helper()
	if v := os.Getenv("E2E_TOKEN_" + strings.ToUpper(user)); v != "" {
		return v
	}
	raw, err := os.ReadFile("testdata/" + user + ".jwt") //nolint:gosec // a fixed test user
	require.NoError(t, err)
	return strings.TrimSpace(string(raw))
}

type bearer struct{ value string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.value)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, tok string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearer{tok}}, DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func names(tools []*mcp.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	slices.Sort(out)
	return out
}

// listed is every tool the gateway lists to tok on the aggregated endpoint.
func listed(t *testing.T, tok string) []string {
	t.Helper()
	res, err := connect(t, env(t, "E2E_ALL_URL"), tok).ListTools(t.Context(), nil)
	require.NoError(t, err)
	return names(res.Tools)
}

type found struct {
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Total int `json:"total"`
}

func search(t *testing.T, cs *mcp.ClientSession, query string, limit int) found {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "search_tools", Arguments: map[string]any{"query": query, "limit": limit},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "search_tools failed: %v", res.Content)
	var out found
	decode(t, res.StructuredContent, &out)
	return out
}

func decode(t *testing.T, v, into any) {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, into))
}

func (f found) names() []string {
	out := make([]string, 0, len(f.Tools))
	for _, tl := range f.Tools {
		out = append(out, tl.Name)
	}
	return out
}

func TestTheGatewayRefusesCallersWithoutAToken(t *testing.T) {
	t.Parallel()
	for _, url := range []string{env(t, "E2E_SEARCH_URL"), env(t, "E2E_ALL_URL")} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, url)
	}
}

func TestTheProxyOffersFiveTools(t *testing.T) {
	t.Parallel()

	res, err := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice")).ListTools(t.Context(), nil)

	require.NoError(t, err)
	assert.Equal(
		t,
		[]string{"call_read_tool", "call_read_tools", "call_write_tool", "describe_tool", "search_tools"},
		names(res.Tools),
	)
}

// Each caller's catalog is built from that caller's own list, as the
// gateway's authorization rules filter it.
func TestEachCallerSearchesTheToolsTheGatewayListsForThem(t *testing.T) {
	t.Parallel()
	for _, user := range []string{"alice", "bob"} {
		tok := token(t, user)
		want := listed(t, tok)
		cs := connect(t, env(t, "E2E_SEARCH_URL"), tok)

		got := search(t, cs, "tool", 25)

		assert.Equal(t, len(want), got.Total, user)
		for _, name := range got.names() {
			assert.Contains(t, want, name, "%s found a tool the gateway does not list for them", user)
		}
	}
}

func TestSearchFindsTheTool(t *testing.T) {
	t.Parallel()
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice"))

	assert.Contains(t, search(t, cs, "remove a user account", 3).names(), "admin_delete_user")
	assert.Contains(t, search(t, cs, "weather forecast", 3).names(), "weather_get_forecast")
}

func TestSearchUnderstandsLithuanian(t *testing.T) {
	t.Parallel()
	if os.Getenv("E2E_SEMANTIC") == "0" {
		t.Skip("the proxy runs without the built-in model")
	}
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice"))

	assert.Contains(t, search(t, cs, "ar rytoj lis", 3).names(), "weather_get_forecast")
	assert.Contains(t, search(t, cs, "ištrink naudotoją", 3).names(), "admin_delete_user")
}

func TestDescribeReturnsTheUpstreamSchema(t *testing.T) {
	t.Parallel()
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice"))

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "describe_tool", Arguments: map[string]any{"names": []string{"admin_echo"}},
	})

	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	var out struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema any    `json:"inputSchema"`
		} `json:"tools"`
	}
	decode(t, res.StructuredContent, &out)
	require.Len(t, out.Tools, 1)
	assert.NotNil(t, out.Tools[0].InputSchema)
}

func TestCallReachesTheUpstreamTool(t *testing.T) {
	t.Parallel()
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice"))

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "call_read_tool",
		Arguments: map[string]any{"name": "admin_echo", "arguments": map[string]any{"word": "labas"}},
	})

	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	var out struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	decode(t, res.StructuredContent, &out)
	assert.Equal(t, "echo", out.Tool)
	assert.Equal(t, map[string]any{"word": "labas"}, out.Arguments)
}

// Several reads go in one call through the gateway, each with its own
// result, and a write among them is refused alone.
func TestCallReadsReachesEachUpstreamTool(t *testing.T) {
	t.Parallel()
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice"))

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "call_read_tools",
		Arguments: map[string]any{"calls": []any{
			map[string]any{"name": "admin_echo", "arguments": map[string]any{"word": "labas"}},
			map[string]any{"name": "admin_echo", "arguments": map[string]any{"word": "hello"}},
			map[string]any{"name": "admin_delete_user"},
		}},
	})

	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	var out struct {
		Results []*struct {
			Arguments map[string]any `json:"arguments"`
		} `json:"results"`
	}
	decode(t, res.StructuredContent, &out)
	require.Len(t, out.Results, 3)
	assert.Equal(t, map[string]any{"word": "labas"}, out.Results[0].Arguments)
	assert.Equal(t, map[string]any{"word": "hello"}, out.Results[1].Arguments)
	assert.Nil(t, out.Results[2], "the refused call has no result")
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text + "\n")
		}
	}
	assert.Contains(t, text.String(), "[3/3] admin_delete_user: failed")
}

// The gateway gives a backend 10 s to send its response's headers, unless
// the route says otherwise, and the route to the proxy does not. A call
// that takes longer passes because the proxy sends the headers at once.
func TestCallsLongerThanTheGatewayWaitsForHeadersPass(t *testing.T) {
	t.Parallel()
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice"))
	start := time.Now()

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "call_read_tool",
		Arguments: map[string]any{"name": "admin_wait", "arguments": map[string]any{"seconds": 12}},
	})

	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	assert.GreaterOrEqual(t, time.Since(start), 12*time.Second)
}

// A caller cannot reach through the proxy a tool the gateway withholds from
// them, whether the proxy's catalog or the gateway refuses it.
func TestCallRefusesToolsTheGatewayWithholds(t *testing.T) {
	t.Parallel()
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "bob"))

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "call_write_tool", Arguments: map[string]any{"name": "admin_delete_user"},
	})

	require.NoError(t, err)
	assert.True(t, res.IsError)
}

// Reading is no way to write: call_read_tool refuses a tool the upstream
// does not mark read-only, even for a caller allowed to call it.
func TestCallReadRefusesWrites(t *testing.T) {
	t.Parallel()
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice"))
	before := adminCalls(t, cs)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "call_read_tool", Arguments: map[string]any{"name": "admin_delete_user"},
	})

	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Equal(t, before["delete_user"], adminCalls(t, cs)["delete_user"], "the tool never ran")
}

// adminCalls is how many calls each tool of the admin server received, as
// its count_calls tool answers through the proxy. No test reaches
// delete_user, so its count holds still while tests run in parallel.
func adminCalls(t *testing.T, cs *mcp.ClientSession) map[string]int {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "call_read_tool", Arguments: map[string]any{"name": "admin_count_calls"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	var out struct {
		Calls map[string]int `json:"calls"`
	}
	decode(t, res.StructuredContent, &out)
	return out.Calls
}

// The proxy forwards the gateway's instructions in its handshake, after its
// own; agentgateway's begin with a description of itself.
func TestHandshakeCarriesTheGatewaysInstructions(t *testing.T) {
	t.Parallel()

	got := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice")).InitializeResult().Instructions

	assert.Contains(t, got, "search_tools")
	assert.Contains(t, got, "gateway")
}

func TestSearchReturnsInputSchemasOfTheBestMatches(t *testing.T) {
	t.Parallel()
	cs := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice"))

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "search_tools", Arguments: map[string]any{"query": "weather forecast", "limit": 5},
	})

	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	var out struct {
		Tools []struct {
			InputSchema any `json:"inputSchema"`
		} `json:"tools"`
	}
	decode(t, res.StructuredContent, &out)
	require.NotEmpty(t, out.Tools)
	assert.NotNil(t, out.Tools[0].InputSchema)
}

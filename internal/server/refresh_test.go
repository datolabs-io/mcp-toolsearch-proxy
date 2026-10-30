package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

// counting is a lister that counts its lists, and fails them with err when
// it is set.
type counting struct {
	lists atomic.Int32
	err   error
}

func (c *counting) list(context.Context, string) (catalog.Listing, error) {
	c.lists.Add(1)
	if c.err != nil {
		return catalog.Listing{}, c.err
	}
	return catalog.Listing{Tools: []*mcp.Tool{{Name: "echo"}}, Instructions: "upstream"}, nil
}

// A model guessing tool names must not list the whole upstream each time.
func TestUnknownNamesDoNotListAgainAndAgain(t *testing.T) {
	t.Parallel()
	l := &counting{}
	opts := &server.Options{MaxInstructions: 1000}
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(l.list, nil), newGate()))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")

	for i := range 10 {
		res, err := cs.CallTool(t.Context(), callParams(fmt.Sprintf("guess_%d", i), nil))
		require.NoError(t, err)
		require.True(t, res.IsError)
		assert.Contains(t, text(res), "no tool", "an unknown name is told so")
	}

	assert.LessOrEqual(t, l.lists.Load(), int32(2))
}

// With the upstream down, a session lists it once, not once per request.
func TestAFailingUpstreamIsListedOncePerSession(t *testing.T) {
	t.Parallel()
	l := &counting{err: errors.New("down")}
	opts := &server.Options{MaxInstructions: 1000}
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(l.list, nil), newGate()))
	t.Cleanup(ts.Close)

	cs := connect(t, ts.URL+"/mcp")
	_, err := cs.ListTools(t.Context(), nil)
	require.NoError(t, err)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: server.ToolSearch, Arguments: map[string]any{"query": "weather"},
	})
	require.NoError(t, err)

	assert.True(t, res.IsError)
	assert.Equal(t, int32(1), l.lists.Load())
}

// Only the handshake looks up the upstream's instructions: other requests
// do not list for them, whether or not they name their method in a header.
func TestOnlyTheHandshakeListsForInstructions(t *testing.T) {
	t.Parallel()
	l := &counting{err: errors.New("down")}
	opts := &server.Options{MaxInstructions: 1000}
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(l.list, nil), newGate()))
	t.Cleanup(ts.Close)

	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"params":{"x":"` + strings.Repeat("a", 100<<10) + `"},"method":"initialize"}`,
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	assert.Zero(t, l.lists.Load())
}

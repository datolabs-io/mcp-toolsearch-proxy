package server_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

// tokens records the Authorization value of every request the proxy makes
// upstream, listings and calls alike.
type tokens struct {
	g  *gate
	mu sync.Mutex
	// seen holds each value received, in order.
	seen []string
}

func (r *tokens) CallTool(
	ctx context.Context, authorization, name string, args json.RawMessage,
) (*mcp.CallToolResult, error) {
	r.record(authorization)
	return r.g.CallTool(ctx, authorization, name, args)
}

func (r *tokens) record(authorization string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, authorization)
}

func (r *tokens) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

func (r *tokens) list(ctx context.Context, authorization string) (catalog.Listing, error) {
	r.record(authorization)
	tools, err := r.g.list(ctx, authorization)
	return catalog.Listing{Tools: tools, Instructions: "Upstream says hello."}, err
}

// Serve runs the proxy on a transport without HTTP, such as stdio: the
// handshake carries the upstream's instructions, tools are called, and no
// request upstream carries a credential, since there is no caller's to pass
// on and the proxy holds none.
func TestServeOverATransportSendsNoCredential(t *testing.T) {
	t.Parallel()
	up := &tokens{g: newGate()}
	store := catalog.NewStore(up.list, nil)
	serverT, clientT := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, &server.Options{MaxInstructions: 1000}, store, up, serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), clientT, nil)
	require.NoError(t, err)
	assert.Contains(t, cs.InitializeResult().Instructions, "Upstream says hello.")

	res, err := cs.CallTool(t.Context(), callParams("echo", map[string]any{"text": "hi"}))
	require.NoError(t, err)
	require.False(t, res.IsError, "call failed: %v", res.Content)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, "hi", text.Text)

	seen := up.values()
	require.NotEmpty(t, seen)
	for _, v := range seen {
		assert.Empty(t, v, "a request upstream carried a credential")
	}

	require.NoError(t, cs.Close())
	cancel()
	require.NoError(t, <-done)
}

// Without instructions to forward, the handshake carries the proxy's own.
func TestServeWithoutUpstreamInstructions(t *testing.T) {
	t.Parallel()
	g := newGate()
	store := catalog.NewStore(catalog.Tools(g.list), nil)
	serverT, clientT := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, nil, store, g, serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), clientT, nil)
	require.NoError(t, err)
	assert.Contains(t, cs.InitializeResult().Instructions, "search_tools")
	assert.NotContains(t, cs.InitializeResult().Instructions, "upstream server's own instructions")
	require.NoError(t, cs.Close())
	cancel()
	require.NoError(t, <-done)
}

// A client that goes away, as when stdin closes, ends Serve without an
// error.
func TestServeEndsWhenTheClientGoesAway(t *testing.T) {
	t.Parallel()
	g := newGate()
	store := catalog.NewStore(catalog.Tools(g.list), nil)
	serverT, clientT := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- server.Serve(t.Context(), nil, store, g, serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), clientT, nil)
	require.NoError(t, err)
	require.NoError(t, cs.Close())
	require.NoError(t, <-done)
}

// serveInMemory runs Serve with opts over g until the test ends, and
// returns a connected client and the channel Serve's result arrives on.
func serveInMemory(
	ctx context.Context, t *testing.T, opts *server.Options, g *gate,
) (*mcp.ClientSession, <-chan error) {
	t.Helper()
	store := catalog.NewStore(catalog.Tools(g.list), nil)
	serverT, clientT := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, opts, store, g, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), clientT, nil)
	require.NoError(t, err)
	return cs, done
}

// Over a transport without HTTP, as over HTTP, a request beyond
// MaxInFlight is refused rather than served, and counted.
func TestServeLimitsRequestsInFlight(t *testing.T) {
	t.Parallel()
	g := newGate()
	var rejected atomic.Int32
	cs, _ := serveInMemory(t.Context(), t, &server.Options{
		MaxInFlight: 1, OnRejected: func() { rejected.Add(1) },
	}, g)
	held := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(t.Context(), callParams("hold", nil))
		held <- err
	}()
	<-g.entered

	_, err := cs.CallTool(t.Context(), callParams("echo", map[string]any{"text": "hi"}))
	require.ErrorContains(t, err, "busy")
	assert.Equal(t, int32(1), rejected.Load())

	close(g.open)
	require.NoError(t, <-held)
	res, err := cs.CallTool(t.Context(), callParams("echo", map[string]any{"text": "hi"}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

// When ctx is done, a call still running gets ShutdownGrace to finish, and
// Serve returns after it whether or not the call has.
func TestServeStopsWithinTheGrace(t *testing.T) {
	t.Parallel()
	g := newGate()
	t.Cleanup(func() { close(g.open) })
	ctx, cancel := context.WithCancel(t.Context())
	cs, done := serveInMemory(ctx, t, &server.Options{ShutdownGrace: 50 * time.Millisecond}, g)
	go func() { _, _ = cs.CallTool(t.Context(), callParams("hold", nil)) }()
	<-g.entered

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop within the grace")
	}
}

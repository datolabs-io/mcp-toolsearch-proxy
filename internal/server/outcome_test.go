package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// outcomes records the outcome of every tool call, as a metrics
// middleware sees it.
type outcomes struct {
	mu   sync.Mutex
	seen []string
}

func (o *outcomes) middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method == "tools/call" {
				o.mu.Lock()
				o.seen = append(o.seen, server.Outcome(ctx, res, err))
				o.mu.Unlock()
			}
			return res, err
		}
	}
}

func (o *outcomes) last() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.seen) == 0 {
		return ""
	}
	return o.seen[len(o.seen)-1]
}

// outcomeFixture starts the proxy in front of r, or of an upstream that is
// down when r is nil, recording outcomes.
func outcomeFixture(t *testing.T, r *recording) (*mcp.ClientSession, *outcomes) {
	t.Helper()
	var up *httptest.Server
	if r != nil {
		r.release = make(chan struct{})
		up = httptest.NewServer(r.handler())
		t.Cleanup(up.Close)
		t.Cleanup(func() { close(r.release) })
	} else {
		up = httptest.NewServer(http.NotFoundHandler())
		up.Close()
	}
	client := upstream.New(up.URL, &upstream.Options{ListRetries: -1, CallTimeout: 200 * time.Millisecond})
	rec := &outcomes{}
	opts := &server.Options{Middleware: []mcp.Middleware{rec.middleware()}}
	ts := httptest.NewServer(server.Handler(server.New(opts, catalog.NewStore(listing(client), nil), client), opts))
	t.Cleanup(ts.Close)
	return connect(t, ts.URL+"/mcp"), rec
}

func TestFailuresOfTheUpstreamAreNotToolErrors(t *testing.T) {
	t.Parallel()
	cs, rec := outcomeFixture(t, nil)

	for _, p := range []struct{ tool, raw string }{
		{server.ToolSearch, `{"query":"weather"}`},
		{server.ToolDescribe, `{"names":["x"]}`},
		{server.ToolCallWrite, `{"name":"x"}`},
	} {
		res := rawCall(t, cs, p.tool, p.raw)
		require.True(t, res.IsError)
		assert.Equal(t, server.OutcomeUpstreamError, rec.last(), "%s without a catalog", p.tool)
	}
}

func TestCallOutcomes(t *testing.T) {
	t.Parallel()
	r := &recording{listed: []*mcp.Tool{tool("broken", nil), tool("hang", nil), tool("fine", nil)}}
	cs, rec := outcomeFixture(t, r)

	for _, tc := range []struct{ raw, read, want string }{
		{raw: `{"name":"fine"}`, want: server.OutcomeOK},
		{raw: `{"name":"broken"}`, want: server.OutcomeUpstreamError},
		{raw: `{"name":"hang"}`, want: server.OutcomeUpstreamError},
		{raw: `{"name":"fine"}`, read: "read", want: server.OutcomeToolError},
		{raw: `{"name":"fine","extra":1}`, want: server.OutcomeToolError},
	} {
		name := server.ToolCallWrite
		if tc.read != "" {
			name = server.ToolCallRead
		}
		rawCall(t, cs, name, tc.raw)
		assert.Equal(t, tc.want, rec.last(), "%s %s", name, tc.raw)
	}
}

func TestOutcomeWithoutTheServer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	assert.Equal(t, server.OutcomeOK, server.Outcome(ctx, &mcp.CallToolResult{}, nil))
	assert.Equal(t, server.OutcomeToolError, server.Outcome(ctx, &mcp.CallToolResult{IsError: true}, nil))
	assert.Equal(t, server.OutcomeError, server.Outcome(ctx, nil, context.Canceled))
}

func TestRefusedAndInFlightRequestsAreReported(t *testing.T) {
	t.Parallel()
	gate := newGate()
	var rejected atomic.Int32
	var mu sync.Mutex
	inFlight, peak := 0, 0
	opts := &server.Options{
		MaxInFlight: 1,
		OnRejected:  func() { rejected.Add(1) },
		OnInFlight: func(delta int) {
			mu.Lock()
			defer mu.Unlock()
			inFlight += delta
			peak = max(peak, inFlight)
		},
	}
	current := func() int {
		mu.Lock()
		defer mu.Unlock()
		return inFlight
	}
	ts := httptest.NewServer(server.Handler(newServer(t, opts, gate), opts))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")
	held := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(t.Context(), callParams("hold", nil))
		held <- err
	}()
	waitFor(t, gate.entered, "the call to hold its slot")
	release := sync.OnceFunc(func() { close(gate.open) })
	t.Cleanup(release)

	full, _ := postInit(t, ts.URL+"/mcp")
	assert.Equal(t, 1, current(), "the held call")
	release()
	require.NoError(t, <-held)

	assert.Equal(t, http.StatusServiceUnavailable, full)
	assert.Equal(t, int32(1), rejected.Load())
	assert.Eventually(t, func() bool { return current() == 0 }, time.Second, time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, peak, "a refused request is never in flight")
}

func TestHandshakesWithoutUpstreamInstructionsAreReported(t *testing.T) {
	t.Parallel()
	var missed atomic.Int32
	onMissed := func() { missed.Add(1) }

	connect(t, proxyWith(t, &server.Options{MaxInstructions: 1000, OnMissedInstructions: onMissed}, false))
	assert.Zero(t, missed.Load(), "the upstream's instructions arrived")
	connect(t, proxyWith(t, &server.Options{OnMissedInstructions: onMissed}, true))
	assert.Zero(t, missed.Load(), "none are forwarded, so none are missed")
	connect(t, proxyWith(t, &server.Options{MaxInstructions: 1000, OnMissedInstructions: onMissed}, true))
	assert.Equal(t, int32(1), missed.Load(), "the upstream is down")
}

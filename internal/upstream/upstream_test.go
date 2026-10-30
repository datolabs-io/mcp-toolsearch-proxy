package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// recorder keeps the Authorization header of every request it serves.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.Header.Get("Authorization"))
		r.mu.Unlock()
		next.ServeHTTP(w, req)
	})
}

func (r *recorder) headers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

type echoIn struct {
	Text string `json:"text"`
}

// fake serves n tools, where tool "echo" answers with its text and with
// the caller's Authorization, and tool "fail" fails.
func fake(t *testing.T, n int, stateless bool) (*httptest.Server, *recorder) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, &mcp.ServerOptions{PageSize: 3})
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "Echo text."},
		func(_ context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: in.Text}, &mcp.TextContent{Text: req.Extra.Header.Get("Authorization")},
			}}, nil, nil
		})
	mcp.AddTool(
		srv,
		&mcp.Tool{Name: "fail"},
		func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("broken")
		},
	)
	for i := range n {
		mcp.AddTool(
			srv,
			&mcp.Tool{Name: fmt.Sprintf("tool_%02d", i)},
			func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{}, nil, nil
			},
		)
	}
	rec := &recorder{}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: stateless, JSONResponse: true})
	ts := httptest.NewServer(rec.wrap(h))
	t.Cleanup(ts.Close)
	return ts, rec
}

func TestListToolsFollowsPages(t *testing.T) {
	t.Parallel()
	ts, _ := fake(t, 10, true)

	tools, err := upstream.New(ts.URL, nil).ListTools(t.Context(), "Bearer a")

	require.NoError(t, err)
	assert.Len(t, tools, 12)
}

func TestEveryRequestCarriesTheCallersAuthorization(t *testing.T) {
	t.Parallel()
	for _, stateless := range []bool{true, false} {
		ts, rec := fake(t, 0, stateless)
		c := upstream.New(ts.URL, nil)

		_, err := c.ListTools(t.Context(), "Bearer alice")
		require.NoError(t, err)
		res, err := c.CallTool(t.Context(), "Bearer bob", "echo", json.RawMessage(`{"text":"hi"}`))
		require.NoError(t, err)

		require.Len(t, res.Content, 2)
		tc, ok := res.Content[1].(*mcp.TextContent)
		require.True(t, ok)
		assert.Equal(t, "Bearer bob", tc.Text)
		seen := rec.headers()
		require.NotEmpty(t, seen)
		for _, h := range seen {
			assert.Contains(t, []string{"Bearer alice", "Bearer bob"}, h, "stateless=%v", stateless)
		}
		assert.Equal(t, "Bearer alice", seen[0])
		assert.Equal(t, "Bearer bob", seen[len(seen)-1])
	}
}

func TestNoAuthorizationIsSentWithoutOne(t *testing.T) {
	t.Parallel()
	ts, rec := fake(t, 0, true)

	_, err := upstream.New(ts.URL, nil).ListTools(t.Context(), "")

	require.NoError(t, err)
	for _, h := range rec.headers() {
		assert.Empty(t, h)
	}
}

func TestCallToolReturnsToolErrorsAsResults(t *testing.T) {
	t.Parallel()
	ts, _ := fake(t, 0, true)

	res, err := upstream.New(ts.URL, nil).CallTool(t.Context(), "", "fail", nil)

	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestUnreachableUpstream(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close()
	c := upstream.New(ts.URL, nil)

	_, err := c.ListTools(t.Context(), "")
	require.ErrorIs(t, err, upstream.ErrUpstream)
	_, err = c.CallTool(t.Context(), "", "echo", nil)
	require.ErrorIs(t, err, upstream.ErrUpstream)
}

func TestUpstreamRefusingTheCaller(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(ts.Close)

	_, err := upstream.New(ts.URL, nil).ListTools(t.Context(), "Bearer expired")

	require.ErrorIs(t, err, upstream.ErrUpstream)
}

// hosts records the Host header of every request it serves.
type hosts struct {
	mu   sync.Mutex
	seen []string
}

func (h *hosts) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Host)
		h.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func TestHostOverride(t *testing.T) {
	t.Parallel()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	mcp.AddTool(
		srv,
		&mcp.Tool{Name: "echo"},
		func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		},
	)
	h := &hosts{}
	ts := httptest.NewServer(h.wrap(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	t.Cleanup(ts.Close)

	// The SDK's server accepts only loopback names on loopback, so the
	// override is a loopback name other than the URL's 127.0.0.1.
	want := "localhost:" + ts.URL[strings.LastIndex(ts.URL, ":")+1:]
	c := upstream.New(ts.URL, &upstream.Options{Host: want})
	_, err := c.ListTools(t.Context(), "Bearer a")
	require.NoError(t, err)
	_, err = c.CallTool(t.Context(), "Bearer a", "echo", nil)
	require.NoError(t, err)

	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotEmpty(t, h.seen)
	for _, got := range h.seen {
		assert.Equal(t, want, got)
	}
}

// methods records the Mcp-Method header of every request it serves and,
// with refuse, answers server/discover with 500, as some servers built
// before MCP 2026-07-28 do.
type methods struct {
	mu     sync.Mutex
	seen   []string
	refuse bool
}

func (m *methods) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.seen = append(m.seen, r.Header.Get("Mcp-Method"))
		m.mu.Unlock()
		if m.refuse && r.Header.Get("Mcp-Method") == "server/discover" {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// discoverServer returns a server with one tool behind wrap.
func discoverServer(t *testing.T, wrap func(http.Handler) http.Handler, stateless bool) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	mcp.AddTool(
		srv,
		&mcp.Tool{Name: "echo"},
		func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		},
	)
	ts := httptest.NewServer(wrap(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: stateless, JSONResponse: true})))
	t.Cleanup(ts.Close)
	return ts
}

func TestDefaultProtocolIsTheLatest(t *testing.T) {
	t.Parallel()
	m := &methods{}
	ts := discoverServer(t, m.wrap, true)

	tools, err := upstream.New(ts.URL, nil).ListTools(t.Context(), "")

	require.NoError(t, err)
	assert.Len(t, tools, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	assert.Contains(t, m.seen, "server/discover", "the newest protocol opens with server/discover")
}

func TestPinnedProtocolSkipsDiscover(t *testing.T) {
	t.Parallel()
	m := &methods{refuse: true}
	ts := discoverServer(t, m.wrap, true)

	tools, err := upstream.New(ts.URL, &upstream.Options{ProtocolVersion: "2025-11-25"}).ListTools(t.Context(), "")

	require.NoError(t, err, "the 2025-11-25 handshake works where server/discover fails")
	assert.Len(t, tools, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	assert.NotContains(t, m.seen, "server/discover")
}

func TestCallsEndWithTheCaller(t *testing.T) {
	t.Parallel()
	ts, _ := fake(t, 0, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := upstream.New(ts.URL, nil).ListTools(ctx, "")

	require.ErrorIs(t, err, upstream.ErrUpstream)
}

// failure is how flaky answers the first n requests of a method, or every
// one when n is negative.
type failure struct {
	status, n int
}

// flaky counts the requests of each method it serves, by MCP method, read
// from the body where the protocol version sends no Mcp-Method header, or,
// for any other HTTP method, by HTTP method, and fails them as fails says.
type flaky struct {
	mu    sync.Mutex
	fails map[string]failure
	stall time.Duration
	seen  map[string]int
}

func (f *flaky) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := r.Header.Get("Mcp-Method")
		switch {
		case r.Method != http.MethodPost:
			m = r.Method
		case m == "":
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &msg)
			m = msg.Method
		}
		f.mu.Lock()
		f.seen[m]++
		n := f.seen[m]
		fail, ok := f.fails[m]
		f.mu.Unlock()
		if m == http.MethodDelete && f.stall > 0 {
			select {
			case <-r.Context().Done():
			case <-time.After(f.stall):
			}
		}
		if ok && (fail.n < 0 || n <= fail.n) {
			http.Error(w, http.StatusText(fail.status), fail.status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *flaky) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[method]
}

func flakyServer(t *testing.T, f *flaky) *httptest.Server {
	t.Helper()
	f.seen = map[string]int{}
	return discoverServer(t, f.wrap, true)
}

func TestListingRetriesOnceAfterAnUpstreamFailure(t *testing.T) {
	t.Parallel()
	f := &flaky{fails: map[string]failure{"tools/list": {status: http.StatusInternalServerError, n: 1}}}
	ts := flakyServer(t, f)

	tools, err := upstream.New(ts.URL, nil).ListTools(t.Context(), "")

	require.NoError(t, err, "a single failure is retried")
	assert.Len(t, tools, 1)
	assert.Equal(t, 2, f.count("tools/list"))
}

func TestListingStopsAfterItsRetries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		retries, tries int
	}{{retries: 1, tries: 2}, {retries: 2, tries: 3}, {retries: -1, tries: 1}} {
		t.Run(strconv.Itoa(tc.retries), func(t *testing.T) {
			t.Parallel()
			f := &flaky{fails: map[string]failure{"tools/list": {status: http.StatusBadGateway, n: -1}}}
			ts := flakyServer(t, f)

			_, err := upstream.New(ts.URL, &upstream.Options{ListRetries: tc.retries}).ListTools(t.Context(), "")

			require.ErrorIs(t, err, upstream.ErrUpstream)
			assert.Equal(t, tc.tries, f.count("tools/list"))
		})
	}
}

func TestListingRetriesWithinItsTimeout(t *testing.T) {
	t.Parallel()
	f := &flaky{fails: map[string]failure{"tools/list": {status: http.StatusBadGateway, n: -1}}}
	ts := flakyServer(t, f)
	c := upstream.New(ts.URL, &upstream.Options{ListRetries: 100, ListTimeout: time.Second})

	start := time.Now()
	_, err := c.ListTools(t.Context(), "")

	require.ErrorIs(t, err, upstream.ErrUpstream)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Equal(t, 3, f.count("tools/list"), "tries at 0, 250 and 750 ms; the next would be at 1750")
}

func TestRefusedCallerIsNotRetried(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			f := &flaky{fails: map[string]failure{"tools/list": {status: status, n: -1}}}
			ts := flakyServer(t, f)

			_, err := upstream.New(ts.URL, nil).ListTools(t.Context(), "Bearer expired")

			require.ErrorIs(t, err, upstream.ErrUpstream)
			assert.Equal(t, 1, f.count("tools/list"), "a refusal is an answer, not a failure")
		})
	}
}

func TestToolCallsAreNotRetried(t *testing.T) {
	t.Parallel()
	f := &flaky{fails: map[string]failure{"tools/call": {status: http.StatusInternalServerError, n: 1}}}
	ts := flakyServer(t, f)

	_, err := upstream.New(ts.URL, nil).CallTool(t.Context(), "", "echo", nil)

	require.ErrorIs(t, err, upstream.ErrUpstream)
	assert.Equal(t, 1, f.count("tools/call"), "a call may have had its effect")
}

func TestListingRetryEndsWithTheCaller(t *testing.T) {
	t.Parallel()
	f := &flaky{fails: map[string]failure{"tools/list": {status: http.StatusBadGateway, n: -1}}}
	ts := flakyServer(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := upstream.New(ts.URL, nil).ListTools(ctx, "")

	require.ErrorIs(t, err, upstream.ErrUpstream, "the failure is kept")
	require.ErrorIs(t, err, context.Canceled, "and why the retries stopped")
}

func TestRefusedHandshakeThatRecoversStillRetries(t *testing.T) {
	t.Parallel()
	f := &flaky{fails: map[string]failure{
		"server/discover": {status: http.StatusUnauthorized, n: -1},
		"tools/list":      {status: http.StatusInternalServerError, n: 1},
	}}
	ts := flakyServer(t, f)

	tools, err := upstream.New(ts.URL, nil).ListTools(t.Context(), "Bearer x")

	require.NoError(t, err, "the fallback handshake worked, so the listing failure is what decides")
	assert.Len(t, tools, 1)
	assert.Equal(t, 2, f.count("tools/list"))
}

func TestFailedSessionCloseDoesNotDelayTheRetry(t *testing.T) {
	t.Parallel()
	f := &flaky{
		fails: map[string]failure{"tools/list": {status: http.StatusInternalServerError, n: 1}},
		stall: 3 * time.Second,
	}
	f.seen = map[string]int{}
	ts := discoverServer(t, f.wrap, false)

	start := time.Now()
	tools, err := upstream.New(ts.URL, &upstream.Options{ProtocolVersion: "2025-11-25"}).ListTools(t.Context(), "")

	require.NoError(t, err)
	assert.Len(t, tools, 1)
	assert.Less(t, time.Since(start), 2*time.Second, "the failed session is closed in the background")
}

// rawServer serves tool "raw", which answers with the bytes of its
// arguments as they arrived, tool "broken", which answers with a JSON-RPC
// error, and tool "hang", which never answers.
func rawServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "raw", Version: "0"}, nil)
	object := map[string]any{"type": "object"}
	srv.AddTool(&mcp.Tool{Name: "raw", InputSchema: object},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}},
			}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "broken", InputSchema: object},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "no such record"}
		})
	release := make(chan struct{})
	srv.AddTool(&mcp.Tool{Name: "hang", InputSchema: object},
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			select {
			case <-ctx.Done():
			case <-release:
			}
			return &mcp.CallToolResult{}, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) })
	return ts
}

func TestCallToolForwardsArgumentsUnchanged(t *testing.T) {
	t.Parallel()
	ts := rawServer(t)
	args := json.RawMessage(`{"id":1234567890123456789,"ratio":1.10}`)

	res, err := upstream.New(ts.URL, nil).CallTool(t.Context(), "", "raw", args)

	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, string(args), tc.Text)
}

// callError returns the *upstream.Error of err.
func callError(t *testing.T, err error) *upstream.Error {
	t.Helper()
	var e *upstream.Error
	require.ErrorAs(t, err, &e)
	require.ErrorIs(t, err, upstream.ErrUpstream)
	return e
}

func TestCallToolFailures(t *testing.T) {
	t.Parallel()
	ts := rawServer(t)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(unauthorized.Close)
	dropping := flakyServer(t, &flaky{fails: map[string]failure{"tools/call": {status: http.StatusBadGateway, n: -1}}})

	for _, tc := range []struct {
		name, url, tool, class string
		sent                   bool
		answer                 string
	}{
		{name: "answered", url: ts.URL, tool: "broken", class: upstream.ClassAnswered, sent: true, answer: "no such record"},
		{name: "timeout", url: ts.URL, tool: "hang", class: upstream.ClassTimeout, sent: true},
		{name: "refused", url: closed.URL, tool: "raw", class: upstream.ClassRefused},
		{name: "unauthorized", url: unauthorized.URL, tool: "raw", class: upstream.ClassUnauthorized},
		{name: "dropped", url: dropping.URL, tool: "echo", class: upstream.ClassDropped, sent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := upstream.New(tc.url, &upstream.Options{CallTimeout: 300 * time.Millisecond})

			_, err := c.CallTool(t.Context(), "", tc.tool, nil)

			e := callError(t, err)
			assert.Equal(t, tc.class, e.Class)
			assert.Equal(t, tc.sent, e.Sent)
			assert.Equal(t, tc.answer, e.Answer)
		})
	}
}

func TestCallAfterARefusedDiscoverIsNotUnauthorized(t *testing.T) {
	t.Parallel()
	f := &flaky{fails: map[string]failure{
		"server/discover": {status: http.StatusUnauthorized, n: -1},
		"tools/call":      {status: http.StatusBadGateway, n: -1},
	}}
	ts := flakyServer(t, f)

	_, err := upstream.New(ts.URL, nil).CallTool(t.Context(), "Bearer x", "echo", nil)

	e := callError(t, err)
	assert.Equal(t, upstream.ClassDropped, e.Class, "the fallback handshake worked, so the call may have run")
	assert.True(t, e.Sent)
}

func TestErrorDetailNeverHoldsTheUpstreamsMessage(t *testing.T) {
	t.Parallel()
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"bad token s3cr3t"}}`)
	}))
	t.Cleanup(refusing.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	_, err := upstream.New(refusing.URL, nil).CallTool(t.Context(), "Bearer x", "echo", nil)
	e := callError(t, err)
	assert.Equal(t, upstream.ClassUnauthorized, e.Class)
	assert.Empty(t, e.Detail())
	assert.NotContains(t, e.Detail(), "s3cr3t")

	_, err = upstream.New(closed.URL, nil).CallTool(t.Context(), "", "echo", nil)
	assert.Contains(t, callError(t, err).Detail(), strings.TrimPrefix(closed.URL, "http://"),
		"transport detail is kept for the log")
}

func TestListFailuresAreClassified(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(unauthorized.Close)

	_, err := upstream.New(closed.URL, &upstream.Options{ListRetries: -1}).ListTools(t.Context(), "")
	assert.Equal(t, upstream.ClassRefused, callError(t, err).Class)
	_, err = upstream.New(unauthorized.URL, nil).ListTools(t.Context(), "Bearer x")
	assert.Equal(t, upstream.ClassUnauthorized, callError(t, err).Class)
}

// requests records what OnRequest is told.
type requests struct {
	mu   sync.Mutex
	seen []string
}

func (r *requests) record(op, outcome string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if took <= 0 {
		outcome += " without a duration"
	}
	r.seen = append(r.seen, op+" "+outcome)
}

func (r *requests) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func TestEveryOperationIsReported(t *testing.T) {
	t.Parallel()
	ts, _ := fake(t, 0, true)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	rec := &requests{}
	opts := &upstream.Options{OnRequest: rec.record, ListRetries: 2}

	_, err := upstream.New(ts.URL, opts).ListTools(t.Context(), "")
	require.NoError(t, err)
	_, err = upstream.New(ts.URL, opts).CallTool(t.Context(), "", "fail", nil)
	require.NoError(t, err, "a tool reporting failure is an answer")
	_, err = upstream.New(closed.URL, opts).ListTools(t.Context(), "")
	require.Error(t, err)
	_, err = upstream.New(closed.URL, opts).CallTool(t.Context(), "", "echo", nil)
	require.Error(t, err)

	assert.Equal(t, []string{
		upstream.OpList + " " + upstream.OutcomeOK,
		upstream.OpCall + " " + upstream.OutcomeOK,
		upstream.OpList + " " + upstream.ClassRefused,
		upstream.OpCall + " " + upstream.ClassRefused,
	}, rec.all(), "one report per operation, a listing's retries included in it")
}

// bigServer serves tool "big", whose result holds a text of n bytes, in
// JSON responses, or in event streams with sse.
func bigServer(t *testing.T, n int, sse bool) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "big", Version: "0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "big"},
		func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			text := strings.Repeat("x", n)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: !sse}))
	t.Cleanup(ts.Close)
	return ts
}

func TestCallToolRefusesAResultOverTheLimit(t *testing.T) {
	t.Parallel()
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%t", sse), func(t *testing.T) {
			t.Parallel()
			ts := bigServer(t, 64<<10, sse)
			c := upstream.New(ts.URL, &upstream.Options{MaxResponseBytes: 32 << 10, CallTimeout: 10 * time.Second})

			start := time.Now()
			_, err := c.CallTool(t.Context(), "", "big", nil)

			e := callError(t, err)
			require.ErrorIs(t, err, upstream.ErrTooLarge)
			assert.Equal(t, upstream.ClassTooLarge, e.Class)
			assert.True(t, e.Sent, "the tool ran")
			assert.Less(t, time.Since(start), 5*time.Second, "the call fails at once, not at its timeout")
		})
	}
}

func TestCallToolPassesAResultWithinTheLimit(t *testing.T) {
	t.Parallel()
	ts := bigServer(t, 16<<10, false)
	c := upstream.New(ts.URL, &upstream.Options{MaxResponseBytes: 32 << 10})

	res, err := c.CallTool(t.Context(), "", "big", nil)

	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestListingOverItsLimitsFailsWithoutRetrying(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts upstream.Options
	}{
		{name: "tools", opts: upstream.Options{MaxListTools: 5}},
		{name: "pages", opts: upstream.Options{MaxListPages: 2}},
		{name: "bytes", opts: upstream.Options{MaxResponseBytes: 256}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			once, onceRec := fake(t, 10, true)
			retried, retriedRec := fake(t, 10, true)
			noRetries := tc.opts
			noRetries.ListRetries = -1

			_, err := upstream.New(retried.URL, &tc.opts).ListTools(t.Context(), "Bearer a")
			_, onceErr := upstream.New(once.URL, &noRetries).ListTools(t.Context(), "Bearer a")

			e := callError(t, err)
			require.ErrorIs(t, err, upstream.ErrTooLarge)
			require.Error(t, onceErr)
			assert.Equal(t, upstream.ClassTooLarge, e.Class)
			assert.Len(t, retriedRec.headers(), len(onceRec.headers()), "a listing over a limit is not tried again")
		})
	}
}

// pageSizes records the size of every response body it serves.
type pageSizes struct {
	mu    sync.Mutex
	sizes []int
}

func (p *pageSizes) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countingWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.sizes = append(p.sizes, cw.n)
	})
}

func (p *pageSizes) largest() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	largest := 0
	for _, n := range p.sizes {
		largest = max(largest, n)
	}
	return largest
}

type countingWriter struct {
	http.ResponseWriter

	n int
}

func (w *countingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.n += n
	return n, err
}

// Every page of a listing is kept, so the limit covers them together, not
// each on its own.
func TestListingLimitCoversEveryPage(t *testing.T) {
	t.Parallel()
	ts, _ := fake(t, 10, true)
	sizes := &pageSizes{}
	target, err := url.Parse(ts.URL)
	require.NoError(t, err)
	measured := httptest.NewServer(sizes.wrap(httputil.NewSingleHostReverseProxy(target)))
	t.Cleanup(measured.Close)
	_, err = upstream.New(measured.URL, nil).ListTools(t.Context(), "Bearer a")
	require.NoError(t, err)
	page := int64(sizes.largest())

	_, err = upstream.New(ts.URL, &upstream.Options{MaxResponseBytes: 2 * page, ListRetries: -1}).
		ListTools(t.Context(), "Bearer a")

	require.ErrorIs(t, err, upstream.ErrTooLarge, "four pages of up to %d bytes are over %d", page, 2*page)
}

func TestListingWithinItsLimits(t *testing.T) {
	t.Parallel()
	ts, _ := fake(t, 10, true)

	tools, err := upstream.New(ts.URL, &upstream.Options{MaxListTools: 12, MaxListPages: 4}).
		ListTools(t.Context(), "Bearer a")

	require.NoError(t, err, "12 tools in pages of 3 fit")
	assert.Len(t, tools, 12)
}

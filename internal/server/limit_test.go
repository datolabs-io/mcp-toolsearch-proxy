package server_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

const initRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
	`"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// postInit POSTs an initialize to url and returns the status and headers
// of the response.
func postInit(t *testing.T, url string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(initRequest))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header
}

// withheldBody sends ts the headers of a POST whose body never comes, and
// returns the status and headers of the response it gets regardless, and
// whether the server closes the connection.
func withheldBody(t *testing.T, ts *httptest.Server) (int, http.Header, bool) {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	// Well short of the read timeout: an answer that waits for the body
	// fails the test instead of hanging it.
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n"+
		"Accept: application/json, text/event-stream\r\nContent-Length: %d\r\n\r\n",
		ts.Listener.Addr(), len(initRequest))
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "the refusal comes without the body")
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, resp.Close
}

func waitFor(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	t.Parallel()

	srv := server.HTTPServer("127.0.0.1:0", http.NotFoundHandler())

	assert.Equal(t, 10*time.Second, srv.ReadHeaderTimeout)
	assert.Equal(t, time.Minute, srv.ReadTimeout, "a body must arrive within a minute")
	assert.Equal(t, 2*time.Minute, srv.IdleTimeout, "idle connections are closed")
	assert.Zero(t, srv.WriteTimeout, "an upstream tool call can take minutes")
}

func TestRequestsInFlightAreLimited(t *testing.T) {
	t.Parallel()
	gate := newGate()
	opts := &server.Options{MaxInFlight: 1}
	ts := httptest.NewServer(server.Handler(newServer(t, opts, gate), opts))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")
	held := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(t.Context(), callParams("hold", nil))
		held <- err
	}()
	waitFor(t, gate.entered, "the first call to hold its slot")
	// Let the held call go even when the test fails first, or closing the
	// server would wait for it forever.
	release := sync.OnceFunc(func() { close(gate.open) })
	t.Cleanup(release)

	full, fullHeader := postInit(t, ts.URL+"/mcp")
	withheld, withheldHeader, closed := withheldBody(t, ts)
	health, _ := get(t, ts, server.HealthPath)
	release()
	require.NoError(t, <-held)
	after, _ := postInit(t, ts.URL+"/mcp")

	assert.Equal(t, http.StatusServiceUnavailable, full)
	assert.Equal(t, "1", fullHeader.Get("Retry-After"))
	assert.Equal(t, http.StatusServiceUnavailable, withheld, "refused before the body arrives")
	assert.Equal(t, "1", withheldHeader.Get("Retry-After"))
	assert.True(t, closed, "the connection is closed rather than drained")
	assert.Equal(t, http.StatusOK, health, "liveness is not limited")
	assert.Equal(t, http.StatusOK, after, "the slot is free again")
}

// postSize POSTs to url an initialize padded to about n bytes, and returns
// the status of the response.
func postSize(t *testing.T, url string, n int) int {
	t.Helper()
	body := strings.TrimSuffix(initRequest, "}") + `,"pad":"` + strings.Repeat("x", n) + `"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// Each request holds memory in proportion to its body, so the bodies in
// flight share a budget besides the count: a large call leaves room for
// small requests but not for another large one.
func TestRequestBytesInFlightAreLimited(t *testing.T) {
	t.Parallel()
	gate := newGate()
	var rejected atomic.Int32
	opts := &server.Options{
		MaxInFlight: 8, MaxInFlightBytes: 64 << 10, OnRejected: func() { rejected.Add(1) },
	}
	ts := httptest.NewServer(server.Handler(newServer(t, opts, gate), opts))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")
	held := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(t.Context(), callParams("hold", map[string]any{"pad": strings.Repeat("x", 40<<10)}))
		held <- err
	}()
	waitFor(t, gate.entered, "the large call to hold its share")
	release := sync.OnceFunc(func() { close(gate.open) })
	t.Cleanup(release)

	small := postSize(t, ts.URL+"/mcp", 100)
	large := postSize(t, ts.URL+"/mcp", 40<<10)
	release()
	require.NoError(t, <-held)
	after := postSize(t, ts.URL+"/mcp", 40<<10)

	assert.Equal(t, http.StatusOK, small, "a small request fits beside the large one")
	assert.Equal(t, http.StatusServiceUnavailable, large, "a second large body does not fit")
	assert.Equal(t, http.StatusOK, after, "the share is free again")
	assert.Equal(t, int32(1), rejected.Load(), "a refusal for want of room counts as rejected")
}

// postChunked POSTs an initialize to url without giving its length, and
// returns the status of the response.
func postChunked(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url,
		io.MultiReader(strings.NewReader(initRequest)))
	require.NoError(t, err)
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// A request whose length is not given may carry the largest body allowed,
// so it weighs that much, or the whole budget when that is less: it is
// served alone, but not beside another.
func TestRequestOfUnknownLengthWeighsTheMost(t *testing.T) {
	t.Parallel()
	gate := newGate()
	opts := &server.Options{MaxInFlightBytes: 64 << 10}
	ts := httptest.NewServer(server.Handler(newServer(t, opts, gate), opts))
	t.Cleanup(ts.Close)
	alone := postChunked(t, ts.URL+"/mcp")
	cs := connect(t, ts.URL+"/mcp")
	held := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(t.Context(), callParams("hold", nil))
		held <- err
	}()
	waitFor(t, gate.entered, "a small call to hold its share")
	release := sync.OnceFunc(func() { close(gate.open) })
	t.Cleanup(release)

	beside := postChunked(t, ts.URL+"/mcp")
	known := postSize(t, ts.URL+"/mcp", 100)
	release()
	require.NoError(t, <-held)

	assert.Equal(t, http.StatusOK, alone, "served when nothing else is")
	assert.Equal(t, http.StatusServiceUnavailable, beside)
	assert.Equal(t, http.StatusOK, known, "one of known length fits")
}

// The read timeout bounds the request, body included, but net/http ends
// the connection's read deadline once the body is read: a call that takes
// longer keeps its context and is answered.
func TestACallOutlastsTheReadTimeout(t *testing.T) {
	t.Parallel()
	gate := newGate()
	gate.slow = 500 * time.Millisecond
	h := server.Handler(newServer(t, nil, gate), nil)
	// ended records a request whose context ended before it was answered.
	var ended atomic.Bool
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = server.HTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if r.Context().Err() != nil {
			ended.Store(true)
		}
	}))
	ts.Config.ReadTimeout = 100 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")

	res, err := cs.CallTool(t.Context(), callParams("slow", nil))

	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.False(t, ended.Load(), "the read timeout covers the body, not the call")
}

// A client that wants to hear of tool list changes opens a stream that
// stays open; the tools never change, so the server offers none and the
// client's calls keep their slots.
func TestToolListChangesAreNotOffered(t *testing.T) {
	t.Parallel()
	opts := &server.Options{MaxInFlight: 1}
	ts := httptest.NewServer(server.Handler(newServer(t, opts, nil), opts))
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {},
	})
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	caps := cs.InitializeResult().Capabilities
	require.NotNil(t, caps.Tools)
	assert.False(t, caps.Tools.ListChanged, "the tools never change")
	for range 3 {
		res, cerr := cs.CallTool(t.Context(), callParams("echo", map[string]any{"text": "hi"}))
		require.NoError(t, cerr)
		assert.False(t, res.IsError)
	}
}

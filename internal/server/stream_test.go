package server_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

// protocolVersions are the versions a client may speak: one with the
// initialize handshake, and one with server/discover.
func protocolVersions() []string { return []string{"2025-11-25", "2026-07-28"} }

// impatientFront is a reverse proxy in front of target that gives up on a
// response whose headers take longer than wait, as gateways do with a
// backend they wait on.
func impatientFront(t *testing.T, target string, wait time.Duration) *httptest.Server {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	rp := &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(u) },
		Transport: &http.Transport{ResponseHeaderTimeout: wait},
		ErrorLog:  nil,
	}
	ts := httptest.NewServer(rp)
	t.Cleanup(ts.Close)
	return ts
}

// connectVersion connects to endpoint speaking version.
func connectVersion(t *testing.T, endpoint, version string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: endpoint, DisableStandaloneSSE: true},
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// A call that takes longer than a proxy in front waits for response
// headers still reaches the caller: the headers go out as soon as the call
// starts, and comments keep the stream busy until its result.
func TestLongCallsPassAProxyThatWaitsForHeaders(t *testing.T) {
	t.Parallel()
	for _, version := range protocolVersions() {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			g := newGate()
			g.slow = 800 * time.Millisecond
			opts := &server.Options{KeepAlive: 100 * time.Millisecond}
			ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(catalog.Tools(g.list), nil), g))
			t.Cleanup(ts.Close)
			front := impatientFront(t, ts.URL, 300*time.Millisecond)
			cs := connectVersion(t, front.URL+"/mcp", version)

			slow, err := cs.CallTool(t.Context(), callParams("slow", nil))
			require.NoError(t, err, "the front proxy gave up on the call")
			fast, err := cs.CallTool(t.Context(), callParams("echo", map[string]any{"text": "labas"}))
			require.NoError(t, err)

			assert.False(t, slow.IsError, "%v", slow.Content)
			assert.False(t, fast.IsError, "%v", fast.Content)
			assert.Equal(t, "labas", text(fast))
		})
	}
}

// postRaw POSTs body to url as a client of version, and returns the
// response's status and body.
func postRaw(t *testing.T, url, version, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", version)
	var msg struct {
		Method string `json:"method"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &msg))
	req.Header.Set("Mcp-Method", msg.Method)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

// openStream POSTs body to url as a client of version, and returns the
// response's status, headers and body as it arrives.
func openStream(t *testing.T, url, version, body string) (int, http.Header, *bufio.Reader) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", version)
	// Headers that never come fail the test rather than hang it.
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp.StatusCode, resp.Header, bufio.NewReader(resp.Body)
}

// readUntil reads r up to and including the first line that holds want,
// failing the test when it does not arrive within a few seconds, and
// returns what it read.
func readUntil(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var read strings.Builder
		for {
			line, err := r.ReadString('\n')
			read.WriteString(line)
			if strings.Contains(line, want) || err != nil {
				got <- read.String()
				return
			}
		}
	}()
	select {
	case s := <-got:
		require.Contains(t, s, want)
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("%q did not arrive", want)
		return ""
	}
}

// holdCall is a tools/call of the gate's tool that holds until let go.
const holdCall = `{"jsonrpc":"2.0","id":2,"method":"tools/call",` +
	`"params":{"name":"call_write_tool","arguments":{"name":"hold"}}}`

// A call that waits on the upstream is answered as a stream of server-sent
// events: the headers and comments while it waits, as they are sent, then
// its result.
func TestLongCallsAreStreamedWithComments(t *testing.T) {
	t.Parallel()
	g := newGate()
	opts := &server.Options{KeepAlive: 50 * time.Millisecond}
	ts := httptest.NewServer(server.Handler(newServer(t, opts, g), opts))
	t.Cleanup(ts.Close)
	release := sync.OnceFunc(func() { close(g.open) })
	t.Cleanup(release)

	status, header, body := openStream(t, ts.URL+"/mcp", "2025-11-25", holdCall)
	waiting := readUntil(t, body, ": keepalive")
	release()
	result := readUntil(t, body, `"id":2`)

	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "text/event-stream", header.Get("Content-Type"))
	assert.NotContains(t, waiting, "event: message", "nothing but comments before the result")
	assert.Contains(t, result, "event: message")
}

// MCP 2025-03-26 allows a batch, whose quick answers may be written before
// a call in it starts to wait: the call's response is kept alive all the
// same, with the quick answers sent at once.
func TestBatchesWithLongCallsAreKeptAlive(t *testing.T) {
	t.Parallel()
	g := newGate()
	opts := &server.Options{KeepAlive: 50 * time.Millisecond}
	ts := httptest.NewServer(server.Handler(newServer(t, opts, g), opts))
	t.Cleanup(ts.Close)
	release := sync.OnceFunc(func() { close(g.open) })
	t.Cleanup(release)

	status, _, body := openStream(t, ts.URL+"/mcp", "2025-03-26",
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},`+holdCall+`]`)
	waiting := readUntil(t, body, ": keepalive")
	release()
	result := readUntil(t, body, `"id":2`)

	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, waiting, `"id":1`, "the ping is answered while the call waits")
	assert.Contains(t, result, "event: message")
}

// A request whose answer needs nothing upstream sends no comment, and a
// notification is accepted without a body, as before.
func TestQuickRequestsAndNotifications(t *testing.T) {
	t.Parallel()
	opts := &server.Options{KeepAlive: time.Millisecond}
	ts := httptest.NewServer(server.Handler(newServer(t, opts, nil), opts))
	t.Cleanup(ts.Close)
	for _, version := range protocolVersions() {
		note, noteBody := postRaw(t, ts.URL+"/mcp", version,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		list, listBody := postRaw(t, ts.URL+"/mcp", version, `{"jsonrpc":"2.0","id":1,"method":"tools/list",`+
			`"params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"`+version+`",`+
			`"io.modelcontextprotocol/clientCapabilities":{}}}}`)

		assert.Equal(t, http.StatusAccepted, note, version)
		assert.Empty(t, noteBody, version)
		assert.Equal(t, http.StatusOK, list, version)
		assert.Contains(t, listBody, server.ToolSearch, version)
		assert.NotContains(t, listBody, "keepalive", version)
	}
}

// A body over the limit is refused, and the connection closed rather than
// drained, as the transport does without the stream around it.
func TestBodiesOverTheLimitAreRefused(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(server.Handler(newServer(t, nil, nil), nil))
	t.Cleanup(ts.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` +
		strings.Repeat("a", server.MaxRequestBytes) + `"}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, bufio.NewReader(resp.Body))
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.True(t, resp.Close, "the connection is closed")
}

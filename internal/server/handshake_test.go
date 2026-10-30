package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
)

// cutHere is in the note that ends instructions cut at the limit.
const cutHere = "cut here"

// Each caller's handshake carries the upstream's instructions as listed for
// that caller, so one caller's never reach another.
func TestEachCallerGetsTheirOwnUpstreamInstructions(t *testing.T) {
	t.Parallel()
	f := proxy(t)

	alice := connectWith(t, f.url, "Bearer alice").InitializeResult().Instructions
	bob := connectWith(t, f.url, "Bearer bob").InitializeResult().Instructions
	again := connectWith(t, f.url, "Bearer alice").InitializeResult().Instructions

	assert.Contains(t, alice, upstreamInstructions+usersInstructions)
	assert.Contains(t, bob, upstreamInstructions)
	assert.NotContains(t, bob, usersInstructions)
	assert.Equal(t, alice, again)
	for _, got := range []string{alice, bob} {
		assert.True(t, strings.HasPrefix(got, "This server gives access"), "the proxy's own guidance comes first")
		assert.NotContains(t, got, cutHere)
	}
}

// handshakeWith returns the instructions of a handshake with a proxy
// forwarding at most limit characters of upstream, the upstream's own.
func handshakeWith(t *testing.T, upstream string, limit int) string {
	t.Helper()
	list := func(context.Context, string) (catalog.Listing, error) {
		return catalog.Listing{Instructions: upstream}, nil
	}
	opts := &server.Options{MaxInstructions: limit}
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(list, nil), newGate()))
	t.Cleanup(ts.Close)
	return connect(t, ts.URL+"/mcp").InitializeResult().Instructions
}

func TestUpstreamInstructionsAtTheLimitAreNotCut(t *testing.T) {
	t.Parallel()
	for name, upstream := range map[string]string{
		"ascii":      strings.Repeat("a", 10000),
		"lithuanian": strings.Repeat("ž", 10000),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := handshakeWith(t, upstream, 10000)

			assert.True(t, strings.HasSuffix(got, upstream), "forwarded whole")
			assert.NotContains(t, got, cutHere)
		})
	}
}

// Real gateways have sent instructions of tens of thousands of characters.
func TestLongUpstreamInstructionsAreCutAtTheLimit(t *testing.T) {
	t.Parallel()
	upstream := strings.Repeat("Įrankis grąžina orų prognozę. ", 900) // 27,000 characters
	require.Len(t, []rune(upstream), 27000)

	got := handshakeWith(t, upstream, 10000)

	kept := string([]rune(upstream)[:10000])
	assert.Contains(t, got, kept+"\n\n[The upstream server's instructions are "+cutHere)
	assert.NotContains(t, got, string([]rune(upstream)[:10001]))
}

// searchRequest is a search_tools call sent without a handshake first, as
// a stateless client may.
const searchRequest = `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
	`"params":{"name":"search_tools","arguments":{"query":"echo"}}}`

// postSearch sends searchRequest to ts as authorization and returns the
// response's status and body, or an error, for a goroutine other than the
// test's.
func postSearch(t *testing.T, ts *httptest.Server, authorization string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(searchRequest))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", authorization)
	resp, err := ts.Client().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// Handshakes and searches of one new caller, all waiting on its first
// listing, share it, and every handshake gets the instructions it brought.
func TestHandshakesAndSearchesShareTheFirstBuild(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var builds, inFlight atomic.Int32
	g := newGate()
	list := func(ctx context.Context, auth string) (catalog.Listing, error) {
		builds.Add(1)
		<-release
		tools, err := g.list(ctx, auth)
		return catalog.Listing{Tools: tools, Instructions: "built"}, err
	}
	opts := &server.Options{
		MaxInstructions: maxInstructions, HandshakeWait: time.Minute,
		OnInFlight: func(delta int) { inFlight.Add(int32(delta)) }, //nolint:gosec // -1 or 1
	}
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(list, nil), g))
	t.Cleanup(ts.Close)

	var wg sync.WaitGroup
	handshakes, searches := make([]string, 3), make([]string, 3)
	for i := range handshakes {
		wg.Go(func() {
			cs, err := dial(t, ts.URL+"/mcp", "Bearer carol")
			if err != nil {
				t.Error(err)
				return
			}
			handshakes[i] = cs.InitializeResult().Instructions
		})
		wg.Go(func() {
			status, body, err := postSearch(t, ts, "Bearer carol")
			if err != nil {
				t.Error(err)
				return
			}
			assert.Equal(t, http.StatusOK, status)
			searches[i] = body
		})
	}
	// Every request is in, the handshakes waiting for the listing and the
	// searches for it or about to.
	require.Eventually(t, func() bool { return inFlight.Load() == 6 }, 10*time.Second, time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), builds.Load())
	for _, instructions := range handshakes {
		assert.True(t, strings.HasSuffix(instructions, "built"), instructions)
	}
	for _, body := range searches {
		assert.Contains(t, body, `"name":"echo"`)
		assert.NotContains(t, body, `"isError":true`)
	}
}

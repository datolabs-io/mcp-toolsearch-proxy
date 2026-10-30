package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// lockedBuffer is a buffer safe for the log's writes and the test's reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// hostRecorder is a fake upstream that remembers the Host header of every
// request it receives.
type hostRecorder struct {
	mu    sync.Mutex
	hosts []string
}

func (h *hostRecorder) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.hosts)
}

func (h *hostRecorder) handler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "gateway", Version: "0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "weather_get_forecast", Description: "Weather forecast for a place."},
		func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		// A public name arrives on a loopback address here, which the SDK
		// would take for DNS rebinding.
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.hosts = append(h.hosts, r.Host)
		h.mu.Unlock()
		mcpHandler.ServeHTTP(w, r)
	})
}

// listening waits for serve's "listening" record in log and returns the
// addresses it names: the proxy's, then the metrics'.
func listening(t *testing.T, log *lockedBuffer) (string, string) {
	t.Helper()
	var rec struct {
		Msg     string `json:"msg"`
		Addr    string `json:"addr"`
		Metrics string `json:"metrics"`
	}
	require.Eventually(t, func() bool {
		for line := range strings.SplitSeq(log.String(), "\n") {
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "listening" {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "serve never listened: %s", log)
	return rec.Addr, rec.Metrics
}

// serve serves on ports of its own choosing, sends the configured Host
// upstream instead of the URL's, and answers metrics.
func TestServe(t *testing.T) {
	t.Parallel()
	up := &hostRecorder{}
	gw := httptest.NewServer(up.handler())
	t.Cleanup(gw.Close)
	cfg, err := parseConfig([]string{
		"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0", "-upstream", gw.URL + "/mcp",
		"-upstream-host", "gateway.example.com", "-embedder", "none", "-log-format", "json", "-model-dir", "/m",
	}, envOf(nil), io.Discard)
	require.NoError(t, err)
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, newLogger(log, slog.LevelInfo, logJSON)) }()
	addr, metricsAddr := listening(t, log)

	// A transport of its own: httptest servers closing in other tests close
	// the default transport's idle connections.
	hc := &http.Client{Transport: &http.Transport{}}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: "http://" + addr + "/mcp", DisableStandaloneSSE: true, HTTPClient: hc,
	}, nil)
	require.NoError(t, err)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: server.ToolSearch, Arguments: map[string]any{"query": "weather"},
	})
	require.NoError(t, err)
	require.NoError(t, cs.Close())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+metricsAddr+"/metrics", nil)
	require.NoError(t, err)
	resp, err := hc.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	cancel()

	assert.False(t, res.IsError, "%v", res.Content)
	require.NotEmpty(t, up.seen())
	for _, host := range up.seen() {
		assert.Equal(t, "gateway.example.com", host)
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `mcp_toolsearch_upstream_requests_total{op="list",outcome="ok"} 1`,
		"the listing is counted")
	select {
	case code := <-done:
		assert.Zero(t, code)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// Every setting reaches the part it configures: a setting dropped on its
// way there would pass every other test.
func TestOptionsCarryEverySetting(t *testing.T) {
	t.Parallel()
	cfg, err := parseConfig([]string{
		"-upstream",
		"http://gw/mcp",
		"-model-dir",
		"/m",
		"-upstream-host",
		"gw.example.com",
		"-upstream-protocol",
		"2025-11-25",
		"-call-timeout",
		"41s",
		"-list-retries",
		"5",
		"-list-timeout",
		"43s",
		"-max-response-bytes",
		"44MiB",
		"-max-tools",
		"45",
		"-catalog-ttl",
		"46s",
		"-catalog-max-age",
		"47m",
		"-max-catalogs",
		"48",
		"-path",
		"/tools",
		"-max-in-flight",
		"49",
		"-allowed-origins",
		"https://app.example.com",
		"-allowed-hosts",
		"proxy.example.com",
		"-max-instructions",
		"50",
		"-search-schemas",
		"4",
		"-handshake-wait",
		"51s",
		"-write-tools",
		"github_*",
		"-max-in-flight-bytes",
		"52MiB",
		"-max-batch-calls",
		"5",
	}, envOf(nil), io.Discard)
	require.NoError(t, err)
	logger := slog.New(slog.DiscardHandler)
	var emb catalog.Embedder = embedderStub{}

	up, store, srv := options(cfg, emb, logger)

	for field, pair := range map[string][2]any{
		"upstreamHost":                {cfg.upstreamHost, up.Host},
		"upstreamProtocol":            {cfg.upstreamProtocol, up.ProtocolVersion},
		"callTimeout":                 {cfg.callTimeout, up.CallTimeout},
		"listRetries":                 {cfg.listRetries, up.ListRetries},
		"listTimeout":                 {cfg.listTimeout, up.ListTimeout},
		"maxResponseBytes":            {cfg.maxResponseBytes, up.MaxResponseBytes},
		"maxTools":                    {cfg.maxTools, up.MaxListTools},
		"catalogTTL":                  {cfg.catalogTTL, store.TTL},
		"catalogMaxAge":               {cfg.catalogMaxAge, store.MaxAge},
		"maxCatalogs":                 {cfg.maxCatalogs, store.MaxSize},
		"path":                        {cfg.path, srv.Path},
		"maxInFlight":                 {cfg.maxInFlight, srv.MaxInFlight},
		"allowedOrigins":              {cfg.allowedOrigins, srv.AllowedOrigins},
		"allowedHosts":                {cfg.allowedHosts, srv.AllowedHosts},
		"maxInstructions":             {cfg.maxInstructions, srv.MaxInstructions},
		"searchSchemas":               {cfg.searchSchemas, srv.SearchSchemas},
		"handshakeWait":               {cfg.handshakeWait, srv.HandshakeWait},
		"writeTools":                  {cfg.writeTools, srv.WriteTools},
		"maxInFlightBytes":            {cfg.maxInFlightBytes, srv.MaxInFlightBytes},
		"maxBatchCalls":               {cfg.maxBatchCalls, srv.MaxBatchCalls},
		"callTimeout of a batch":      {cfg.callTimeout, srv.CallTimeout},
		"maxResponseBytes of a batch": {cfg.maxResponseBytes, srv.MaxResponseBytes},
	} {
		assert.NotZero(t, pair[0], "%s: set it to something other than its default", field)
		assert.Equal(t, pair[0], pair[1], field)
	}
	assert.Equal(t, server.Name, up.Name)
	assert.Equal(t, emb, store.Embedder)
	assert.Same(t, logger, store.Logger)
	assert.Same(t, logger, srv.Logger)
	assert.Equal(t, srv.Version, up.Version)

	// The settings options does not take are used by serve and run
	// themselves. A new setting must be added to one list or the other.
	elsewhere := []string{
		"command", "addr", "metricsAddr", "upstream", "embedder", "modelDir", "shutdownGrace", "logLevel", "logFormat",
	}
	passed := []string{
		"upstreamHost", "upstreamProtocol", "callTimeout", "listRetries", "listTimeout", "maxResponseBytes",
		"maxTools", "catalogTTL", "catalogMaxAge", "maxCatalogs", "path", "maxInFlight", "allowedOrigins",
		"allowedHosts", "maxInstructions", "searchSchemas", "handshakeWait", "writeTools", "maxInFlightBytes",
		"maxBatchCalls",
	}
	for f := range reflect.TypeFor[config]().Fields() {
		assert.True(t, slices.Contains(passed, f.Name) || slices.Contains(elsewhere, f.Name),
			"config.%s is neither passed on by options nor used elsewhere", f.Name)
	}
}

// listRetries 0, none, is the upstream client's -1: its zero is the
// default.
func TestNoListRetriesReachTheClientAsNone(t *testing.T) {
	t.Parallel()
	cfg, err := parseConfig(
		[]string{"-upstream", "http://gw/mcp", "-model-dir", "/m", "-list-retries", "0"},
		envOf(nil),
		io.Discard,
	)
	require.NoError(t, err)

	up, _, _ := options(cfg, nil, slog.New(slog.DiscardHandler))

	assert.Equal(t, -1, up.ListRetries)
	assert.Equal(t, upstream.DefaultListRetries, listRetries(upstream.DefaultListRetries))
}

// maxBatchCalls 0 leaves call_read_tools out, which is the server's -1:
// its zero is the default.
func TestNoBatchCallsReachTheServerAsNone(t *testing.T) {
	t.Parallel()
	cfg, err := parseConfig(
		[]string{"-upstream", "http://gw/mcp", "-model-dir", "/m", "-max-batch-calls", "0"},
		envOf(nil),
		io.Discard,
	)
	require.NoError(t, err)

	_, _, srv := options(cfg, nil, slog.New(slog.DiscardHandler))

	assert.Equal(t, -1, srv.MaxBatchCalls)
}

type embedderStub struct{}

func (embedderStub) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

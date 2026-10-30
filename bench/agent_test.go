package bench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI is a Messages API that answers each request with the next of its
// scripted contents, and keeps the requests it got.
type fakeAPI struct {
	mu       sync.Mutex
	script   []string // content arrays, as JSON
	stops    []string
	requests []map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	i := len(f.requests)
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","model":"m","content":` + f.script[i] +
		`,"stop_reason":"` + f.stops[i] + `","usage":{"input_tokens":100,"output_tokens":10,` +
		`"cache_read_input_tokens":1000,"cache_creation_input_tokens":50}}`))
}

func newAgent(t *testing.T, f *fakeAPI) agentSetup {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	return agentSetup{
		client: anthropic.NewClient(option.WithBaseURL(ts.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
		model:  "claude-test", effort: anthropic.OutputConfigEffortMedium, maxTurns: 4,
		prices: prices{In: 4, Out: 20, CacheRead: 0.2, CacheWrite: 5},
	}
}

// newEnv serves testTools behind an upstream and the real proxy.
func newEnv(t *testing.T) *env {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mcp-toolsearch-proxy")
	require.NoError(t, buildProxy(t.Context(), bin))
	e, err := startEnv(t.Context(), testTools(), bin, "-embedder", "none")
	require.NoError(t, err)
	t.Cleanup(e.Close)
	return e
}

const forecastCall = `[{"type":"tool_use","id":"tu_1","name":"weather_get_forecast","input":{"location":"Kaunas"}}]`

func rain() Task {
	return Task{Lang: "en", Query: "will it rain tomorrow", Want: "weather_get_forecast"}
}

// field is v[key] of a decoded JSON object, as T, failing the test when it
// is not one.
func field[T any](t *testing.T, v any, key string) T { //nolint:ireturn // whatever the test asks for
	t.Helper()
	obj, ok := v.(map[string]any)
	require.True(t, ok, "an object: %v", v)
	out, ok := obj[key].(T)
	require.True(t, ok, "%s: %v", key, obj[key])
	return out
}

// A run that calls the wanted tool succeeds there and then, and its tokens
// and cost are what the API reported.
func TestRunSucceedsOnTheWantedCall(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := &fakeAPI{script: []string{forecastCall}, stops: []string{"tool_use"}}
	a := newAgent(t, f)

	res := a.run(t.Context(), modeAll, e, rain(), "w1")

	assert.True(t, res.Success, res.Err)
	assert.Equal(t, 1, res.Turns)
	assert.Equal(t, []string{"weather_get_forecast"}, res.Calls)
	assert.Equal(t, usage{Input: 100, CacheRead: 1000, CacheWrite: 50, Output: 10}, res.Usage)
	assert.InDelta(t, (100*4+1000*0.2+50*5+10*20)/1e6, res.Cost, 1e-12)
	assert.Positive(t, res.Seconds, "the run's time is kept")
	req := f.requests[0]
	assert.Equal(t, "claude-test", req["model"])
	assert.Len(t, req["tools"], 2, "every tool loaded")
	assert.Equal(t, map[string]any{"type": "ephemeral"}, req["cache_control"])
}

// Anthropic's tool search gets every tool deferred, and the search tool.
func TestRunDefersToolsBehindToolSearch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := &fakeAPI{
		script: []string{`[{"type":"server_tool_use","id":"srv_1","name":"tool_search_tool_bm25",` +
			`"input":{"query":"rain forecast"}},` + forecastCall[1:]},
		stops: []string{"tool_use"},
	}

	res := newAgent(t, f).run(t.Context(), modeToolSearch, e, rain(), "w2")

	assert.True(t, res.Success, res.Err)
	assert.Equal(t, []string{"server:tool_search_tool_bm25", "weather_get_forecast"}, res.Used)
	assert.Equal(t, []string{"rain forecast"}, res.Searches)
	tools := field[[]any](t, f.requests[0], "tools")
	require.Len(t, tools, 3)
	for _, tl := range tools[:2] {
		assert.True(t, field[bool](t, tl, "defer_loading"))
	}
	assert.Equal(t, "tool_search_tool_bm25_20251119", field[string](t, tools[2], "type"))
}

// Through the proxy, the model sees the proxy's tools and guidance, and a
// call through call_read_tool reaches the upstream.
func TestRunThroughTheProxy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := &fakeAPI{
		script: []string{
			`[{"type":"tool_use","id":"tu_1","name":"search_tools","input":{"query":"weather forecast"}}]`,
			`[{"type":"tool_use","id":"tu_2","name":"call_read_tool","input":` +
				`{"name":"weather_get_forecast","arguments":{"location":"Kaunas"}}}]`,
		},
		stops: []string{"tool_use", "tool_use"},
	}

	res := newAgent(t, f).run(t.Context(), modeProxy, e, rain(), "w3")

	assert.True(t, res.Success, res.Err)
	assert.Equal(t, 2, res.Turns)
	assert.Equal(t, []string{"search_tools", "call_read_tool"}, res.Used, "what the model called")
	assert.Equal(t, []string{"weather forecast"}, res.Searches)
	req := f.requests[0]
	names := []string{}
	for _, tl := range field[[]any](t, req, "tools") {
		names = append(names, field[string](t, tl, "name"))
	}
	assert.Contains(t, names, "search_tools")
	assert.NotContains(t, names, "weather_get_forecast")
	system := field[string](t, field[[]any](t, req, "system")[0], "text")
	assert.Contains(t, system, e.proxied.instructions)
	second := field[[]any](t, f.requests[1], "messages")
	assert.Len(t, second, 3, "the question, the search, its result")
}

// A run that ends without the wanted call fails, with the tools it called
// instead and why it ended.
func TestRunFailsWithoutTheWantedCall(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := &fakeAPI{
		script: []string{`[{"type":"tool_use","id":"tu_1","name":"mail_send_message","input":{}}]`,
			`[{"type":"text","text":"Done."}]`},
		stops: []string{"tool_use", "end_turn"},
	}

	res := newAgent(t, f).run(t.Context(), modeAll, e, rain(), "w4")

	assert.False(t, res.Success)
	assert.Equal(t, 2, res.Turns)
	assert.Equal(t, []string{"mail_send_message"}, res.Calls)
	assert.Equal(t, "end_turn", res.Stop)
}

// A run stops at the turn limit.
func TestRunStopsAtMaxTurns(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	call := `[{"type":"tool_use","id":"tu","name":"mail_send_message","input":{}}]`
	f := &fakeAPI{
		script: []string{call, call, call, call, call},
		stops:  []string{"tool_use", "tool_use", "tool_use", "tool_use", "tool_use"},
	}

	res := newAgent(t, f).run(t.Context(), modeAll, e, rain(), "w5")

	assert.False(t, res.Success)
	assert.Equal(t, 4, res.Turns)
	assert.Equal(t, "max_turns", res.Stop)
}

// runAll runs every task in every mode, each worker as its own caller.
func TestRunAll(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	script := make([]string, 6)
	stops := make([]string, 6)
	for i := range script {
		script[i], stops[i] = forecastCall, "tool_use"
	}
	f := &fakeAPI{script: script, stops: stops}
	var seen int

	res := runAll(t.Context(), newAgent(t, f), e, []mode{modeAll, modeToolSearch}, []Task{rain()}, 3, 2,
		func(runResult) { seen++ })

	require.Len(t, res, 6)
	assert.Equal(t, 6, seen)
	for _, r := range res {
		assert.True(t, r.Success, r.Err)
	}
}

// A search through the proxy is kept as its query, or its queries joined.
func TestUsedRecordsQueries(t *testing.T) {
	t.Parallel()
	var msg anthropic.Message
	require.NoError(t, json.Unmarshal([]byte(`{"id":"m","type":"message","role":"assistant","model":"m",`+
		`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1},"content":[`+
		`{"type":"tool_use","id":"a","name":"search_tools","input":{"query":"weather"}},`+
		`{"type":"tool_use","id":"b","name":"search_tools","input":{"queries":["storm warnings","my location"]}}]}`), &msg))

	names, searches := used(&msg)

	assert.Equal(t, []string{"search_tools", "search_tools"}, names)
	assert.Equal(t, []string{"weather", "storm warnings | my location"}, searches)
}

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// secret appears in a token, a query, arguments and results, and must never
// reach the log.
const secret = "s3cr3t-7f1c"

type forecastIn struct {
	Place string `json:"place" jsonschema:"City name"`
}

type forecastOut struct {
	Forecast string `json:"forecast"`
}

// gateway is a fake upstream gateway: everyone may use the weather tools,
// only "Bearer alice" the user administration. Tools added later appear on the next list.
type gateway struct {
	mu     sync.Mutex
	extra  []string
	output map[string]any // tools added later with an output schema of their own
	calls  map[string]int // tools/call requests received, by tool name
}

// called is how many times the tool name was called upstream.
func (g *gateway) called(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[name]
}

// countCalls counts the tools/call requests srv receives.
func (g *gateway) countCalls(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && method == "tools/call" {
			g.mu.Lock()
			if g.calls == nil {
				g.calls = map[string]int{}
			}
			g.calls[p.Name]++
			g.mu.Unlock()
		}
		return next(ctx, method, req)
	}
}

func (g *gateway) add(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.extra = append(g.extra, name)
}

// addWithOutput adds a tool whose output schema is schema, which may be a
// boolean schema.
func (g *gateway) addWithOutput(name string, schema any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.output == nil {
		g.output = map[string]any{}
	}
	g.output[name] = schema
}

// upstreamInstructions are the fake gateway's instructions, and
// usersInstructions follow them for "Bearer alice" alone, who may use the
// user administration.
const (
	upstreamInstructions = "[weather] Temperatures are in degrees Celsius."
	usersInstructions    = "\n[users] Deleting a user cannot be undone."
)

// readOnly marks a fake upstream tool as one that only reads.
//
//nolint:gochecknoglobals // a constant annotation
var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true}

func (g *gateway) server(authorization string) *mcp.Server {
	instructions := upstreamInstructions
	if authorization == "Bearer alice" {
		instructions += usersInstructions
	}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "gateway", Version: "0"},
		&mcp.ServerOptions{Instructions: instructions},
	)
	srv.AddReceivingMiddleware(g.countCalls)
	mcp.AddTool(
		srv,
		&mcp.Tool{Name: "weather_get_forecast", Description: "Weather forecast for a place.", Annotations: readOnly},
		func(_ context.Context, req *mcp.CallToolRequest, in forecastIn) (*mcp.CallToolResult, forecastOut, error) {
			if in.Place == "" {
				return nil, forecastOut{}, errors.New("place is required")
			}
			return nil, forecastOut{
				Forecast: "rain in " + in.Place + " for " + req.Extra.Header.Get("Authorization"),
			}, nil
		},
	)
	mcp.AddTool(
		srv,
		&mcp.Tool{Name: "weather_get_alerts", Description: "Severe weather warnings.", Annotations: readOnly},
		func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		},
	)
	if authorization == "Bearer alice" {
		mcp.AddTool(srv, &mcp.Tool{Name: "users_delete", Description: "Delete a user account."},
			func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "deleted"}}}, nil, nil
			})
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, name := range g.extra {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "Added later."},
			func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{}, nil, nil
			})
	}
	for name, schema := range g.output {
		srv.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, OutputSchema: schema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
	}
	return srv
}

// The defaults of main for what the proxy passes on.
const (
	maxInstructions = 10000
	searchSchemas   = 3
)

type fixture struct {
	url  string
	gw   *gateway
	logs *bytes.Buffer
	mu   *sync.Mutex
}

func (f fixture) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs.String()
}

// syncWriter serializes writes to a buffer.
type syncWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// proxy starts the proxy as main does, with ProxyHandler and the default
// limits, in front of a fake gateway, logging at debug.
func proxy(t *testing.T) fixture {
	t.Helper()
	gw := &gateway{}
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return gw.server(r.Header.Get("Authorization")) },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	))
	t.Cleanup(up.Close)
	client := upstream.New(up.URL, nil)
	f := fixture{gw: gw, logs: &bytes.Buffer{}, mu: &sync.Mutex{}}
	logger := slog.New(
		slog.NewJSONHandler(syncWriter{mu: f.mu, b: f.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	opts := &server.Options{Logger: logger, MaxInstructions: maxInstructions, SearchSchemas: searchSchemas}
	// A call naming a tool added since the catalog was built lists again at
	// once, without waiting for the catalog to age.
	store := catalog.NewStore(listing(client), &catalog.StoreOptions{MinRelist: time.Nanosecond})
	ts := httptest.NewServer(server.ProxyHandler(opts, store, client))
	t.Cleanup(ts.Close)
	f.url = ts.URL + "/mcp"
	return f
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return res
}

//nolint:ireturn // T is the caller's own type
func structured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	require.False(t, res.IsError, "result: %v", text(res))
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var out T
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func text(res *mcp.CallToolResult) string {
	var b bytes.Buffer
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestOffersFiveTools(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	res, err := cs.ListTools(t.Context(), nil)

	require.NoError(t, err)
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	assert.ElementsMatch(
		t,
		[]string{
			server.ToolSearch,
			server.ToolDescribe,
			server.ToolCallRead,
			server.ToolCallReads,
			server.ToolCallWrite,
		},
		names,
	)
	assert.Equal(t, server.Name, cs.InitializeResult().ServerInfo.Name)
	assert.Contains(t, cs.InitializeResult().Instructions, server.ToolSearch)
}

func TestSearchFindsTheCallersTools(t *testing.T) {
	t.Parallel()
	f := proxy(t)
	alice, bob := connectWith(t, f.url, "Bearer alice"), connectWith(t, f.url, "Bearer bob")

	forAlice := structured[server.SearchOut](
		t,
		call(t, alice, server.ToolSearch, map[string]any{"query": "delete a user account"}),
	)
	forBob := structured[server.SearchOut](
		t,
		call(t, bob, server.ToolSearch, map[string]any{"query": "delete a user account"}),
	)

	require.NotEmpty(t, forAlice.Tools)
	assert.Equal(t, "users_delete", forAlice.Tools[0].Name)
	assert.Equal(t, 3, forAlice.Total)
	assert.Equal(t, 2, forBob.Total)
	for _, tl := range forBob.Tools {
		assert.NotEqual(t, "users_delete", tl.Name, "bob never sees a tool the gateway does not list for him")
	}
}

func TestSearchLimit(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	out := structured[server.SearchOut](
		t,
		call(t, cs, server.ToolSearch, map[string]any{"query": "weather", "limit": 1}),
	)

	assert.Len(t, out.Tools, 1)
}

func TestSearchNeedsAQuery(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	res := call(t, cs, server.ToolSearch, map[string]any{"query": "  "})

	assert.True(t, res.IsError)
}

func TestDescribeReturnsSchemas(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	out := structured[server.DescribeOut](t, call(t, cs, server.ToolDescribe, map[string]any{
		"names": []string{"weather_get_forecast", "users_delete"},
	}))

	require.Len(t, out.Tools, 1)
	assert.Equal(t, "weather_get_forecast", out.Tools[0].Name)
	schema, err := json.Marshal(out.Tools[0].InputSchema)
	require.NoError(t, err)
	assert.Contains(t, string(schema), "City name")
	assert.NotNil(t, out.Tools[0].OutputSchema)
	assert.Equal(t, []string{"users_delete"}, out.Unknown, "not listed for a caller without a token")
}

func TestDescribeBounds(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	assert.True(t, call(t, cs, server.ToolDescribe, map[string]any{"names": []string{}}).IsError)
	many := make([]string, 11)
	for i := range many {
		many[i] = "weather_get_forecast"
	}
	assert.True(t, call(t, cs, server.ToolDescribe, map[string]any{"names": many}).IsError)
}

func TestCallForwardsArgumentsAndTheCallersToken(t *testing.T) {
	t.Parallel()
	cs := connectWith(t, proxy(t).url, "Bearer bob")

	res := call(t, cs, server.ToolCallRead, map[string]any{
		"name": "weather_get_forecast", "arguments": map[string]any{"place": "Vilnius"},
	})

	out := structured[forecastOut](t, res)
	assert.Equal(t, "rain in Vilnius for Bearer bob", out.Forecast)
}

func TestCallPassesToolErrorsThrough(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	res := call(
		t,
		cs,
		server.ToolCallRead,
		map[string]any{"name": "weather_get_forecast", "arguments": map[string]any{}},
	)

	assert.True(t, res.IsError)
	assert.Contains(t, text(res), "place", "the upstream error reaches the caller")
}

func TestCallRefusesToolsTheCallerMayNotUse(t *testing.T) {
	t.Parallel()
	cs := connectWith(t, proxy(t).url, "Bearer bob")

	res := call(t, cs, server.ToolCallWrite, map[string]any{"name": "users_delete"})

	assert.True(t, res.IsError)
	assert.Contains(t, text(res), "no tool")
}

func TestCallSuggestsSimilarTools(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	res := call(t, cs, server.ToolCallRead, map[string]any{"name": "weather_forecast"})

	assert.True(t, res.IsError)
	assert.Contains(t, text(res), "weather_get_forecast")
}

func TestCallFindsToolsAddedSinceTheCatalogWasBuilt(t *testing.T) {
	t.Parallel()
	f := proxy(t)
	cs := connect(t, f.url)
	structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{"query": "weather"}))
	f.gw.add("weather_get_radar")

	res := call(t, cs, server.ToolCallWrite, map[string]any{"name": "weather_get_radar"})

	assert.False(t, res.IsError, text(res))
}

func TestCallRefusesMalformedArguments(t *testing.T) {
	t.Parallel()
	f := proxy(t)
	cs := connect(t, f.url)

	for _, args := range []map[string]any{
		{},
		{"name": "weather_get_forecast", "arguments": []any{1, 2}},
		{"name": "weather_get_forecast", "arguments": "place=Vilnius"},
		{"name": "weather_get_forecast", "arguments": 7},
	} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: server.ToolCallRead, Arguments: args})

		require.NoError(t, err, "a tool error, not a protocol error: %v", args)
		assert.True(t, res.IsError, "%v", args)
	}
	assert.Zero(t, f.gw.called("weather_get_forecast"), "no malformed call reaches the upstream")
}

func TestUnreachableUpstreamIsAToolError(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	client := upstream.New(down.URL, nil)
	ts := httptest.NewServer(server.Handler(server.New(nil, catalog.NewStore(listing(client), nil), client), nil))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")

	res := call(t, cs, server.ToolSearch, map[string]any{"query": "weather"})

	assert.True(t, res.IsError)
	assert.Contains(t, text(res), "could not be loaded")
}

func TestTheLogHoldsNoQueryArgumentsResultOrToken(t *testing.T) {
	t.Parallel()
	f := proxy(t)
	cs := connectWith(t, f.url, "Bearer alice-"+secret)

	call(t, cs, server.ToolSearch, map[string]any{"query": "weather " + secret})
	call(t, cs, server.ToolSearch, map[string]any{"query": ""})
	call(t, cs, server.ToolDescribe, map[string]any{"names": []string{secret}})
	call(
		t,
		cs,
		server.ToolCallRead,
		map[string]any{"name": "weather_get_forecast", "arguments": map[string]any{"place": secret}},
	)
	call(
		t,
		cs,
		server.ToolCallRead,
		map[string]any{"name": "weather_get_forecast", "arguments": map[string]any{"days": secret}},
	)
	call(t, cs, server.ToolCallWrite, map[string]any{"name": "missing_tool", "arguments": map[string]any{"x": secret}})
	_, _ = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "made_up_" + secret})

	log := f.log()
	assert.Contains(t, log, "tool call failed", "failures are logged")
	assert.Contains(t, log, `"target":"missing_tool"`, "with the tool the call named")
	assert.NotContains(t, log, secret)
}

func TestCallToolSchemaTakesAnObject(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	res, err := cs.ListTools(t.Context(), nil)

	require.NoError(t, err)
	for _, tl := range res.Tools {
		if tl.Name != server.ToolCallRead && tl.Name != server.ToolCallWrite {
			continue
		}
		raw, merr := json.Marshal(tl.InputSchema)
		require.NoError(t, merr)
		assert.JSONEq(t, `{"type":"object","description":"The tool's arguments, as its input schema describes them."}`,
			string(property(t, raw, "arguments")))
	}
}

// property returns the property key of the JSON schema raw.
func property(t *testing.T, raw []byte, key string) json.RawMessage {
	t.Helper()
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(raw, &s))
	return s.Properties[key]
}

// Clients differ in how much of JSON Schema they accept, and some drop a
// server's whole tool list over one schema they reject. The schemas keep to
// plain object schemas: no boolean schema such as `true` anywhere.
func TestToolSchemasHoldNoBooleanSchemas(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	res, err := cs.ListTools(t.Context(), nil)

	require.NoError(t, err)
	for _, tl := range res.Tools {
		for kind, s := range map[string]any{"input": tl.InputSchema, "output": tl.OutputSchema} {
			if s == nil {
				continue
			}
			raw, merr := json.Marshal(s)
			require.NoError(t, merr)
			var v any
			require.NoError(t, json.Unmarshal(raw, &v))
			assert.Empty(t, booleanSchemas(v, "$"), "%s schema of %s", kind, tl.Name)
		}
	}
}

// booleanSchemas returns the paths in a decoded JSON Schema where a
// subschema is a boolean, under any keyword that holds subschemas;
// `additionalProperties: false` alone is allowed, as the usual closed object.
// The e2e tests walk schemas with a copy of it.
func booleanSchemas(v any, path string) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	sub := func(p string, s any) {
		if _, isBool := s.(bool); isBool {
			out = append(out, p)
			return
		}
		out = append(out, booleanSchemas(s, p)...)
	}
	list := func(key string, l []any) {
		for i, s := range l {
			sub(fmt.Sprintf("%s.%s[%d]", path, key, i), s)
		}
	}
	for _, key := range []string{
		"additionalItems", "contains", "not", "if", "then", "else",
		"propertyNames", "unevaluatedItems", "unevaluatedProperties",
	} {
		if s, has := m[key]; has {
			sub(path+"."+key, s)
		}
	}
	// items is a schema, or before draft 2020-12 a list of them.
	if s, has := m["items"]; has {
		if l, isList := s.([]any); isList {
			list("items", l)
		} else {
			sub(path+".items", s)
		}
	}
	if s, has := m["additionalProperties"]; has && s != false {
		sub(path+".additionalProperties", s)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if l, has := m[key].([]any); has {
			list(key, l)
		}
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if set, has := m[key].(map[string]any); has {
			for name, s := range set {
				sub(path+"."+key+"."+name, s)
			}
		}
	}
	// Before draft 2019-09, dependencies held schemas and lists of names.
	if set, has := m["dependencies"].(map[string]any); has {
		for name, s := range set {
			if _, names := s.([]any); !names {
				sub(path+".dependencies."+name, s)
			}
		}
	}
	return out
}

func TestBooleanSchemasFindsEveryKeyword(t *testing.T) {
	t.Parallel()

	for _, schema := range []string{
		`{"anyOf":[true]}`, `{"$defs":{"x":true}}`, `{"additionalProperties":true}`,
		`{"properties":{"a":{"items":true}}}`, `{"not":false}`,
		`{"items":[{"type":"string"},true]}`, `{"items":[{"items":[false]}]}`,
		`{"dependencies":{"a":true}}`, `{"dependencies":{"a":{"properties":{"b":false}}}}`,
	} {
		var v any
		require.NoError(t, json.Unmarshal([]byte(schema), &v))
		assert.NotEmpty(t, booleanSchemas(v, "$"), schema)
	}
	for _, schema := range []string{
		`{"type":"object","additionalProperties":false}`,
		`{"dependencies":{"a":["b","c"]}}`, `{"items":[{"type":"string"}]}`,
	} {
		var v any
		require.NoError(t, json.Unmarshal([]byte(schema), &v))
		assert.Empty(t, booleanSchemas(v, "$"), schema)
	}
}

func TestDescribeReturnsToolsWithBooleanOutputSchemas(t *testing.T) {
	t.Parallel()
	f := proxy(t)
	f.gw.addWithOutput("weather_raw", true)
	cs := connect(t, f.url)

	res := call(t, cs, server.ToolDescribe, map[string]any{"names": []string{"weather_raw"}})

	out := structured[server.DescribeOut](t, res)
	require.Len(t, out.Tools, 1)
	assert.Equal(t, true, out.Tools[0].OutputSchema)
}

func TestCallReadRefusesToolsThatMayChangeThings(t *testing.T) {
	t.Parallel()
	f := proxy(t)
	cs := connectWith(t, f.url, "Bearer alice")

	res := call(t, cs, server.ToolCallRead, map[string]any{"name": "users_delete"})

	assert.True(t, res.IsError)
	assert.Contains(t, text(res), server.ToolCallWrite)
	assert.Zero(t, f.gw.called("users_delete"), "the tool never ran")
	call(t, cs, server.ToolCallWrite, map[string]any{"name": "users_delete"})
	assert.Equal(t, 1, f.gw.called("users_delete"), "calls are counted")
}

func TestCallReadRefusesToolsWithoutAnnotations(t *testing.T) {
	t.Parallel()
	f := proxy(t)
	cs := connect(t, f.url)
	f.gw.add("weather_set_units")

	res := call(t, cs, server.ToolCallRead, map[string]any{"name": "weather_set_units"})

	assert.True(t, res.IsError, "an unmarked tool may change anything")
}

func TestCallWriteCallsAnyTool(t *testing.T) {
	t.Parallel()
	cs := connectWith(t, proxy(t).url, "Bearer alice")

	deleted := call(t, cs, server.ToolCallWrite, map[string]any{"name": "users_delete"})
	read := call(t, cs, server.ToolCallWrite, map[string]any{"name": "weather_get_alerts"})

	assert.False(t, deleted.IsError, text(deleted))
	assert.Equal(t, "deleted", text(deleted))
	assert.False(t, read.IsError, text(read))
}

func TestCallToolsAreAnnotatedForClientPermissions(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)

	res, err := cs.ListTools(t.Context(), nil)

	require.NoError(t, err)
	byName := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl
	}
	require.Contains(t, byName, server.ToolCallRead)
	require.Contains(t, byName, server.ToolCallWrite)
	assert.True(t, byName[server.ToolCallRead].Annotations.ReadOnlyHint, "clients may let reads run unasked")
	assert.False(t, byName[server.ToolCallWrite].Annotations.ReadOnlyHint)
	require.NotNil(t, byName[server.ToolCallWrite].Annotations.DestructiveHint)
	assert.True(t, *byName[server.ToolCallWrite].Annotations.DestructiveHint)
}

func TestSearchSaysWhichToolsOnlyRead(t *testing.T) {
	t.Parallel()
	cs := connectWith(t, proxy(t).url, "Bearer alice")

	out := structured[server.SearchOut](
		t,
		call(t, cs, server.ToolSearch, map[string]any{"query": "weather user", "limit": 10}),
	)

	readOnly := map[string]bool{}
	for _, tl := range out.Tools {
		readOnly[tl.Name] = tl.ReadOnly
	}
	assert.True(t, readOnly["weather_get_forecast"])
	assert.False(t, readOnly["users_delete"])
}

// strictGateway acts like a gateway whose servers do not all speak MCP
// 2026-07-28: it fails server/discover, and refuses an initialize that
// claims the 2026-07-28 version in its headers, as agentgateway rightly does.
func strictGateway(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Method") == "server/discover" {
			http.Error(w, "upstream error", http.StatusInternalServerError)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"method":"initialize"`)) &&
			r.Header.Get("Mcp-Protocol-Version") == "2026-07-28" {
			http.Error(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found: initialize"}}`,
				http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// The client's own protocol version must not leak into the proxy's requests
// upstream: a fallback initialize that carries a 2026-07-28 header is refused.
func TestCatalogBuildsWhenTheUpstreamFallsBackToInitialize(t *testing.T) {
	t.Parallel()
	gw := &gateway{}
	up := httptest.NewServer(strictGateway(mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return gw.server(r.Header.Get("Authorization")) },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)))
	t.Cleanup(up.Close)
	client := upstream.New(up.URL, nil)
	ts := httptest.NewServer(server.Handler(server.New(nil, catalog.NewStore(listing(client), nil), client), nil))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")

	out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{"query": "weather"}))
	res := call(t, cs, server.ToolCallRead, map[string]any{
		"name": "weather_get_forecast", "arguments": map[string]any{"place": "Kaunas"},
	})

	assert.NotEmpty(t, out.Tools)
	assert.False(t, res.IsError, text(res))
}

// proxyWith starts the proxy, as main does, with opts in front of a fake
// gateway, or of an unreachable one when down is set.
func proxyWith(t *testing.T, opts *server.Options, down bool) string {
	t.Helper()
	gw := &gateway{}
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return gw.server(r.Header.Get("Authorization")) },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	))
	t.Cleanup(up.Close)
	if down {
		up.Close()
	}
	client := upstream.New(up.URL, nil)
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(listing(client), nil), client))
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp"
}

func TestHandshakeForwardsTheUpstreamInstructions(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxyWith(t, &server.Options{MaxInstructions: 1000}, false))

	got := cs.InitializeResult().Instructions

	assert.Contains(t, got, "search_tools", "the proxy's own guidance comes first")
	assert.Contains(t, got, upstreamInstructions, "then the upstream's, unchanged")
}

func TestUpstreamInstructionsAreCapped(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxyWith(t, &server.Options{MaxInstructions: 10}, false))

	got := cs.InitializeResult().Instructions

	assert.Contains(t, got, upstreamInstructions[:10])
	assert.NotContains(t, got, upstreamInstructions)
	assert.Contains(t, got, "cut here", "the model is told text is missing")
}

func TestNoUpstreamInstructionsWhenTheLimitIsZero(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxyWith(t, &server.Options{}, false))

	assert.NotContains(t, cs.InitializeResult().Instructions, "Celsius")
}

func TestHandshakeWorksWhileTheUpstreamIsDown(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxyWith(t, &server.Options{MaxInstructions: 1000}, true))

	got := cs.InitializeResult().Instructions

	assert.Contains(t, got, "search_tools")
	assert.NotContains(t, got, "Celsius")
}

func TestSearchIncludesSchemasOfTheBestMatches(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxyWith(t, &server.Options{SearchSchemas: 1}, false))

	out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{"query": "weather forecast"}))
	none := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{
		"query": "weather forecast", "schemas": false,
	}))

	require.GreaterOrEqual(t, len(out.Tools), 2)
	assert.Equal(t, "weather_get_forecast", out.Tools[0].Name)
	schema, err := json.Marshal(out.Tools[0].InputSchema)
	require.NoError(t, err)
	assert.Contains(t, string(schema), "City name")
	assert.Nil(t, out.Tools[1].InputSchema, "only the best SearchSchemas matches")
	assert.Nil(t, none.Tools[0].InputSchema, "schemas: false asks for names only")
}

func TestSearchWithoutSchemas(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxyWith(t, &server.Options{}, false))

	out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{"query": "weather forecast"}))

	require.NotEmpty(t, out.Tools)
	assert.Nil(t, out.Tools[0].InputSchema)
}

// stalled is an upstream that never answers until the test ends.
func stalled(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		ts.Close()
	})
	return ts.URL
}

// A stalled upstream must not hold the handshake: without a catalog to
// take instructions from, it waits a little and goes on without them.
func TestHandshakeDoesNotWaitForAStalledUpstream(t *testing.T) {
	t.Parallel()
	client := upstream.New(stalled(t), nil)
	opts := &server.Options{MaxInstructions: 1000, HandshakeWait: 100 * time.Millisecond}
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(listing(client), nil), client))
	t.Cleanup(ts.Close)

	start := time.Now()
	cs := connect(t, ts.URL+"/mcp")

	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Contains(t, cs.InitializeResult().Instructions, "search_tools")
}

// One request lists upstream at most once for its instructions, however
// often the transport asks for its server.
func TestOneRequestBuildsTheCatalogOnce(t *testing.T) {
	t.Parallel()
	var builds atomic.Int32
	list := func(context.Context, string) (catalog.Listing, error) {
		builds.Add(1)
		return catalog.Listing{}, errors.New("down")
	}
	opts := &server.Options{MaxInstructions: 1000}
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(list, nil), newGate()))
	t.Cleanup(ts.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},`+
			`"clientInfo":{"name":"t","version":"0"}}}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(1), builds.Load())
}

// An expired catalog still lends its instructions at once: they rarely
// change, and the next search lists again anyway.
func TestHandshakeUsesAnExpiredCatalog(t *testing.T) {
	t.Parallel()
	var builds atomic.Int32
	list := func(context.Context, string) (catalog.Listing, error) {
		builds.Add(1)
		return catalog.Listing{Instructions: "kept"}, nil
	}
	store := catalog.NewStore(list, &catalog.StoreOptions{TTL: time.Nanosecond, MaxAge: time.Hour})
	_, err := store.Get(t.Context(), "", false)
	require.NoError(t, err)
	opts := &server.Options{MaxInstructions: 1000}
	ts := httptest.NewServer(server.ProxyHandler(opts, store, newGate()))
	t.Cleanup(ts.Close)

	cs := connect(t, ts.URL+"/mcp")

	assert.Contains(t, cs.InitializeResult().Instructions, "kept")
}

// Lexical search costs tools times query words, so a long query or name is
// refused before the catalog is searched.
func TestLongQueriesAndNamesAreRefused(t *testing.T) {
	t.Parallel()
	cs := connect(t, proxy(t).url)
	long := strings.Repeat("ą", 1001)
	name := strings.Repeat("n", 257)

	search := call(t, cs, server.ToolSearch, map[string]any{"query": long})
	describe := call(t, cs, server.ToolDescribe, map[string]any{"names": []string{"weather_get_forecast", name}})
	read := call(t, cs, server.ToolCallRead, map[string]any{"name": name})
	write := call(t, cs, server.ToolCallWrite, map[string]any{"name": name})
	atLimit := call(t, cs, server.ToolSearch, map[string]any{"query": strings.Repeat("ą", 1000)})

	assert.True(t, search.IsError)
	assert.Contains(t, text(search), "at most 1000 characters")
	for _, res := range []*mcp.CallToolResult{describe, read, write} {
		assert.True(t, res.IsError)
		assert.Contains(t, text(res), "at most 256 bytes")
		assert.NotContains(t, text(res), name, "the name is not repeated back")
	}
	assert.False(t, atLimit.IsError, "a query of 1000 characters is searched")
}

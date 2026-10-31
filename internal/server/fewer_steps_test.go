package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
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

// stepsFixture starts the proxy in front of r, with opts, listing tools as
// list gives them when it is set, and returns a client session, the
// outcomes of its calls and the proxy's log.
func stepsFixture(
	t *testing.T, r *recording, list func() []*mcp.Tool, opts *server.Options,
) (*mcp.ClientSession, *outcomes, *syncBuffer) {
	t.Helper()
	r.release = make(chan struct{})
	up := httptest.NewServer(r.handler())
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(r.release) })
	client := upstream.New(up.URL, nil)
	lister := listing(client)
	if list != nil {
		lister = catalog.Tools(func(context.Context, string) ([]*mcp.Tool, error) { return list(), nil })
	}
	logs := &syncBuffer{}
	rec := &outcomes{}
	o := server.Options{}
	if opts != nil {
		o = *opts
	}
	o.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	o.Middleware = append(o.Middleware, rec.middleware())
	ts := httptest.NewServer(server.Handler(server.New(&o, catalog.NewStore(lister, nil), client), &o))
	t.Cleanup(ts.Close)
	return connect(t, ts.URL+"/mcp"), rec, logs
}

// schemaTool is a tool with the input schema raw, as JSON.
func schemaTool(t *testing.T, name, raw string, ann *mcp.ToolAnnotations) *mcp.Tool {
	t.Helper()
	var schema any
	require.NoError(t, json.Unmarshal([]byte(raw), &schema), raw)
	return &mcp.Tool{Name: name, Description: "Tool " + name + ".", InputSchema: schema, Annotations: ann}
}

// recordsSchema is written with its keys sorted, as the proxy returns it.
const recordsSchema = `{"properties":{"id":{"type":"integer"},` +
	`"kind":{"enum":["open","closed"],"type":"string"}},"required":["id"],"type":"object"}`

func TestCallRefusesArgumentsThatDoNotFitWithTheSchema(t *testing.T) {
	t.Parallel()
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	r := &recording{listed: []*mcp.Tool{tool("records_get", ro)}}
	list := func() []*mcp.Tool { return []*mcp.Tool{schemaTool(t, "records_get", recordsSchema, ro)} }
	cs, rec, logs := stepsFixture(t, r, list, nil)

	for args, want := range map[string]string{
		`{}`:                            `"id" is required and missing`,
		`{"id":"` + secret + `"}`:       `"id" is a string, but must be integer`,
		`{"id":1,"kind":"archived"}`:    `"kind" is "archived", but must be one of "open", "closed"`,
		`{"idd":1}`:                     `the schema names no "idd"`,
		`{"id":1.5}`:                    `"id" is a number, but must be integer`,
		`{"id":null,"kind":["open"]}`:   `"kind" is an array`,
		`{"id":{"value":1},"kind":"x"}`: `"id" is an object`,
	} {
		for _, tl := range []string{server.ToolCallRead, server.ToolCallWrite} {
			res := rawCall(t, cs, tl, `{"name":"records_get","arguments":`+args+`}`)

			require.True(t, res.IsError, "%s %s", tl, args)
			assert.Contains(t, text(res), want, args)
			assert.Contains(t, text(res), "was not called")
			assert.Contains(t, text(res), "Input schema of records_get: "+recordsSchema)
			assert.Equal(t, server.OutcomeInvalidArguments, rec.last(), args)
		}
	}
	assert.Zero(t, r.calls.Load(), "nothing reaches the upstream")
	assert.Contains(t, logs.String(), `"outcome":"invalid_arguments"`)
	assert.NotContains(t, logs.String(), secret, "the arguments are never logged")
}

func TestCallRefusalSaysWhenTheSchemaIsTooLargeToInclude(t *testing.T) {
	t.Parallel()
	props := make([]string, 0, 400)
	for i := range 400 {
		props = append(props, fmt.Sprintf(`"field_%03d":{"type":"string","description":"A field of the record."}`, i))
	}
	big := `{"type":"object","properties":{` + strings.Join(props, ",") + `},"required":["field_000"]}`
	r := &recording{listed: []*mcp.Tool{tool("records_put", nil)}}
	cs, _, _ := stepsFixture(t, r, func() []*mcp.Tool { return []*mcp.Tool{schemaTool(t, "records_put", big, nil)} },
		nil)

	res := rawCall(t, cs, server.ToolCallWrite, `{"name":"records_put","arguments":{}}`)

	require.True(t, res.IsError)
	assert.Contains(t, text(res), `"field_000" is required and missing`)
	assert.Contains(t, text(res), "too large to include")
	assert.Contains(t, text(res), server.ToolDescribe)
	assert.Less(t, len(text(res)), 2<<10)
	assert.Zero(t, r.calls.Load())
}

// Schemas as servers publish them, with arguments their servers accept: a
// check of the proxy's must never refuse one.
func TestCallPassesArgumentsRealSchemasAccept(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ schema, args string }{
		"python optional and enum ref": {
			`{"type":"object","properties":{"q":{"type":"string"},"limit":{"anyOf":[{"type":"integer"},` +
				`{"type":"null"}],"default":null},"color":{"$ref":"#/$defs/Color"}},"required":["q"],` +
				`"$defs":{"Color":{"enum":["red","green"],"title":"Color","type":"string"}}}`,
			`{"q":"x","limit":null,"color":"red"}`,
		},
		"python optional given": {
			`{"type":"object","properties":{"limit":{"anyOf":[{"type":"integer"},{"type":"null"}]}}}`,
			`{"limit":5}`,
		},
		"python v1 allOf ref": {
			`{"type":"object","properties":{"c":{"allOf":[{"$ref":"#/definitions/C"}],"description":"x"}},` +
				`"definitions":{"C":{"enum":["a","b"],"type":"string"}}}`,
			`{"c":"b"}`,
		},
		"zod strict object with an extra key": {
			`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{` +
				`"a":{"type":["string","null"]},"b":{"type":"number"}},"required":["a"],"additionalProperties":false}`,
			`{"a":null,"b":1.5,"extra":1}`,
		},
		"zod relative ref": {
			`{"type":"object","properties":{"a":{"type":"object","properties":{"x":{"type":"string"}}},` +
				`"b":{"$ref":"#/properties/a"}}}`,
			`{"a":{"x":1},"b":{"x":2}}`,
		},
		"go sdk": {
			`{"type":"object","properties":{"place":{"type":"string","description":"City"}},"required":["place"],` +
				`"additionalProperties":false}`,
			`{"place":"Vilnius"}`,
		},
		"open object":    {`{"type":"object","additionalProperties":true}`, `{"anything":[1,{"b":2}]}`},
		"no properties":  {`{"type":"object"}`, `{"x":1}`},
		"no arguments":   {`{"type":"object","properties":{"x":{"type":"string"}}}`, `{}`},
		"openapi null":   {`{"type":"object","properties":{"a":{"type":"string","nullable":true}}}`, `{"a":null}`},
		"integral float": {`{"type":"object","properties":{"n":{"type":"integer"}}}`, `{"n":5.0}`},
		"format is not checked": {
			`{"type":"object","properties":{"d":{"type":"string","format":"date-time"}}}`, `{"d":"tomorrow"}`,
		},
		"nested values are not checked": {
			`{"type":"object","properties":{"f":{"type":"object","properties":{"n":{"type":"integer"}},` +
				`"required":["n"]}}}`,
			`{"f":{"n":"x"}}`,
		},
		"overlapping oneOf": {
			`{"type":"object","properties":{"f":{"oneOf":[{"type":"object","properties":{"a":{"type":"integer"}}},` +
				`{"type":"object","properties":{"b":{"type":"integer"}}}]}}}`,
			`{"f":{"a":1,"b":2}}`,
		},
		"remote ref is not checked": {
			`{"type":"object","properties":{"a":{"$ref":"https://schemas.example.com/a.json"}},"required":["a"]}`,
			`{}`,
		},
		"pattern that does not compile": {
			`{"type":"object","properties":{"a":{"type":"string","pattern":"(?<=x)y"}},"required":["a"]}`, `{}`,
		},
		"draft 4 exclusiveMinimum": {
			`{"type":"object","properties":{"n":{"type":"number","minimum":0,"exclusiveMinimum":true}},` +
				`"required":["n"]}`,
			`{}`,
		},
		"draft 7 root ref with a required sibling": {
			`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","$ref":"#/definitions/In",` +
				`"required":["legacy"],"definitions":{"In":{"type":"object","properties":{"a":{"type":"string"}}}}}`,
			`{}`,
		},
		"refs relative to a nested resource": {
			`{"type":"object","$defs":{"Value":{"type":"string"}},"properties":{"v":{"$id":"https://example.com/v",` +
				`"$defs":{"Value":{"type":"integer"}},"$ref":"#/$defs/Value"}}}`,
			`{"v":1}`,
		},
		"a property named id": {
			`{"type":"object","properties":{"id":{"type":"string"},"q":{"type":"string"}},"required":["q"]}`,
			`{"q":"x","id":"y"}`,
		},
		"pattern properties": {
			`{"type":"object","properties":{"a":{"type":"string"}},"patternProperties":{"^x-":{"type":"string"}}}`,
			`{"x-trace":"1"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recording{listed: []*mcp.Tool{tool("thing", nil)}}
			cs, rec, _ := stepsFixture(t, r,
				func() []*mcp.Tool { return []*mcp.Tool{schemaTool(t, "thing", c.schema, nil)} }, nil)

			res := rawCall(t, cs, server.ToolCallWrite, `{"name":"thing","arguments":`+c.args+`}`)

			require.False(t, res.IsError, text(res))
			assert.Equal(t, int32(1), r.calls.Load())
			assert.Equal(t, server.OutcomeOK, rec.last())
		})
	}
}

func TestCallWithInvalidParamsFromTheUpstreamReturnsTheSchema(t *testing.T) {
	t.Parallel()
	schema := `{"properties":{"id":{"type":"string"}},"type":"object"}`
	r := &recording{listed: []*mcp.Tool{tool("broken", nil)}}
	cs, _, _ := stepsFixture(t, r, func() []*mcp.Tool { return []*mcp.Tool{schemaTool(t, "broken", schema, nil)} },
		nil)

	res := rawCall(t, cs, server.ToolCallWrite, `{"name":"broken","arguments":{"id":"r1"}}`)

	require.True(t, res.IsError)
	assert.Contains(t, text(res), "the upstream returned an error: no such record")
	assert.Contains(t, text(res), "Input schema of broken: "+schema)
	assert.Equal(t, int32(1), r.calls.Load())
}

func TestCallOfATypoReturnsTheIntendedToolsSchema(t *testing.T) {
	t.Parallel()
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	r := &recording{listed: []*mcp.Tool{tool("records_list", ro), tool("users_list", ro)}}
	list := func() []*mcp.Tool {
		return []*mcp.Tool{schemaTool(t, "records_list", recordsSchema, ro), tool("users_list", ro)}
	}
	cs, _, _ := stepsFixture(t, r, list, nil)

	for _, name := range []string{"records_lst", "Records_List", "records-list"} {
		res := rawCall(t, cs, server.ToolCallRead, `{"name":"`+name+`"}`)

		require.True(t, res.IsError)
		assert.Contains(t, text(res), "Similar: ", name)
		assert.Contains(t, text(res), "Did you mean records_list?", name)
		assert.Contains(t, text(res), "Input schema of records_list: "+recordsSchema, name)
	}
	for _, name := range []string{"weather_get", "list", "records"} {
		res := rawCall(t, cs, server.ToolCallRead, `{"name":"`+name+`"}`)

		require.True(t, res.IsError)
		assert.NotContains(t, text(res), "Did you mean", name)
		assert.NotContains(t, text(res), "Input schema", name)
	}
	assert.Zero(t, r.calls.Load())
}

func TestCallReadsRunsEachReadAndReportsEachCall(t *testing.T) {
	t.Parallel()
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	r := &recording{listed: []*mcp.Tool{
		tool("records_get", ro), tool("records_count", ro), tool("records_delete", nil), tool("records_odd", ro),
	}}
	list := func() []*mcp.Tool {
		return []*mcp.Tool{
			schemaTool(t, "records_get", recordsSchema, ro), tool("records_count", ro), tool("records_delete", nil),
			tool("records_odd", ro), tool("records_odd", nil),
		}
	}
	cs, rec, logs := stepsFixture(t, r, list, &server.Options{WriteTools: []string{"records_count"}})

	res := rawCall(t, cs, server.ToolCallReads, `{"calls":[`+
		`{"name":"records_get","arguments":{"id":1234567890123456789}},`+
		`{"name":"records_delete","arguments":{"id":1}},`+
		`{"name":"records_nope"},`+
		`{"name":"records_get","arguments":{"id":"`+secret+`"}},`+
		`{"name":"records_count"},`+
		`{"name":"records_odd"},`+
		`{"name":"records_get","arguments":{"id":7}}]}`)

	require.False(t, res.IsError, "some calls succeeded: %s", text(res))
	out := text(res)
	for _, want := range []string{
		"[1/7] records_get: ok", "[2/7] records_delete: failed", "[3/7] records_nope: failed",
		"[4/7] records_get: failed", "[5/7] records_count: failed", "[6/7] records_odd: failed",
		"[7/7] records_get: ok", "may change things", "no tool \"records_nope\"", `"id" is a string`,
		"among the tools that may change things", "not all of them only read",
	} {
		assert.Contains(t, out, want)
	}
	assert.Less(t, strings.Index(out, "[1/7]"), strings.Index(out, "[7/7]"), "in order")
	assert.Equal(t, int32(2), r.calls.Load(), "only the valid reads reach the upstream")
	r.mu.Lock()
	args := slices.Clone(r.args)
	r.mu.Unlock()
	assert.Contains(t, args, `{"id":1234567890123456789}`, "arguments go as sent")
	assert.Equal(t, server.OutcomeOK, rec.last())
	assert.Contains(t, logs.String(), `"msg":"batch call failed"`)
	assert.NotContains(t, logs.String(), secret)
}

func TestCallReadsFailsOnlyWhenEveryCallFails(t *testing.T) {
	t.Parallel()
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	r := &recording{listed: []*mcp.Tool{tool("records_get", ro), tool("records_delete", nil)}}
	list := func() []*mcp.Tool {
		return []*mcp.Tool{schemaTool(t, "records_get", recordsSchema, ro), tool("records_delete", nil)}
	}
	cs, rec, _ := stepsFixture(t, r, list, nil)

	invalid := rawCall(t, cs, server.ToolCallReads,
		`{"calls":[{"name":"records_get","arguments":{}},{"name":"records_get","arguments":{"id":"x"}}]}`)
	assert.True(t, invalid.IsError)
	assert.Equal(t, server.OutcomeInvalidArguments, rec.last())

	mixed := rawCall(t, cs, server.ToolCallReads, `{"calls":[{"name":"records_get"},{"name":"records_delete"}]}`)
	assert.True(t, mixed.IsError)
	assert.Equal(t, server.OutcomeToolError, rec.last())
	assert.Zero(t, r.calls.Load())
}

func TestCallReadsReturnsStructuredContentInOrder(t *testing.T) {
	t.Parallel()
	f := proxy(t)
	cs := connect(t, f.url)

	res := call(t, cs, server.ToolCallReads, map[string]any{"calls": []any{
		map[string]any{"name": "weather_get_forecast", "arguments": map[string]any{"place": "Vilnius"}},
		map[string]any{"name": "weather_get_alerts"},
		map[string]any{"name": "weather_get_forecast", "arguments": map[string]any{"place": "Kaunas"}},
	}})

	out := structured[struct {
		Results []*struct {
			Forecast string `json:"forecast"`
		} `json:"results"`
	}](t, res)
	require.Len(t, out.Results, 3)
	assert.Contains(t, out.Results[0].Forecast, "Vilnius")
	assert.Contains(t, out.Results[2].Forecast, "Kaunas")
	assert.Equal(t, 2, f.gw.called("weather_get_forecast"))
	assert.Equal(t, 1, f.gw.called("weather_get_alerts"))
}

func TestCallReadsReadsItsInputStrictly(t *testing.T) {
	t.Parallel()
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	r := &recording{listed: []*mcp.Tool{tool("a_read", ro), tool("b_delete_all", nil)}}
	cs, _, _ := stepsFixture(t, r, nil, &server.Options{MaxBatchCalls: 3})
	one := `{"name":"a_read"}`

	for _, raw := range []string{
		`{}`,
		`[]`,
		`{"calls":[]}`,
		`{"calls":null}`,
		`{"calls":{"name":"a_read"}}`,
		`{"calls":"a_read"}`,
		`{"Calls":[` + one + `]}`,
		`{"calls":[` + one + `],"calls":[{"name":"b_delete_all"}]}`,
		`{"calls":[` + one + `],"name":"b_delete_all"}`,
		`{"name":"a_read"}`,
		`{"calls":[` + one + `,` + one + `,` + one + `,` + one + `]}`,
		`{"calls":[{"name":"a_read","NAME":"b_delete_all"}]}`,
		`{"calls":[{"name":"a_read","name":"b_delete_all"}]}`,
		`{"calls":[{"name":"a_read","arguments":{"x":1,"x":2}}]}`,
		`{"calls":[{"name":"a_read","arguments":[1]}]}`,
		`{"calls":[{"name":"a_read","extra":1}]}`,
		`{"calls":[1]}`,
	} {
		res := rawCall(t, cs, server.ToolCallReads, raw)
		assert.True(t, res.IsError, raw)
	}
	assert.Zero(t, r.calls.Load(), "nothing reaches the upstream")
}

// parallelCounter is a fake upstream whose tool "wait" takes a while, parallelCounter how
// many of its calls run at once.
type parallelCounter struct {
	active, peak atomic.Int32
	calls        atomic.Int32
	wait         time.Duration
}

func (c *parallelCounter) CallTool(ctx context.Context, _, _ string, _ json.RawMessage) (*mcp.CallToolResult, error) {
	c.calls.Add(1)
	n := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-time.After(c.wait):
	case <-ctx.Done():
		return nil, &upstream.Error{Class: upstream.ClassTimeout, Sent: true}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
}

// parallelProxy serves the proxy in front of c, whose one tool, "wait",
// only reads.
func parallelProxy(t *testing.T, c *parallelCounter, opts *server.Options) *mcp.ClientSession {
	t.Helper()
	list := catalog.Tools(func(context.Context, string) ([]*mcp.Tool, error) {
		return []*mcp.Tool{tool("wait", &mcp.ToolAnnotations{ReadOnlyHint: true})}, nil
	})
	ts := httptest.NewServer(server.Handler(server.New(opts, catalog.NewStore(list, nil), c), opts))
	t.Cleanup(ts.Close)
	return connect(t, ts.URL+"/mcp")
}

func batchOf(n int) string {
	calls := make([]string, n)
	for i := range calls {
		calls[i] = `{"name":"wait"}`
	}
	return `{"calls":[` + strings.Join(calls, ",") + `]}`
}

func TestCallReadsRunsCallsInParallelWithinALimit(t *testing.T) {
	t.Parallel()
	c := &parallelCounter{wait: 150 * time.Millisecond}
	cs := parallelProxy(t, c, nil)

	start := time.Now()
	res := rawCall(t, cs, server.ToolCallReads, batchOf(8))

	require.False(t, res.IsError, text(res))
	assert.Equal(t, int32(8), c.calls.Load())
	assert.Greater(t, c.peak.Load(), int32(1), "calls run in parallel")
	assert.LessOrEqual(t, c.peak.Load(), int32(4), "but no more than a few at once")
	assert.Less(t, time.Since(start), 8*c.wait)
}

func TestCallReadsEndsWithinTheCallTimeout(t *testing.T) {
	t.Parallel()
	c := &parallelCounter{wait: time.Minute}
	cs := parallelProxy(t, c, &server.Options{CallTimeout: 300 * time.Millisecond})

	start := time.Now()
	res := rawCall(t, cs, server.ToolCallReads, batchOf(6))

	assert.Less(t, time.Since(start), 5*time.Second)
	require.True(t, res.IsError)
	assert.Contains(t, text(res), "[6/6] wait: failed")
	assert.Contains(t, text(res), "ran out of time before this call started")
	assert.Contains(t, text(res), "no result arrived (timeout)")
	assert.Equal(t, int32(4), c.calls.Load(), "calls still waiting for a slot are not started")
}

func TestCallReadsLeavesOutResultsPastTheResponseLimit(t *testing.T) {
	t.Parallel()
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	r := &recording{listed: []*mcp.Tool{tool("big", ro)}}
	cs, _, _ := stepsFixture(t, r, nil, &server.Options{MaxResponseBytes: 10 << 10})

	res := rawCall(t, cs, server.ToolCallReads,
		`{"calls":[{"name":"big"},{"name":"big"},{"name":"big"},{"name":"big"}]}`)

	require.False(t, res.IsError, "the first results fit")
	assert.Contains(t, text(res), "this result is left out")
	assert.Less(t, len(text(res)), 10<<10)
	assert.Equal(t, int32(4), r.calls.Load())
}

func TestCallReadsKeepsTheResponseAliveBehindAProxyThatWaitsForHeaders(t *testing.T) {
	t.Parallel()
	g := newGate()
	g.slow = slowCall
	list := catalog.Tools(func(context.Context, string) ([]*mcp.Tool, error) {
		return []*mcp.Tool{tool("slow", &mcp.ToolAnnotations{ReadOnlyHint: true})}, nil
	})
	opts := &server.Options{KeepAlive: 100 * time.Millisecond}
	ts := httptest.NewServer(server.ProxyHandler(opts, catalog.NewStore(list, nil), g))
	t.Cleanup(ts.Close)
	cs := connect(t, impatientFront(t, ts.URL, frontWait).URL+"/mcp")

	res := rawCall(t, cs, server.ToolCallReads, `{"calls":[{"name":"slow"},{"name":"slow"}]}`)

	assert.False(t, res.IsError, text(res))
}

func TestCallReadsTakesAsManyCallsAsConfigured(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		max  int
		want any // maxItems, or nil for no tool
	}{{0, float64(server.DefaultMaxBatchCalls)}, {3, float64(3)}, {-1, nil}} {
		g := newGate()
		cs := connect(t, func() string {
			opts := &server.Options{MaxBatchCalls: c.max}
			ts := httptest.NewServer(server.Handler(newServer(t, opts, g), opts))
			t.Cleanup(ts.Close)
			return ts.URL + "/mcp"
		}())

		res, err := cs.ListTools(t.Context(), nil)

		require.NoError(t, err)
		var schema any
		for _, tl := range res.Tools {
			if tl.Name == server.ToolCallReads {
				schema = tl.InputSchema
			}
		}
		if c.want == nil {
			assert.Nil(t, schema, "0 leaves the tool out")
			assert.Len(t, res.Tools, 4)
			continue
		}
		raw, err := json.Marshal(schema)
		require.NoError(t, err)
		var s struct {
			Properties struct {
				Calls struct {
					MaxItems any `json:"maxItems"`
				} `json:"calls"`
			} `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(raw, &s))
		assert.Equal(t, c.want, s.Properties.Calls.MaxItems)
	}
}

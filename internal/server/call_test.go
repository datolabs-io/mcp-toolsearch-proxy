package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// recording is a fake upstream whose tools, listed in the order given,
// record the arguments of every call as they arrived. Tool "broken"
// answers with a JSON-RPC error, tool "hang" does not answer and tool
// "big" answers with 4 KiB of text.
type recording struct {
	listed  []*mcp.Tool
	release chan struct{}
	calls   atomic.Int32
	mu      sync.Mutex
	args    []string
}

func (r *recording) server() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "recording", Version: "0"}, nil)
	for _, tl := range r.listed {
		srv.AddTool(tl, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			r.calls.Add(1)
			r.mu.Lock()
			r.args = append(r.args, string(req.Params.Arguments))
			r.mu.Unlock()
			switch req.Params.Name {
			case "broken":
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "no such record"}
			case "hang":
				select {
				case <-ctx.Done():
				case <-r.release:
				}
			case "big":
				return &mcp.CallToolResult{Content: []mcp.Content{
					&mcp.TextContent{Text: strings.Repeat("x", 4<<10)},
				}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
		})
	}
	return srv
}

func (r *recording) lastArgs() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.args) == 0 {
		return ""
	}
	return r.args[len(r.args)-1]
}

// handler serves r. An mcp.Server keeps one tool per name, so tests of a
// repeated name give the catalog its listing directly (see callFixture).
func (r *recording) handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return r.server() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

func tool(name string, ann *mcp.ToolAnnotations) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: "Tool " + name + ".", InputSchema: map[string]any{"type": "object"},
		Annotations: ann}
}

// callFixture starts the proxy in front of r, listing tools as list gives
// them, and returns a client session and the proxy's log.
func callFixture(
	t *testing.T, r *recording, list func() []*mcp.Tool, opts *upstream.Options,
) (*mcp.ClientSession, *syncBuffer) {
	t.Helper()
	r.release = make(chan struct{})
	up := httptest.NewServer(r.handler())
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(r.release) })
	client := upstream.New(up.URL, opts)
	lister := listing(client)
	if list != nil {
		lister = catalog.Tools(func(context.Context, string) ([]*mcp.Tool, error) { return list(), nil })
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	serverOpts := &server.Options{Logger: logger}
	ts := httptest.NewServer(server.Handler(server.New(serverOpts, catalog.NewStore(lister, nil), client), serverOpts))
	t.Cleanup(ts.Close)
	return connect(t, ts.URL+"/mcp"), logs
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// rawCall calls tool with the JSON input raw, byte for byte.
func rawCall(t *testing.T, cs *mcp.ClientSession, tool, raw string) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: json.RawMessage(raw)})
	require.NoError(t, err)
	return res
}

func TestCallForwardsArgumentsByteForByte(t *testing.T) {
	t.Parallel()
	r := &recording{listed: []*mcp.Tool{tool("records_delete", nil)}}
	cs, _ := callFixture(t, r, nil, nil)

	res := rawCall(t, cs, server.ToolCallWrite,
		`{"name":"records_delete","arguments":{"id":1234567890123456789,"ratio":1.10,`+
			`"a":{"id":1},"b":[{"id":1},{"id":2}],"c":[]}}`)

	require.False(t, res.IsError, text(res))
	assert.JSONEq(t, `{"id":1234567890123456789,"ratio":1.10,"a":{"id":1},"b":[{"id":1},{"id":2}],"c":[]}`,
		r.lastArgs(), "a key may repeat across objects")
	assert.Contains(t, r.lastArgs(), "1234567890123456789")
	assert.Contains(t, r.lastArgs(), "1.10")
}

func TestCallWithoutArgumentsSendsNone(t *testing.T) {
	t.Parallel()
	r := &recording{listed: []*mcp.Tool{tool("records_list", &mcp.ToolAnnotations{ReadOnlyHint: true})}}
	cs, _ := callFixture(t, r, nil, nil)

	for _, raw := range []string{`{"name":"records_list"}`, `{"name":"records_list","arguments":null}`} {
		res := rawCall(t, cs, server.ToolCallRead, raw)
		require.False(t, res.IsError, text(res))
		assert.Contains(t, []string{"", "null", "{}"}, r.lastArgs(), raw)
	}
}

func TestCallRefusesAmbiguousInput(t *testing.T) {
	t.Parallel()
	r := &recording{listed: []*mcp.Tool{
		tool("a_read", &mcp.ToolAnnotations{ReadOnlyHint: true}), tool("b_delete_all", nil),
	}}
	cs, _ := callFixture(t, r, nil, nil)

	for _, raw := range []string{
		`{"name":"a_read","NAME":"b_delete_all"}`,
		`{"Name":"b_delete_all"}`,
		`{"name":"a_read","name":"b_delete_all"}`,
		`{"name":"a_read","arguments":{},"arguments":{"x":1}}`,
		`{"name":"a_read","extra":1}`,
		`{"name":"a_read","arguments":{"x":1,"x":2}}`,
		`{"name":"a_read","arguments":{"r":{"id":1,"id":2}}}`,
		`{"name":"a_read","arguments":{"r":[1,{"s":{"id":1,"id":2}}]}}`,
		`{"name":"a_read","arguments":[1]}`,
		`{"name":"a_read","arguments":"x"}`,
		`{"name":1}`,
		`["a_read"]`,
	} {
		for _, tl := range []string{server.ToolCallRead, server.ToolCallWrite} {
			res := rawCall(t, cs, tl, raw)
			assert.True(t, res.IsError, "%s %s", tl, raw)
		}
	}
	assert.Zero(t, r.calls.Load(), "nothing reaches the upstream")
}

func TestCallReadRefusesARepeatedName(t *testing.T) {
	t.Parallel()
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	for _, order := range [][]*mcp.Tool{
		{tool("dup", nil), tool("dup", ro)},
		{tool("dup", ro), tool("dup", nil)},
	} {
		r := &recording{listed: []*mcp.Tool{tool("dup", nil)}}
		cs, _ := callFixture(t, r, func() []*mcp.Tool { return order }, nil)

		res := rawCall(t, cs, server.ToolCallRead, `{"name":"dup"}`)

		assert.True(t, res.IsError, text(res))
		assert.Contains(t, text(res), server.ToolCallWrite)
		assert.Zero(t, r.calls.Load())
		out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{"query": "dup"}))
		require.Len(t, out.Tools, 1, "search shows the name once")
		assert.False(t, out.Tools[0].ReadOnly)
		assert.NotEmpty(t, out.Tools[0].Note)
	}
}

func TestCallReadCallsARepeatedNameWhenEveryCopyReads(t *testing.T) {
	t.Parallel()
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	r := &recording{listed: []*mcp.Tool{tool("dup", ro)}}
	cs, _ := callFixture(t, r, func() []*mcp.Tool { return []*mcp.Tool{tool("dup", ro), tool("dup", ro)} }, nil)

	res := rawCall(t, cs, server.ToolCallRead, `{"name":"dup"}`)

	assert.False(t, res.IsError, text(res))
	assert.Equal(t, int32(1), r.calls.Load())
}

func TestCallReadRefusesAReadOnlyToolMarkedDestructive(t *testing.T) {
	t.Parallel()
	r := &recording{listed: []*mcp.Tool{
		tool("odd", &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: ptr(true)}),
	}}
	cs, _ := callFixture(t, r, nil, nil)

	res := rawCall(t, cs, server.ToolCallRead, `{"name":"odd"}`)

	assert.True(t, res.IsError)
	assert.Zero(t, r.calls.Load())
	out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{"query": "odd"}))
	require.NotEmpty(t, out.Tools)
	assert.False(t, out.Tools[0].ReadOnly)
}

func ptr[T any](v T) *T { return &v }

func TestCallSaysTheUpstreamAnswered(t *testing.T) {
	t.Parallel()
	r := &recording{listed: []*mcp.Tool{tool("broken", nil)}}
	cs, _ := callFixture(t, r, nil, nil)

	res := rawCall(t, cs, server.ToolCallWrite, `{"name":"broken"}`)

	assert.True(t, res.IsError)
	assert.True(t, strings.HasPrefix(text(res), "the upstream returned an error: no such record"), text(res))
	assert.Contains(t, text(res), "Input schema of broken", "invalid params come with the schema")
}

func TestCallWriteWithoutAResultMayHaveRun(t *testing.T) {
	t.Parallel()
	r := &recording{listed: []*mcp.Tool{tool("hang", nil)}}
	cs, logs := callFixture(t, r, nil, &upstream.Options{CallTimeout: 200 * time.Millisecond})

	res := rawCall(t, cs, server.ToolCallWrite, `{"name":"hang","arguments":{"x":"`+secret+`"}}`)

	assert.True(t, res.IsError)
	assert.Contains(t, text(res), "no result arrived (timeout)")
	assert.Contains(t, text(res), "may still have run")
	assert.Contains(t, text(res), "read tool")
	assert.NotContains(t, text(res), "127.0.0.1")
	assert.Contains(t, logs.String(), `"class":"timeout"`)
	assert.NotContains(t, logs.String(), secret)
}

func TestCallToAnUnreachableUpstreamKeepsItsAddressOut(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	client := upstream.New(down.URL, &upstream.Options{ListRetries: -1})
	lister := catalog.Tools(func(context.Context, string) ([]*mcp.Tool, error) {
		return []*mcp.Tool{tool("records_delete", nil)}, nil
	})
	logs := &syncBuffer{}
	opts := &server.Options{Logger: slog.New(slog.NewJSONHandler(logs, nil))}
	ts := httptest.NewServer(server.Handler(server.New(opts, catalog.NewStore(lister, nil), client), opts))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")

	res := rawCall(t, cs, server.ToolCallWrite, `{"name":"records_delete"}`)

	assert.True(t, res.IsError)
	assert.Contains(t, text(res), "refused")
	assert.Contains(t, text(res), "was not called")
	host := strings.TrimPrefix(down.URL, "http://")
	assert.NotContains(t, text(res), host)
	assert.Contains(t, logs.String(), host, "the detail goes to the log")
}

func TestFailedCatalogKeepsTheUpstreamAddressOut(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	client := upstream.New(down.URL, &upstream.Options{ListRetries: -1})
	ts := httptest.NewServer(server.Handler(server.New(nil, catalog.NewStore(listing(client), nil), client), nil))
	t.Cleanup(ts.Close)
	cs := connect(t, ts.URL+"/mcp")
	host := strings.TrimPrefix(down.URL, "http://")

	for _, res := range []*mcp.CallToolResult{
		call(t, cs, server.ToolSearch, map[string]any{"query": "weather"}),
		call(t, cs, server.ToolDescribe, map[string]any{"names": []string{"x"}}),
		rawCall(t, cs, server.ToolCallWrite, `{"name":"x"}`),
	} {
		assert.True(t, res.IsError)
		assert.Contains(t, text(res), "could not be loaded")
		assert.Contains(t, text(res), "refused")
		assert.NotContains(t, text(res), host)
	}
}

func TestCallOverTheResponseLimitFailsClearly(t *testing.T) {
	t.Parallel()
	r := &recording{listed: []*mcp.Tool{tool("big", &mcp.ToolAnnotations{ReadOnlyHint: true})}}
	cs, logs := callFixture(t, r, nil, &upstream.Options{MaxResponseBytes: 2 << 10})

	write := rawCall(t, cs, server.ToolCallWrite, `{"name":"big"}`)
	read := rawCall(t, cs, server.ToolCallRead, `{"name":"big"}`)

	for _, res := range []*mcp.CallToolResult{write, read} {
		assert.True(t, res.IsError)
		assert.Contains(t, text(res), "larger than this proxy passes on")
		assert.NotContains(t, text(res), "127.0.0.1")
	}
	assert.Contains(t, text(write), "may still have run")
	assert.NotContains(t, text(read), "may still have run")
	assert.Contains(t, logs.String(), `"class":"too_large"`)
}

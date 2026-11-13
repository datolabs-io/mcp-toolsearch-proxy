package server_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

func boolPtr(v bool) *bool { return &v }

// playlistTools are a media server's tools, annotated in every way the
// spec allows, and not at all.
func playlistTools() []*mcp.Tool {
	schema := map[string]any{"type": "object"}
	return []*mcp.Tool{
		{Name: "playlist_list", Description: "List playlists.", InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}},
		{Name: "playlist_create", Description: "Create a playlist.", InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false)}},
		{Name: "playlist_add_item", Description: "Add an item to a playlist.", InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: true}},
		{Name: "playlist_delete", Description: "Delete a playlist.", InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), IdempotentHint: true}},
		{Name: "playlist_rename", Description: "Rename a playlist.", InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{Title: "Rename"}},
		{Name: "playlist_clear", Description: "Remove every item of a playlist.", InputSchema: schema},
		{Name: "playlist_shuffle", Description: "Shuffle a playlist.", InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(true)}},
	}
}

// recorder is an upstream that answers every call and counts them, and
// fails the ones named in fail as if the answer never came.
type recorder struct {
	mu    sync.Mutex
	calls map[string]int
	fail  map[string]bool
}

func (r *recorder) CallTool(_ context.Context, _, name string, _ json.RawMessage) (*mcp.CallToolResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[name]++
	if r.fail[name] {
		return nil, &upstream.Error{Class: upstream.ClassDropped, Sent: true}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
}

func (r *recorder) called(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

func playlistProxy(t *testing.T, opts *server.Options, rec *recorder) *mcp.ClientSession {
	t.Helper()
	list := func(context.Context, string) ([]*mcp.Tool, error) { return playlistTools(), nil }
	store := catalog.NewStore(catalog.Tools(list), nil)
	ts := httptest.NewServer(server.Handler(server.New(opts, store, rec), nil))
	t.Cleanup(ts.Close)
	return connect(t, ts.URL+"/mcp")
}

func TestEachToolIsCalledOnlyThroughTheCallToolForItsClass(t *testing.T) {
	t.Parallel()
	// Which call tools call each upstream tool: reads go through all three,
	// writes that only add through the write and destructive tools, and
	// anything that may delete or overwrite, or says nothing, through the
	// destructive tool alone.
	want := map[string][3]bool{
		"playlist_list":     {true, true, true},
		"playlist_create":   {false, true, true},
		"playlist_add_item": {false, true, true},
		"playlist_delete":   {false, false, true},
		"playlist_rename":   {false, false, true},
		"playlist_clear":    {false, false, true},
		"playlist_shuffle":  {false, false, true},
	}
	via := [3]string{server.ToolCallRead, server.ToolCallWrite, server.ToolCallDestructive}
	for name, allowed := range want {
		for i, tool := range via {
			rec := &recorder{}
			cs := playlistProxy(t, nil, rec)

			res := call(t, cs, tool, map[string]any{"name": name})

			if allowed[i] {
				assert.False(t, res.IsError, "%s through %s: %s", name, tool, text(res))
				assert.Equal(t, 1, rec.called(name), "%s through %s", name, tool)
				continue
			}
			assert.True(t, res.IsError, "%s through %s", name, tool)
			assert.Zero(t, rec.called(name), "%s through %s never runs", name, tool)
			if i == 0 && want[name][1] {
				assert.Contains(t, text(res), server.ToolCallWrite, name)
			} else {
				assert.Contains(t, text(res), server.ToolCallDestructive, name)
			}
		}
	}
}

func TestSearchShowsAllFourAnnotationsWithTheSpecDefaults(t *testing.T) {
	t.Parallel()
	cs := playlistProxy(t, nil, &recorder{})

	out := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{
		"query": "playlist", "limit": 10,
	}))

	got := map[string]server.Summary{}
	for _, s := range out.Tools {
		got[s.Name] = s
	}
	type hints struct{ readOnly, destructive, idempotent, openWorld bool }
	wantHints := map[string]hints{
		"playlist_list":     {true, false, true, false},
		"playlist_create":   {false, false, false, true},
		"playlist_add_item": {false, false, true, true},
		"playlist_delete":   {false, true, true, true},
		"playlist_rename":   {false, true, false, true},
		"playlist_clear":    {false, true, false, true},
		"playlist_shuffle":  {false, true, false, true},
	}
	require.Len(t, got, len(wantHints))
	for name, want := range wantHints {
		s := got[name]
		assert.Equal(t, want, hints{s.ReadOnly, s.Destructive, s.Idempotent, s.OpenWorld}, name)
	}
}

func TestDescribeFillsInTheSpecDefaults(t *testing.T) {
	t.Parallel()
	cs := playlistProxy(t, nil, &recorder{})

	out := structured[server.DescribeOut](t, call(t, cs, server.ToolDescribe, map[string]any{
		"names": []string{"playlist_clear", "playlist_create", "playlist_rename"},
	}))

	require.Len(t, out.Tools, 3)
	for _, d := range out.Tools {
		a := d.Annotations
		require.NotNil(t, a, d.Name)
		require.NotNil(t, a.DestructiveHint, d.Name)
		require.NotNil(t, a.OpenWorldHint, d.Name)
		assert.False(t, a.ReadOnlyHint, d.Name)
		assert.True(t, *a.OpenWorldHint, d.Name)
		assert.Equal(t, d.Name != "playlist_create", *a.DestructiveHint, d.Name)
	}
	assert.Equal(t, "Rename", out.Tools[2].Annotations.Title, "the upstream's own title is kept")
}

func TestListedWriteToolsAreDestructive(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	cs := playlistProxy(t, &server.Options{WriteTools: []string{"playlist_list", "playlist_add_*"}}, rec)

	found := structured[server.SearchOut](t, call(t, cs, server.ToolSearch, map[string]any{
		"query": "playlist", "limit": 10,
	}))
	asRead := call(t, cs, server.ToolCallRead, map[string]any{"name": "playlist_list"})
	asWrite := call(t, cs, server.ToolCallWrite, map[string]any{"name": "playlist_add_item"})
	asDestructive := call(t, cs, server.ToolCallDestructive, map[string]any{"name": "playlist_add_item"})

	for _, s := range found.Tools {
		if s.Name == "playlist_list" || s.Name == "playlist_add_item" {
			assert.False(t, s.ReadOnly, s.Name)
			assert.True(t, s.Destructive, s.Name)
			assert.False(t, s.Idempotent, s.Name, "the operator does not trust its annotations")
		}
	}
	assert.True(t, asRead.IsError)
	assert.Contains(t, text(asRead), server.ToolCallDestructive)
	assert.True(t, asWrite.IsError)
	assert.Contains(t, text(asWrite), server.ToolCallDestructive)
	assert.False(t, asDestructive.IsError, text(asDestructive))
	assert.Equal(t, 1, rec.called("playlist_add_item"))
}

func TestWriteAndDestructiveCallToolsAreAnnotatedApart(t *testing.T) {
	t.Parallel()
	cs := playlistProxy(t, nil, &recorder{})

	res, err := cs.ListTools(t.Context(), nil)

	require.NoError(t, err)
	byName := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl
	}
	write, destructive := byName[server.ToolCallWrite], byName[server.ToolCallDestructive]
	require.NotNil(t, write)
	require.NotNil(t, destructive)
	assert.False(t, write.Annotations.ReadOnlyHint)
	require.NotNil(t, write.Annotations.DestructiveHint)
	assert.False(t, *write.Annotations.DestructiveHint, "clients may let writes that only add run unasked")
	assert.False(t, destructive.Annotations.ReadOnlyHint)
	require.NotNil(t, destructive.Annotations.DestructiveHint)
	assert.True(t, *destructive.Annotations.DestructiveHint)
}

func TestALostResultSaysWhetherTheToolIsIdempotent(t *testing.T) {
	t.Parallel()
	rec := &recorder{fail: map[string]bool{"playlist_add_item": true, "playlist_create": true}}
	cs := playlistProxy(t, nil, rec)

	again := call(t, cs, server.ToolCallWrite, map[string]any{"name": "playlist_add_item"})
	once := call(t, cs, server.ToolCallWrite, map[string]any{"name": "playlist_create"})

	assert.True(t, again.IsError)
	assert.Contains(t, text(again), "may still have run")
	assert.Contains(t, text(again), "idempotent")
	assert.True(t, once.IsError)
	assert.Contains(t, text(once), "may still have run")
	assert.Contains(t, text(once), "not marked idempotent")
}

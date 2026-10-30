package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// baselineUser is the user message every count includes, so that what a
// count adds over the baseline is the tools and the system prompt alone.
const baselineUser = "Hello."

// counter counts the input tokens of a request with this system prompt,
// user message and tools; with toolSearch, the tools are deferred behind
// Anthropic's tool search.
type counter interface {
	Count(ctx context.Context, system, user string, tools []anthropic.ToolParam, toolSearch bool) (int64, error)
}

// apiCounter counts with the Messages API's count_tokens endpoint.
type apiCounter struct {
	client anthropic.Client
	model  string
}

func (c apiCounter) Count(
	ctx context.Context,
	system, user string,
	tools []anthropic.ToolParam,
	toolSearch bool,
) (int64, error) {
	params := anthropic.MessageCountTokensParams{
		Model:    c.model,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	}
	if system != "" {
		params.System = anthropic.MessageCountTokensParamsSystemUnion{OfString: anthropic.String(system)}
	}
	for _, tp := range tools {
		if toolSearch {
			tp.DeferLoading = anthropic.Bool(true)
		}
		params.Tools = append(params.Tools, anthropic.MessageCountTokensToolUnionParam{OfTool: &tp})
	}
	if toolSearch {
		params.Tools = append(params.Tools, anthropic.MessageCountTokensToolParamOfToolSearchToolBm25_20251119(
			anthropic.ToolSearchToolBm25_20251119TypeToolSearchToolBm25_20251119))
	}
	res, err := c.client.Messages.CountTokens(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("count tokens: %w", err)
	}
	return res.InputTokens, nil
}

// toolParam is the Anthropic tool a client sends for an MCP tool.
func toolParam(t *mcp.Tool) (anthropic.ToolParam, error) {
	raw, err := json.Marshal(t.InputSchema)
	if err != nil {
		return anthropic.ToolParam{}, fmt.Errorf("schema of %s: %w", t.Name, err)
	}
	var s map[string]any
	if err = json.Unmarshal(raw, &s); err != nil {
		return anthropic.ToolParam{}, fmt.Errorf("schema of %s: %w", t.Name, err)
	}
	in := anthropic.ToolInputSchemaParam{Properties: s["properties"], ExtraFields: map[string]any{}}
	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			if name, isName := r.(string); isName {
				in.Required = append(in.Required, name)
			}
		}
	}
	for k, v := range s {
		if k != schemaType && k != "properties" && k != "required" {
			in.ExtraFields[k] = v
		}
	}
	tp := anthropic.ToolParam{Name: t.Name, InputSchema: in}
	if t.Description != "" {
		tp.Description = anthropic.String(t.Description)
	}
	return tp, nil
}

// listing is what a client loads from an MCP server: its tools, as
// Anthropic tools, and its instructions.
type listing struct {
	tools        []anthropic.ToolParam
	instructions string
}

func list(ctx context.Context, cs *mcp.ClientSession) (listing, error) {
	l := listing{instructions: cs.InitializeResult().Instructions}
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return listing{}, fmt.Errorf("list tools: %w", err)
		}
		tp, terr := toolParam(t)
		if terr != nil {
			return listing{}, terr
		}
		l.tools = append(l.tools, tp)
	}
	return l, nil
}

// size is what something costs a client: bytes, and tokens when counted
// (-1 when not).
type size struct {
	Bytes  int   `json:"bytes"`
	Tokens int64 `json:"tokens"`
}

// contextRow is what each way costs before the model does anything, for a
// catalog of Tools: the upstream's own listing, the same deferred behind
// Anthropic's tool search, the proxy's, and one search_tools result on
// average.
type contextRow struct {
	Tools      int  `json:"tools"`
	Direct     size `json:"direct"`
	ToolSearch size `json:"tool_search"`
	Proxy      size `json:"proxy"`
	Search     size `json:"search"`
}

// contextSetup is what measureContext needs besides the sizes: the proxy
// binary and its flags, the evaluation set and the distractor domains.
type contextSetup struct {
	bin     string
	flags   []string
	core    []Tool
	domains []domain
	tasks   []Task
}

// measureContext measures a contextRow per catalog size. Without a counter
// it measures bytes only.
func measureContext(ctx context.Context, s contextSetup, sizes []int, c counter) ([]contextRow, error) {
	rows := make([]contextRow, 0, len(sizes))
	for _, n := range sizes {
		row, err := measureSize(ctx, s, n, c)
		if err != nil {
			return nil, fmt.Errorf("%d tools: %w", n, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func measureSize(ctx context.Context, s contextSetup, n int, c counter) (contextRow, error) {
	tools, err := catalog(s.core, s.domains, n)
	if err != nil {
		return contextRow{}, err
	}
	up, err := startUpstream(ctx, tools)
	if err != nil {
		return contextRow{}, err
	}
	defer up.Close()
	p, err := startProxy(ctx, s.bin, up.URL, s.flags...)
	if err != nil {
		return contextRow{}, err
	}
	defer p.Close()

	ds, err := connect(ctx, up.URL, "context")
	if err != nil {
		return contextRow{}, err
	}
	defer func() { _ = ds.Close() }()
	ps, err := connect(ctx, p.URL, "context")
	if err != nil {
		return contextRow{}, err
	}
	defer func() { _ = ps.Close() }()
	direct, err := list(ctx, ds)
	if err != nil {
		return contextRow{}, err
	}
	proxy, err := list(ctx, ps)
	if err != nil {
		return contextRow{}, err
	}

	m := meter{c: c}
	row := contextRow{Tools: n}
	if row.Direct, err = m.measure(ctx, direct.instructions, direct.tools, false); err != nil {
		return contextRow{}, err
	}
	if row.ToolSearch, err = m.measure(ctx, direct.instructions, direct.tools, true); err != nil {
		return contextRow{}, err
	}
	if row.Proxy, err = m.measure(ctx, proxy.instructions, proxy.tools, false); err != nil {
		return contextRow{}, err
	}
	row.Search, err = m.searches(ctx, ps, s.tasks)
	return row, err
}

// meter measures sizes over the baseline request, counted once.
type meter struct {
	c        counter
	baseline int64
	counted  bool
}

// over is the tokens of a request with system, user and tools, less the
// baseline request's.
func (m *meter) over(
	ctx context.Context,
	system, user string,
	tools []anthropic.ToolParam,
	toolSearch bool,
) (int64, error) {
	if !m.counted {
		n, err := m.c.Count(ctx, "", baselineUser, nil, false)
		if err != nil {
			return 0, err
		}
		m.baseline, m.counted = n, true
	}
	n, err := m.c.Count(ctx, system, user, tools, toolSearch)
	if err != nil {
		return 0, err
	}
	return n - m.baseline, nil
}

func (m *meter) measure(
	ctx context.Context,
	system string,
	tools []anthropic.ToolParam,
	toolSearch bool,
) (size, error) {
	raw, err := json.Marshal(tools)
	if err != nil {
		return size{}, fmt.Errorf("encode tools: %w", err)
	}
	sz := size{Bytes: len(system) + len(raw), Tokens: -1}
	if toolSearch {
		// What is sent is the same; what the model is given is not.
		sz.Bytes = -1
	}
	if m.c == nil {
		return sz, nil
	}
	if sz.Tokens, err = m.over(ctx, system, baselineUser, tools, toolSearch); err != nil {
		return size{}, err
	}
	return sz, nil
}

// searches is the average size of a search_tools result over the tasks'
// questions, as the model reads it: the result's text.
func (m *meter) searches(ctx context.Context, ps *mcp.ClientSession, tasks []Task) (size, error) {
	var total size
	for _, task := range tasks {
		res, err := ps.CallTool(
			ctx,
			&mcp.CallToolParams{Name: "search_tools", Arguments: map[string]any{argQuery: task.Query}},
		)
		if err != nil {
			return size{}, fmt.Errorf("search: %w", err)
		}
		text := resultText(res)
		total.Bytes += len(text)
		if m.c != nil {
			n, oerr := m.over(ctx, "", baselineUser+"\n\n"+text, nil, false)
			if oerr != nil {
				return size{}, oerr
			}
			total.Tokens += n
		}
	}
	avg := size{Bytes: total.Bytes / max(1, len(tasks)), Tokens: -1}
	if m.c != nil {
		avg.Tokens = total.Tokens / int64(max(1, len(tasks)))
	}
	return avg, nil
}

// resultText is the text of a tool result, as a client hands it to the
// model.
func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

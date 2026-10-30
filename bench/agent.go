package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mode is how the model is given the catalog.
type mode string

const (
	modeAll        mode = "all"        // every tool loaded
	modeToolSearch mode = "toolsearch" // every tool deferred behind Anthropic's BM25 tool search
	modeProxy      mode = "proxy"      // the proxy's tools, in front of the catalog
)

// agentSystem is the system prompt of every run, the same in every mode
// but for the proxy's guidance, which a client adds after it.
const agentSystem = "You are an assistant that acts through the tools you have. Do what the user asks with them. " +
	"Do not ask the user anything: assume reasonable values for whatever they left out."

const (
	// maxTokens bounds each response; the runs end at a tool call well before.
	maxTokens = 16000
	// perMillion is what prices are per.
	perMillion = 1e6
	// stopError is the stop of a run that an error ended.
	stopError = "error"
)

// prices are dollars per million tokens.
type prices struct {
	In, Out, CacheRead, CacheWrite float64
}

// agentSetup is the model a run talks to, and how.
type agentSetup struct {
	client   anthropic.Client
	model    string
	effort   anthropic.OutputConfigEffort
	maxTurns int
	prices   prices
}

// usage is the tokens a run took, summed over its turns.
type usage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
}

// runResult is one run of one task in one mode.
type runResult struct {
	Mode     mode     `json:"mode"`
	Tools    int      `json:"tools"`
	Lang     string   `json:"lang"`
	Query    string   `json:"query"`
	Want     string   `json:"want"`
	Repeat   int      `json:"repeat"`
	Success  bool     `json:"success"`
	Turns    int      `json:"turns"`
	Calls    []string `json:"calls"`    // the upstream tools called, in order
	Used     []string `json:"used"`     // the tools the model called, in order: its own and server tools, as server:<name>
	Searches []string `json:"searches"` // what the model searched for, with search_tools or a server tool search
	Usage    usage    `json:"usage"`
	Cost     float64  `json:"cost_usd"`
	Seconds  float64  `json:"seconds"`
	Stop     string   `json:"stop"` // why the run ended: a stop reason, max_turns or error
	Err      string   `json:"error,omitempty"`
}

// env is a catalog served by an upstream, with the proxy in front of it,
// and what a client lists from each.
type env struct {
	tools   int
	up      *upstream
	proxy   *proxyProc
	direct  listing
	proxied listing

	mu       sync.Mutex
	sessions map[string]*mcp.ClientSession
}

// startEnv serves tools, starts the proxy binary in front of them with
// flags, and lists both.
func startEnv(ctx context.Context, tools []Tool, bin string, flags ...string) (*env, error) {
	up, err := startUpstream(ctx, tools)
	if err != nil {
		return nil, err
	}
	e := &env{tools: len(tools), up: up, sessions: map[string]*mcp.ClientSession{}}
	if e.proxy, err = startProxy(ctx, bin, up.URL, flags...); err != nil {
		e.Close()
		return nil, err
	}
	for target, l := range map[string]*listing{up.URL: &e.direct, e.proxy.URL: &e.proxied} {
		var cs *mcp.ClientSession
		if cs, err = e.session(ctx, target, "lister"); err != nil {
			e.Close()
			return nil, err
		}
		if *l, err = list(ctx, cs); err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}

// Close closes the sessions and stops the proxy and the upstream.
func (e *env) Close() {
	for _, cs := range e.sessions {
		_ = cs.Close()
	}
	if e.proxy != nil {
		e.proxy.Close()
	}
	e.up.Close()
}

// session is the MCP session to url of the caller with this token, opened
// once.
func (e *env) session(ctx context.Context, url, tok string) (*mcp.ClientSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := url + " " + tok
	if cs, ok := e.sessions[key]; ok {
		return cs, nil
	}
	cs, err := connect(ctx, url, tok)
	if err != nil {
		return nil, err
	}
	e.sessions[key] = cs
	return cs, nil
}

// run asks the model task's question in mode, as the caller with this
// token, and runs its tool calls until it calls the wanted tool, ends its
// turn, or reaches the turn limit.
func (a agentSetup) run(ctx context.Context, m mode, e *env, task Task, tok string) runResult {
	start := time.Now()
	res := a.turns(ctx, m, e, task, tok)
	res.Seconds = time.Since(start).Seconds()
	return res
}

// turns is run without its time.
func (a agentSetup) turns(ctx context.Context, m mode, e *env, task Task, tok string) runResult {
	res := runResult{Mode: m, Tools: e.tools, Lang: task.Lang, Query: task.Query, Want: task.Want}

	e.up.Reset(tok)
	target, system, tools := e.up.URL, agentSystem, e.direct.tools
	if m == modeProxy {
		target, tools = e.proxy.URL, e.proxied.tools
		if e.proxied.instructions != "" {
			system += "\n\n" + e.proxied.instructions
		}
	}
	cs, err := e.session(ctx, target, tok)
	if err != nil {
		res.Stop, res.Err = stopError, err.Error()
		return res
	}
	params := anthropic.MessageNewParams{
		Model:        a.model,
		MaxTokens:    maxTokens,
		System:       []anthropic.TextBlockParam{{Text: system}},
		Tools:        toolUnion(tools, m == modeToolSearch),
		OutputConfig: anthropic.OutputConfigParam{Effort: a.effort},
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(task.Query))},
	}

	for res.Turns < a.maxTurns {
		var msg *anthropic.Message
		if msg, err = a.client.Messages.New(ctx, params); err != nil {
			res.Stop, res.Err = stopError, err.Error()
			return res
		}
		res.Turns++
		a.add(&res, msg.Usage)
		params.Messages = append(params.Messages, msg.ToParam())
		names, queries := used(msg)
		res.Used, res.Searches = append(res.Used, names...), append(res.Searches, queries...)

		if msg.StopReason == anthropic.StopReasonPauseTurn {
			continue // a server tool, such as the tool search, paused the turn
		}
		if msg.StopReason != anthropic.StopReasonToolUse {
			res.Stop = string(msg.StopReason)
			return res
		}
		results := a.callTools(ctx, cs, msg)
		res.Calls = e.up.Calls(tok)
		if slices.Contains(res.Calls, task.Want) {
			res.Success, res.Stop = true, "wanted_call"
			return res
		}
		params.Messages = append(params.Messages, anthropic.NewUserMessage(results...))
	}
	res.Stop = "max_turns"
	return res
}

// callTools runs every tool call in msg and returns their results.
func (a agentSetup) callTools(
	ctx context.Context,
	cs *mcp.ClientSession,
	msg *anthropic.Message,
) []anthropic.ContentBlockParamUnion {
	var results []anthropic.ContentBlockParamUnion
	for _, block := range msg.Content {
		use, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal(use.Input, &args); err != nil {
			results = append(results, anthropic.NewToolResultBlock(use.ID, "arguments are not a JSON object", true))
			continue
		}
		out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: use.Name, Arguments: args})
		if err != nil {
			results = append(results, anthropic.NewToolResultBlock(use.ID, err.Error(), true))
			continue
		}
		results = append(results, anthropic.NewToolResultBlock(use.ID, resultText(out), out.IsError))
	}
	return results
}

// add adds what a turn took to res.
func (a agentSetup) add(res *runResult, u anthropic.Usage) {
	res.Usage.Input += u.InputTokens
	res.Usage.CacheRead += u.CacheReadInputTokens
	res.Usage.CacheWrite += u.CacheCreationInputTokens
	res.Usage.Output += u.OutputTokens
	p := a.prices
	res.Cost += (float64(u.InputTokens)*p.In + float64(u.CacheReadInputTokens)*p.CacheRead +
		float64(u.CacheCreationInputTokens)*p.CacheWrite + float64(u.OutputTokens)*p.Out) / perMillion
}

// toolUnion is tools for a request; deferred behind the BM25 tool search
// when toolSearch.
func toolUnion(tools []anthropic.ToolParam, toolSearch bool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools)+1)
	for _, tp := range tools {
		if toolSearch {
			tp.DeferLoading = anthropic.Bool(true)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	if toolSearch {
		out = append(
			out,
			anthropic.ToolUnionParam{OfToolSearchToolBm25_20251119: &anthropic.ToolSearchToolBm25_20251119Param{
				Type: anthropic.ToolSearchToolBm25_20251119TypeToolSearchToolBm25_20251119,
			}},
		)
	}
	return out
}

// String is the mode's name.
func (m mode) String() string { return string(m) }

// parseMode is the mode named s.
func parseMode(s string) (mode, error) {
	for _, m := range []mode{modeAll, modeToolSearch, modeProxy} {
		if string(m) == s {
			return m, nil
		}
	}
	return "", fmt.Errorf("unknown mode %q: all, toolsearch or proxy", s)
}

// runAll runs every task in every mode repeats times, parallel at once,
// each worker as a caller of its own, and hands each result to done as it
// comes, one at a time.
func runAll(
	ctx context.Context,
	a agentSetup,
	e *env,
	ms []mode,
	tasks []Task,
	repeats, parallel int,
	done func(runResult),
) []runResult {
	type job struct {
		m      mode
		task   Task
		repeat int
	}
	jobs := make(chan job)
	go func() {
		defer close(jobs)
		for r := range repeats {
			for _, task := range tasks {
				for _, m := range ms {
					select {
					case jobs <- job{m, task, r}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	var mu sync.Mutex
	var results []runResult
	var wg sync.WaitGroup
	for w := range max(1, parallel) {
		wg.Go(func() {
			tok := fmt.Sprintf("worker-%d", w)
			for j := range jobs {
				r := a.run(ctx, j.m, e, j.task, tok)
				r.Repeat = j.repeat
				mu.Lock()
				results = append(results, r)
				done(r)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return results
}

// used is the tools msg calls, the client's by name and Anthropic's own as
// server:<name>, and the queries of its searches, several in one joined
// with " | ".
func used(msg *anthropic.Message) ([]string, []string) {
	var names, queries []string
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.ToolUseBlock:
			names = append(names, b.Name)
			var in struct {
				Query   string   `json:"query"`
				Queries []string `json:"queries"`
			}
			if b.Name == "search_tools" && json.Unmarshal(b.Input, &in) == nil {
				if len(in.Queries) > 0 {
					in.Query = strings.Join(in.Queries, " | ")
				}
				queries = append(queries, in.Query)
			}
		case anthropic.ServerToolUseBlock:
			names = append(names, "server:"+string(b.Name))
			if in, ok := b.Input.(map[string]any); ok {
				if q, isText := in[argQuery].(string); isText {
					queries = append(queries, q)
				}
			}
		}
	}
	return names, queries
}

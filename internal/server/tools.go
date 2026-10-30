package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// Tool names.
const (
	ToolSearch    = "search_tools"
	ToolDescribe  = "describe_tool"
	ToolCallRead  = "call_read_tool"
	ToolCallReads = "call_read_tools"
	ToolCallWrite = "call_write_tool"
)

// Search limits.
const (
	defaultLimit = 8
	maxLimit     = 25
	maxDescribe  = 10
	// maxQueries bounds the queries of one search.
	maxQueries = 5
	// summaryRunes bounds the description search_tools shows of a tool.
	summaryRunes = 200
	// suggestions is how many tools an unknown name suggests instead.
	suggestions = 3
	// maxQueryRunes bounds a query: lexical search costs the catalog's
	// tools times the query's words.
	maxQueryRunes = 1000
	// maxNameBytes bounds a tool name asked for, which is looked up and,
	// when unknown, searched for.
	maxNameBytes = 256
)

// errLongQuery and errLongName refuse a query or name over the limits.
var (
	errLongQuery = fmt.Errorf("query is too long: at most %d characters; describe the task in fewer words",
		maxQueryRunes)
	errLongName = fmt.Errorf("a tool name is too long: at most %d bytes; give a name search_tools returned",
		maxNameBytes)
)

// methodCallTool is the MCP method that calls a tool.
const methodCallTool = "tools/call"

// SearchIn is the input of search_tools: a query, or several.
type SearchIn struct {
	Query   string   `json:"query,omitempty"`
	Queries []string `json:"queries,omitempty"`
	Limit   int      `json:"limit,omitempty"`
	Schemas *bool    `json:"schemas,omitempty"`
}

// Summary is a tool as search_tools shows it.
type Summary struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// ReadOnly says the tool only reads, so call_read_tool can call it;
	// call_write_tool calls any other.
	ReadOnly bool `json:"readOnly" jsonschema:"True when the tool only reads: call it with call_read_tool."`
	// Note warns of a name the upstream lists more than once.
	Note string `json:"note,omitempty" jsonschema:"A warning about this tool, when there is one."`
	// InputSchema is the tool's input schema, for the best matches only.
	InputSchema any `json:"inputSchema,omitempty"`
}

// SearchOut is the output of search_tools: the matches of query in Tools,
// or those of each of queries in Results.
type SearchOut struct {
	// Tools is left out only when nil, with queries: a query that matches
	// nothing still gets an empty list, as it always has.
	Tools   []Summary      `json:"tools,omitzero"    jsonschema:"The best matches of query, best first."`
	Results []SearchResult `json:"results,omitempty" jsonschema:"With queries, the best matches of each, in the order asked."`
	// Total is the number of tools the catalog holds.
	Total int `json:"total" jsonschema:"How many tools the catalog holds in all."`
}

// SearchResult is the matches of one of a search's queries.
type SearchResult struct {
	Query string    `json:"query"`
	Tools []Summary `json:"tools" jsonschema:"The best matches, best first."`
}

// DescribeIn is the input of describe_tool.
type DescribeIn struct {
	Names []string `json:"names" jsonschema:"Names of tools, as search_tools gave them; at most 10."`
}

// Detail is a tool as describe_tool shows it: everything its server lists.
type Detail struct {
	Name         string               `json:"name"`
	Title        string               `json:"title,omitempty"`
	Description  string               `json:"description,omitempty"`
	InputSchema  any                  `json:"inputSchema,omitempty"`
	OutputSchema any                  `json:"outputSchema,omitempty"`
	Annotations  *mcp.ToolAnnotations `json:"annotations,omitempty"`
	// Note warns of a name the upstream lists more than once, whose
	// annotations may not be those of the tool a call reaches.
	Note string `json:"note,omitempty"`
}

// DescribeOut is the output of describe_tool.
type DescribeOut struct {
	Tools   []Detail `json:"tools"`
	Unknown []string `json:"unknown,omitempty" jsonschema:"Names no tool of the catalog has."`
	// Similar holds, for each unknown name, the tools whose names come
	// closest.
	Similar map[string][]string `json:"similar,omitempty" jsonschema:"For each unknown name, the tools closest to it."`
}

// CallIn is the input of call_read_tool and call_write_tool.
type CallIn struct {
	Name      string          `json:"name"                jsonschema:"The tool's name, as search_tools gave it."`
	Arguments json.RawMessage `json:"arguments,omitempty" jsonschema:"The tool's arguments, as its input schema describes them."`
}

// CallsIn is the input of call_read_tools.
type CallsIn struct {
	Calls []CallIn `json:"calls"`
}

type tools struct {
	store    *catalog.Store
	caller   Caller
	onSearch func(mode string)
	schemas  int      // input schemas returned with the best matches
	writes   []string // names or patterns of tools always treated as writes
	logger   *slog.Logger
	batch    batchLimits
}

func (t *tools) register(srv *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	mcp.AddTool(srv, &mcp.Tool{
		Name:  ToolSearch,
		Title: "Search tools",
		Description: "Find the tools that can do what you describe, among all the tools you may use. " +
			"Returns names and short descriptions, best match first, with the input schemas of the best few; " +
			"read the input of any other with describe_tool. Give queries instead of query to find several " +
			"things at once, such as a tool for the task and one for a value the user did not give.",
		InputSchema:  searchInSchema(),
		OutputSchema: searchOutSchema(),
		Annotations:  readOnly,
	}, t.search)
	mcp.AddTool(srv, &mcp.Tool{
		Name:         ToolDescribe,
		Title:        "Describe tools",
		Description:  "Return the full description and input schema of tools found with search_tools.",
		OutputSchema: describeOutSchema(),
		Annotations:  readOnly,
	}, t.describe)
	// Two call tools, so that a client can let reads run freely and still
	// ask before anything else: calls of tools marked read-only, and calls
	// of all others.
	srv.AddTool(&mcp.Tool{
		Name:  ToolCallRead,
		Title: "Call a read-only tool",
		Description: "Call a tool that only reads (readOnly in search_tools results), with arguments matching " +
			"the input schema from search_tools or describe_tool. Returns the tool's own result, or, when the " +
			"arguments do not fit, what is wrong and the schema. Tools that may change anything are refused: call " +
			"those with call_write_tool.",
		InputSchema: callInSchema(),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return t.call(ctx, req, true)
	})
	if t.batch.calls > 0 {
		srv.AddTool(&mcp.Tool{
			Name:  ToolCallReads,
			Title: "Call several read-only tools",
			Description: fmt.Sprintf("Call up to %d tools that only read (readOnly in search_tools results) at "+
				"once, in parallel: each entry of calls is a name and arguments, as call_read_tool takes them. "+
				"Returns every call's result in order, each labelled with its number, its tool and whether it "+
				"failed; one that fails does not stop the others. Tools that may change anything are refused.",
				t.batch.calls),
			InputSchema: callsInSchema(t.batch.calls),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
		}, t.callReads)
	}
	// Writes are called one at a time, on purpose: each keeps its own
	// approval in the client, which a batch would fold into one.
	srv.AddTool(&mcp.Tool{
		Name:  ToolCallWrite,
		Title: "Call a tool that may change things",
		Description: "Call a tool that may create, change or delete something (not readOnly in search_tools " +
			"results), with arguments matching the input schema from search_tools or describe_tool. Returns the " +
			"tool's own result, or, when the arguments do not fit, what is wrong and the schema. Prefer " +
			"call_read_tool for tools that only read.",
		InputSchema: callInSchema(),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return t.call(ctx, req, false)
	})
}

// callInSchema is the input schema of the call tools. The arguments are
// passed on as they come, but they are an object: json.RawMessage alone
// would describe any JSON value.
func callInSchema() *jsonschema.Schema {
	in, err := jsonschema.For[CallIn](nil)
	if err != nil {
		panic(fmt.Sprintf("server: call schema: %v", err))
	}
	in.Properties["arguments"] = &jsonschema.Schema{
		Type: "object", Description: in.Properties["arguments"].Description,
	}
	return in
}

// callsInSchema is the input schema of call_read_tools, taking at most
// limit calls: an object whose calls are each the input of call_read_tool.
func callsInSchema(limit int) *jsonschema.Schema {
	entry := callInSchema()
	return &jsonschema.Schema{
		Type: typeObject, Required: []string{keyCalls}, AdditionalProperties: entry.AdditionalProperties,
		Properties: map[string]*jsonschema.Schema{keyCalls: {
			Type:        typeArray,
			Description: fmt.Sprintf("The calls, 1 to %d, each a tool's name and its arguments.", limit),
			Items:       entry, MinItems: ptr(1), MaxItems: ptr(limit),
		}},
	}
}

// readOnly reports whether the upstream server marks every one of copies,
// the tools it lists under one name, as one that only reads. A tool without
// annotations may change anything, and one marked both read-only and
// destructive contradicts itself, so it counts as one that may change
// things.
func readOnly(copies []*mcp.Tool) bool {
	for _, tl := range copies {
		a := tl.Annotations
		if a == nil || !a.ReadOnlyHint || (a.DestructiveHint != nil && *a.DestructiveHint) {
			return false
		}
	}
	return len(copies) > 0
}

// listedWrite reports whether the operator lists the tool called name among
// the writes (Options.WriteTools).
func (t *tools) listedWrite(name string) bool {
	for _, pattern := range t.writes {
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

// reads reports whether call_read_tool may call copies, the tools listed
// under one name: the upstream marks them all as ones that only read, and
// the operator does not list the name among the writes.
func (t *tools) reads(copies []*mcp.Tool) bool {
	return readOnly(copies) && !t.listedWrite(copies[0].Name)
}

// annotations returns tl's annotations, without the read-only hint when
// the operator lists tl among the writes.
func (t *tools) annotations(tl *mcp.Tool) *mcp.ToolAnnotations {
	if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint || !t.listedWrite(tl.Name) {
		return tl.Annotations
	}
	a := *tl.Annotations
	a.ReadOnlyHint = false
	return &a
}

// ambiguousNote warns of a name the upstream lists n times.
func ambiguousNote(n int) string {
	if n <= 1 {
		return ""
	}
	return fmt.Sprintf("The upstream lists %d tools under this name and decides which one a call reaches; "+
		"it counts as read-only only if all of them are.", n)
}

func ptr[T any](v T) *T { return &v }

// searchInSchema is the input schema of search_tools, which takes query
// or queries, not both.
func searchInSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: typeObject, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
		Properties: map[string]*jsonschema.Schema{
			"query": {Type: typeString, Description: "What you want to do, in your own words and in any language."},
			"queries": {
				Type:     typeArray,
				Items:    &jsonschema.Schema{Type: typeString},
				MinItems: ptr(1),
				MaxItems: ptr(maxQueries),
				Description: fmt.Sprintf("Instead of query, 1 to %d things to find at once, each with results of "+
					"its own.", maxQueries),
			},
			"limit": {
				Type: typeInteger,
				Description: fmt.Sprintf(
					"How many tools to return for each query, 1 to %d; %d when omitted.",
					maxLimit,
					defaultLimit,
				),
			},
			"schemas": {
				// Null, as for any optional argument, means the default.
				Types: []string{typeNull, typeBoolean},
				Description: "Include the input schemas of the best matches, so a call can follow without " +
					"describe_tool; true when omitted.",
			},
		},
	}
}

// searchOutSchema is the output schema of search_tools, with the input
// schema of a match described as describeOutSchema does.
func searchOutSchema() *jsonschema.Schema {
	s, err := jsonschema.For[SearchOut](nil)
	if err != nil {
		panic(fmt.Sprintf("server: search_tools schema: %v", err))
	}
	// Each place gets a schema of its own: a schema is a tree.
	matchSchema := func() *jsonschema.Schema {
		return &jsonschema.Schema{
			Description: "The tool's input schema as its server lists it, for the best matches only, once in a search.",
		}
	}
	s.Properties["tools"].Items.Properties["inputSchema"] = matchSchema()
	s.Properties["results"].Items.Properties["tools"].Items.Properties["inputSchema"] = matchSchema()
	return s
}

// describeOutSchema is the output schema of describe_tool. The schemas it
// returns are any JSON, which the schema inferred from DescribeOut writes as
// the boolean schema `true`; some clients reject a server's whole tool list
// over a boolean schema, so they are described by a schema with nothing but
// a description, which accepts any value just as well.
func describeOutSchema() *jsonschema.Schema {
	s, err := jsonschema.For[DescribeOut](nil)
	if err != nil {
		panic(fmt.Sprintf("server: describe_tool schema: %v", err))
	}
	detail := s.Properties["tools"].Items
	for _, name := range []string{"inputSchema", "outputSchema"} {
		// A schema with only a description accepts any value, as `true`
		// does, including an upstream tool's own boolean output schema.
		detail.Properties[name] = &jsonschema.Schema{
			Description: "The tool's " + name + " as its server lists it.",
		}
	}
	return s
}

// authorization is the caller's Authorization header, forwarded upstream.
func authorization(extra *mcp.RequestExtra) string {
	if extra == nil || extra.Header == nil {
		return ""
	}
	return extra.Header.Get("Authorization")
}

// errNoCatalog is a failure to list the caller's tools.
var errNoCatalog = errors.New("the tool catalog could not be loaded from the upstream server")

// errEmptyCatalog says the upstream listed no tools for the caller, so
// searching again cannot help.
var errEmptyCatalog = errors.New("the upstream server lists no tools for this account, so there is nothing " +
	"to search or call; the account may not have been given access to any")

// catalog returns the caller's catalog. Its error says only how the listing
// failed, coarsely: the detail, which holds the upstream's address, is the
// store's to log.
//
// It keeps the response alive from here on, since a listing, and a call
// after it, may take longer than proxies in front wait for a response.
func (t *tools) catalog(ctx context.Context, req *mcp.CallToolRequest, fresh bool) (*catalog.Catalog, error) {
	startKeepAlive(ctx)
	c, err := t.store.Get(ctx, authorization(req.Extra), fresh)
	if err != nil {
		upstreamFailed(ctx)
		return nil, fmt.Errorf("%w (%s)", errNoCatalog, failure(err).Class)
	}
	return c, nil
}

// failure returns err as an *upstream.Error, or as one that got no result
// for any other error.
func failure(err error) *upstream.Error {
	var e *upstream.Error
	if errors.As(err, &e) {
		return e
	}
	return &upstream.Error{Class: upstream.ClassDropped, Sent: true}
}

func (t *tools) search(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in SearchIn,
) (*mcp.CallToolResult, SearchOut, error) {
	queries, err := searchQueries(in)
	if err != nil {
		return nil, SearchOut{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)
	c, err := t.catalog(ctx, req, false)
	if err != nil {
		return nil, SearchOut{}, err
	}
	if c.Len() == 0 {
		return nil, SearchOut{}, errEmptyCatalog
	}
	withSchemas := t.schemas
	if in.Schemas != nil && !*in.Schemas {
		withSchemas = 0
	}
	// A tool found by more than one query brings its schema once.
	given := map[string]bool{}
	find := func(query string) []Summary {
		found, mode := c.Search(ctx, t.store.Embedder(), query, limit)
		t.onSearch(mode)
		tools := make([]Summary, 0, len(found))
		for i, tl := range found {
			copies := c.Copies(tl.Name)
			s := Summary{
				Name: tl.Name, Title: tl.Title, Description: summary(tl.Description),
				ReadOnly: t.reads(copies), Note: ambiguousNote(len(copies)),
			}
			if i < withSchemas && !given[tl.Name] && schemaFits(tl.InputSchema) {
				s.InputSchema, given[tl.Name] = tl.InputSchema, true
			}
			tools = append(tools, s)
		}
		return tools
	}
	out := SearchOut{Total: c.Len()}
	if in.Queries == nil {
		out.Tools = find(queries[0])
		return nil, out, nil
	}
	for _, q := range queries {
		out.Results = append(out.Results, SearchResult{Query: q, Tools: find(q)})
	}
	return nil, out, nil
}

// searchQueries is what a search looks for: its query, or its queries.
func searchQueries(in SearchIn) ([]string, error) {
	if in.Queries == nil {
		query := strings.TrimSpace(in.Query)
		if query == "" {
			return nil, errors.New("query is empty: describe what you want to do")
		}
		if utf8.RuneCountInString(query) > maxQueryRunes {
			return nil, errLongQuery
		}
		return []string{query}, nil
	}
	switch {
	case strings.TrimSpace(in.Query) != "":
		return nil, errors.New("give query or queries, not both")
	case len(in.Queries) == 0:
		return nil, errors.New("queries is empty: describe what you want to do")
	case len(in.Queries) > maxQueries:
		return nil, fmt.Errorf("too many queries: at most %d in one search", maxQueries)
	}
	queries := make([]string, 0, len(in.Queries))
	for i, q := range in.Queries {
		q = strings.TrimSpace(q)
		if q == "" {
			return nil, fmt.Errorf("query %d is empty: describe what you want to do", i+1)
		}
		if utf8.RuneCountInString(q) > maxQueryRunes {
			return nil, errLongQuery
		}
		queries = append(queries, q)
	}
	return queries, nil
}

func (t *tools) describe(
	ctx context.Context, req *mcp.CallToolRequest, in DescribeIn,
) (*mcp.CallToolResult, DescribeOut, error) {
	if len(in.Names) == 0 {
		return nil, DescribeOut{}, errors.New("names is empty: give the names search_tools returned")
	}
	if len(in.Names) > maxDescribe {
		return nil, DescribeOut{}, fmt.Errorf("at most %d tools at a time", maxDescribe)
	}
	for _, name := range in.Names {
		if len(name) > maxNameBytes {
			return nil, DescribeOut{}, errLongName
		}
	}
	c, err := t.catalog(ctx, req, false)
	if err != nil {
		return nil, DescribeOut{}, err
	}
	if c.Len() == 0 {
		return nil, DescribeOut{}, errEmptyCatalog
	}
	out := DescribeOut{Tools: []Detail{}}
	for _, name := range in.Names {
		tl, ok := c.Tool(name)
		if !ok {
			out.Unknown = append(out.Unknown, name)
			if near := similar(ctx, c, t.store.Embedder(), name); len(near) > 0 {
				if out.Similar == nil {
					out.Similar = map[string][]string{}
				}
				out.Similar[name] = near
			}
			continue
		}
		out.Tools = append(out.Tools, Detail{
			Name: tl.Name, Title: tl.Title, Description: tl.Description,
			InputSchema: tl.InputSchema, OutputSchema: tl.OutputSchema, Annotations: t.annotations(tl),
			Note: ambiguousNote(len(c.Copies(name))),
		})
	}
	return nil, out, nil
}

// call calls the upstream tool req names. With readOnlyOnly, as for
// call_read_tool, a tool the upstream does not mark read-only is refused, so
// that a client's permission to read never lets a write through.
func (t *tools) call(ctx context.Context, req *mcp.CallToolRequest, readOnlyOnly bool) (*mcp.CallToolResult, error) {
	in, err := parseCall(req.Params.Arguments)
	if err != nil {
		return toolError(fmt.Sprintf("input of %s: %v", req.Params.Name, err)), nil
	}
	if msg := checkName(in.Name); msg != "" {
		return toolError(msg), nil
	}
	c, err := t.catalog(ctx, req, false)
	if err != nil {
		return toolError(err.Error()), nil
	}
	res, outcome := t.callOne(ctx, req, c, in, readOnlyOnly)
	if outcome == OutcomeInvalidArguments {
		invalidArguments(ctx)
	}
	return res, nil
}

// checkName says what is wrong with name, a tool to call, before any
// lookup; nothing when it may be looked up.
func checkName(name string) string {
	switch {
	case name == "":
		return "name is empty: give a name search_tools returned"
	case len(name) > maxNameBytes:
		return errLongName.Error()
	default:
		return ""
	}
}

// callOne calls the upstream tool in names, from the caller's catalog c, as
// call does, and returns its result with the outcome it counts as.
func (t *tools) callOne(
	ctx context.Context, req *mcp.CallToolRequest, c *catalog.Catalog, in callInput, readOnlyOnly bool,
) (*mcp.CallToolResult, string) {
	copies := c.Copies(in.Name)
	if len(copies) == 0 {
		// The catalog may predate the tool: list once more before refusing.
		var err error
		if c, err = t.catalog(ctx, req, true); err != nil {
			return toolError(err.Error()), OutcomeUpstreamError
		}
		if copies = c.Copies(in.Name); len(copies) == 0 {
			return toolError(unknownTool(ctx, c, t.store.Embedder(), in.Name)), OutcomeToolError
		}
	}
	if readOnlyOnly && !t.reads(copies) {
		return toolError(notARead(in.Name, copies)), OutcomeToolError
	}
	// Under a repeated name, the upstream decides which tool a call reaches,
	// so no one schema can be held against the arguments.
	if len(copies) == 1 {
		if problems, unknown := argumentProblems(copies[0].InputSchema, in.Arguments); len(problems) > 0 {
			return toolError(refusal(in.Name, copies[0].InputSchema, problems, unknown)), OutcomeInvalidArguments
		}
	}
	res, err := t.caller.CallTool(ctx, authorization(req.Extra), in.Name, in.Arguments)
	if err != nil {
		return toolError(t.noResult(ctx, copies, err, readOnlyOnly)), OutcomeUpstreamError
	}
	if res.IsError {
		return res, OutcomeToolError
	}
	return res, OutcomeOK
}

// notARead explains why call_read_tool does not call name, listed as
// copies.
func notARead(name string, copies []*mcp.Tool) string {
	if readOnly(copies) {
		return fmt.Sprintf("this proxy lists %s among the tools that may change things, so %s "+
			"does not call it: call it with %s", name, ToolCallRead, ToolCallWrite)
	}
	if len(copies) > 1 {
		return fmt.Sprintf("the upstream lists %d tools named %s and not all of them only read, "+
			"so %s does not call it: call it with %s", len(copies), name, ToolCallRead, ToolCallWrite)
	}
	return fmt.Sprintf("%s may change things, so %s does not call it: call it with %s",
		name, ToolCallRead, ToolCallWrite)
}

// noResult logs why a call to the upstream tool listed as copies got no
// result and returns what the caller is told. The text says whether the upstream
// answered and whether the tool may have run, never the transport's
// detail, which holds the upstream's address. The log has that detail,
// but never a message of the upstream's, which may repeat the arguments.
func (t *tools) noResult(ctx context.Context, copies []*mcp.Tool, err error, readOnlyOnly bool) string {
	upstreamFailed(ctx)
	name := copies[0].Name
	f := failure(err)
	attrs := []any{"target", truncate(name), "class", f.Class, "sent", f.Sent}
	if detail := f.Detail(); detail != "" {
		attrs = append(attrs, "err", detail)
	}
	t.logger.WarnContext(ctx, "upstream call failed", attrs...)
	switch {
	case f.Class == upstream.ClassAnswered && f.Code == jsonrpc.CodeInvalidParams && len(copies) == 1:
		return "the upstream returned an error: " + f.Answer + "\n" + schemaNote(name, copies[0].InputSchema)
	case f.Class == upstream.ClassAnswered:
		return "the upstream returned an error: " + f.Answer
	case f.Class == upstream.ClassUnauthorized:
		return "the upstream server refused the caller's credential (unauthorized); the tool was not called"
	case f.Class == upstream.ClassTooLarge && !f.Sent:
		return "the upstream server's answer is larger than this proxy passes on; the tool was not called"
	case f.Class == upstream.ClassTooLarge && readOnlyOnly:
		return "the tool's result is larger than this proxy passes on: ask for less, such as a smaller page " +
			"or fewer fields"
	case f.Class == upstream.ClassTooLarge:
		return "the tool's result is larger than this proxy passes on, so it is not returned; the tool may " +
			"still have run. Check its effect with a read tool before calling it again."
	case !f.Sent:
		return fmt.Sprintf("the upstream server could not be reached (%s); the tool was not called", f.Class)
	case readOnlyOnly:
		return fmt.Sprintf("no result arrived (%s) from the upstream server", f.Class)
	default:
		return fmt.Sprintf("no result arrived (%s); the tool may still have run. "+
			"Check its effect with a read tool before calling it again.", f.Class)
	}
}

// callInput is the input of a call tool, as parseCall reads it.
type callInput struct {
	Name string
	// Arguments are the tool's arguments as the caller sent them, a JSON
	// object, or empty when absent or null.
	Arguments json.RawMessage
}

// parseCall reads raw, the input of a call tool. It accepts exactly the
// keys of CallIn, each at most once and in its exact case, and arguments
// that are an object without repeated keys: anything else could show one
// tool or value in a client's approval prompt and call another.
func parseCall(raw json.RawMessage) (callInput, error) {
	var in callInput
	seen := map[string]bool{}
	err := objectMembers(raw, func(key string, value json.RawMessage) error {
		if seen[key] {
			return fmt.Errorf("%q is given twice", key)
		}
		seen[key] = true
		switch key {
		case "name":
			if err := json.Unmarshal(value, &in.Name); err != nil {
				return errors.New("name must be a string")
			}
		case "arguments":
			if string(value) == "null" {
				return nil
			}
			if err := uniqueKeys(value); err != nil {
				if errors.Is(err, errNotObject) {
					return errors.New("arguments must be a JSON object")
				}
				return err
			}
			in.Arguments = value
		default:
			return fmt.Errorf("unknown key %q: give only name and arguments", truncate(key))
		}
		return nil
	})
	if errors.Is(err, errNotObject) {
		return callInput{}, errors.New("the input must be a JSON object")
	}
	if err != nil {
		return callInput{}, err
	}
	return in, nil
}

// errNotObject is JSON that is not an object.
var errNotObject = errors.New("not a JSON object")

// objectMembers calls member for each key and raw value of raw, a JSON
// object, in order, repeated keys included.
func objectMembers(raw json.RawMessage, member func(key string, value json.RawMessage) error) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errNotObject
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return errNotObject
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if decodeErr := dec.Decode(&value); decodeErr != nil {
			return errNotObject
		}
		if merr := member(key, value); merr != nil {
			return merr
		}
	}
	if _, err := dec.Token(); err != nil {
		return errNotObject
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errNotObject
	}
	return nil
}

// level is an object or array uniqueKeys is inside.
type level struct {
	keys map[string]bool // the keys of an object so far; nil for an array
	// wantKey says the object's next token is a key or its end.
	wantKey bool
}

// uniqueKeys checks that raw is a JSON object in which no object, at any
// depth and inside arrays too, repeats a key: a client could show one of
// two values and the upstream use the other.
func uniqueKeys(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var stack []*level
	// valueDone marks the end of a value inside the object on top, if any.
	valueDone := func() {
		if len(stack) > 0 && stack[len(stack)-1].keys != nil {
			stack[len(stack)-1].wantKey = true
		}
	}
	for first := true; ; first = false {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) && !first && len(stack) == 0 {
			return nil
		}
		if err != nil || (first && tok != json.Delim('{')) || (!first && len(stack) == 0) {
			return errNotObject
		}
		if len(stack) > 0 && stack[len(stack)-1].wantKey {
			top := stack[len(stack)-1]
			if tok == json.Delim('}') {
				stack = stack[:len(stack)-1]
				valueDone()
				continue
			}
			key, _ := tok.(string)
			if top.keys[key] {
				return fmt.Errorf("key %q is given twice in arguments", truncate(key))
			}
			top.keys[key], top.wantKey = true, false
			continue
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &level{keys: map[string]bool{}, wantKey: true})
		case json.Delim('['):
			stack = append(stack, &level{})
		case json.Delim(']'), json.Delim('}'):
			stack = stack[:len(stack)-1]
			valueDone()
		default:
			valueDone()
		}
	}
}

// truncate cuts s, a name or key a client chose, to maxTarget bytes at
// most.
func truncate(s string) string {
	if len(s) > maxTarget {
		return s[:maxTarget]
	}
	return s
}

// maxSearchSchema bounds an input schema search_tools returns, in bytes of
// JSON: a larger one is left to describe_tool, so that one tool cannot
// flood a search result.
const maxSearchSchema = 8 << 10

// schemaFits reports whether schema is small enough for a search result.
func schemaFits(schema any) bool {
	raw, err := json.Marshal(schema)
	return err == nil && len(raw) <= maxSearchSchema
}

// unknownTool explains that no tool is called name and suggests the ones
// whose names are closest.
func unknownTool(ctx context.Context, c *catalog.Catalog, emb catalog.Embedder, name string) string {
	if c.Len() == 0 {
		return errEmptyCatalog.Error()
	}
	msg := fmt.Sprintf("no tool %q in the catalog; find tools with %s", name, ToolSearch)
	names := similar(ctx, c, emb, name)
	near, found := closest(c.Names(), name)
	if found && !slices.Contains(names, near) {
		names = append([]string{near}, names...)
	}
	if len(names) > 0 {
		msg += ". Similar: " + strings.Join(names, ", ")
	}
	if found {
		if tl, ok := c.Tool(near); ok {
			msg += fmt.Sprintf(". Did you mean %s? ", near) + schemaNote(near, tl.InputSchema)
		}
	}
	return msg
}

// similar returns the names of the tools closest to name.
func similar(ctx context.Context, c *catalog.Catalog, emb catalog.Embedder, name string) []string {
	found, _ := c.Search(ctx, emb, name, suggestions)
	names := make([]string, 0, len(found))
	for _, tl := range found {
		names = append(names, tl.Name)
	}
	return names
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// summary shortens a description to its first paragraph, cut at
// summaryRunes at a word boundary.
func summary(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= summaryRunes {
		return s
	}
	runes := []rune(s)[:summaryRunes]
	cut := string(runes)
	// i is a byte index; the bound counts characters.
	if i := strings.LastIndexByte(cut, ' '); i >= 0 && utf8.RuneCountInString(cut[:i]) > summaryRunes/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Outcomes of a tool call.
const (
	OutcomeOK        = "ok"
	OutcomeToolError = "tool_error"
	// OutcomeUpstreamError is a call that failed because the upstream
	// server could not list the caller's tools or gave a call no result:
	// the proxy's side failed, not the tool.
	OutcomeUpstreamError = "upstream_error"
	// OutcomeInvalidArguments is a call the proxy refused because its
	// arguments do not fit the tool's input schema, so the upstream was
	// not called.
	OutcomeInvalidArguments = "invalid_arguments"
	OutcomeError            = "error"
)

// Outcome classifies what a tool call returned: a result, a result that
// reports the tool failed, a result that reports the upstream failed, a
// call refused for its arguments, or no result. ctx is the call's, as a
// middleware of the server receives it; only then can the upstream's
// failures be told from the tool's, or a refusal.
func Outcome(ctx context.Context, res mcp.Result, err error) string {
	if err != nil {
		return OutcomeError
	}
	r, ok := res.(*mcp.CallToolResult)
	if !ok || !r.IsError {
		return OutcomeOK
	}
	s, tracked := ctx.Value(callKey{}).(*callState)
	switch {
	case tracked && s.upstream.Load():
		return OutcomeUpstreamError
	case tracked && s.invalid.Load():
		return OutcomeInvalidArguments
	default:
		return OutcomeToolError
	}
}

// callKey holds, in a tool call's context, its *callState.
type callKey struct{}

// callState is what the server's tools note of a call, for its outcome.
type callState struct {
	// upstream is set when the call failed for want of the upstream.
	upstream atomic.Bool
	// invalid is set when the call was refused for its arguments.
	invalid atomic.Bool
}

// trackCalls gives every tool call a *callState, outside every other
// middleware so that all of them can read its outcome.
func trackCalls(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == methodCallTool {
			ctx = context.WithValue(ctx, callKey{}, &callState{})
		}
		return next(ctx, method, req)
	}
}

// upstreamFailed notes that the call of ctx failed for want of the
// upstream.
func upstreamFailed(ctx context.Context) {
	if s, ok := ctx.Value(callKey{}).(*callState); ok {
		s.upstream.Store(true)
	}
}

// invalidArguments notes that the call of ctx was refused for its
// arguments.
func invalidArguments(ctx context.Context) {
	if s, ok := ctx.Value(callKey{}).(*callState); ok {
		s.invalid.Store(true)
	}
}

// logFailures logs one line for each tool call that fails: the tool, for
// the call tools the upstream tool they named, the outcome and how long it took,
// never the query, the arguments, the result or the error text, which may
// repeat them.
func logFailures(logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if method != methodCallTool || !ok || call.Params == nil {
				return next(ctx, method, req)
			}
			start := time.Now()
			res, err := next(ctx, method, req)
			outcome := Outcome(ctx, res, err)
			if outcome == OutcomeOK {
				return res, err
			}
			attrs := []any{"tool", ToolName(call.Params.Name), "outcome", outcome}
			if call.Params.Name == ToolCallRead || call.Params.Name == ToolCallWrite {
				attrs = append(attrs, "target", target(call.Params.Arguments))
			}
			attrs = append(attrs, "took", time.Since(start).Round(time.Millisecond))
			logger.Log(ctx, slog.LevelInfo, "tool call failed", attrs...)
			return res, err
		}
	}
}

// Unknown names a tool the server does not offer, so that names clients
// make up reach neither logs nor metric labels.
const Unknown = "unknown"

// ToolName returns name when it is one of the server's tools and Unknown
// otherwise.
func ToolName(name string) string {
	switch name {
	case ToolSearch, ToolDescribe, ToolCallRead, ToolCallReads, ToolCallWrite:
		return name
	default:
		return Unknown
	}
}

// maxTarget bounds the upstream tool name logged, which a client chooses.
const maxTarget = 128

// target returns the upstream tool name of a call tool's arguments, and
// nothing else of them; nothing for input the call tools refuse.
func target(args json.RawMessage) string {
	in, err := parseCall(args)
	if err != nil {
		return ""
	}
	return truncate(in.Name)
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultMaxBatchCalls is how many calls call_read_tools takes at once by
// default.
const DefaultMaxBatchCalls = 10

// MaxBatchCallsLimit bounds Options.MaxBatchCalls: each call of a batch
// may take a whole response's worth of memory while it runs.
const MaxBatchCallsLimit = 50

// keyCalls is the key of call_read_tools' input.
const keyCalls = "calls"

// entryOverhead bounds what a batch adds to the result of each call: its
// label, or a note in its place.
const entryOverhead = 512

// batchParallel is how many calls of a batch run upstream at once.
const batchParallel = 4

// batchLimits bound call_read_tools.
type batchLimits struct {
	calls   int           // calls a batch may hold; 0 leaves the tool out
	timeout time.Duration // for the whole batch
	bytes   int64         // of every result together
}

// callReads serves call_read_tools: the calls of its input run as
// call_read_tool would run each, in parallel, and their results come back in
// order. A call that fails does not stop the others; the result is an error
// only when all of them failed.
func (t *tools) callReads(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	calls, err := parseCalls(req.Params.Arguments, t.batch.calls)
	if err != nil {
		return toolError(fmt.Sprintf("input of %s: %v", ToolCallReads, err)), nil
	}
	// The batch as a whole, the catalog's lookup included, takes no longer
	// than one call may.
	ctx, cancel := context.WithTimeout(ctx, t.batch.timeout)
	defer cancel()
	c, err := t.catalog(ctx, req, false)
	if err != nil {
		return toolError(err.Error()), nil
	}
	results := make([]*mcp.CallToolResult, len(calls))
	outcomes := make([]string, len(calls))
	// Room is kept for what the batch adds to each result: its label, or a
	// note in its place.
	var budget atomic.Int64
	budget.Store(t.batch.bytes - int64(len(calls))*entryOverhead)
	slots := make(chan struct{}, batchParallel)
	var wg sync.WaitGroup
	for i, in := range calls {
		wg.Go(func() {
			if msg := checkName(in.Name); msg != "" {
				results[i], outcomes[i] = toolError(msg), OutcomeToolError
				return
			}
			late := toolError("the batch ran out of time before this call started; the tool was not called")
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				results[i], outcomes[i] = late, OutcomeUpstreamError
				return
			}
			// A slot freed as the time ran out is no time to start in.
			if ctx.Err() != nil {
				<-slots
				results[i], outcomes[i] = late, OutcomeUpstreamError
				return
			}
			res, outcome := t.callOne(ctx, req, c, in, true)
			// Results past the budget are dropped as they come, so that a
			// batch holds no more than one response's worth. The slot is held
			// until then, so that no more than batchParallel results are
			// being sized at once.
			if size := resultSize(res); budget.Add(-size) < 0 {
				res, outcome = toolError("this result is left out: the results of the batch together are "+
					"larger than this proxy passes on; ask for less, or call this tool alone"), OutcomeToolError
			}
			<-slots
			results[i], outcomes[i] = res, outcome
		})
	}
	wg.Wait()
	return t.batchResult(ctx, calls, results, outcomes), nil
}

// batchResult joins the results of calls into one: each call's content
// after a line naming it and whether it failed, and the structured content
// of all of them, in order, when any has some.
func (t *tools) batchResult(
	ctx context.Context, calls []callInput, results []*mcp.CallToolResult, outcomes []string,
) *mcp.CallToolResult {
	out := &mcp.CallToolResult{IsError: true}
	var structured []any
	anyStructured, invalid := false, true
	for i, res := range results {
		status := "ok"
		if res.IsError {
			status = "failed"
			t.logger.InfoContext(ctx, "batch call failed", "tool", ToolCallReads, "index", i+1,
				"target", truncate(calls[i].Name), "outcome", outcomes[i])
		} else {
			out.IsError = false
		}
		invalid = invalid && outcomes[i] == OutcomeInvalidArguments
		out.Content = append(out.Content, &mcp.TextContent{
			Text: fmt.Sprintf("[%d/%d] %s: %s", i+1, len(results), truncate(calls[i].Name), status),
		})
		out.Content = append(out.Content, res.Content...)
		structured = append(structured, res.StructuredContent)
		anyStructured = anyStructured || res.StructuredContent != nil
	}
	if anyStructured {
		// Structured content is an object, so the list goes in one.
		out.StructuredContent = map[string]any{"results": structured}
	}
	if out.IsError && invalid {
		invalidArguments(ctx)
	}
	return out
}

// resultSize is about how many bytes res takes as JSON.
func resultSize(res *mcp.CallToolResult) int64 {
	raw, err := json.Marshal(res)
	if err != nil {
		return 0
	}
	return int64(len(raw))
}

// parseCalls reads raw, the input of call_read_tools: only calls, once and
// in that case, a list of 1 to limit inputs of call_read_tool, each read
// as parseCall reads one.
func parseCalls(raw json.RawMessage, limit int) ([]callInput, error) {
	var calls []callInput
	seen := false
	err := objectMembers(raw, func(key string, value json.RawMessage) error {
		if key != keyCalls {
			return fmt.Errorf("unknown key %q: give only calls", truncate(key))
		}
		if seen {
			return errors.New(`"calls" is given twice`)
		}
		seen = true
		var entries []json.RawMessage
		if json.Unmarshal(value, &entries) != nil || bytes.TrimSpace(value)[0] != '[' {
			return errors.New("calls must be a list")
		}
		if len(entries) == 0 || len(entries) > limit {
			return fmt.Errorf("calls must hold 1 to %d calls", limit)
		}
		for i, e := range entries {
			in, err := parseCall(e)
			if err != nil {
				return fmt.Errorf("call %d: %w", i+1, err)
			}
			calls = append(calls, in)
		}
		return nil
	})
	if errors.Is(err, errNotObject) {
		return nil, errors.New("the input must be a JSON object")
	}
	if err != nil {
		return nil, err
	}
	if !seen {
		return nil, errors.New("calls is missing: give a list of calls, each a name and arguments")
	}
	return calls, nil
}

// A typo is at most maxTypoEdits characters wrong, and one in typoRunes.
const (
	maxTypoEdits = 2
	typoRunes    = 5
)

// closest returns the one name among names that name is clearly a typo of:
// the same but for case, or for a character or two in a longer name, with
// no other name as close. A guess any less sure is left to the similar
// names.
func closest(names []string, name string) (string, bool) {
	want := []rune(strings.ToLower(name))
	limit := min(maxTypoEdits, len(want)/typoRunes)
	best, bestDist, ties := "", limit+1, 0
	for _, n := range names {
		if n == name {
			continue
		}
		if d := utf8.RuneCountInString(n) - len(want); d > limit || -d > limit {
			continue
		}
		d := distance(want, []rune(strings.ToLower(n)), limit)
		switch {
		case d < bestDist:
			best, bestDist, ties = n, d, 1
		case d == bestDist:
			ties++
		}
	}
	return best, ties == 1 && bestDist <= limit
}

// distance is the Levenshtein distance of a and b, or limit+1 once it is
// sure to exceed limit.
func distance(a, b []rune, limit int) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		low := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			low = min(low, cur[j])
		}
		if low > limit {
			return limit + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

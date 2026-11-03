package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultMaxInFlight is how many requests the MCP endpoint serves at once
// by default.
const DefaultMaxInFlight = 8

// DefaultMaxInFlightBytes is how many bytes of request bodies the MCP
// endpoint serves at once by default. A tool call holds several copies of
// its body while it is decoded, checked and sent on, about ten times its
// size at its peak, so this leaves room for one call of the largest body
// allowed beside small requests.
const DefaultMaxInFlightBytes = 32 << 20

// retryAfter is what a request refused for want of a slot is told to wait,
// in seconds.
const retryAfter = "1"

// limit serves next at most n requests at a time, whose bodies add up to
// at most budget bytes, refusing any more with 503 and Retry-After, without
// waiting for their bodies, rather than queueing them. A request weighs its
// Content-Length, or MaxRequestBytes when that is not given, and at most
// the whole budget, so any one request is served when nothing else is.
// The count bounds the requests held open, such as calls waiting on the
// upstream; the budget bounds the memory their bodies take. It tells
// rejected of each refusal, for either reason, and inFlight of each request
// it starts (1) and finishes (-1).
func limit(n int, budget int64, next http.Handler, rejected func(), inFlight func(delta int)) http.Handler {
	sl := &slots{n: n, budget: budget}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := sl.take(r.ContentLength)
		if !ok {
			rejected()
			// Closing the connection lets the refusal go out at once:
			// otherwise net/http first reads what is left of the body.
			w.Header().Set("Connection", "close")
			w.Header().Set("Retry-After", retryAfter)
			http.Error(w, "the server is busy; try again shortly", http.StatusServiceUnavailable)
			return
		}
		inFlight(1)
		// Counted down while the slot is still held, so the count never
		// exceeds n.
		defer func() {
			inFlight(-1)
			release()
		}()
		next.ServeHTTP(w, r)
	})
}

// errBusy refuses a request beyond the limits over a transport without
// HTTP, as limit's 503 does over HTTP.
var errBusy = errors.New("the server is busy; try again shortly")

// limitRequests is limit for a transport without HTTP, as a middleware:
// requests, notifications aside, are served at most n at a time, weighing
// MaxRequestBytes each against budget, since their size is not known, and
// refused with errBusy beyond that.
func limitRequests(n int, budget int64, rejected func(), inFlight func(delta int)) mcp.Middleware {
	sl := &slots{n: n, budget: budget}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if strings.HasPrefix(method, "notifications/") {
				return next(ctx, method, req)
			}
			release, ok := sl.take(-1)
			if !ok {
				rejected()
				return nil, errBusy
			}
			inFlight(1)
			defer func() {
				inFlight(-1)
				release()
			}()
			return next(ctx, method, req)
		}
	}
}

// slots counts requests in flight, at most n, and the bytes they weigh,
// at most budget.
type slots struct {
	n      int
	budget int64
	mu     sync.Mutex
	count  int
	used   int64
}

// take reserves a slot for a request of size bytes, or MaxRequestBytes
// when size is negative or larger, and at most the whole budget, and
// returns the function that releases it; ok is false when none is free.
func (s *slots) take(size int64) (func(), bool) {
	weight := size
	if weight < 0 || weight > MaxRequestBytes {
		weight = MaxRequestBytes
	}
	weight = min(weight, s.budget)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count >= s.n || s.used+weight > s.budget {
		return nil, false
	}
	s.count++
	s.used += weight
	return func() {
		s.mu.Lock()
		s.count--
		s.used -= weight
		s.mu.Unlock()
	}, true
}

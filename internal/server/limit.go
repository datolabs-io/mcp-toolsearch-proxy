package server

import (
	"net/http"
	"sync"
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
	var (
		mu    sync.Mutex
		count int
		used  int64
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		weight := r.ContentLength
		if weight < 0 || weight > MaxRequestBytes {
			weight = MaxRequestBytes
		}
		weight = min(weight, budget)
		mu.Lock()
		ok := count < n && used+weight <= budget
		if ok {
			count++
			used += weight
		}
		mu.Unlock()
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
			mu.Lock()
			count--
			used -= weight
			mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

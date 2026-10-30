package server

import (
	"context"
	"mime"
	"net/http"
	"sync"
	"time"
)

// DefaultKeepAlive is how often, by default, a response waiting on the
// upstream server sends a comment: well inside the idle timeouts of common
// proxies, and a few bytes each time.
const DefaultKeepAlive = 5 * time.Second

// keepAliveComment is a server-sent events comment, which clients skip.
const keepAliveComment = ": keepalive\n\n"

// keepAliveKey holds, in a request's context, its *keepAliveWriter.
type keepAliveKey struct{}

// keepAliveWriter is the response writer of an MCP request, which can keep
// the response alive while the request waits on the upstream server. It
// passes through everything but flushes. Once started it has sent the
// response's headers, and what was written before, and sends a comment
// every interval until the request ends; the status the transport sets
// later is ignored. Every write is serialized, since the comments come from
// a goroutine of their own.
//
// A comment goes out only between events, and only after an interval in
// which the transport wrote nothing. A write is usually the result, which
// the request ends right after, so the result is not flushed before the
// request is served, and a client does not read it while the request still
// holds its slot (see limit). In an MCP 2025-03-26 batch, a write can also
// be another request's answer while the call still waits; the comments
// then go on.
type keepAliveWriter struct {
	http.ResponseWriter

	done     <-chan struct{} // closed when the request ends, which ends the comments
	interval time.Duration

	mu          sync.Mutex
	wroteHeader bool
	status      int     // the status sent, once wroteHeader
	started     bool    // the headers are sent and the comments begun
	wrote       bool    // the transport wrote since the last tick
	tail        [2]byte // the last two bytes written, to tell where an event ends
	stop        chan struct{}
	stopped     chan struct{}
}

// keepAlive serves next with a response writer that the request's tools
// can keep alive (see startKeepAlive), sending a comment every interval.
func keepAlive(interval time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			// The transport bounds the body too, but through the writer
			// below, which hides from net/http that the limit was hit: the
			// connection would then be drained rather than closed.
			r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		}
		kw := &keepAliveWriter{ResponseWriter: w, done: r.Context().Done(), interval: interval}
		defer kw.finish()
		next.ServeHTTP(kw, r.WithContext(context.WithValue(r.Context(), keepAliveKey{}, kw)))
	})
}

// startKeepAlive keeps the response to the request of ctx alive, when it is
// a stream of server-sent events whose headers are not yet sent: a proxy in
// front that waits for the headers, or for the next bytes, does not give up
// on a call that waits long on the upstream. A tool calls it before it
// reaches upstream, after the transport has set the response's headers and
// before it writes the result.
func startKeepAlive(ctx context.Context) {
	if kw, ok := ctx.Value(keepAliveKey{}).(*keepAliveWriter); ok {
		kw.start()
	}
}

// WriteHeader sends code, unless the headers are sent already: once the
// response is kept alive, its status is 200.
func (w *keepAliveWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.wroteHeader {
		return
	}
	w.wroteHeader, w.status = true, code
	w.ResponseWriter.WriteHeader(code)
}

// Write writes p, and ends the comments when they have begun.
func (w *keepAliveWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.wroteHeader, w.status = true, http.StatusOK
	}
	if w.started {
		w.wrote = true
	}
	for _, b := range p[max(0, len(p)-len(w.tail)):] {
		w.tail = [2]byte{w.tail[1], b}
	}
	return w.ResponseWriter.Write(p) //nolint:wrapcheck // passed through unchanged
}

// FlushError does nothing: what the transport writes goes out when the
// keepalive starts or when the request is served, never before the slot is
// freed. http.ResponseController calls it, rather than flushing the writer
// underneath.
func (w *keepAliveWriter) FlushError() error { return nil }

// Flush does what FlushError does, for callers of http.Flusher.
func (w *keepAliveWriter) Flush() {}

// Unwrap returns the writer underneath, for http.ResponseController.
func (w *keepAliveWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *keepAliveWriter) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A batch of MCP 2025-03-26 may have answered another request already.
	if w.started || (w.wroteHeader && w.status != http.StatusOK) {
		return
	}
	if mt, _, err := mime.ParseMediaType(w.Header().Get("Content-Type")); err != nil || mt != "text/event-stream" {
		return
	}
	w.started = true
	if !w.wroteHeader {
		w.wroteHeader, w.status = true, http.StatusOK
		w.ResponseWriter.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
	w.stop, w.stopped = make(chan struct{}), make(chan struct{})
	go w.comment()
}

// comment sends a comment every interval until stopped, the request ends
// or a write fails, skipping an interval in which the transport wrote and
// any tick that falls inside an event.
func (w *keepAliveWriter) comment() {
	defer close(w.stopped)
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-w.done:
			return
		case <-tick.C:
		}
		w.mu.Lock()
		if w.wrote || w.inEvent() {
			w.wrote = false
			w.mu.Unlock()
			continue
		}
		_, err := w.ResponseWriter.Write([]byte(keepAliveComment))
		if err == nil {
			err = http.NewResponseController(w.ResponseWriter).Flush()
		}
		w.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// inEvent reports whether the transport stopped writing inside an event,
// which ends with a blank line.
func (w *keepAliveWriter) inEvent() bool {
	return w.tail != [2]byte{} && w.tail != [2]byte{'\n', '\n'}
}

// finish stops the comments, and returns once none can be written, before
// the request's handler returns.
func (w *keepAliveWriter) finish() {
	w.mu.Lock()
	started := w.started
	w.mu.Unlock()
	if started {
		close(w.stop)
		<-w.stopped
	}
}

package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

// newStream is a keepAliveWriter for a server-sent events response, and
// the recorder underneath it, whose body is read once the writer is done.
func newStream(interval time.Duration, done <-chan struct{}) (*keepAliveWriter, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	return &keepAliveWriter{ResponseWriter: rec, done: done, interval: interval}, rec
}

// In an MCP 2025-03-26 batch, another request's answer can be written after
// a call has started the comments. The call still waits, so the comments go
// on, between events and never inside one.
func TestKeepAliveOutlastsOtherAnswersOfABatch(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		done := make(chan struct{})
		kw, rec := newStream(time.Second, done)

		kw.start()
		_, _ = kw.Write([]byte("event: message\ndata: {\"id\":1}\n\n"))
		time.Sleep(3 * time.Second)
		_, _ = kw.Write([]byte("event: message\n"))
		time.Sleep(3 * time.Second)
		_, _ = kw.Write([]byte("data: {\"id\":2}\n\n"))
		close(done)
		kw.finish()

		body := rec.Body.String()
		ping, call := strings.Index(body, `"id":1`), strings.Index(body, "event: message\ndata: {\"id\":2}")
		assert.Positive(t, ping)
		assert.Positive(t, call, "no comment inside the call's event: %q", body)
		assert.Contains(t, body[ping:call], keepAliveComment, "comments after the first answer")
	})
}

// The transport's result is not followed by a comment: the request ends
// within the interval after its last write.
func TestKeepAliveSendsNothingRightAfterAWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		done := make(chan struct{})
		kw, rec := newStream(time.Second, done)

		kw.start()
		time.Sleep(1500 * time.Millisecond)
		_, _ = kw.Write([]byte("event: message\ndata: {\"id\":2}\n\n"))
		time.Sleep(time.Second)
		close(done)
		kw.finish()

		assert.Equal(t, keepAliveComment+"event: message\ndata: {\"id\":2}\n\n", rec.Body.String())
	})
}

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// refused is a listing that could not connect, as upstream.Client reports
// one.
func refused(t *testing.T) error {
	t.Helper()
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close()
	_, err := upstream.New(ts.URL, &upstream.Options{ListRetries: -1}).List(t.Context(), "")
	var e *upstream.Error
	if !errors.As(err, &e) || e.Class != upstream.ClassRefused {
		t.Fatalf("want a refused listing, got %v", err)
	}
	return err
}

func TestBuildFailuresAreNotLoggedPerRequest(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	now := time.Unix(1_000_000, 0)
	b := newBuildLog(slog.New(slog.NewTextHandler(&out, nil)), time.Minute, func() time.Time { return now })
	down := refused(t)
	other := &net.OpError{Op: "read", Err: syscall.ECONNRESET}

	for range 50 {
		b.record(t.Context(), 0, down)
	}
	assert.Equal(t, 1, strings.Count(out.String(), "catalog build failed"), "one line for the outage's start")

	b.record(t.Context(), 0, fmt.Errorf("%w", other))
	assert.Equal(t, 2, strings.Count(out.String(), "catalog build failed"), "a new kind of failure is logged at once")

	now = now.Add(time.Minute)
	b.record(t.Context(), 0, down)
	assert.Equal(t, 3, strings.Count(out.String(), "catalog build failed"), "then once a window")
	assert.Contains(t, out.String(), "suppressed=49")

	b.record(t.Context(), 12, nil)
	assert.Contains(t, out.String(), "catalog builds succeed again")
	b.record(t.Context(), 12, nil)
	assert.Equal(t, 1, strings.Count(out.String(), "succeed again"), "only once")

	b.record(t.Context(), 0, down)
	assert.Equal(t, 4, strings.Count(out.String(), "catalog build failed"), "a new outage is logged at once")

	b.record(t.Context(), 12, errors.New("embed failed"))
	assert.Contains(t, out.String(), "lexically", "an embedding failure is no outage, and logged as it comes")
}

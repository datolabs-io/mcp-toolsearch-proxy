package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// buildLogWindow is how often a kind of build failure that goes on is
// logged again: the longest a failing caller backs off.
const buildLogWindow = time.Minute

// classOther is the class of a build failure that is not the upstream
// client's.
const classOther = "other"

// buildLog logs catalog builds without repeating itself through an outage,
// when every caller's builds fail: the first failure of each class, then
// one line per window with how many failed since, and one line when a
// build succeeds again. A build left lexical only is logged as it comes.
type buildLog struct {
	logger *slog.Logger
	window time.Duration
	now    func() time.Time

	mu sync.Mutex
	// classes holds, for each class failing since the last success, when
	// it was last logged and how many failures were not.
	classes map[string]*failing
	// failed counts the failures since the last success.
	failed int
}

type failing struct {
	logged     time.Time
	suppressed int
}

func newBuildLog(logger *slog.Logger, window time.Duration, now func() time.Time) *buildLog {
	return &buildLog{logger: logger, window: window, now: now, classes: map[string]*failing{}}
}

// record logs, if it is due, a build of n tools that err failed (n is 0)
// or left lexical only. The log says which step failed and how, never
// whose tools or which.
func (b *buildLog) record(ctx context.Context, n int, err error) {
	switch {
	case err == nil:
		b.succeeded(ctx)
	case n > 0:
		b.logger.WarnContext(ctx, "catalog searched lexically only: its tools could not be embedded",
			"tools", n, "err", err)
	default:
		b.failedWith(ctx, err)
	}
}

func (b *buildLog) succeeded(ctx context.Context) {
	b.mu.Lock()
	failed := b.failed
	b.failed = 0
	clear(b.classes)
	b.mu.Unlock()
	if failed > 0 {
		b.logger.InfoContext(ctx, "catalog builds succeed again", "failed", failed)
	}
}

func (b *buildLog) failedWith(ctx context.Context, err error) {
	class, detail := classOther, err.Error()
	var e *upstream.Error
	if errors.As(err, &e) {
		class, detail = e.Class, e.Detail()
	}
	now := b.now()
	b.mu.Lock()
	b.failed++
	f, ok := b.classes[class]
	if ok && now.Sub(f.logged) < b.window {
		f.suppressed++
		b.mu.Unlock()
		return
	}
	if !ok {
		f = &failing{}
		b.classes[class] = f
	}
	suppressed := f.suppressed
	f.logged, f.suppressed = now, 0
	b.mu.Unlock()

	attrs := []any{"class", class}
	if detail != "" {
		attrs = append(attrs, "err", detail)
	}
	if suppressed > 0 {
		attrs = append(attrs, "suppressed", suppressed)
	}
	b.logger.WarnContext(ctx, "catalog build failed", attrs...)
}

package metrics

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// Metrics records into its own registry. Its labels take a fixed set of
// values: never a token, a caller or an upstream tool's name.
type Metrics struct {
	reg           *prometheus.Registry
	calls         *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	builds        *prometheus.CounterVec
	buildDuration prometheus.Histogram
	searches      *prometheus.CounterVec
	upstream      *prometheus.CounterVec
	upstreamTime  *prometheus.HistogramVec
	rejected      prometheus.Counter
	inFlight      prometheus.Gauge
	stale         prometheus.Counter
	handshakes    prometheus.Counter
	ttl           time.Duration
	now           func() time.Time
	served        window
}

// Label names.
const (
	labelTool    = "tool"
	labelOutcome = "outcome"
	labelOp      = "op"
)

// Outcomes of a catalog build.
const (
	BuildOK          = "ok"
	BuildLexicalOnly = "lexical_only"
	BuildError       = "error"
)

// Options says where Metrics reads what it does not record itself. A nil
// *Options is valid.
type Options struct {
	// Catalogs reports how many catalogs are kept, and MaxTools how many
	// tools the largest catalog served holds (see catalog.Store.MaxTools);
	// nil reports 0.
	Catalogs, MaxTools func() int
	// TTL is the catalogs' TTL: a catalog served older than it is stale.
	// Zero means catalog.DefaultTTL.
	TTL time.Duration
	// Now returns the time; nil means time.Now.
	Now func() time.Time
}

// servedWindow is how long the age of a catalog served is remembered for
// mcp_toolsearch_catalog_served_age_seconds: between one and two of them.
const servedWindow = time.Minute

// Buckets, in seconds: of a tool call or upstream operation, which may take
// as long as the call timeout, and of a catalog build, bounded by the list
// timeout.
//
//nolint:gochecknoglobals // constant bucket bounds
var (
	callBuckets  = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 120}
	buildBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 20, 30}
)

// New returns metrics in a fresh registry, with the Go runtime and process
// collectors.
func New(opts *Options) *Metrics {
	var o Options
	if opts != nil {
		o = *opts
	}
	zero := func() int { return 0 }
	if o.Catalogs == nil {
		o.Catalogs = zero
	}
	if o.MaxTools == nil {
		o.MaxTools = zero
	}
	if o.TTL <= 0 {
		o.TTL = catalog.DefaultTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_toolsearch_tool_calls_total",
			Help: "Tool calls by tool and outcome: ok, tool_error, upstream_error when the upstream failed " +
				"rather than the tool, invalid_arguments when the proxy refused arguments that do not fit the " +
				"tool's input schema, or error.",
		}, []string{labelTool, labelOutcome}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "mcp_toolsearch_tool_call_duration_seconds", Help: "How long tool calls take, by tool.",
			Buckets: callBuckets,
		}, []string{labelTool}),
		builds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_toolsearch_catalog_builds_total",
			Help: "Catalog builds by outcome: ok, lexical_only when the tools could not be embedded, or error.",
		}, []string{labelOutcome}),
		buildDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "mcp_toolsearch_catalog_build_duration_seconds",
			Help: "How long catalog builds take, listing included, failed ones too.", Buckets: buildBuckets,
		}),
		searches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_toolsearch_searches_total", Help: "Searches by mode: hybrid or lexical.",
		}, []string{"mode"}),
		upstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_toolsearch_upstream_requests_total",
			Help: "Operations upstream by op (list, call) and outcome: ok, or why no answer came " +
				"(answered, timeout, refused, unauthorized, dropped, too_large).",
		}, []string{labelOp, labelOutcome}),
		upstreamTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mcp_toolsearch_upstream_request_duration_seconds",
			Help:    "How long operations upstream take, by op, a listing's retries included.",
			Buckets: callBuckets,
		}, []string{labelOp}),
		rejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mcp_toolsearch_requests_rejected_total",
			Help: "Requests refused with 503 because as many as -max-in-flight, or -max-in-flight-bytes of bodies, were being served.",
		}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mcp_toolsearch_in_flight", Help: "Requests the MCP endpoint is serving.",
		}),
		stale: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mcp_toolsearch_catalog_stale_served_total",
			Help: "Catalogs served older than -catalog-ttl, as while listing them again fails.",
		}),
		handshakes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mcp_toolsearch_handshakes_without_instructions_total",
			Help: "Handshakes answered without the upstream's instructions, for want of the caller's catalog.",
		}),
		ttl: o.TTL, now: o.Now,
	}
	m.served.start = o.Now()
	m.reg.MustRegister(m.calls, m.duration, m.builds, m.buildDuration, m.searches, m.upstream, m.upstreamTime,
		m.rejected, m.inFlight, m.stale, m.handshakes,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "mcp_toolsearch_catalogs", Help: "Catalogs kept, one per caller.",
		}, func() float64 { return float64(o.Catalogs()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "mcp_toolsearch_catalog_tools",
			Help: "Tools in the largest catalog that would be served, whichever caller listed last.",
		}, func() float64 { return float64(o.MaxTools()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "mcp_toolsearch_catalog_served_age_seconds",
			Help: "Age of the oldest catalog served in the last minute or two; 0 when none was.",
		}, func() float64 { return m.served.value(m.now()).Seconds() }),
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	// Every outcome starts at zero, so that rate and increase see the first
	// failure of a kind, and the end of an outage.
	for _, outcome := range []string{BuildOK, BuildLexicalOnly, BuildError} {
		m.builds.WithLabelValues(outcome)
	}
	for _, op := range []string{upstream.OpList, upstream.OpCall} {
		for _, outcome := range []string{
			upstream.OutcomeOK, upstream.ClassAnswered, upstream.ClassTimeout, upstream.ClassRefused,
			upstream.ClassUnauthorized, upstream.ClassDropped, upstream.ClassTooLarge,
		} {
			m.upstream.WithLabelValues(op, outcome)
		}
	}
	return m
}

// BuildInfo records the version the proxy was built as. Call it once.
func (m *Metrics) BuildInfo(version string) {
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mcp_toolsearch_build_info", Help: "Always 1, labelled with the proxy's version.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	info.Set(1)
	m.reg.MustRegister(info)
}

// Build records a catalog build of n tools that took took: err, when not
// nil, either failed it (n is 0) or left it lexical only.
func (m *Metrics) Build(n int, took time.Duration, err error) {
	m.buildDuration.Observe(took.Seconds())
	switch {
	case err == nil:
		m.builds.WithLabelValues(BuildOK).Inc()
	case n > 0:
		m.builds.WithLabelValues(BuildLexicalOnly).Inc()
	default:
		m.builds.WithLabelValues(BuildError).Inc()
	}
}

// Serve records a catalog served at age.
func (m *Metrics) Serve(age time.Duration) {
	if age >= m.ttl {
		m.stale.Inc()
	}
	m.served.observe(m.now(), age)
}

// Upstream records an operation upstream, as upstream.Options.OnRequest
// is told of it.
func (m *Metrics) Upstream(op, outcome string, took time.Duration) {
	m.upstream.WithLabelValues(op, outcome).Inc()
	m.upstreamTime.WithLabelValues(op).Observe(took.Seconds())
}

// Rejected records a request refused for want of a slot.
func (m *Metrics) Rejected() { m.rejected.Inc() }

// InFlight records a request starting (1) or finishing (-1).
func (m *Metrics) InFlight(delta int) { m.inFlight.Add(float64(delta)) }

// MissedInstructions records a handshake answered without the upstream's
// instructions.
func (m *Metrics) MissedInstructions() { m.handshakes.Inc() }

// Search records the mode of a search.
func (m *Metrics) Search(mode string) { m.searches.WithLabelValues(mode).Inc() }

// Handler serves the metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// Middleware records every tools/call the server receives, by tool for the
// server's own and as server.Unknown for any other name.
func (m *Metrics) Middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if method != "tools/call" || !ok || call.Params == nil {
				return next(ctx, method, req)
			}
			name := server.ToolName(call.Params.Name)
			start := time.Now()
			res, err := next(ctx, method, req)
			m.calls.WithLabelValues(name, server.Outcome(ctx, res, err)).Inc()
			m.duration.WithLabelValues(name).Observe(time.Since(start).Seconds())
			return res, err
		}
	}
}

// window keeps the largest age observed in the current servedWindow and in
// the one before it.
type window struct {
	mu        sync.Mutex
	start     time.Time
	cur, prev time.Duration
}

func (w *window) observe(now time.Time, age time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.roll(now)
	w.cur = max(w.cur, age)
}

func (w *window) value(now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.roll(now)
	return max(w.cur, w.prev)
}

// roll moves the window on to now.
func (w *window) roll(now time.Time) {
	switch elapsed := now.Sub(w.start); {
	case elapsed >= 2*servedWindow:
		w.start, w.cur, w.prev = now, 0, 0
	case elapsed >= servedWindow:
		w.start, w.cur, w.prev = w.start.Add(servedWindow), 0, w.cur
	}
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

// Name is the server's name in the MCP handshake.
const Name = "mcp-toolsearch-proxy"

// HealthPath answers liveness probes. It never contacts the upstream server.
const HealthPath = "/healthz"

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = time.Minute
	idleTimeout       = 2 * time.Minute
)

// MaxRequestBytes bounds a request body: room for a tool call carrying a
// large file in base64, with the JSON around it.
const MaxRequestBytes = 24 << 20

// instructions tell a client how to use the tools.
const instructions = "This server gives access to a large catalog of tools through a few. " +
	"Call search_tools with what you want to do, in any language, to find candidate tools; " +
	"it returns the input schemas of the best matches, so call describe_tool only for a tool returned without one; " +
	"then call a tool by name with its arguments: call_read_tool for tools marked readOnly " +
	"(call_read_tools for several at once), call_write_tool for any other. " +
	"Wrong arguments come back with the schema: fix them and call again. " +
	"Search again with other words when nothing fits: the catalog is larger than any one search shows."

// Caller calls a tool of the upstream server for the caller whose
// Authorization header value is authorization, with args, a JSON object or
// empty, as the caller sent it. An error that is an *upstream.Error tells
// whether the upstream answered and whether the call may have run.
type Caller interface {
	CallTool(ctx context.Context, authorization, name string, args json.RawMessage) (*mcp.CallToolResult, error)
}

// Options configures the server. A nil *Options is valid.
type Options struct {
	// Version is reported in the MCP handshake.
	Version string
	// Path is where the MCP endpoint is served; empty means "/mcp".
	Path string
	// AllowedOrigins are the browser origins, such as
	// "https://app.example.com", whose requests are served; a request with
	// any other Origin header is refused. Empty refuses every browser page.
	AllowedOrigins []string
	// AllowedHosts are the Host headers served when a request does not
	// arrive on a loopback address, each a name, matching any port, or a
	// name and port. Empty checks no Host there.
	AllowedHosts []string
	// Middleware wraps every request the server receives, such as to
	// record metrics.
	Middleware []mcp.Middleware
	// Logger receives the server's log, with a line for each tool call that
	// fails; nil discards. It never receives a query, arguments, a result
	// or a credential. The MCP transport's own records reach it from
	// warnings up.
	Logger *slog.Logger
	// MaxInFlight is how many requests the MCP endpoint serves at once; one
	// more is refused with 503 and Retry-After. Zero means
	// DefaultMaxInFlight.
	MaxInFlight int
	// MaxInFlightBytes is how many bytes of request bodies the MCP
	// endpoint serves at once; a request that does not fit is refused as
	// one over MaxInFlight is. Zero means DefaultMaxInFlightBytes.
	MaxInFlightBytes int64
	// OnSearch, if set, is told the mode of every search (see
	// catalog.ModeHybrid).
	OnSearch func(mode string)
	// OnRejected, if set, is told of every request the MCP endpoint
	// refuses for want of a slot (see MaxInFlight).
	OnRejected func()
	// OnInFlight, if set, is told 1 as the MCP endpoint starts serving a
	// request and -1 as it finishes.
	OnInFlight func(delta int)
	// OnMissedInstructions, if set, is told of every handshake
	// ProxyHandler answers without the upstream server's instructions
	// because the caller's catalog could not be had in time. A catalog
	// whose upstream gave no instructions is not one.
	OnMissedInstructions func()
	// MaxInstructions is how many characters of the upstream server's
	// instructions ProxyHandler forwards in the handshake, after the
	// proxy's own; longer instructions are cut, with a note saying so. Zero
	// forwards none.
	MaxInstructions int
	// HandshakeWait is how long ProxyHandler waits for a caller's first
	// catalog to take the upstream's instructions from, before answering
	// the handshake without them; zero means DefaultHandshakeWait. The
	// catalog's build carries on regardless.
	HandshakeWait time.Duration
	// SearchSchemas is how many of the best matches search_tools returns
	// with their input schemas, so a call can follow without
	// describe_tool. Zero returns none.
	SearchSchemas int
	// WriteTools are names of upstream tools, or patterns as path.Match
	// reads them, that are always treated as writes, whatever the upstream
	// marks them: call_read_tool refuses them. It only narrows what the
	// upstream's annotations allow.
	WriteTools []string
	// MaxBatchCalls is how many calls call_read_tools takes at once, at
	// most MaxBatchCallsLimit. Zero means DefaultMaxBatchCalls; negative
	// leaves call_read_tools out.
	MaxBatchCalls int
	// CallTimeout bounds a whole call_read_tools batch, as the upstream
	// client bounds each call; zero means upstream.DefaultCallTimeout.
	CallTimeout time.Duration
	// MaxResponseBytes bounds the results of a call_read_tools batch
	// together, as the upstream client bounds each; zero means
	// upstream.DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// KeepAlive is how often a response waiting on the upstream server
	// sends a comment, so that proxies in front see it is alive; zero means
	// DefaultKeepAlive.
	KeepAlive time.Duration
}

func (o *Options) withDefaults() Options {
	var out Options
	if o != nil {
		out = *o
	}
	if out.Path == "" {
		out.Path = "/mcp"
	}
	if out.Version == "" {
		out.Version = "devel"
	}
	if out.Logger == nil {
		out.Logger = slog.New(slog.DiscardHandler)
	}
	if out.MaxInFlight <= 0 {
		out.MaxInFlight = DefaultMaxInFlight
	}
	if out.MaxInFlightBytes <= 0 {
		out.MaxInFlightBytes = DefaultMaxInFlightBytes
	}
	if out.HandshakeWait <= 0 {
		out.HandshakeWait = DefaultHandshakeWait
	}
	if out.KeepAlive <= 0 {
		out.KeepAlive = DefaultKeepAlive
	}
	switch {
	case out.MaxBatchCalls == 0:
		out.MaxBatchCalls = DefaultMaxBatchCalls
	case out.MaxBatchCalls < 0:
		out.MaxBatchCalls = 0
	}
	out.MaxBatchCalls = min(out.MaxBatchCalls, MaxBatchCallsLimit)
	if out.CallTimeout <= 0 {
		out.CallTimeout = upstream.DefaultCallTimeout
	}
	if out.MaxResponseBytes <= 0 {
		out.MaxResponseBytes = upstream.DefaultMaxResponseBytes
	}
	if out.OnSearch == nil {
		out.OnSearch = func(string) {}
	}
	if out.OnRejected == nil {
		out.OnRejected = func() {}
	}
	if out.OnInFlight == nil {
		out.OnInFlight = func(int) {}
	}
	if out.OnMissedInstructions == nil {
		out.OnMissedInstructions = func() {}
	}
	return out
}

func (o *Options) transportLogger() *slog.Logger {
	return slog.New(minLevel{Handler: o.Logger.Handler(), min: slog.LevelWarn})
}

// minLevel drops records below min.
type minLevel struct {
	slog.Handler

	min slog.Level
}

func (h minLevel) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.Handler.Enabled(ctx, l)
}

func (h minLevel) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevel{Handler: h.Handler.WithAttrs(attrs), min: h.min}
}

func (h minLevel) WithGroup(name string) slog.Handler {
	return minLevel{Handler: h.Handler.WithGroup(name), min: h.min}
}

// New returns the MCP server with its tools, searching the catalogs
// of store and calling tools through caller. Its instructions are the
// proxy's own; ProxyHandler adds the upstream server's for each caller.
func New(opts *Options, store *catalog.Store, caller Caller) *mcp.Server {
	return newServer(opts.withDefaults(), store, caller, "")
}

// newServer returns the server New does, with upstream, the upstream
// server's instructions as forwarded, after its own.
func newServer(o Options, store *catalog.Store, caller Caller, upstream string) *mcp.Server {
	text := instructions
	if upstream != "" {
		text += upstreamPreamble + upstream
	}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: Name, Title: "Tool search", Version: o.Version},
		&mcp.ServerOptions{
			Logger: o.transportLogger(), Instructions: text,
			// The tools never change, so the server offers no list
			// changes: a client listening for them would hold a request
			// open, and with it one of the requests in flight.
			Capabilities: &mcp.ServerCapabilities{
				Tools: &mcp.ToolCapabilities{ListChanged: false},
			},
		},
	)
	srv.AddReceivingMiddleware(logFailures(o.Logger))
	srv.AddReceivingMiddleware(o.Middleware...)
	// Added last, so it wraps the others.
	srv.AddReceivingMiddleware(trackCalls)
	t := &tools{
		store: store, caller: caller, onSearch: o.OnSearch, schemas: o.SearchSchemas, writes: o.WriteTools,
		logger: o.Logger,
		batch:  batchLimits{calls: o.MaxBatchCalls, timeout: o.CallTimeout, bytes: o.MaxResponseBytes},
	}
	t.register(srv)
	return srv
}

// Handler serves srv at opts.Path over streamable HTTP without sessions,
// so a restart or a proxy in front never strands a client's session, and a
// liveness probe at HealthPath. Requests from browser pages, and off
// loopback from Host names not listed, are refused (see opts.AllowedOrigins
// and opts.AllowedHosts).
func Handler(srv *mcp.Server, opts *Options) http.Handler {
	return handler(func(*http.Request) *mcp.Server { return srv }, nil, opts)
}

// ProxyHandler serves the proxy as Handler does, answering each caller's
// handshake with the upstream server's instructions for that caller after
// its own (see Options.MaxInstructions). The proxy holds no credential, so
// it learns them from the caller's catalog, built with the caller's token;
// when that fails, as without a token or while the upstream is down, the
// handshake carries the proxy's own instructions alone.
func ProxyHandler(opts *Options, store *catalog.Store, caller Caller) http.Handler {
	p := &perCaller{o: opts.withDefaults(), store: store, caller: caller, servers: map[string]*mcp.Server{}}
	return handler(p.server, p.resolve, opts)
}

// DefaultHandshakeWait is how long a handshake waits by default for a
// caller's first catalog: long enough for a gateway's usual list, short
// against a client's connect timeout.
const DefaultHandshakeWait = 3 * time.Second

// upstreamKey holds, in a request's context, the upstream instructions
// resolved for it.
type upstreamKey struct{}

// maxServers bounds the servers perCaller keeps, one for each distinct
// text of upstream instructions.
const maxServers = 16

// perCaller keeps a server for each text of upstream instructions it has
// forwarded, since instructions are fixed when a server is made.
type perCaller struct {
	o       Options
	store   *catalog.Store
	caller  Caller
	mu      sync.Mutex
	servers map[string]*mcp.Server
}

// resolve finds the upstream instructions for a handshake once, before the
// transport asks for its server (which it may do more than once), and
// passes them on in the request's context. Only the handshake carries
// instructions, so no other request looks them up or waits for them.
func (p *perCaller) resolve(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.o.MaxInstructions > 0 {
			var hs bool
			if hs, r = handshake(r); hs {
				r = r.WithContext(context.WithValue(r.Context(), upstreamKey{}, p.instructions(r)))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// JSON-RPC methods whose answer carries the server's instructions.
const (
	methodInitialize = "initialize"
	methodDiscover   = "server/discover"
)

// maxPeek bounds how much of a body handshake reads: a handshake is far
// smaller, and a larger body, such as a tool call, is left unread.
const maxPeek = 64 << 10

// handshake reports whether r is a handshake, initialize or server/discover,
// and returns r with its body intact. A client of MCP 2026-07-28 names the
// method in the Mcp-Method header; for an older one, a short body is read
// for it.
func handshake(r *http.Request) (bool, *http.Request) {
	if r.Method != http.MethodPost {
		return false, r
	}
	if m := r.Header.Get("Mcp-Method"); m != "" {
		return m == methodInitialize || m == methodDiscover, r
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, maxPeek+1))
	r2 := r.Clone(r.Context())
	r2.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	if err != nil || len(head) > maxPeek {
		return false, r2
	}
	var one struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(head, &one) == nil {
		return one.Method == methodInitialize || one.Method == methodDiscover, r2
	}
	var batch []struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(head, &batch) == nil {
		for _, m := range batch {
			if m.Method == methodInitialize || m.Method == methodDiscover {
				return true, r2
			}
		}
	}
	return false, r2
}

// instructions returns the upstream instructions to forward to the caller
// of r, capped. A kept catalog lends its own at once, up to its max age, since
// they rarely change. Without one, a build starts and is waited for at
// most HandshakeWait; it carries on regardless and fills the store, so
// the caller's next search does not wait for it either.
func (p *perCaller) instructions(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if c, ok := p.store.Cached(auth); ok {
		return capInstructions(c.Instructions(), p.o.MaxInstructions)
	}
	built := make(chan *catalog.Catalog, 1)
	go func() {
		if c, err := p.store.Get(context.WithoutCancel(r.Context()), auth, false); err == nil {
			built <- c
		}
		close(built)
	}()
	timer := time.NewTimer(p.o.HandshakeWait)
	defer timer.Stop()
	select {
	case c, ok := <-built:
		if ok {
			return capInstructions(c.Instructions(), p.o.MaxInstructions)
		}
	case <-timer.C:
	case <-r.Context().Done():
	}
	p.o.OnMissedInstructions()
	return ""
}

func (p *perCaller) server(r *http.Request) *mcp.Server {
	upstream, _ := r.Context().Value(upstreamKey{}).(string)
	p.mu.Lock()
	defer p.mu.Unlock()
	if srv, ok := p.servers[upstream]; ok {
		return srv
	}
	if len(p.servers) >= maxServers {
		clear(p.servers)
	}
	srv := newServer(p.o, p.store, p.caller, upstream)
	p.servers[upstream] = srv
	return srv
}

// upstreamPreamble introduces the upstream server's instructions.
const upstreamPreamble = "\n\nThe upstream server's own instructions follow. They describe the tools reached " +
	"through search_tools and the call tools, by their upstream names.\n\n"

// cutNote ends upstream instructions cut at the limit.
const cutNote = "\n\n[The upstream server's instructions are cut here: they are longer than this proxy forwards.]"

// capInstructions returns s, or its first limit characters followed by
// cutNote when it is longer.
func capInstructions(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + cutNote
}

// handler serves the MCP endpoint with a server from getServer, through
// wrap when it is set, behind the guard and the limit on requests.
//
// Responses are streams of server-sent events rather than single JSON
// bodies, so that a tool waiting long on the upstream can send the headers
// at once and comments until its result (see startKeepAlive): a proxy in
// front that times out waiting for headers, or for bytes, would otherwise
// give up on a call the upstream is still serving.
func handler(
	getServer func(*http.Request) *mcp.Server,
	wrap func(http.Handler) http.Handler,
	opts *Options,
) http.Handler {
	o := opts.withDefaults()
	mux := http.NewServeMux()
	var mcpHandler http.Handler = mcp.NewStreamableHTTPHandler(
		getServer,
		&mcp.StreamableHTTPOptions{
			Stateless:           true,
			JSONResponse:        false,
			Logger:              o.transportLogger(),
			MaxRequestBodyBytes: MaxRequestBytes,
		},
	)
	if wrap != nil {
		mcpHandler = wrap(mcpHandler)
	}
	mcpHandler = keepAlive(o.KeepAlive, mcpHandler)
	mux.Handle(o.Path, limit(o.MaxInFlight, o.MaxInFlightBytes, mcpHandler, o.OnRejected, o.OnInFlight))
	mux.HandleFunc("GET "+HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return newGuard(mux, o.AllowedOrigins, o.AllowedHosts)
}

// HTTPServer returns an *http.Server for h on addr with conservative
// timeouts: for a request's headers, for the whole request with its body,
// and for a connection left idle. Write timeouts are left to each request's
// context, since an upstream tool call may take minutes.
func HTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}

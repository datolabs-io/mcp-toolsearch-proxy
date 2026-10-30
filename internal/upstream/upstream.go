package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Default deadlines for a request to the upstream server.
const (
	DefaultListTimeout = 15 * time.Second
	DefaultCallTimeout = 2 * time.Minute
)

// DefaultListRetries is how many times a failed listing is tried again.
const DefaultListRetries = 3

// retryDelay is the pause before the first retry of a listing; each one
// after waits twice as long as the one before.
const retryDelay = 250 * time.Millisecond

// Default limits on what the upstream server sends: the bytes of the
// responses to one operation, a tool call or a whole listing, and the pages
// and tools of a listing. A response is held whole, and decoded and encoded
// again on its way to the caller, so one large result costs about three
// times its size.
const (
	DefaultMaxResponseBytes = 32 << 20
	DefaultMaxListPages     = 100
	DefaultMaxListTools     = 10000
)

// ErrTooLarge is an upstream answer over one of the client's limits.
var ErrTooLarge = errors.New("the upstream's answer is over the proxy's limit")

// ErrUpstream wraps every failure to reach the upstream server or to get an
// answer from it, as opposed to a tool that answered with an error.
var ErrUpstream = errors.New("upstream")

// Options configures a Client. A nil *Options is valid.
type Options struct {
	// HTTPClient sends the requests; nil means http.DefaultClient's
	// transport with no overall timeout, since the deadlines below bound
	// each request.
	HTTPClient *http.Client
	// Name and Version identify the proxy to the upstream server.
	Name, Version string
	// ListTimeout bounds listing the tools; CallTimeout a tool call. Zero
	// means the defaults.
	ListTimeout, CallTimeout time.Duration
	// Host, when set, is the Host header of every request instead of the
	// endpoint's, for an upstream reached at an internal address whose
	// servers accept only their public name.
	Host string
	// ListRetries is how many times a failed listing is tried again within
	// ListTimeout. Zero means DefaultListRetries; negative, none.
	ListRetries int
	// ProtocolVersion is the MCP version to speak upstream, such as
	// "2025-11-25"; empty means the newest the SDK supports. An older
	// version helps an upstream that fails the newest handshake.
	ProtocolVersion string
	// OnRequest, if set, is told of every operation upstream as it ends:
	// OpList or OpCall, OutcomeOK or the Class of its *Error, and how long
	// it took, a listing's retries included. A tool that answers with an
	// error result is OutcomeOK: the upstream answered.
	OnRequest func(op, outcome string, took time.Duration)
	// MaxResponseBytes bounds the response bodies the upstream server
	// sends for one operation after the handshake: a tool call's result,
	// or every page of a listing together. One that sends more fails with
	// ErrTooLarge. Zero means DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// MaxListPages and MaxListTools bound a listing; one with more fails
	// with ErrTooLarge. Zero means the defaults.
	MaxListPages, MaxListTools int
}

// Operations, as Options.OnRequest is told them.
const (
	OpList = "list"
	OpCall = "call"
)

// OutcomeOK is an operation upstream that got its answer.
const OutcomeOK = "ok"

// Client reaches one upstream MCP server on behalf of callers, each request
// carrying the caller's own Authorization header and no credential of the
// proxy's. It speaks the newest protocol the SDK knows and falls back to
// older ones as the server requires. It is safe for concurrent use.
type Client struct {
	endpoint string
	base     *http.Client
	impl     *mcp.Implementation
	list     time.Duration
	call     time.Duration
	host     string
	protocol string
	retries  int
	report   func(op, outcome string, took time.Duration)
	maxBody  int64
	maxPages int
	maxTools int
}

// New returns a client of the MCP server at endpoint, a streamable HTTP URL.
func New(endpoint string, opts *Options) *Client {
	var o Options
	if opts != nil {
		o = *opts
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
	if o.Name == "" {
		o.Name = "mcp-toolsearch-proxy"
	}
	if o.Version == "" {
		o.Version = "devel"
	}
	if o.ListTimeout <= 0 {
		o.ListTimeout = DefaultListTimeout
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if o.MaxListPages <= 0 {
		o.MaxListPages = DefaultMaxListPages
	}
	if o.MaxListTools <= 0 {
		o.MaxListTools = DefaultMaxListTools
	}
	switch {
	case o.ListRetries == 0:
		o.ListRetries = DefaultListRetries
	case o.ListRetries < 0:
		o.ListRetries = 0
	}
	if o.OnRequest == nil {
		o.OnRequest = func(string, string, time.Duration) {}
	}
	return &Client{
		endpoint: endpoint, base: o.HTTPClient,
		impl: &mcp.Implementation{Name: o.Name, Version: o.Version},
		list: o.ListTimeout, call: o.CallTimeout, host: o.Host, protocol: o.ProtocolVersion,
		retries: o.ListRetries, report: o.OnRequest, maxBody: o.MaxResponseBytes, maxPages: o.MaxListPages,
		maxTools: o.MaxListTools,
	}
}

// Listing is what the upstream server offers a caller: its tools, and the
// instructions it gave in the handshake.
type Listing struct {
	Tools        []*mcp.Tool
	Instructions string
}

// List returns every tool the upstream server lists for the caller whose
// Authorization header value is authorization, following pages, with the
// server's instructions. A listing that fails is tried again, up to the
// client's retries and with a growing pause, unless the upstream refused
// the caller or the listing is over the client's limits: a gateway's
// fan-out to many servers fails now and then on its own, and listing again
// changes neither.
func (c *Client) List(ctx context.Context, authorization string) (Listing, error) {
	start := time.Now()
	out, err := c.listAll(ctx, authorization)
	c.done(OpList, start, err)
	return out, err
}

// ListTools returns the tools of List.
func (c *Client) ListTools(ctx context.Context, authorization string) ([]*mcp.Tool, error) {
	l, err := c.List(ctx, authorization)
	return l.Tools, err
}

// CallTool calls the upstream tool name with args, a JSON object or empty,
// for the caller whose Authorization header value is authorization, and
// returns its result as the upstream server gave it. The arguments are sent
// as they are, byte for byte, so numbers keep their precision. A tool that
// fails reports so in the result; an error, an *Error, means the call got
// no result, as for a result over the client's limit (ErrTooLarge). A call
// is never retried, since one that failed may still have had its effect.
func (c *Client) CallTool(
	ctx context.Context, authorization, name string, args json.RawMessage,
) (*mcp.CallToolResult, error) {
	start := time.Now()
	res, err := c.callOnce(ctx, authorization, name, args)
	c.done(OpCall, start, err)
	return res, err
}

// listAll is List, unreported.
func (c *Client) listAll(ctx context.Context, authorization string) (Listing, error) {
	ctx, cancel := detach(ctx, c.list)
	defer cancel()
	delay := retryDelay
	for retry := 0; ; retry++ {
		out, refused, err := c.listOnce(ctx, authorization)
		if err == nil {
			return out, nil
		}
		if refused || retry == c.retries || errors.Is(err, ErrTooLarge) {
			return out, classify(err, refused, true)
		}
		select {
		case <-ctx.Done():
			return Listing{}, classify(fmt.Errorf("%w (%w)", err, ctx.Err()), false, true)
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// callOnce is CallTool, unreported.
func (c *Client) callOnce(
	ctx context.Context, authorization, name string, args json.RawMessage,
) (*mcp.CallToolResult, error) {
	ctx, cancel := detach(ctx, c.call)
	defer cancel()
	t := c.transport(authorization)
	cs, err := c.connect(ctx, t)
	if err != nil {
		return nil, classify(t.over(err), t.refused.Load(), false)
	}
	defer func() { _ = cs.Close() }()
	// What counts from here is how the call went.
	t.start()
	params := &mcp.CallToolParams{Name: name}
	if len(args) > 0 {
		params.Arguments = args
	}
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		return nil, classify(t.over(fmt.Errorf("%w: call tool: %w", ErrUpstream, err)), t.refused.Load(), true)
	}
	return res, nil
}

// done reports the operation op, which started at start and ended with
// err.
func (c *Client) done(op string, start time.Time, err error) {
	outcome := OutcomeOK
	if err != nil {
		outcome = ClassDropped
		var e *Error
		if errors.As(err, &e) {
			outcome = e.Class
		}
	}
	c.report(op, outcome, time.Since(start))
}

// Classes of failure to get a result from the upstream server, coarse
// enough to tell a model without the transport's detail.
const (
	// ClassAnswered is an upstream that answered with a JSON-RPC error.
	ClassAnswered = "answered"
	// ClassTimeout is a request that got no answer in time.
	ClassTimeout = "timeout"
	// ClassRefused is an upstream that could not be connected to.
	ClassRefused = "refused"
	// ClassUnauthorized is an upstream that refused the caller's
	// credential, with 401 or 403.
	ClassUnauthorized = "unauthorized"
	// ClassDropped is any other failure: a connection that broke, or an
	// HTTP error without an answer.
	ClassDropped = "dropped"
	// ClassTooLarge is an answer over the client's limits (ErrTooLarge).
	ClassTooLarge = "too_large"
)

// Error is a failure to get a result from the upstream server. Its text
// holds the transport's detail, such as the upstream's address, for the
// log; Class, Sent and Answer are what may be told to the caller.
type Error struct {
	// Class is one of the Class constants.
	Class string
	// Sent is false when the failure came before the request was sent, so
	// a tool call cannot have run. True means it may have reached the tool.
	Sent bool
	// Answer is the upstream's error message, when it answered with one.
	Answer string
	// Code is the JSON-RPC error code of that answer, such as
	// jsonrpc.CodeInvalidParams; zero without one.
	Code int64

	err error
	// answered says the error holds a message of the upstream's, as an
	// HTTP error with a JSON-RPC body does whatever its class.
	answered bool
}

func (e *Error) Error() string { return e.err.Error() }

// Detail is the error's text for the log: the transport's detail, or
// nothing when it holds a message of the upstream's, which may repeat a
// call's arguments.
func (e *Error) Detail() string {
	if e.answered || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *Error) Unwrap() error { return e.err }

// classify returns err as an *Error, where refused says whether the
// upstream answered a request with 401 or 403, and sent whether the
// request may have reached the upstream.
func classify(err error, refused, sent bool) *Error {
	e := &Error{Class: ClassDropped, Sent: sent, err: err}
	var wire *jsonrpc.Error
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var netErr net.Error
	e.answered = errors.As(err, &wire) && !local(wire)
	switch {
	case refused:
		e.Class = ClassUnauthorized
	case errors.Is(err, ErrTooLarge):
		e.Class = ClassTooLarge
	case e.answered:
		e.Class, e.Answer, e.Code = ClassAnswered, wire.Message, wire.Code
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		e.Class = ClassTimeout
	case errors.Is(err, syscall.ECONNREFUSED) || errors.As(err, &dnsErr) ||
		(errors.As(err, &opErr) && opErr.Op == "dial"):
		e.Class = ClassRefused
	}
	return e
}

// JSON-RPC error codes the SDK makes up on the proxy's side of the
// connection, for a request its transport rejected or a connection closing:
// no answer of the upstream's.
const (
	codeClientClosing = -32003
	codeServerClosing = -32004
	codeRejected      = -32005
)

// local reports whether wire is an error of the SDK's own making rather than
// the upstream's answer.
func local(wire *jsonrpc.Error) bool {
	switch wire.Code {
	case codeClientClosing, codeServerClosing, codeRejected:
		return true
	default:
		return false
	}
}

// listOnce lists the tools on one connection, and says whether the upstream
// refused the caller. The connection is closed in the background: closing
// a session of a stateful upstream waits for its DELETE, which would hold
// up the catalog or the next try.
func (c *Client) listOnce(ctx context.Context, authorization string) (Listing, bool, error) {
	t := c.transport(authorization)
	cs, err := c.connect(ctx, t)
	if err != nil {
		return Listing{}, t.refused.Load(), t.over(err)
	}
	defer func() { go func() { _ = cs.Close() }() }()
	// What counts from here is how the listing went.
	t.start()
	var out Listing
	if res := cs.InitializeResult(); res != nil {
		out.Instructions = res.Instructions
	}
	params := &mcp.ListToolsParams{}
	for page := 1; ; page++ {
		res, lerr := cs.ListTools(ctx, params)
		if lerr != nil {
			return Listing{}, t.refused.Load(), t.over(fmt.Errorf("%w: list tools: %w", ErrUpstream, lerr))
		}
		if len(out.Tools)+len(res.Tools) > c.maxTools {
			return Listing{}, false, fmt.Errorf("%w: %w: more than %d tools", ErrUpstream, ErrTooLarge, c.maxTools)
		}
		out.Tools = append(out.Tools, res.Tools...)
		if res.NextCursor == "" {
			return out, false, nil
		}
		if page == c.maxPages {
			return Listing{}, false, fmt.Errorf("%w: %w: more than %d pages of tools", ErrUpstream, ErrTooLarge,
				c.maxPages)
		}
		params.Cursor = res.NextCursor
	}
}

// detach returns a context for a request upstream: it ends when parent
// does or after timeout, but carries none of parent's values. A caller's
// context may hold what the SDK knows of the caller's own MCP session, such
// as its protocol version, which go-sdk v1.8.0 would send on the upstream
// connection's fallback initialize; an upstream then rightly refuses an
// initialize that claims MCP 2026-07-28. It also drops trace context, so
// once a go-sdk release stops the leak (go-sdk #1165), use the caller's
// context with the timeout instead (issue #11).
func detach(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// transport returns the round tripper for one connection whose requests
// carry authorization.
func (c *Client) transport(authorization string) *withAuthorization {
	t := &withAuthorization{next: c.base.Transport, value: authorization, host: c.host, limit: c.maxBody}
	t.start()
	return t
}

// connect opens a connection to the upstream server whose requests go
// through t. Each is used for one operation and closed: the protocol
// keeps no state between requests, and a connection kept per caller would
// outlive the token it carries.
func (c *Client) connect(ctx context.Context, t *withAuthorization) (*mcp.ClientSession, error) {
	hc := *c.base
	hc.Transport = t
	client := mcp.NewClient(c.impl, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: c.endpoint, HTTPClient: &hc, DisableStandaloneSSE: true, MaxRetries: -1,
	}, &mcp.ClientSessionOptions{ProtocolVersion: c.protocol})
	if err != nil {
		return nil, fmt.Errorf("%w: connect: %w", ErrUpstream, err)
	}
	return cs, nil
}

// withAuthorization sets the Authorization header of every request it sends,
// or removes it when value is empty, so a request never carries anything
// but the caller's own credential, and its Host when host is set. It
// notes whether the upstream refused the caller, and cuts off the response
// bodies of an operation once they add up to more than limit, noting that
// too: the SDK reads a body whole, and a listing keeps every page.
type withAuthorization struct {
	next     http.RoundTripper
	value    string
	host     string
	limit    int64
	left     atomic.Int64 // bytes of the limit not yet read
	refused  atomic.Bool
	tooLarge atomic.Bool
}

func (t *withAuthorization) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.value == "" {
		r.Header.Del("Authorization")
	} else {
		r.Header.Set("Authorization", t.value)
	}
	if t.host != "" {
		r.Host = t.host
	}
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(r)
	if err != nil {
		return resp, err //nolint:wrapcheck // the SDK reports transport errors itself
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.refused.Store(true)
	}
	if resp.ContentLength > t.left.Load() {
		_ = resp.Body.Close()
		t.tooLarge.Store(true)
		return nil, ErrTooLarge
	}
	resp.Body = &limitedBody{ReadCloser: resp.Body, t: t}
	return resp, nil
}

// start begins an operation once connected: a refused server/discover is
// followed by an initialize that worked, and neither the handshake's
// responses nor their failures count against what follows.
func (t *withAuthorization) start() {
	t.refused.Store(false)
	t.tooLarge.Store(false)
	t.left.Store(t.limit)
}

// over returns err as an ErrTooLarge when a response was cut off at the
// limit, since the SDK reports a failed read without its cause.
func (t *withAuthorization) over(err error) error {
	if err != nil && t.tooLarge.Load() && !errors.Is(err, ErrTooLarge) {
		return fmt.Errorf("%w: responses over %d bytes: %w", ErrTooLarge, t.limit, err)
	}
	return err
}

// limitedBody reads a response body while the bytes left of t's limit
// last, and fails with ErrTooLarge, noting so in t, when there is more.
type limitedBody struct {
	io.ReadCloser

	t *withAuthorization
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if left := max(b.t.left.Load(), 0); int64(len(p)) > left+1 {
		p = p[:left+1]
	}
	n, err := b.ReadCloser.Read(p)
	if left := b.t.left.Add(-int64(n)); left < 0 {
		b.t.tooLarge.Store(true)
		return max(n+int(left), 0), ErrTooLarge
	}
	return n, err //nolint:wrapcheck // the body's own error, as io.Reader returns it
}

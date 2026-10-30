package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/embed"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/metrics"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

const (
	exitFailure = 1
	exitUsage   = 2
	// fetchTimeout bounds downloading the model.
	fetchTimeout = 30 * time.Minute
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := parseConfig(args, getenv, stderr)
	if err != nil {
		return exitUsage
	}
	logger := newLogger(stderr, cfg.logLevel, cfg.logFormat)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.command == cmdFetchModel {
		return fetchModel(ctx, cfg, logger)
	}
	return serve(ctx, cfg, logger)
}

// newLogger returns a logger writing records from level up to w, as JSON
// when format is logJSON and as text otherwise.
func newLogger(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == logJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// fetchModel downloads the built-in model into cfg.modelDir.
func fetchModel(ctx context.Context, cfg config, logger *slog.Logger) int {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	src := embed.DefaultSource
	logger.InfoContext(ctx, "fetching model", "model", src.Name, "dir", cfg.modelDir)
	if err := embed.Fetch(ctx, &http.Client{}, src, cfg.modelDir); err != nil {
		logger.ErrorContext(ctx, "fetch model", "err", err)
		return exitFailure
	}
	logger.InfoContext(ctx, "model ready", "model", src.Name, "dir", cfg.modelDir)
	return 0
}

// loadEmbedder loads the embedder cfg names, or returns nil for none.
//
//nolint:ireturn // the store takes any embedder
func loadEmbedder(ctx context.Context, cfg config, logger *slog.Logger) (catalog.Embedder, error) {
	if cfg.embedder == embedNone {
		logger.InfoContext(ctx, "semantic search off: searching lexically only")
		return nil, nil //nolint:nilnil // no embedder is a valid choice
	}
	start := time.Now()
	m, err := embed.LoadStatic(cfg.modelDir, embed.DefaultSource.Name)
	if err != nil {
		return nil, errors.New("the built-in model could not be loaded from " + cfg.modelDir +
			": run mcp-toolsearch-proxy fetch-model, or set -embedder none (" + err.Error() + ")")
	}
	logger.InfoContext(ctx, "model loaded", "model", m.Name(), "dim", m.Dim(),
		"took", time.Since(start).Round(time.Millisecond))
	return m, nil
}

// options returns what cfg configures of the upstream client, the catalog
// store and the server, before metrics are wired to them.
func options(
	cfg config, emb catalog.Embedder, logger *slog.Logger,
) (*upstream.Options, *catalog.StoreOptions, *server.Options) {
	upstreamOpts := &upstream.Options{
		Name: server.Name, Version: buildVersion(), CallTimeout: cfg.callTimeout, Host: cfg.upstreamHost,
		ListRetries: listRetries(cfg.listRetries), ListTimeout: cfg.listTimeout,
		ProtocolVersion: cfg.upstreamProtocol, MaxResponseBytes: cfg.maxResponseBytes, MaxListTools: cfg.maxTools,
	}
	storeOpts := &catalog.StoreOptions{
		Embedder: emb, TTL: cfg.catalogTTL, MaxAge: cfg.catalogMaxAge, MaxSize: cfg.maxCatalogs, Logger: logger,
	}
	opts := &server.Options{
		Version: buildVersion(), Path: cfg.path, Logger: logger, MaxInFlight: cfg.maxInFlight,
		AllowedOrigins: cfg.allowedOrigins, AllowedHosts: cfg.allowedHosts,
		MaxInstructions: cfg.maxInstructions, SearchSchemas: cfg.searchSchemas, HandshakeWait: cfg.handshakeWait,
		WriteTools: cfg.writeTools, MaxInFlightBytes: cfg.maxInFlightBytes,
		MaxBatchCalls: batchCalls(cfg.maxBatchCalls), CallTimeout: cfg.callTimeout,
		MaxResponseBytes: cfg.maxResponseBytes,
	}
	return upstreamOpts, storeOpts, opts
}

// batchCalls is the server's MaxBatchCalls for n, where 0 leaves
// call_read_tools out: the server's zero is the default.
func batchCalls(n int) int {
	if n == 0 {
		return -1
	}
	return n
}

// serve serves the proxy over HTTP, and metrics on their own address when
// one is set.
func serve(ctx context.Context, cfg config, logger *slog.Logger) int {
	emb, err := loadEmbedder(ctx, cfg, logger)
	if err != nil {
		logger.ErrorContext(ctx, "embedder", "err", err)
		return exitFailure
	}
	upstreamOpts, storeOpts, opts := options(cfg, emb, logger)
	var servers []*http.Server
	var store *catalog.Store
	builds := newBuildLog(logger, buildLogWindow, time.Now)
	storeOpts.OnBuild = func(n int, _ time.Duration, err error) { builds.record(ctx, n, err) }
	if cfg.metricsAddr != "" {
		m := metrics.New(&metrics.Options{
			Catalogs: func() int { return store.Len() }, MaxTools: func() int { return store.MaxTools() },
			TTL: cfg.catalogTTL,
		})
		m.BuildInfo(buildVersion())
		storeOpts.OnBuild = func(n int, took time.Duration, err error) {
			builds.record(ctx, n, err)
			m.Build(n, took, err)
		}
		storeOpts.OnServe, upstreamOpts.OnRequest = m.Serve, m.Upstream
		opts.OnSearch, opts.OnRejected, opts.OnInFlight = m.Search, m.Rejected, m.InFlight
		opts.OnMissedInstructions = m.MissedInstructions
		opts.Middleware = append(opts.Middleware, m.Middleware())
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", m.Handler())
		servers = append(servers, server.HTTPServer(cfg.metricsAddr, mux))
	}
	client := upstream.New(cfg.upstream, upstreamOpts)
	store = catalog.NewStore(func(ctx context.Context, authorization string) (catalog.Listing, error) {
		l, lerr := client.List(ctx, authorization)
		return catalog.Listing{Tools: l.Tools, Instructions: l.Instructions}, lerr
	}, storeOpts)
	// The proxy's server comes last, after the metrics' when there is one.
	servers = append(servers, server.HTTPServer(cfg.addr, server.ProxyHandler(opts, store, client)))

	listeners, err := listen(ctx, servers)
	if err != nil {
		logger.ErrorContext(ctx, "server stopped", "err", err)
		return exitFailure
	}
	errc := start(servers, listeners)
	metricsAddr := ""
	if len(listeners) > 1 {
		metricsAddr = listeners[0].Addr().String()
	}
	logger.InfoContext(ctx, "listening", "addr", listeners[len(listeners)-1].Addr().String(), "path", cfg.path,
		"metrics", metricsAddr, "upstream", redact(cfg.upstream), "embedder", cfg.embedder)

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	// Requests running get the grace to finish: closing their connections
	// would cancel tool calls upstream, which may still complete there while
	// the client sees no result.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.shutdownGrace)
	defer cancel()
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Go(func() {
			if serr := srv.Shutdown(shutdownCtx); serr != nil {
				_ = srv.Close()
			}
		})
	}
	wg.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.ErrorContext(ctx, "server stopped", "err", err)
		return exitFailure
	}
	return 0
}

// listen opens a listener for each server's address, in order, so the log
// can name the addresses bound, a port 0 resolved; on an error it closes
// those it opened.
func listen(ctx context.Context, servers []*http.Server) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(servers))
	for _, srv := range servers {
		l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", srv.Addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return nil, fmt.Errorf("listen: %w", err)
		}
		listeners = append(listeners, l)
	}
	return listeners, nil
}

// start serves each server on its listener, and returns the channel each
// sends its error to when it stops.
func start(servers []*http.Server, listeners []net.Listener) <-chan error {
	errc := make(chan error, len(servers))
	for i, srv := range servers {
		go func() { errc <- srv.Serve(listeners[i]) }()
	}
	return errc
}

// buildVersion is the version set at build time, or the module version Go
// recorded, or "devel".
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "devel"
}

// redact returns raw, a URL, without credentials or a query, for the log.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// listRetries is the upstream.Options value for n retries, where 0 is none.
func listRetries(n int) int {
	if n == 0 {
		return -1
	}
	return n
}

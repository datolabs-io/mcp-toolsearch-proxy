package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/server"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/upstream"
)

var errUsage = errors.New("usage")

// Commands.
const (
	cmdServe      = "serve"
	cmdFetchModel = "fetch-model"
)

// Embedders.
const (
	embedBuiltin = "builtin"
	embedNone    = "none"
)

// Defaults of what the proxy passes on: the upstream server's instructions
// up to this many characters, and the input schemas of this many of the
// best search matches.
const (
	defaultMaxInstructions = 10000
	defaultSearchSchemas   = 3
)

// shutdownMargin is how much longer than a tool call may take the proxy
// waits, by default, for the requests running when it stops.
const shutdownMargin = 15 * time.Second

// protocolLatest asks for the newest MCP version the SDK supports.
const protocolLatest = "latest"

// Log formats.
const (
	logText = "text"
	logJSON = "json"
)

type config struct {
	command      string
	addr         string
	path         string
	metricsAddr  string
	maxInFlight  int
	upstream     string
	upstreamHost string
	// upstreamProtocol is the MCP version spoken upstream; empty for the
	// newest the SDK supports.
	upstreamProtocol string
	embedder         string
	modelDir         string
	catalogTTL       time.Duration
	catalogMaxAge    time.Duration
	maxCatalogs      int
	maxInstructions  int
	searchSchemas    int
	maxBatchCalls    int
	callTimeout      time.Duration
	listRetries      int
	listTimeout      time.Duration
	handshakeWait    time.Duration
	// shutdownGrace is how long requests running get to finish when the
	// proxy stops.
	shutdownGrace time.Duration
	logLevel      slog.Level
	logFormat     string

	// Origins and Host names served (see server.Options).
	allowedOrigins, allowedHosts []string
	// writeTools are names or patterns of tools always treated as writes.
	writeTools []string
	// Memory bounds: request bodies in flight, an upstream response, and
	// the tools of a listing.
	maxInFlightBytes, maxResponseBytes int64
	maxTools                           int
}

// parseConfig reads an optional command and flags from args, with defaults
// from getenv.
func parseConfig(args []string, getenv func(string) string, out io.Writer) (config, error) {
	return parseConfigWith(args, getenv, out, nil)
}

// parseConfigWith is parseConfig, handing the flag set to inspect, when it
// is set, before parsing: the configuration reference is generated from it.
func parseConfigWith(
	args []string, getenv func(string) string, out io.Writer, inspect func(*flag.FlagSet),
) (config, error) {
	command := cmdServe
	if len(args) > 0 && (args[0] == cmdServe || args[0] == cmdFetchModel) {
		command, args = args[0], args[1:]
	}
	env := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}
	var cfg config
	var level, inFlight, ttl, maxCatalogs, callTimeout, maxInstructions, searchSchemas, listRetries string
	fs := newFlagSet(out)
	fs.StringVar(&cfg.addr, "addr", env("MCP_ADDR", "127.0.0.1:8000"), "listen address (MCP_ADDR)")
	fs.StringVar(&cfg.path, "path", env("MCP_PATH", "/mcp"), "MCP endpoint path (MCP_PATH)")
	fs.StringVar(&cfg.metricsAddr, "metrics-addr", env("MCP_METRICS_ADDR", ""),
		"listen address for Prometheus metrics at /metrics; empty for none (MCP_METRICS_ADDR)")
	inFlight = env("MCP_MAX_IN_FLIGHT", strconv.Itoa(server.DefaultMaxInFlight))
	fs.StringVar(&inFlight, "max-in-flight", inFlight,
		"how many MCP requests are served at once; one more is refused with 503 (MCP_MAX_IN_FLIGHT)")
	fs.StringVar(&cfg.upstream, "upstream", env("TOOLSEARCH_UPSTREAM", ""),
		"streamable HTTP URL of the MCP server whose tools are searched, such as a gateway (TOOLSEARCH_UPSTREAM)")
	fs.StringVar(&cfg.upstreamHost, "upstream-host", env("TOOLSEARCH_UPSTREAM_HOST", ""),
		"Host header for upstream requests instead of the URL's, for a gateway reached at an internal address "+
			"whose servers accept only its public name (TOOLSEARCH_UPSTREAM_HOST)")
	fs.StringVar(&cfg.upstreamProtocol, "upstream-protocol", env("TOOLSEARCH_UPSTREAM_PROTOCOL", protocolLatest),
		"MCP version to speak upstream, such as 2025-11-25, or latest for the newest supported; an older one helps "+
			"a gateway whose servers fail the newest handshake (TOOLSEARCH_UPSTREAM_PROTOCOL)")
	fs.StringVar(&cfg.embedder, "embedder", env("TOOLSEARCH_EMBEDDER", embedBuiltin),
		"semantic search: builtin, the model fetch-model downloads, or none for lexical search only "+
			"(TOOLSEARCH_EMBEDDER)")
	fs.StringVar(&cfg.modelDir, "model-dir", env("TOOLSEARCH_MODEL_DIR", defaultModelDir(getenv)),
		"directory of the built-in model, as fetch-model writes it (TOOLSEARCH_MODEL_DIR)")
	ttl = env("TOOLSEARCH_CATALOG_TTL", catalog.DefaultTTL.String())
	fs.StringVar(&ttl, "catalog-ttl", ttl,
		"how long a caller's catalog is used before the tools are listed again; an older one is still used while "+
			"they are listed again in the background, up to -catalog-max-age; call_read_tool's read-only check reads "+
			"it, so it may lag the upstream by that long (TOOLSEARCH_CATALOG_TTL)")
	maxCatalogs = env("TOOLSEARCH_MAX_CATALOGS", strconv.Itoa(catalog.DefaultMaxSize))
	fs.StringVar(&maxCatalogs, "max-catalogs", maxCatalogs,
		"how many callers' catalogs are kept; each new token is a new caller (TOOLSEARCH_MAX_CATALOGS)")
	maxInstructions = env("TOOLSEARCH_MAX_INSTRUCTIONS", strconv.Itoa(defaultMaxInstructions))
	fs.StringVar(&maxInstructions, "max-instructions", maxInstructions,
		"how many characters of the upstream server's instructions the handshake forwards; longer ones are cut, "+
			"0 forwards none (TOOLSEARCH_MAX_INSTRUCTIONS)")
	searchSchemas = env("TOOLSEARCH_SEARCH_SCHEMAS", strconv.Itoa(defaultSearchSchemas))
	fs.StringVar(&searchSchemas, "search-schemas", searchSchemas,
		"how many of the best search matches come with their input schemas; 0 for none (TOOLSEARCH_SEARCH_SCHEMAS)")
	batch := batchFlag(fs, env)
	callTimeout = env("TOOLSEARCH_CALL_TIMEOUT", upstream.DefaultCallTimeout.String())
	fs.StringVar(&callTimeout, "call-timeout", callTimeout,
		"how long a call to an upstream tool may take (TOOLSEARCH_CALL_TIMEOUT)")
	listRetries = env("TOOLSEARCH_LIST_RETRIES", strconv.Itoa(upstream.DefaultListRetries))
	fs.StringVar(&listRetries, "list-retries", listRetries,
		"how many times a failed listing of the upstream's tools is tried again, waiting longer each time; "+
			"0 for none (TOOLSEARCH_LIST_RETRIES)")
	fs.StringVar(&level, "log-level", env("MCP_LOG_LEVEL", "info"),
		"least severe log records written: debug, info, warn or error (MCP_LOG_LEVEL)")
	fs.StringVar(&cfg.logFormat, "log-format", env("MCP_LOG_FORMAT", logText),
		"log format: text or json (MCP_LOG_FORMAT)")
	allowed, shutdown, timings := allowedFlags(fs, env), shutdownFlag(fs, env), timingFlags(fs, env)
	writes, sizes := writeToolsFlag(fs, env), sizeFlags(fs, env)
	if inspect != nil {
		inspect(fs)
	}
	if err := fs.Parse(args); err != nil {
		return config{}, errUsage
	}
	cfg.command = command
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(out, "unexpected argument %q\n", fs.Arg(0))
		return config{}, errUsage
	}
	for _, check := range []func() error{
		func() error { return cfg.settleLog(level) },
		func() error { return cfg.settle(inFlight, ttl, maxCatalogs, callTimeout) },
		func() error { return shutdown(&cfg) },
		func() error { return cfg.settleCounts(maxInstructions, searchSchemas, listRetries) },
		func() error { return batch(&cfg) },
		func() error { return timings(&cfg) },
		func() error { return allowed(&cfg) },
		func() error { return writes(&cfg) },
		func() error { return sizes(&cfg) },
	} {
		if err := check(); err != nil {
			_, _ = fmt.Fprintln(out, err)
			return config{}, errUsage
		}
	}
	return cfg, nil
}

// newFlagSet returns an empty flag set that writes its usage to out.
func newFlagSet(out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("mcp-toolsearch-proxy", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(out, "Usage: mcp-toolsearch-proxy [serve|fetch-model] [flags]\n\n"+
			"serve (the default) serves the proxy; fetch-model downloads the built-in embedding model.\n\n")
		fs.PrintDefaults()
	}
	return fs
}

// settle checks the settings parsed and fills in what follows from them.
func (c *config) settle(inFlight, ttl, maxCatalogs, callTimeout string) error {
	if c.modelDir == "" {
		return errors.New("-model-dir is required: no state directory to default to")
	}
	if c.command == cmdFetchModel {
		return nil
	}
	if c.upstream == "" {
		return errors.New("-upstream is required: the URL of the MCP server whose tools are searched")
	}
	if u, err := url.Parse(c.upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("-upstream: %q is not an http or https URL", c.upstream)
	}
	if c.upstreamHost != "" {
		if err := server.ValidateHost(c.upstreamHost); err != nil {
			return fmt.Errorf("-upstream-host: %w", err)
		}
	}
	if c.upstreamProtocol == protocolLatest {
		c.upstreamProtocol = ""
	} else if supported := mcp.SupportedProtocolVersions(); !slices.Contains(supported, c.upstreamProtocol) {
		return fmt.Errorf("-upstream-protocol: %q is not latest or one of %s",
			c.upstreamProtocol, strings.Join(supported, ", "))
	}
	if c.embedder != embedBuiltin && c.embedder != embedNone {
		return fmt.Errorf("-embedder: %q is not builtin or none", c.embedder)
	}
	if !strings.HasPrefix(c.path, "/") {
		return errors.New("-path must start with /")
	}
	var err error
	if c.maxInFlight, err = strconv.Atoi(inFlight); err != nil || c.maxInFlight < 1 {
		return fmt.Errorf("-max-in-flight: %q is not a positive number", inFlight)
	}
	if c.maxCatalogs, err = strconv.Atoi(maxCatalogs); err != nil || c.maxCatalogs < 1 {
		return fmt.Errorf("-max-catalogs: %q is not a positive number", maxCatalogs)
	}
	if c.catalogTTL, err = time.ParseDuration(ttl); err != nil || c.catalogTTL <= 0 {
		return fmt.Errorf("-catalog-ttl: %q is not a positive duration", ttl)
	}
	if c.callTimeout, err = time.ParseDuration(callTimeout); err != nil || c.callTimeout <= 0 {
		return fmt.Errorf("-call-timeout: %q is not a positive duration", callTimeout)
	}
	return nil
}

// shutdownFlag adds the flag of the shutdown grace to fs, and returns the
// function that checks it and sets it in a config once fs is parsed and the
// call timeout settled. The grace defaults to the call timeout plus
// shutdownMargin, so a call running when the proxy stops can finish rather
// than be cut off after its upstream tool ran.
func shutdownFlag(fs *flag.FlagSet, env func(name, fallback string) string) func(*config) error {
	grace := env("MCP_SHUTDOWN_GRACE", "")
	fs.StringVar(&grace, "shutdown-grace", grace,
		"how long the requests running get to finish when the proxy stops, before their connections are "+
			"closed; a write cut off may still complete upstream. Empty for -call-timeout plus "+
			shutdownMargin.String()+" (MCP_SHUTDOWN_GRACE)")
	return func(c *config) error {
		if c.command == cmdFetchModel {
			return nil
		}
		if grace == "" {
			c.shutdownGrace = c.callTimeout + shutdownMargin
			return nil
		}
		var err error
		if c.shutdownGrace, err = time.ParseDuration(grace); err != nil || c.shutdownGrace <= 0 {
			return fmt.Errorf("-shutdown-grace: %q is not a positive duration", grace)
		}
		return nil
	}
}

// settleCounts checks the limits on what the proxy passes on, and on how
// often it lists again.
func (c *config) settleCounts(maxInstructions, searchSchemas, listRetries string) error {
	var err error
	if c.maxInstructions, err = strconv.Atoi(maxInstructions); err != nil || c.maxInstructions < 0 {
		return fmt.Errorf("-max-instructions: %q is not a number of characters", maxInstructions)
	}
	if c.searchSchemas, err = strconv.Atoi(searchSchemas); err != nil || c.searchSchemas < 0 {
		return fmt.Errorf("-search-schemas: %q is not a number of matches", searchSchemas)
	}
	if c.listRetries, err = strconv.Atoi(listRetries); err != nil || c.listRetries < 0 {
		return fmt.Errorf("-list-retries: %q is not a number of retries", listRetries)
	}
	return nil
}

// timingFlags adds the flags of how long catalogs are kept and listings
// waited for to fs, and returns the function that checks them and sets
// them in a config once fs is parsed and the catalog TTL is known.
func timingFlags(fs *flag.FlagSet, env func(name, fallback string) string) func(*config) error {
	maxAge := env("TOOLSEARCH_CATALOG_MAX_AGE", "0")
	fs.StringVar(&maxAge, "catalog-max-age", maxAge,
		"how old a caller's catalog may get while listing the tools again fails, before it is no longer used; "+
			"0 for six times -catalog-ttl (TOOLSEARCH_CATALOG_MAX_AGE)")
	listTimeout := env("TOOLSEARCH_LIST_TIMEOUT", upstream.DefaultListTimeout.String())
	fs.StringVar(&listTimeout, "list-timeout", listTimeout,
		"how long listing the upstream's tools may take, retries included (TOOLSEARCH_LIST_TIMEOUT)")
	handshakeWait := env("TOOLSEARCH_HANDSHAKE_WAIT", server.DefaultHandshakeWait.String())
	fs.StringVar(&handshakeWait, "handshake-wait", handshakeWait,
		"how long a handshake waits for a new caller's catalog, to forward the upstream's instructions, before "+
			"answering without them (TOOLSEARCH_HANDSHAKE_WAIT)")
	return func(c *config) error {
		if c.command == cmdFetchModel {
			return nil
		}
		var err error
		if c.catalogMaxAge, err = time.ParseDuration(maxAge); err != nil || c.catalogMaxAge < 0 {
			return fmt.Errorf("-catalog-max-age: %q is not a duration", maxAge)
		}
		if c.catalogMaxAge == 0 {
			c.catalogMaxAge = catalog.DefaultMaxAgeTTLs * c.catalogTTL
		} else if c.catalogMaxAge < c.catalogTTL {
			return fmt.Errorf("-catalog-max-age: %s is less than -catalog-ttl", c.catalogMaxAge)
		}
		if c.listTimeout, err = time.ParseDuration(listTimeout); err != nil || c.listTimeout <= 0 {
			return fmt.Errorf("-list-timeout: %q is not a positive duration", listTimeout)
		}
		if c.handshakeWait, err = time.ParseDuration(handshakeWait); err != nil || c.handshakeWait <= 0 {
			return fmt.Errorf("-handshake-wait: %q is not a positive duration", handshakeWait)
		}
		return nil
	}
}

// settleLog checks the log level and format.
func (c *config) settleLog(level string) error {
	switch strings.ToLower(level) {
	case "debug":
		c.logLevel = slog.LevelDebug
	case "info":
		c.logLevel = slog.LevelInfo
	case "warn":
		c.logLevel = slog.LevelWarn
	case "error":
		c.logLevel = slog.LevelError
	default:
		return fmt.Errorf("-log-level: %q is not one of debug, info, warn or error", level)
	}
	if c.logFormat != logText && c.logFormat != logJSON {
		return fmt.Errorf("-log-format: %q is not text or json", c.logFormat)
	}
	return nil
}

// allowedFlags adds the flags of the origins and hosts served to fs, and
// returns the function that checks them and sets them in a config once fs
// is parsed.
func allowedFlags(fs *flag.FlagSet, env func(name, fallback string) string) func(*config) error {
	origins, hosts := env("MCP_ALLOWED_ORIGINS", ""), env("MCP_ALLOWED_HOSTS", "")
	fs.StringVar(&origins, "allowed-origins", origins, "comma-separated browser origins, such as "+
		"https://app.example.com, whose requests are served; empty refuses any request with an Origin (MCP_ALLOWED_ORIGINS)")
	fs.StringVar(&hosts, "allowed-hosts", hosts, "comma-separated Host names, with an optional port, served on "+
		"addresses other than loopback; empty checks no Host there (MCP_ALLOWED_HOSTS)")
	return func(c *config) error {
		var err error
		if c.allowedOrigins, err = list(origins, server.ValidateOrigin); err != nil {
			return fmt.Errorf("-allowed-origins: %w", err)
		}
		if c.allowedHosts, err = list(hosts, server.ValidateHost); err != nil {
			return fmt.Errorf("-allowed-hosts: %w", err)
		}
		return nil
	}
}

// writeToolsFlag adds the flag of the tools always treated as writes to fs,
// and returns the function that checks it and sets it in a config once fs
// is parsed.
func writeToolsFlag(fs *flag.FlagSet, env func(name, fallback string) string) func(*config) error {
	tools := env("TOOLSEARCH_WRITE_TOOLS", "")
	fs.StringVar(&tools, "write-tools", tools, "comma-separated names of upstream tools, or patterns such as "+
		"github_*, always treated as writes, so call_read_tool refuses them whatever the upstream marks them "+
		"(TOOLSEARCH_WRITE_TOOLS)")
	return func(c *config) error {
		var err error
		if c.writeTools, err = list(tools, validPattern); err != nil {
			return fmt.Errorf("-write-tools: %w", err)
		}
		return nil
	}
}

// batchFlag adds the flag of how many calls call_read_tools takes to fs,
// and returns the function that checks it and sets it in a config once fs
// is parsed.
func batchFlag(fs *flag.FlagSet, env func(name, fallback string) string) func(*config) error {
	calls := env("TOOLSEARCH_MAX_BATCH_CALLS", strconv.Itoa(server.DefaultMaxBatchCalls))
	fs.StringVar(&calls, "max-batch-calls", calls,
		fmt.Sprintf("how many read-only calls call_read_tools takes at once, up to %d; 0 leaves the tool out "+
			"(TOOLSEARCH_MAX_BATCH_CALLS)", server.MaxBatchCallsLimit))
	return func(c *config) error {
		var err error
		if c.maxBatchCalls, err = strconv.Atoi(calls); err != nil || c.maxBatchCalls < 0 ||
			c.maxBatchCalls > server.MaxBatchCallsLimit {
			return fmt.Errorf("-max-batch-calls: %q is not a number of calls from 0 to %d",
				calls, server.MaxBatchCallsLimit)
		}
		return nil
	}
}

// sizeFlags adds the flags of the proxy's memory bounds to fs, and returns
// the function that checks them and sets them in a config once fs is
// parsed.
func sizeFlags(fs *flag.FlagSet, env func(name, fallback string) string) func(*config) error {
	inFlight := env("MCP_MAX_IN_FLIGHT_BYTES", formatSize(server.DefaultMaxInFlightBytes))
	fs.StringVar(&inFlight, "max-in-flight-bytes", inFlight,
		"how many bytes of request bodies are served at once, such as 32MiB; a request that does not fit is refused "+
			"with 503. A request of unknown length counts as the largest allowed, "+formatSize(server.MaxRequestBytes)+
			" (MCP_MAX_IN_FLIGHT_BYTES)")
	response := env("TOOLSEARCH_MAX_RESPONSE_BYTES", formatSize(upstream.DefaultMaxResponseBytes))
	fs.StringVar(&response, "max-response-bytes", response,
		"the most bytes taken from the upstream for one operation, such as 32MiB: a tool call's result, or every "+
			"page of a listing together; one that sends more fails (TOOLSEARCH_MAX_RESPONSE_BYTES)")
	tools := env("TOOLSEARCH_MAX_TOOLS", strconv.Itoa(upstream.DefaultMaxListTools))
	fs.StringVar(&tools, "max-tools", tools,
		"the most tools a caller's listing may hold; a larger one fails (TOOLSEARCH_MAX_TOOLS)")
	return func(c *config) error {
		var err error
		if c.maxInFlightBytes, err = parseSize(inFlight); err != nil {
			return fmt.Errorf("-max-in-flight-bytes: %w", err)
		}
		if c.maxResponseBytes, err = parseSize(response); err != nil {
			return fmt.Errorf("-max-response-bytes: %w", err)
		}
		if c.maxTools, err = strconv.Atoi(tools); err != nil || c.maxTools < 1 {
			return fmt.Errorf("-max-tools: %q is not a positive number", tools)
		}
		return nil
	}
}

// sizeUnit is a unit of a size, as parseSize reads it.
type sizeUnit struct {
	suffix string
	shift  uint
}

// sizeUnits are the units of a size, largest first.
func sizeUnits() []sizeUnit {
	return []sizeUnit{{"GiB", 30}, {"MiB", 20}, {"KiB", 10}, {"B", 0}}
}

// parseSize reads a positive number of bytes, such as 1048576, 512KiB or
// 32MiB.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	shift := uint(0)
	for _, u := range sizeUnits() {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			s, shift = strings.TrimSpace(n), u.shift
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("%q is not a positive size, such as 32MiB", s)
	}
	return n << shift, nil
}

// formatSize writes n bytes as parseSize reads them, in the largest unit
// that divides n.
func formatSize(n int64) string {
	for _, u := range sizeUnits() {
		if u.shift > 0 && n >= 1<<u.shift && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// validPattern checks a pattern as path.Match reads it.
func validPattern(pattern string) error {
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("not a name or pattern: %w", err)
	}
	return nil
}

// list splits s at commas into the entries valid accepts, leaving out
// blanks.
func list(s string, valid func(string) error) ([]string, error) {
	var out []string
	for entry := range strings.SplitSeq(s, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		if err := valid(entry); err != nil {
			return nil, fmt.Errorf("%q: %w", entry, err)
		}
		out = append(out, entry)
	}
	return out, nil
}

// defaultModelDir is the model directory in systemd's StateDirectory=,
// then under the XDG base directories.
func defaultModelDir(getenv func(string) string) string {
	if d := getenv("STATE_DIRECTORY"); d != "" {
		return filepath.Join(strings.Split(d, ":")[0], "model")
	}
	if d := getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "mcp-toolsearch-proxy", "model")
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "state", "mcp-toolsearch-proxy", "model")
	}
	return ""
}

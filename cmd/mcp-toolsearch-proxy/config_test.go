package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(nil, envOf(map[string]string{
		"TOOLSEARCH_UPSTREAM": "http://gateway:8080/mcp/all", "STATE_DIRECTORY": "/var/lib/p",
	}), &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, cmdServe, cfg.command)
	assert.Equal(t, "127.0.0.1:8000", cfg.addr)
	assert.Equal(t, "/mcp", cfg.path)
	assert.Equal(t, embedBuiltin, cfg.embedder)
	assert.Equal(t, "/var/lib/p/model", cfg.modelDir)
	assert.Equal(t, 10*time.Minute, cfg.catalogTTL)
	assert.Equal(t, 16, cfg.maxCatalogs)
	assert.Equal(t, 2*time.Minute, cfg.callTimeout)
	assert.Empty(t, cfg.upstreamProtocol, "latest: the newest the SDK supports")
	assert.Equal(t, 10000, cfg.maxInstructions)
	assert.Equal(t, 3, cfg.searchSchemas)
	assert.Equal(t, 10, cfg.maxBatchCalls)
	assert.Equal(t, 3, cfg.listRetries)
	assert.Equal(t, 15*time.Second, cfg.listTimeout)
	assert.Equal(t, 3*time.Second, cfg.handshakeWait)
	assert.Equal(t, time.Hour, cfg.catalogMaxAge, "six TTLs")
	assert.Equal(t, 2*time.Minute+15*time.Second, cfg.shutdownGrace, "the call timeout and a margin")
	assert.Empty(t, cfg.writeTools)
	assert.Equal(t, int64(32<<20), cfg.maxInFlightBytes)
	assert.Equal(t, int64(32<<20), cfg.maxResponseBytes)
	assert.Equal(t, 10000, cfg.maxTools)
}

func TestConfigSizes(t *testing.T) {
	t.Parallel()
	base := []string{"-upstream", "http://gw/mcp", "-model-dir", "/m"}

	cfg, err := parseConfig(append(base, "-max-response-bytes", "64MiB", "-max-in-flight-bytes", "1048576"),
		envOf(map[string]string{"TOOLSEARCH_MAX_TOOLS": "500", "MCP_MAX_IN_FLIGHT_BYTES": "1GiB"}),
		&bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, int64(64<<20), cfg.maxResponseBytes)
	assert.Equal(t, int64(1<<20), cfg.maxInFlightBytes, "the flag wins")
	assert.Equal(t, 500, cfg.maxTools)
	for in, want := range map[string]int64{"512KiB": 512 << 10, "2GiB": 2 << 30, "7": 7, "3 MiB": 3 << 20} {
		got, perr := parseSize(in)
		require.NoError(t, perr, in)
		assert.Equal(t, want, got, in)
	}
}

func TestShutdownGraceFollowsTheCallTimeout(t *testing.T) {
	t.Parallel()
	base := []string{"-upstream", "http://gw/mcp", "-model-dir", "/m"}

	short, err := parseConfig(append(base, "-call-timeout", "30s"), envOf(nil), &bytes.Buffer{})
	require.NoError(t, err)
	set, err := parseConfig(base, envOf(map[string]string{"MCP_SHUTDOWN_GRACE": "5m"}), &bytes.Buffer{})
	require.NoError(t, err)
	flag, err := parseConfig(append(base, "-shutdown-grace", "1m"),
		envOf(map[string]string{"MCP_SHUTDOWN_GRACE": "5m"}), &bytes.Buffer{})
	require.NoError(t, err)

	assert.Equal(t, 45*time.Second, short.shutdownGrace)
	assert.Equal(t, 5*time.Minute, set.shutdownGrace)
	assert.Equal(t, time.Minute, flag.shutdownGrace)
}

func TestConfigTimingsFromTheEnvironment(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(nil, envOf(map[string]string{
		"TOOLSEARCH_UPSTREAM": "http://gw/mcp", "TOOLSEARCH_MODEL_DIR": "/m", "TOOLSEARCH_CATALOG_TTL": "1m",
		"TOOLSEARCH_CATALOG_MAX_AGE": "5m", "TOOLSEARCH_LIST_TIMEOUT": "20s", "TOOLSEARCH_HANDSHAKE_WAIT": "1s",
	}), &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, cfg.catalogMaxAge)
	assert.Equal(t, 20*time.Second, cfg.listTimeout)
	assert.Equal(t, time.Second, cfg.handshakeWait)
}

func TestConfigWriteTools(t *testing.T) {
	t.Parallel()
	env := envOf(map[string]string{
		"TOOLSEARCH_UPSTREAM": "http://gw/mcp", "TOOLSEARCH_MODEL_DIR": "/m",
		"TOOLSEARCH_WRITE_TOOLS": "github_*, ,mail_send",
	})

	fromEnv, err := parseConfig(nil, env, &bytes.Buffer{})
	require.NoError(t, err)
	fromFlag, err := parseConfig([]string{"-write-tools", "plex_*"}, env, &bytes.Buffer{})
	require.NoError(t, err)

	assert.Equal(t, []string{"github_*", "mail_send"}, fromEnv.writeTools)
	assert.Equal(t, []string{"plex_*"}, fromFlag.writeTools)
}

func TestConfigFlagsOverrideTheEnvironment(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig([]string{
		"-upstream", "https://gw.example.com/mcp", "-embedder", "none", "-catalog-ttl", "1m",
		"-allowed-hosts", "proxy.example.com", "-log-format", "json", "-upstream-host", "gw.example.com",
		"-upstream-protocol", "2025-11-25", "-max-instructions", "0", "-search-schemas", "5", "-list-retries", "0",
	}, envOf(map[string]string{"TOOLSEARCH_UPSTREAM": "http://other/mcp", "HOME": "/home/u"}), &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, "https://gw.example.com/mcp", cfg.upstream)
	assert.Equal(t, embedNone, cfg.embedder)
	assert.Equal(t, time.Minute, cfg.catalogTTL)
	assert.Equal(t, []string{"proxy.example.com"}, cfg.allowedHosts)
	assert.Equal(t, "gw.example.com", cfg.upstreamHost)
	assert.Equal(t, "2025-11-25", cfg.upstreamProtocol)
	assert.Zero(t, cfg.maxInstructions, "0 forwards none")
	assert.Equal(t, 5, cfg.searchSchemas)
	assert.Zero(t, cfg.listRetries)
	assert.Equal(t, "/home/u/.local/state/mcp-toolsearch-proxy/model", cfg.modelDir)
}

func TestConfigFetchModelNeedsNoUpstream(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig([]string{cmdFetchModel, "-model-dir", "/tmp/m"}, envOf(nil), &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, cmdFetchModel, cfg.command)
	assert.Equal(t, "/tmp/m", cfg.modelDir)
}

func TestConfigRefusesBadSettings(t *testing.T) {
	t.Parallel()
	base := []string{"-upstream", "http://gw/mcp", "-model-dir", "/m"}

	for _, args := range [][]string{
		{"-model-dir", "/m"},
		{"-upstream", "gw:8080", "-model-dir", "/m"},
		{"-upstream", "ftp://gw/mcp", "-model-dir", "/m"},
		append(base, "-embedder", "openai"),
		append(base, "-path", "mcp"),
		append(base, "-max-in-flight", "0"),
		append(base, "-max-catalogs", "x"),
		append(base, "-catalog-ttl", "0s"),
		append(base, "-call-timeout", "soon"),
		append(base, "-shutdown-grace", "0s"),
		append(base, "-shutdown-grace", "later"),
		append(base, "-log-level", "loud"),
		append(base, "-log-format", "xml"),
		append(base, "-allowed-origins", "null"),
		append(base, "-allowed-hosts", "a b"),
		append(base, "-upstream-host", "a b"),
		append(base, "-upstream-protocol", "newest"),
		append(base, "-upstream-protocol", "2025-11-24"),
		append(base, "-upstream-protocol", "2026-99-99"),
		append(base, "-max-instructions", "-1"),
		append(base, "-search-schemas", "many"),
		append(base, "-max-batch-calls", "-1"),
		append(base, "-max-batch-calls", "51"),
		append(base, "-max-batch-calls", "few"),
		append(base, "-list-retries", "-1"),
		append(base, "-list-timeout", "0s"),
		append(base, "-handshake-wait", "-1s"),
		append(base, "-catalog-max-age", "soon"),
		append(base, "-catalog-ttl", "10m", "-catalog-max-age", "5m"),
		append(base, "-write-tools", "github_["),
		append(base, "-max-response-bytes", "0"),
		append(base, "-max-response-bytes", "32MB"),
		append(base, "-max-in-flight-bytes", "-1KiB"),
		append(base, "-max-tools", "0"),
		append(base, "extra"),
		{"-upstream", "http://gw/mcp"},
	} {
		var out bytes.Buffer
		_, err := parseConfig(args, envOf(nil), &out)
		require.ErrorIs(t, err, errUsage, "%v", args)
		assert.NotEmpty(t, out.String(), "%v explains itself", args)
	}
}

func TestRunExitsWithUsageErrors(t *testing.T) {
	t.Parallel()

	assert.Equal(t, exitUsage, run([]string{"-nope"}, envOf(nil), &bytes.Buffer{}))
}

func TestRunFailsWithoutTheModel(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	code := run([]string{"-upstream", "http://127.0.0.1:1/mcp", "-model-dir", t.TempDir(), "-addr", "127.0.0.1:0"},
		envOf(nil), &out)

	assert.Equal(t, exitFailure, code)
	assert.Contains(t, out.String(), "fetch-model")
}

func TestRedact(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "https://gw.example.com/mcp", redact("https://user:pass@gw.example.com/mcp?token=x#f"))
}

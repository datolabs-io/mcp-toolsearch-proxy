# Deploying

The proxy is one static binary. It needs the URL of its upstream MCP server and, for semantic search, the built-in
model downloaded once. It holds no secret. Put it behind something that authenticates clients and passes their token
on: it has no authentication of its own, and the upstream refuses requests without a token it accepts.

## Packages

```bash
sudo apt install ./mcp-toolsearch-proxy_<version>_linux_amd64.deb   # or: sudo dnf install ./…rpm
```

The package installs the binary, a hardened systemd unit, the `mcp-toolsearch-proxy` system user and
`/etc/mcp-toolsearch-proxy/env`. It does not start the service. Then:

1. Set `TOOLSEARCH_UPSTREAM` in `/etc/mcp-toolsearch-proxy/env`, and `MCP_ADDR` if it should listen beyond loopback.
2. Download the model as the service user, into the service's state directory:

   ```bash
   sudo runuser -u mcp-toolsearch-proxy -- env STATE_DIRECTORY=/var/lib/mcp-toolsearch-proxy \
     mcp-toolsearch-proxy fetch-model
   ```

   It fetches about 530 MB from Hugging Face at a pinned revision and checks each file's digest; run it again to
   repair a damaged copy. Set `TOOLSEARCH_EMBEDDER=none` instead to search lexically only.
3. `sudo systemctl enable --now mcp-toolsearch-proxy`, and check `journalctl -u mcp-toolsearch-proxy` for
   `model loaded` and `listening`.

The service uses about 250 MB with the model loaded. The unit caps it at 512 MB and sets `GOMEMLIMIT=450MiB`, so the
Go runtime collects garbage harder before it nears the cap. What grows beyond that is bounded by settings: the catalogs
kept (`TOOLSEARCH_MAX_CATALOGS`, several MB each), the request bodies served at once (`MCP_MAX_IN_FLIGHT_BYTES`; a call
holds about ten times its body while it runs) and what one call or listing takes from the upstream
(`TOOLSEARCH_MAX_RESPONSE_BYTES`, held about three times over, per call in flight). If you raise them, raise
`MemoryMax=` with a drop-in, and `GOMEMLIMIT` in the environment file with it.

A restart, including the one a package upgrade does, can cut off tool calls in flight. The proxy waits
`MCP_SHUTDOWN_GRACE` for them to finish (by default `TOOLSEARCH_CALL_TIMEOUT` plus 15 s) and the unit's
`TimeoutStopSec=` waits a little longer; past that, a write may complete upstream while its client sees no result. If
you raise the call timeout or the grace, raise `TimeoutStopSec=` with a drop-in too.

A proxy in front of the proxy needs no timeout raised for long calls. Responses are server-sent events: a tool call
sends its headers at once and a comment every 5 s until its result, so a front proxy that waits for headers, or gives
up on a response that sends nothing for a while, keeps the call. The proxy's own limit on a call is
`TOOLSEARCH_CALL_TIMEOUT`. A front proxy must pass the stream on as it comes rather than buffer it whole.

## Homebrew

```bash
brew install --cask datolabs-io/tap/mcp-toolsearch-proxy
mcp-toolsearch-proxy fetch-model
mcp-toolsearch-proxy -upstream https://gateway.example.com/mcp/all
```

Without `-model-dir`, the model goes to `~/.local/state/mcp-toolsearch-proxy/model`, or under `$XDG_STATE_HOME` when
that is set.

## Container

```bash
docker run --rm -v toolsearch-model:/model ghcr.io/datolabs-io/mcp-toolsearch-proxy fetch-model
docker run -d -p 8000:8000 -v toolsearch-model:/model:ro \
  -e TOOLSEARCH_UPSTREAM=http://gateway:3000/mcp/all \
  ghcr.io/datolabs-io/mcp-toolsearch-proxy
```

The image listens on `0.0.0.0:8000`, runs as an unprivileged user, and reads the model from `/model`, which a new volume
inherits ownership of.

## Behind a gateway

See [agentgateway.md](agentgateway.md) for a complete setup with agentgateway, and [clients.md](clients.md) for
connecting claude.ai or Claude Code. With any gateway:

- the route to the proxy authenticates clients and forwards their `Authorization` header;
- `TOOLSEARCH_UPSTREAM` is an endpoint that lists and calls tools for the token it is given;
- set `MCP_ALLOWED_HOSTS` to the names the proxy is reached by when it listens beyond loopback.

## Without a gateway

The upstream can be any MCP server that speaks streamable HTTP, and the client connects to the proxy directly.

A single server that checks bearer tokens itself, such as a hosted MCP server with personal access tokens, needs no
gateway: the proxy passes each client's token through, and the server decides what it may see and call.

```bash
mcp-toolsearch-proxy -upstream https://mcp.example.com/mcp -model-dir ./model
claude mcp add --transport http example http://127.0.0.1:8000/mcp --header "Authorization: Bearer <token>"
```

An upstream without authentication works too, for a local server with many tools:

```bash
mcp-toolsearch-proxy -upstream http://127.0.0.1:9000/mcp -embedder none
```

Every caller then shares one catalog, the one listed without a token, and anyone who reaches the proxy can call every
tool. Keep it on loopback (the default `MCP_ADDR`), or put something in front that authenticates clients.

## Checking it

- `GET /healthz` answers `ok` without contacting the upstream.
- With `MCP_METRICS_ADDR`, `GET /metrics` serves Prometheus metrics; `mcp_toolsearch_catalog_builds_total` by outcome
  shows whether the upstream lists tools for callers, and `mcp_toolsearch_searches_total{mode="lexical"}` rising
  where it should be hybrid shows the model is not in use. [monitoring.md](monitoring.md) lists every metric, with
  example alerts for an upstream outage, saturation and a backend dropping out.
- `mise run test:e2e` with `E2E_SEARCH_URL`, `E2E_ALL_URL`, `E2E_TOKEN_ALICE` and `E2E_TOKEN_BOB` set runs the
  end-to-end tests against a deployment. They assume the fixture tools, so point them at a test stack.

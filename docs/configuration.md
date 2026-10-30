# Configuration

<!-- Generated from the flags by `mise run docs:config`; do not edit by hand. -->

Every setting is a flag with an environment variable as its default; a flag on the command line wins. The proxy takes
no secret. `mcp-toolsearch-proxy -h` prints the same list.

| Flag | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `-addr` | `MCP_ADDR` | `127.0.0.1:8000` | listen address |
| `-allowed-hosts` | `MCP_ALLOWED_HOSTS` | empty | comma-separated Host names, with an optional port, served on addresses other than loopback; empty checks no Host there |
| `-allowed-origins` | `MCP_ALLOWED_ORIGINS` | empty | comma-separated browser origins, such as <https://app.example.com>, whose requests are served; empty refuses any request with an Origin |
| `-call-timeout` | `TOOLSEARCH_CALL_TIMEOUT` | `2m0s` | how long a call to an upstream tool may take |
| `-catalog-max-age` | `TOOLSEARCH_CATALOG_MAX_AGE` | `0` | how old a caller's catalog may get while listing the tools again fails, before it is no longer used; 0 for six times -catalog-ttl |
| `-catalog-ttl` | `TOOLSEARCH_CATALOG_TTL` | `10m0s` | how long a caller's catalog is used before the tools are listed again; an older one is still used while they are listed again in the background, up to -catalog-max-age; call_read_tool's read-only check reads it, so it may lag the upstream by that long |
| `-embedder` | `TOOLSEARCH_EMBEDDER` | `builtin` | semantic search: builtin, the model fetch-model downloads, or none for lexical search only |
| `-handshake-wait` | `TOOLSEARCH_HANDSHAKE_WAIT` | `3s` | how long a handshake waits for a new caller's catalog, to forward the upstream's instructions, before answering without them |
| `-list-retries` | `TOOLSEARCH_LIST_RETRIES` | `3` | how many times a failed listing of the upstream's tools is tried again, waiting longer each time; 0 for none |
| `-list-timeout` | `TOOLSEARCH_LIST_TIMEOUT` | `15s` | how long listing the upstream's tools may take, retries included |
| `-log-format` | `MCP_LOG_FORMAT` | `text` | log format: text or json |
| `-log-level` | `MCP_LOG_LEVEL` | `info` | least severe log records written: debug, info, warn or error |
| `-max-batch-calls` | `TOOLSEARCH_MAX_BATCH_CALLS` | `10` | how many read-only calls call_read_tools takes at once, up to 50; 0 leaves the tool out |
| `-max-catalogs` | `TOOLSEARCH_MAX_CATALOGS` | `16` | how many callers' catalogs are kept; each new token is a new caller |
| `-max-in-flight` | `MCP_MAX_IN_FLIGHT` | `8` | how many MCP requests are served at once; one more is refused with 503 |
| `-max-in-flight-bytes` | `MCP_MAX_IN_FLIGHT_BYTES` | `32MiB` | how many bytes of request bodies are served at once, such as 32MiB; a request that does not fit is refused with 503. A request of unknown length counts as the largest allowed, 24MiB |
| `-max-instructions` | `TOOLSEARCH_MAX_INSTRUCTIONS` | `10000` | how many characters of the upstream server's instructions the handshake forwards; longer ones are cut, 0 forwards none |
| `-max-response-bytes` | `TOOLSEARCH_MAX_RESPONSE_BYTES` | `32MiB` | the most bytes taken from the upstream for one operation, such as 32MiB: a tool call's result, or every page of a listing together; one that sends more fails |
| `-max-tools` | `TOOLSEARCH_MAX_TOOLS` | `10000` | the most tools a caller's listing may hold; a larger one fails |
| `-metrics-addr` | `MCP_METRICS_ADDR` | empty | listen address for Prometheus metrics at /metrics; empty for none |
| `-model-dir` | `TOOLSEARCH_MODEL_DIR` | `$STATE_DIRECTORY/model`, else under `$XDG_STATE_HOME` or `~/.local/state` | directory of the built-in model, as fetch-model writes it |
| `-path` | `MCP_PATH` | `/mcp` | MCP endpoint path |
| `-search-schemas` | `TOOLSEARCH_SEARCH_SCHEMAS` | `3` | how many of the best search matches come with their input schemas; 0 for none |
| `-shutdown-grace` | `MCP_SHUTDOWN_GRACE` | empty | how long the requests running get to finish when the proxy stops, before their connections are closed; a write cut off may still complete upstream. Empty for -call-timeout plus 15s |
| `-upstream` | `TOOLSEARCH_UPSTREAM` | empty | streamable HTTP URL of the MCP server whose tools are searched, such as a gateway |
| `-upstream-host` | `TOOLSEARCH_UPSTREAM_HOST` | empty | Host header for upstream requests instead of the URL's, for a gateway reached at an internal address whose servers accept only its public name |
| `-upstream-protocol` | `TOOLSEARCH_UPSTREAM_PROTOCOL` | `latest` | MCP version to speak upstream, such as 2025-11-25, or latest for the newest supported; an older one helps a gateway whose servers fail the newest handshake |
| `-write-tools` | `TOOLSEARCH_WRITE_TOOLS` | empty | comma-separated names of upstream tools, or patterns such as github_*, always treated as writes, so call_read_tool refuses them whatever the upstream marks them |

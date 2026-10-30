# Monitoring

With `-metrics-addr` (`MCP_METRICS_ADDR`), the proxy serves Prometheus metrics at `GET /metrics` on that address, apart
from the MCP endpoint. No label carries a query, an upstream tool's name, a caller or a token, and every label takes a
fixed set of values (see [privacy.md](privacy.md#metrics)).

## Metrics

| Metric | Type | What it shows |
| --- | --- | --- |
| `mcp_toolsearch_tool_calls_total{tool,outcome}` | counter | Calls of the proxy's tools; a `call_read_tools` batch counts once, and fails only when every call in it failed. `outcome` is `ok`; `tool_error` when the tool reports a failure, or the proxy refuses the input; `invalid_arguments` when the proxy refused a call whose arguments do not fit the tool's input schema, without calling the upstream; `upstream_error` when the upstream could not list the caller's tools or gave a call no result; `error` when the request itself failed, as for a tool the proxy does not offer. Each call of a batch counts in the upstream metrics on its own. |
| `mcp_toolsearch_tool_call_duration_seconds{tool}` | histogram | How long those calls take. |
| `mcp_toolsearch_upstream_requests_total{op,outcome}` | counter | Operations upstream: `op` is `list` (one listing, its retries included) or `call`; `outcome` is `ok` or why no answer came: `answered` (a JSON-RPC error), `timeout`, `refused`, `unauthorized`, `dropped`, or `too_large` when a call's result or a listing is over `-max-response-bytes`, `-max-tools` or 100 pages. A tool that answers with an error result is `ok`. |
| `mcp_toolsearch_upstream_request_duration_seconds{op}` | histogram | How long operations upstream take. |
| `mcp_toolsearch_catalog_builds_total{outcome}` | counter | Catalog builds: `ok`, `lexical_only` when the tools could not be embedded, or `error`. |
| `mcp_toolsearch_catalog_build_duration_seconds` | histogram | How long builds take, listing included, failed ones too. |
| `mcp_toolsearch_catalog_tools` | gauge | Tools in the largest catalog that would be served (one younger than `-catalog-max-age`). |
| `mcp_toolsearch_catalogs` | gauge | Catalogs kept, one per caller. |
| `mcp_toolsearch_catalog_stale_served_total` | counter | Catalogs served older than `-catalog-ttl`, as while listing them again fails. |
| `mcp_toolsearch_catalog_served_age_seconds` | gauge | Age of the oldest catalog served in the last minute or two; 0 when none was. Near `-catalog-max-age`, callers are about to lose their catalogs. |
| `mcp_toolsearch_handshakes_without_instructions_total` | counter | Handshakes answered without the upstream's instructions because the caller's catalog could not be had within `-handshake-wait`. |
| `mcp_toolsearch_requests_rejected_total` | counter | Requests refused with 503 because `-max-in-flight` requests, or `-max-in-flight-bytes` of request bodies, were being served. |
| `mcp_toolsearch_in_flight` | gauge | Requests the MCP endpoint is serving. |
| `mcp_toolsearch_searches_total{mode}` | counter | Searches, `hybrid` or `lexical`, one per query. Lexical where hybrid is expected means the model is not in use. |
| `mcp_toolsearch_build_info{version}` | gauge | Always 1. |

The Go runtime and process collectors are included (`go_*`, `process_*`).

`mcp_toolsearch_catalog_tools` is the largest catalog rather than the one built last, so it does not move with which
caller listed last. When the caller with the most tools stops using the proxy for longer than `-catalog-max-age`, it
drops to the next largest.

## Logs

A tool call that gets no result from the upstream logs a warning with its class and the transport's detail. A build
that fails logs a warning the first time it fails in a given way; while it goes on failing the same way it logs again
at most once a minute, with how many failures it left out (`suppressed`), and once when a build succeeds again. The
catalog store backs off a caller whose builds fail, so an outage is not listed again on every request either.

## Example alerts

These rules are examples to adapt: the thresholds and durations depend on the deployment. They group by `instance`,
so each proxy is judged on its own.

```yaml
groups:
  - name: mcp-toolsearch-proxy
    rules:
      # Builds fail and none succeeds: nobody gets a catalog.
      - alert: ToolSearchCatalogsFailing
        expr: |
          sum by (instance) (increase(mcp_toolsearch_catalog_builds_total{outcome="error"}[10m])) > 0
          unless
          sum by (instance) (increase(mcp_toolsearch_catalog_builds_total{outcome!="error"}[10m])) > 0
        labels:
          severity: critical

      # More than half of the tool calls sent upstream get no result.
      - alert: ToolSearchUpstreamCallsFailing
        expr: |
          sum by (instance) (rate(mcp_toolsearch_upstream_requests_total{op="call",outcome!="ok"}[10m]))
          /
          sum by (instance) (rate(mcp_toolsearch_upstream_requests_total{op="call"}[10m]))
          > 0.5
        for: 10m
        labels:
          severity: warning

      # Requests keep being refused for want of a slot, or of room for their bodies: raise -max-in-flight or
      # -max-in-flight-bytes (and the memory cap with it), or add capacity.
      - alert: ToolSearchSaturated
        expr: rate(mcp_toolsearch_requests_rejected_total[5m]) > 0
        for: 10m
        labels:
          severity: warning

      # Listings are over -max-response-bytes, -max-tools or 100 pages: the callers they belong to get no
      # catalog until the limit is raised.
      - alert: ToolSearchListingsTooLarge
        expr: increase(mcp_toolsearch_upstream_requests_total{op="list",outcome="too_large"}[30m]) > 0
        labels:
          severity: warning

      # The largest catalog lost more than a tenth of its tools, or all of them: a backend dropped out of the
      # listing. Only while builds succeed, so a restart or callers falling idle does not count.
      - alert: ToolSearchBackendMissing
        expr: |
          mcp_toolsearch_catalog_tools < 0.9 * max_over_time(mcp_toolsearch_catalog_tools[1d])
          and on (instance)
          sum by (instance) (increase(mcp_toolsearch_catalog_builds_total{outcome!="error"}[30m])) > 0
        for: 15m
        labels:
          severity: warning

      # Builds take most of the list timeout (15 s by default) and will soon fail.
      - alert: ToolSearchBuildsSlow
        expr: |
          histogram_quantile(0.9,
            sum by (instance, le) (rate(mcp_toolsearch_catalog_build_duration_seconds_bucket[10m]))) > 10
        for: 10m
        labels:
          severity: warning

      # Restarting over and over, as in an out-of-memory loop.
      - alert: ToolSearchRestarting
        expr: changes(process_start_time_seconds[1h]) > 2
        labels:
          severity: warning

      # Memory growing towards the limit; set the threshold above what the deployment uses at rest.
      - alert: ToolSearchMemoryHigh
        expr: process_resident_memory_bytes > 400e6
        for: 10m
        labels:
          severity: warning
```

The build-duration threshold follows the list timeout: with a longer `-list-timeout`, raise it with it.

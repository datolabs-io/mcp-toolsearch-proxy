# With agentgateway

[agentgateway](https://agentgateway.dev) can aggregate many MCP servers behind one endpoint and authorize each tool
per caller. The proxy sits on a second route of the same gateway:

```text
client ──▶ /mcp/search ──▶ mcp-toolsearch-proxy ──▶ /mcp/all ──▶ MCP servers
```

- `/mcp/search` authenticates the client and passes its token to the proxy. Clients connect here.
- `/mcp/all` is the gateway's aggregated endpoint, with its authorization rules. The proxy lists and calls tools here,
  with each client's own token, so every rule applies to calls through the proxy exactly as to direct ones.

[tests/e2e/agentgateway.yaml](../tests/e2e/agentgateway.yaml) is a complete, tested configuration (agentgateway
1.7). The parts that matter:

```yaml
binds:
- port: 3000
  listeners:
  - routes:
    - name: search
      matches:
      - path: {exact: /mcp/search}
      - path: {exact: /.well-known/oauth-protected-resource/mcp/search}
      policies:
        mcpAuthentication:
          mode: strict
          issuer: https://issuer.example.com
          resourceMetadata:
            resource: https://mcp.example.com/mcp/search
        # Send the client's validated token on to the proxy.
        backendAuth:
          passthrough: {}
      backends:
      - mcp:
          statefulMode: stateless
          # Keep the proxy's tool names: search_tools, not toolsearch_search_tools.
          prefixMode: never
          targets:
          - name: toolsearch
            mcp:
              host: http://toolsearch.internal:8000/mcp
    - name: all
      matches:
      - path: {exact: /mcp/all}
      policies:
        mcpAuthentication:
          mode: strict
          issuer: https://issuer.example.com
        mcpAuthorization:
          rules:
          - 'jwt.sub == "alice"'
          - 'jwt.sub == "bob" && mcp.tool.target == "weather"'
      backends:
      - mcp:
          statefulMode: stateless
          prefixMode: always
          targets:
          - name: weather
            mcp: {host: http://weather.internal:8000/mcp}
          - name: admin
            mcp: {host: http://admin.internal:8000/mcp}
```

and the proxy:

```bash
TOOLSEARCH_UPSTREAM=http://gateway.internal:3000/mcp/all mcp-toolsearch-proxy
```

## Common mistakes

agentgateway forwards the Host of each request to the MCP servers behind it, and servers that check Host (against DNS
rebinding) accept only their public name. When the proxy reaches the gateway at an internal address, set
`TOOLSEARCH_UPSTREAM_HOST` to the gateway's public name, or every list fails once one server refuses the internal
address.

The proxy speaks the newest MCP version upstream, which opens with `server/discover`. agentgateway sends that on to
every server behind it, and when one answers with a server error rather than declining the version, the whole
discover fails, and so does the `initialize` the client then falls back to. Until both are fixed, set
`TOOLSEARCH_UPSTREAM_PROTOCOL=2025-11-25` if `search_tools` reports that the catalog could not be loaded.

Without `backendAuth: passthrough` on the proxy's route, the gateway strips the token after validating it, and
`/mcp/all` then refuses every request from the proxy.

Without `prefixMode: never`, the gateway renames the proxy's tools after its target, so clients see
`toolsearch_search_tools` instead of `search_tools`.

agentgateway gives a backend 10 s to send its response's headers, unless the route sets
`policies.timeout.backendRequestTimeout`, and answers the client "upstream call timeout" after that. The proxy sends
its headers as soon as a call starts, so its route needs no timeout of its own. `/mcp/all` does, if a server behind it
answers with one JSON body and has tools that take longer than 10 s: the proxy's call then fails there instead.

```yaml
    - name: all
      policies:
        timeout:
          # Above the proxy's TOOLSEARCH_CALL_TIMEOUT (2 min by default).
          backendRequestTimeout: 150s
```

Both routes see the same token. If your identity provider puts an audience in its tokens, `/mcp/all` has to accept
the audience that clients' tokens are issued for, not one of its own.

Keep both routes stateless. A stateful route ties a client to a session that ends when the gateway restarts, and the
proxy has no session to keep anyway.

`/mcp/all` doesn't have to be public. It applies the same authorization as `/mcp/search`, so exposing it gives no one
extra access, but only the proxy needs it. Put it on an internal listener if you can.

The authorization rules filter what each caller sees in `tools/list`. A tool someone isn't allowed to use never
reaches the proxy's catalog, and if they ask a call tool for it by name, the proxy refuses it (and the gateway would
too).

## Adding the proxy to an existing gateway

When the gateway already serves its aggregated endpoint to clients, keep that route as it is and add the proxy's
route beside it. Clients can move one at a time, and the old route stays a fallback.

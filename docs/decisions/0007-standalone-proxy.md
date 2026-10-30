# 7. A standalone proxy, not a gateway plugin

Date: 2026-10-28 · Status: accepted

## Context

Tool search could live inside a gateway. Some commercial gateways offer it; the open-source gateways this was built
against have no extension point that adds tools of their own, only hooks that filter or rewrite traffic.

## Decision

The proxy is a separate MCP server that speaks only MCP: to clients as a server, to one upstream URL as a client. It
depends on no gateway's API.

## Consequences

It works with any upstream that speaks streamable HTTP MCP and authorizes by bearer token, and with any gateway in
front that forwards the token. A deployment runs one more service and the upstream sees two hops instead of one.
Gateway-specific setups are documentation, tested end to end for agentgateway.

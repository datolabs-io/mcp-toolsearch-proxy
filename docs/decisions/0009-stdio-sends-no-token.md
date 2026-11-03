# 9. Over stdio, upstream requests carry no token

Date: 2026-11-03 · Status: accepted

## Context

Some clients and directories start an MCP server as a local command and speak to it on standard input and output,
and cannot reach one over HTTP. Over stdio there is no request header, so there is no caller's token to pass upstream
(ADR 4). The proxy could take a token of its own from a flag or an environment variable, but that is the credential
ADR 4 rules out: whoever reaches the proxy would act with it.

## Decision

`-transport stdio` (`MCP_TRANSPORT=stdio`) serves the same five tools on standard input and output, and every request
upstream goes without an `Authorization` header, as an HTTP request without one does. The proxy takes no token for
this mode, by flag, environment variable or file. Logs stay on standard error, so nothing else writes to standard
output.

## Consequences

Over stdio the proxy serves only an upstream that needs no credential, such as a local server or a gateway that
authenticates by network. One with authentication refuses the listing, and the proxy has no tools to search. There is
one caller and one catalog, the one listed without a token. Running an authenticated upstream locally takes the HTTP
transport, with the client sending its token, or something in front of the upstream that adds one.

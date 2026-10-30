# 2. Streamable HTTP without sessions

Date: 2026-10-28 · Status: accepted

## Context

MCP's streamable HTTP transport can keep a session per client, held in the server's memory, so a restart, a second
instance or a gateway that pools connections leaves clients holding a session the server no longer knows. MCP
2026-07-28 makes the protocol stateless. The proxy keeps no per-client state that a session would carry.

## Decision

The server runs the transport in stateless mode, answers each POST with a stream of server-sent events, and refuses
GET and DELETE. It serves 2026-07-28 clients and older ones. Its own requests upstream use one connection per
operation. (It first answered with a single JSON body, which a proxy in front could time out waiting for while a long
call ran; see ARCHITECTURE.md, Transport.)

## Consequences

Restarts and gateways in front are invisible to clients. The server cannot send requests to the client, which none
of its tools needs; an upstream tool that asks the client something mid-call does not get an answer through the
proxy.

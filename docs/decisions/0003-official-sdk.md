# 3. The official MCP Go SDK

Date: 2026-10-28 · Status: accepted

## Context

Several Go MCP libraries exist. The proxy is both a server and a client, and the protocol moves quickly.

## Decision

The proxy uses `github.com/modelcontextprotocol/go-sdk`, maintained with the specification, on both sides. Its
client negotiates the newest protocol version and falls back to older ones, so the upstream may be of any version.

## Consequences

Protocol upgrades come with SDK releases through Renovate. The SDK's own dependencies are vendored with ours.

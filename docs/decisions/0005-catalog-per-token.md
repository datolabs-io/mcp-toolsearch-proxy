# 5. A catalog per caller, keyed by a hash of the token

Date: 2026-10-28 · Status: accepted

## Context

Callers may see different tools: the upstream filters its list by who asks. One shared catalog would show every
caller every tool, and listing the tools on every search would make each search as slow as a full list across many
servers.

## Decision

The proxy keeps a catalog per caller, built from the upstream's `tools/list` with the caller's token, and keys it
by a SHA-256 of the whole `Authorization` value. It does not key by a claim such as `sub`, which it cannot verify:
a forged token with another caller's claim would read their catalog. Catalogs expire after a TTL; a call for
a name missing from the catalog lists once more before refusing.

## Consequences

A new token, such as after a refresh, builds a new catalog: one upstream list and, with the model, an embedding of
the tools, which takes milliseconds. The proxy holds no token, only hashes. A tool the upstream adds appears within
the TTL, or at once for a call by name.

# 4. Every upstream request carries the caller's own token

Date: 2026-10-28 · Status: accepted

## Context

The proxy can call every tool its upstream offers. With a credential of its own it would be a confused deputy: a
caller could reach through it what the upstream would refuse them, and every rule would have to be repeated in the
proxy.

## Decision

The proxy has no credential. Each request it makes upstream carries the `Authorization` header of the request it is
serving, and no other; without one, it sends none. The proxy never authorizes anything itself beyond refusing to call
a tool missing from the caller's own catalog.

## Consequences

The upstream authenticates and authorizes every caller as if they had called it directly, and its audit records are
the caller's. The proxy must sit behind something that authenticates clients and passes their token on, and the
upstream must accept that token: in practice, the same gateway on another route.

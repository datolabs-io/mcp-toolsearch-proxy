# 1. Vendored dependencies

Date: 2026-10-28 · Status: accepted

## Context

Releases are built into packages and images whose contents should not depend on what a module proxy serves on the
day, and a reviewer should be able to read every line that goes into a binary.

## Decision

Dependencies are vendored with `go mod vendor` and committed under `vendor/`. Go builds from it without the network.
`mise run vendor` tidies go.mod and refreshes the directory; a pre-commit hook runs it when go.mod or go.sum change.

## Consequences

Builds need no network. A dependency update carries the vendored diff, which is generated and marked as such for
review, and Renovate leaves Go updates for a person to merge.

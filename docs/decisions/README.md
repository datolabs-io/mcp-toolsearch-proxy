# Decisions

Architecture decision records: why the project is the way it is. Add a new numbered file for each decision; supersede
rather than rewrite an accepted one.

| # | Decision |
| --- | --- |
| [1](0001-vendor-dependencies.md) | Vendored dependencies |
| [2](0002-stateless-transport.md) | Streamable HTTP without sessions |
| [3](0003-official-sdk.md) | The official MCP Go SDK |
| [4](0004-callers-own-token.md) | Every upstream request carries the caller's own token |
| [5](0005-catalog-per-token.md) | A catalog per caller, keyed by a hash of the token |
| [6](0006-built-in-static-model.md) | A static embedding model, run in process |
| [7](0007-standalone-proxy.md) | A standalone proxy, not a gateway plugin |
| [8](0008-several-queries-in-one-search.md) | Several queries in one search |

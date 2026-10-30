# Security policy

## Supported versions

Only the latest release receives fixes while the project is on v0.

## Reporting a vulnerability

Please do not open a public issue. Report it privately through
[GitHub's private vulnerability reporting](https://github.com/datolabs-io/mcp-toolsearch-proxy/security/advisories/new).

Include what you found, how to reproduce it, and its impact. Never include real tokens, tool arguments or tool
results in a report; describe them instead.

Of particular interest: any way for a caller to see or call, through the proxy, a tool the upstream would not let
them use directly, and anything that puts a token, query, argument or result in a log.

## What the proxy passes on

The proxy forwards the upstream's instructions to every caller's client, which puts them in the model's context next
to the user's own instructions. Behind a gateway, that text comes from every server the gateway joins, so any one of
them can put instruction-grade text in front of every user of the proxy. Trust each upstream server's instructions as
you trust its tools, or set `-max-instructions 0` to forward none.

`call_read_tool` trusts each upstream tool's `readOnlyHint`. A server that marks a write as read-only gets it called
without the client asking; `-write-tools` names tools to treat as writes regardless (see
[docs/clients.md](docs/clients.md)).

## Response time

We aim to acknowledge reports within 5 working days.

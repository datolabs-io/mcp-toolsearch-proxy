# Privacy

The proxy stands between a model and every tool its user can reach, so it sees queries, arguments and results that
can be as private as the tools are. This page says what it keeps and what it writes down.

## Kept in memory

For each caller, the proxy keeps the tools the upstream listed for them: names, descriptions and schemas, with their
vectors, and the upstream server's instructions, which it forwards to that caller's client in the handshake. The catalog
is looked up by a SHA-256 of the caller's `Authorization` header, and the header itself is not stored with it. A token
is held only while work done for it runs: a request, and a catalog listing the request starts. A listing can outlive
the request that started it: it is shared between a caller's concurrent requests, an expired catalog is listed again
in the background, and the handshake leaves a listing running when it stops waiting. Such a listing holds the token
until it ends, within `-list-timeout` (15 s). A catalog is listed again after `-catalog-ttl` (10 minutes). While that
fails, the old one is still used, up to `-catalog-max-age` (six TTLs, an hour by default), and then dropped. The proxy
keeps at most `-max-catalogs` (16), dropping the oldest first.

Queries, arguments and results pass through without being kept. The only thing the proxy writes to disk is the model
that `fetch-model` downloads.

## Written to the log

A failed tool call gets one line: which of the proxy's tools it was, the upstream tool's name for a call tool, the
outcome and how long it took. A call that got no result from the upstream gets a second line with the same tool name,
a coarse class (timeout, refused, unauthorized, dropped or answered) and, unless the upstream answered, the transport's
error, which can hold the upstream's address. Start-up lines name the upstream URL without credentials or a query, and
the model. Catalog builds that fail are logged with the step that failed, and a build whose listing repeats a tool name
is logged with the repeated names and their counts.

Queries, arguments, results, tokens and anything that identifies the caller are never logged. Neither are error
messages from tools or the upstream's error answers, because they often repeat the arguments. A test sends a secret
through every tool and checks that it never shows up in the log.

## Metrics

With `-metrics-addr`, Prometheus metrics count tool calls by the proxy's tool and outcome, operations upstream by
kind and outcome, catalog builds by outcome, and searches by mode, with timings, the size of the largest catalog, the
number kept and the requests in flight ([monitoring.md](monitoring.md) lists them). No label carries a query, an
upstream tool name, a caller or a token: a name a client makes up is counted as `unknown`.

## Upstream

Each request upstream carries the caller's `Authorization` header and no other credential. The upstream server sees
the caller's requests as the caller's, and its own logs and policies apply to them.

# mcp-toolsearch-proxy architecture

`mcp-toolsearch-proxy` is an MCP server with five tools, `search_tools`, `describe_tool`, `call_read_tool`,
`call_read_tools` and `call_write_tool`, in front of one upstream MCP server with many. It is a client of that upstream
on behalf of each caller, with the caller's own token (ADR 4), and keeps nothing on disk but the model it searches with.

`call_read_tool` calls only tools the upstream marks read-only (`readOnlyHint`), and is itself marked read-only;
`call_write_tool` calls any tool and is marked destructive. A client can then let reads run without asking and keep
asking before everything else. The upstream's annotations decide which is which, so a server that marks a write as
read-only lets it through `call_read_tool`; the gateway's authorization still applies to every call. The check reads
the caller's catalog, so a tool that loses `readOnlyHint` upstream still runs through `call_read_tool` until the
catalog is listed again. That takes up to `-catalog-max-age`: an expired catalog is still served for the request that
starts its relist, and while listing fails. Tools the operator names in `-write-tools` count as writes whatever the
upstream marks them: the setting only narrows what `call_read_tool` allows, and gives the proxy no say over what a
caller may call.

## Layout

```text
cmd/mcp-toolsearch-proxy/  the program: configuration, start-up, signals, fetch-model
internal/server/           the MCP server, its five tools, argument checks, the HTTP handler, logging
internal/catalog/          a catalog of tools per caller, built from the caller's own tools/list (ADR 5)
internal/search/           lexical ranking (BM25 over folded, stemmed words), cosine ranking and their fusion
internal/embed/            the built-in static embedding model: tokenizer, weights, download (ADR 6)
internal/upstream/         the client of the upstream MCP server, forwarding the caller's Authorization
internal/metrics/          Prometheus metrics
tests/e2e/                 end-to-end tests, the agentgateway setup and Docker Compose stack they run against
docs/                      deployment, monitoring, privacy, the agentgateway guide, decisions/ (ADRs)
vendor/                    vendored dependencies (ADR 1)
```

## A request

1. A client calls `search_tools` through the gateway, which authenticates it and passes its token on.
2. The server reads the `Authorization` header of the request and asks the catalog store for that caller's catalog.
   The store keys catalogs by a SHA-256 of the whole header value; on a miss it lists the tools upstream with that
   header, indexes them and embeds them. Concurrent requests of one caller share one build. A catalog older than
   `-catalog-ttl` is still served, and listed again in the background. When that fails, the old catalog stays in use
   up to `-catalog-max-age` (six TTLs by default), and after that the request fails. A failed build is remembered
   per caller for 5 s, doubling with each failure in a row up to a minute, so an upstream that is down is not listed
   again for every request.
3. The query is ranked lexically and, with the model, semantically; reciprocal rank fusion merges the two rankings.
   Names and short descriptions of the best matches go back.
4. The input schemas of the best few matches go back with them, unless the call asks for names only, so a call
   usually follows a search without `describe_tool`. An empty catalog is an error that says the upstream lists no
   tools for the caller, since searching again cannot help.
5. `describe_tool` answers from the same catalog, suggesting the closest tools for a name it does not know. A call tool
   checks the name is in the caller's catalog, listing again once if it is not and the catalog is at least 30 s old,
   then calls the tool upstream with the caller's header and returns its result unchanged.

The handshake is per caller too. MCP gives a server one `instructions` string, which clients put in the model's context;
behind the proxy, the upstream's would otherwise be lost. `ProxyHandler` answers each request with a server whose
instructions are the proxy's own followed by those of the caller's catalog, which its build received in the upstream
handshake, cut at `-max-instructions` characters. Servers are kept per distinct text. Without a catalog, as without a
token or while the upstream is down, the handshake carries the proxy's own instructions alone. Only a handshake
(`initialize`, or `server/discover` under MCP 2026-07-28) looks the instructions up, from the `Mcp-Method` header or the
first 64 KiB of the body. Without a catalog kept, it waits up to `-handshake-wait` (3 s) for one. The proxy does not
split the upstream's text by server: the protocol has no structure for it. Clients cut instructions again, far below
the default cap (Claude Code at 4,096 characters), so only the start of a long upstream text reaches a model.

Every upstream operation opens its own connection and closes it after: the protocol keeps no state between requests, and
a connection kept per caller would outlive the token it carries. The SDK speaks MCP 2026-07-28 and falls back to older
versions as the upstream requires. A listing that fails is tried again, `-list-retries` times (3 by default) with a
pause that starts at 250 ms and doubles, all within the listing's timeout (`-list-timeout`, 15 s), unless the upstream
refused the caller with 401 or 403: a gateway that fans out to many servers fails now and then on its own, and listing
changes nothing. A tool call is never retried, since one that failed may still have had its effect.

The call tools read their input strictly: only `name` and `arguments`, each once and in that case, and `arguments` an
object in which no object, at any depth, repeats a key, so an approval prompt cannot show one tool or value while
another is called. The
arguments then go upstream as the caller's bytes, never decoded and encoded again, so large integers and number
formats arrive unchanged. A call that gets no result says which of three cases it was: the upstream answered with an
error, which is passed on; the upstream could not be reached or refused the credential, so the tool was not called; or
the request went out and no result came back, in which case `call_write_tool` says the tool may still have run. The
text names a coarse class (timeout, refused, unauthorized, dropped, too large) and never the transport's error, which
holds the upstream's address; that goes to the log.

Before a call goes upstream, its arguments are checked against the tool's input schema in the caller's catalog, and a
call that does not fit is refused with what is wrong and the schema (cut to 8 KiB like a search result's, or a note that
it is too large), so the model can retry without `search_tools` or `describe_tool`. The check is shallow on purpose:
required properties, and the type, enum and const of each property given, at the top level only, following local `$ref`s
and `anyOf`, `oneOf` and `allOf` for those keywords and reading OpenAPI's `nullable`. Servers read their own schemas
more loosely than they write them (zod writes `additionalProperties: false` and strips unknown keys at run time; drafts
differ on `$ref` beside other keywords; `format` and `pattern` differ between implementations), so a full validation
would refuse calls they accept. Unknown keys are named in a refusal but never refuse a call alone. A schema that
google/jsonschema-go cannot read or resolve, as with a remote `$ref`, a pattern Go cannot compile or draft 4's boolean
`exclusiveMinimum`, is not checked, nor is one whose root is a `$ref` or that holds `$id`s of its own, whose refs may
not read as the check reads them, nor the arguments of a name listed more than once. A refusal counts as the outcome
`invalid_arguments` and never logs the arguments. An upstream answer of JSON-RPC invalid params (-32602) comes with the
schema too, and an unknown name that is one or two characters off a single tool, or differs only in case, comes with
that tool's schema.

`call_read_tools` takes up to `-max-batch-calls` (10) calls, each read as `call_read_tool` reads its input, and each
through the same checks; one that fails is reported on its own. They run four at a time, all within `-call-timeout`
for the whole batch: a call still waiting for its turn when the time is up is not started. The result holds each
call's content after a line with its number, tool and whether it failed, and is an error only when every call failed;
structured content, when any call has some, is `{"results": [...]}` in order, since MCP wants an object there. Results
past `-max-response-bytes` for the whole batch are dropped as they arrive, with a note in their place. It is a tool of
its own rather than another form of `call_read_tool`'s input, so that each input schema stays a plain object, and
writes have no batch tool, so that each keeps its own approval in the client.

The upstream decides which tool a call reaches, by name. A listing that holds one name more than once is ambiguous:
the catalog indexes the name once, `search_tools` shows it with a note, and it counts as read-only only if every copy
is, so `call_read_tool` refuses it otherwise. A tool marked both `readOnlyHint` and `destructiveHint` contradicts itself
and counts as one that may change things.

When the proxy stops, requests running get `-shutdown-grace` to finish (the call timeout plus 15 s by default) before
their connections are closed. Closing one cancels its call upstream, but a write the upstream already passed on may
still complete there, so the client sees no result for a write that ran. The systemd unit's `TimeoutStopSec=` leaves
room for the default grace; raise both together.

## Memory

The packaged unit caps the service at 512 MB, of which the model takes about 130 MB, and sets `GOMEMLIMIT` a little
below the cap. Everything else that grows is bounded:

- Catalogs: at most `-max-catalogs` (16), one per token, each several MB for a thousand tools. One past
  `-catalog-max-age` is dropped, since it is never served again, by a timer when no request comes.
- Requests: at most `-max-in-flight` at once, and at most `-max-in-flight-bytes` of their bodies (32 MiB). A request
  weighs its `Content-Length`, or the largest body allowed (24 MiB) when it gives none, and at most the whole budget,
  so any request is served alone. A call holds about ten times its body at its peak: the transport's copy, the
  JSON-RPC message, the parsed input and the request upstream. A request that does not fit is refused with 503, as one
  over the count is.
- Upstream responses: the SDK reads one whole, and it is decoded and encoded again on its way back, so a result costs
  about three times its size. What an operation receives after the handshake, a call's result or every page of a
  listing together, is cut off past `-max-response-bytes` (32 MiB). The call fails alone, saying the result was too
  large, and for `call_write_tool` that the tool may have run. A listing fails, and is not retried, past the same size,
  `-max-tools` (10,000) or 100 pages. The bound is per request: calls in flight together may each take that much.
- Input: a query over 1,000 characters, or a tool name over 256 bytes, is refused before any lookup, since lexical
  search costs the catalog's size times the query's words. The tokenizer stops segmenting once it has the pieces it
  reads (512), and segments of the last word only what those pieces can cover, with room for a run of unknown
  characters, which fuses into one piece.

Catalogs of tokens whose listings are the same are not shared: each refreshed token costs a listing and an index of its
own.

## Search

`internal/search` knows nothing of MCP: it ranks `Doc`s, each a name, title, description and parameters.

Lexical ranking splits words at punctuation, underscores and case changes, lowercases them and drops diacritics, so
a query typed without Lithuanian letters still matches. Each word then loses the longest Lithuanian or English
ending it has. BM25 ranks the result, with a tool's name counting three times as much as its description and its
title twice as much.

Semantic ranking embeds each tool's text and the query, and ranks by cosine similarity.

Reciprocal rank fusion (k = 60) merges the top 50 of each ranking. The two scores don't need calibrating against
each other, and a tool that both rankings place well beats one that only one of them puts first.

The evaluation set in `internal/search/testdata/eval` holds the floors tests enforce (see README).

## The built-in model

A model2vec static model is a vector per piece of a tokenizer's vocabulary; a text's embedding is the normalized
mean of its pieces' vectors. `internal/embed` implements the BGE-M3 SentencePiece Unigram tokenizer (NFKC,
punctuation split, Metaspace, Viterbi segmentation) and reads the published safetensors, quantizing each row to
8 bits as it reads, so the 512 MB of float32 weights become 128 MB in memory. A test compares token ids and vectors
with those of the reference Python implementation. Loading takes about a second; embedding a query, microseconds.

The model's files are fetched by `fetch-model`, from a pinned revision, and checked against SHA-256 digests in
`internal/embed/fetch.go`. The server itself never downloads anything.

## Transport

Streamable HTTP in stateless mode (ADR 2): every POST to the MCP path is a complete exchange, answered with a stream of
server-sent events, or 202 for a notification. A tool sends the response's headers as soon as it reaches for the
catalog or the upstream, then a comment (`: keepalive`) every 5 s until its result, which clients skip. Proxies in
front give up on a backend that sends nothing for a while: agentgateway waits 10 s for the headers by default, others
about 100 s for any byte. A call may take up to `-call-timeout`, and a single JSON body could only go out at its end.
`/healthz` answers a liveness probe without contacting the upstream. Requests from browser pages are refused unless
their origin is listed, and off loopback the `Host` header is checked against a list when one is given.

## Configuration

Flags, each with an environment variable as its default; a flag on the command line wins. The proxy takes no secret
at all (ADR 4). [docs/configuration.md](docs/configuration.md) is generated from the flag set, and a test fails when
it falls behind; `mise run docs:config` regenerates it.

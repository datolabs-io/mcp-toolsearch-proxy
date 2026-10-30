# mcp-toolsearch-proxy

[![CI](https://github.com/datolabs-io/mcp-toolsearch-proxy/actions/workflows/ci.yaml/badge.svg)](https://github.com/datolabs-io/mcp-toolsearch-proxy/actions/workflows/ci.yaml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/datolabs-io/mcp-toolsearch-proxy/badge)](https://scorecard.dev/viewer/?uri=github.com/datolabs-io/mcp-toolsearch-proxy)

**mcp-toolsearch-proxy** lets a model use hundreds or thousands of MCP tools without loading them. On 500 tools, each
request carries 97% fewer tokens of tool definitions, a task costs about half as much, and the model reaches the right
tool nearly as often as with every tool loaded ([benchmarks](docs/benchmarks.md)).

It is an [MCP](https://modelcontextprotocol.io) server that puts five tools in front of an MCP server with far too many
to list to a model, such as a gateway that aggregates dozens of servers:

| Tool | Does |
| --- | --- |
| `search_tools` | finds the tools that can do what a query describes, with the input schemas of the best few, for up to 5 queries at once; with the built-in model, in English, Lithuanian or any of 100 other languages |
| `describe_tool` | returns the full description and input schema of up to 10 tools |
| `call_read_tool` | calls a tool the upstream marks read-only, and refuses any other |
| `call_read_tools` | calls up to 10 read-only tools at once, in parallel, with a result for each (`-max-batch-calls`; 0 leaves it out) |
| `call_write_tool` | calls any tool, for those that may create, change or delete something |

A catalog of a thousand tools costs a model over 100,000 tokens of tool definitions before it does anything, and many
clients cap how many tools they load anyway. Behind the proxy, the model sees five, in 1,735 tokens.

A call whose arguments do not fit the tool's input schema (a required property missing, a wrong type, a value outside
an enum) is refused before it reaches the upstream, with what is wrong and the schema, so the model can fix it at once.
The same goes for an unknown name that is clearly a typo of one tool, and for an upstream that answers invalid params.

Calls come in separate tools so that a client can tell them apart: `call_read_tool` and `call_read_tools` are marked
read-only and can be allowed to run without asking, while `call_write_tool` is marked destructive and can keep asking
every time. Writes are never batched, so each keeps its own approval. A tool counts as
read-only only when the upstream server marks it so (`readOnlyHint`); unmarked tools are writes, and so are any the
operator lists in `-write-tools`. Allowing `call_read_tool` allows every read-only tool of every server behind the
upstream, including servers added later; [docs/clients.md](docs/clients.md) says what that grant covers and how to set
up claude.ai and Claude Code.

```text
client ──▶ gateway /mcp/search ──▶ mcp-toolsearch-proxy ──▶ gateway /mcp/all ──▶ MCP servers
```

## How it works

The proxy has no credentials. Each request it sends upstream carries the caller's `Authorization` header and nothing
else, so the upstream server (usually the same gateway, on another route) authenticates and authorizes the caller as
if they had come directly. Whatever the caller isn't allowed to see or call, the proxy can't see or call either.

Each caller gets their own catalog: the tools the upstream lists for that caller. It is kept for ten minutes, then
listed again in the background while the old one is still used, and it is looked up by a hash of the token. A claim
inside the token, such as `sub`, would be easier to key on but can't be trusted without verifying the token, so the
proxy doesn't use one.

Search combines two rankings. BM25 matches words in tool names, descriptions and parameters, with Lithuanian and
English endings stemmed and diacritics ignored. A multilingual embedding model,
[potion-multilingual-128M](https://huggingface.co/minishlab/potion-multilingual-128M) (MIT), matches by meaning. The
model runs inside the proxy, in pure Go, without a GPU or an embedding service, and embedding a query takes
microseconds. Without the model, search falls back to BM25 alone, which matches only words a tool's text shares with
the query: it stems no endings but Lithuanian and English ones, and does not split Chinese or Japanese text into words.

The upstream server's own instructions still reach the model. The proxy answers each caller's handshake with its own
short guidance followed by the upstream's `instructions`, unchanged, as the caller's catalog received them, up to
`-max-instructions` characters (10,000 by default; longer ones are cut, with a note saying so). Clients cut it again,
much sooner: Claude Code keeps about 4,000 characters of a server's instructions, so only the start of a long upstream
text reaches the model (see [docs/clients.md](docs/clients.md#server-instructions-are-cut-by-the-client)). And `search_tools`
returns the input schemas of its best few matches (`-search-schemas`, 3 by default), so a call usually follows a
search directly.

Logs and metrics record tool names, outcomes and timings. Queries, arguments, results and tokens never appear in
them; [docs/privacy.md](docs/privacy.md) has the details. The transport is streamable HTTP without sessions, for
MCP 2026-07-28 clients and older ones, so a restart or a load balancer doesn't break a client's connection.

It works with any upstream that speaks streamable HTTP MCP and authorizes by bearer token.
[docs/agentgateway.md](docs/agentgateway.md) shows a complete [agentgateway](https://agentgateway.dev) setup, which
the end-to-end tests run.

## Status

Early development. The tools, their arguments and the settings may still change before 1.0.

## Install

Homebrew, on macOS or Linux:

```bash
brew install --cask datolabs-io/tap/mcp-toolsearch-proxy
```

Packages (`.deb`, `.rpm`), archives and a container image are attached to each
[release](https://github.com/datolabs-io/mcp-toolsearch-proxy/releases). See [docs/deploy.md](docs/deploy.md).

```bash
mcp-toolsearch-proxy fetch-model -model-dir ./model        # once: ~530 MB, checked against pinned digests
mcp-toolsearch-proxy -upstream https://gateway.example.com/mcp/all -model-dir ./model
```

`-embedder none` skips the model and searches lexically only. [docs/configuration.md](docs/configuration.md) lists
every setting, as `mcp-toolsearch-proxy -h` does; each has an environment variable too. Then connect a client:
[docs/clients.md](docs/clients.md).

Clients that already defer MCP tools and search them on demand, as Claude Code does, gain less from the proxy; see
[when you don't need this](docs/clients.md#when-you-dont-need-this).

## Search quality

The repository holds an evaluation set of made-up tools, described in English, and questions people ask about them
in English and Lithuanian. Recall at 5 today:

| Search | English | Lithuanian |
| --- | --- | --- |
| lexical only (`-embedder none`) | 52% | 0% |
| hybrid with the built-in model | 81% | 79% |

Words alone can't connect a Lithuanian question to an English description. The model can. Tests hold changes to
ranking to these numbers.

## Benchmarks

On a made-up catalog of 500 tools, a model given 30 questions reaches the right tool nearly as often through the proxy
(90%) as with every tool loaded (95%), at about half the cost per task, and more often than with Anthropic's tool
search (87%). [docs/benchmarks.md](docs/benchmarks.md) has the numbers, what a client loads in each mode at up to 1,490
tools, and how to run the benchmarks.

## Development

```bash
mise install          # the pinned toolchain and the git hooks
mise run ci           # lint, scan and unit tests, as CI runs them
mise run test:model   # the built-in model against its reference implementation and the evaluation set
mise run e2e:up       # agentgateway, the proxy and fixture MCP servers, with Docker Compose
mise run test:e2e     # the end-to-end tests, against that stack or any at E2E_SEARCH_URL and E2E_ALL_URL
mise run docs:config  # regenerate docs/configuration.md after changing a flag
```

See [CONTRIBUTING.md](CONTRIBUTING.md) and [ARCHITECTURE.md](ARCHITECTURE.md).

## Licence

[MIT](LICENSE). The built-in model is published by Minish under the MIT licence and is downloaded, not distributed,
by this project.

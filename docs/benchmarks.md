# Benchmarks

The benchmarks in [`bench/`](../bench) measure what a client spends on a large catalog with the proxy in front and
without it. They are Go benchmarks in a module of their own, so their dependencies, the Anthropic SDK among them,
stay out of the proxy's module and its `vendor/`.

Both run on made-up catalogs: the 50 tools of the [evaluation set](../internal/search/testdata/eval), padded with
distractors generated from [`bench/data/domains.json`](../bench/data/domains.json) up to 1,490 tools. The distractors
come from 60 made-up systems (a fleet manager, a library catalogue, a firewall…) with the same six verbs each, so they
share words with the real tools without doing what any question asks. An MCP server in the test process serves the
catalog, and the proxy, built from this repository, runs in front of it with its default settings.

## What a client loads

```bash
mise run bench:context
```

For each catalog size, `BenchmarkContext` reports what a client sends with every request before the model does
anything, as `bytes/listing`, and as `tokens/listing` too when `ANTHROPIC_API_KEY` is set (token counting is free):

| Mode | What it is |
| --- | --- |
| `all` | the upstream's whole tool list, as a client loads it |
| `toolsearch` | the same tools behind Anthropic's tool search (`defer_loading`), counted in tokens only |
| `proxy` | the proxy's tools and its guidance |
| `proxy-search` | one `search_tools` result on top, averaged over 20 questions |

Measured on 2026-10-30 with the built-in model, tokens counted for Claude Opus 5.5, with `search_tools` taking
several queries:

| Tools | `all` | `toolsearch` | `proxy` | `proxy-search` |
| ---: | ---: | ---: | ---: | ---: |
| 50 | 5,118 (11.7 KB) | 643 | 1,735 (4.0 KB) | 496 (1.3 KB) |
| 250 | 27,814 (66.8 KB) | 643 | 1,735 (4.0 KB) | 520 (1.4 KB) |
| 500 | 56,234 (135.8 KB) | 643 | 1,735 (4.0 KB) | 521 (1.4 KB) |
| 1,000 | 112,975 (273.4 KB) | 643 | 1,735 (4.0 KB) | 523 (1.4 KB) |
| 1,490 | 168,322 (407.3 KB) | 643 | 1,735 (4.0 KB) | 521 (1.4 KB) |

Neither the proxy nor Anthropic's tool search costs more as the catalog grows. Tool search starts smaller: its
search tool alone is loaded, where the proxy has five tools and a page of guidance. What each adds per search, and in
how many turns a model gets to the right tool, is what the agent benchmark below measures.

## How a model does with each

```bash
ANTHROPIC_API_KEY=… mise run bench:agent
```

`BenchmarkAgent` gives a model the same questions three ways (every tool loaded, Anthropic's tool search, the proxy)
and runs its tool calls until it calls the tool the evaluation set says answers the question, ends its turn, or has
made 8 requests. Per task, it reports `success/task` (the share that reached the right tool), `turns/task`,
`in-tokens/task` and `out-tokens/task`, `usd/task` and `usd/success`. Requests use prompt caching in every mode, as
clients do.

**Its requests are billed to the API key.** The benchmark is skipped unless `BENCH_AGENT=1` is set, which
`bench:agent` does, so `go test -bench .` alone never calls the API. The defaults (30 questions, half English and half
Lithuanian, on 500 tools, three times each, with Claude Opus 5.5 at medium effort) make 270 runs.

Compare the modes with [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat):

```bash
benchstat -col /mode .cache/bench/agent.txt
```

Each run is also appended to `.cache/bench/agent.jsonl`: the question, whether it succeeded, the tools the model
called (Anthropic's own as `server:<name>`), what it searched for, the upstream tools that ran, why it ended, and its
tokens and cost.

Measured on 2026-10-30 on 500 tools, 30 questions six times each (`BENCH_COUNT=6`), Claude Opus 5.5 at medium
effort, with `search_tools` taking several queries ([ADR 8](decisions/0008-several-queries-in-one-search.md)) and
the made-up tools checking their arguments, as the proxy does; medians, and benchstat's comparison with `all`:

| Mode | Right tool | Turns | Input tokens | Cost per task | Cost per success | 30 questions |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `all` | 95% | 1.37 | 77,047 | $0.0195 | $0.0211 | 28.0 s |
| `toolsearch` | 87% (−9%, p=0.006) | 1.28 | 3,610 | $0.0132 | $0.0152 | 49.3 s |
| `proxy` | 90% (no significant difference, p=0.14) | 2.63 | 6,329 | $0.0104 | $0.0116 | 43.1 s |

Through the proxy, a model reaches the right tool nearly as often as with every tool loaded, at about half the cost per
task and per success, and more often than with Anthropic's tool search, at a fifth less. It takes more turns, since a
search and a call are two; tool search searches inside a turn, on Anthropic's side, which its turns do not show but its
time does. The first of the six passes in each mode costs more than the rest, the most with every tool loaded, since
parallel requests each write the prompt cache before any can read it; the medians leave that out.

What the runs show, from `agent.jsonl`:

- A model through the proxy searches again, when it does, for a value the user did not give, such as where they are,
  today's date or which account they mean, not to rephrase its first search. With several queries in one search it
  looks for both at once: it did in 63 of 180 runs, with no word about it in the proxy's guidance, and runs that
  searched twice fell from 62 to 43.
- Anthropic's tool search is BM25, so a Lithuanian question has to share words with an English description: "kada
  abu laisvi susitikti" (when are both free to meet) never reached `calendar_find_free_time` with it.
- The questions that still fail need a record the made-up tools do not return: the model looks up Maria, or the
  customer to invoice, finds nothing, and asks.

Settings, all optional:

| Variable | Default | Meaning |
| --- | --- | --- |
| `BENCH_SIZES` | `500` (`bench:context`: 50 to 1,490) | catalog sizes, comma-separated |
| `BENCH_MODES` | `all,toolsearch,proxy` | the modes to run |
| `BENCH_TASKS` | `30` | how many questions |
| `BENCH_COUNT` | `3` | how many times each runs |
| `BENCH_MODEL` | `claude-opus-5-5` | the model |
| `BENCH_EFFORT` | `medium` | its output effort |
| `BENCH_PRICES` | `4,20,0.2,5` | dollars per million input, output, cache-read and cache-write tokens; set it with another model |
| `BENCH_PARALLEL` | `4` | runs at once |
| `BENCH_MAX_TURNS` | `8` | requests per run |

A refusal counts as a failure, and no fallback model is used, so that every request a mode makes is answered by the
model named.

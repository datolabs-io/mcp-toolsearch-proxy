# 8. Several queries in one search

Date: 2026-10-30 · Status: accepted

## Context

A model often needs more than the tool for its task: a tool for a value the user did not give, such as where they
are, today's date or which account they mean. Given every tool, it can see at once that none supplies the value, and
assumes one. Through the proxy it has to search to find that out, a turn of its own. In the agent benchmark, a third
of the runs through the proxy searched twice, and the second search was always for such a value, never the first
search rephrased.

## Decision

`search_tools` takes `queries`, up to five, instead of `query`, and returns the matches of each in `results`, in the
order asked. `limit` applies to each query. A tool found by more than one query brings its input schema once, with
the first. `query` is unchanged, so is its result, and giving both is refused. It is an argument of the one search
tool rather than a tool of its own: unlike the call tools, whose split lets a client allow reads and keep asking for
writes, every search only reads, and every tool listed costs each request its definition.

## Consequences

A model can look for the tool and for what it needs in one turn. A search with several queries costs a search per
query, in time and in `mcp_toolsearch_searches_total`, and returns more text than one; the limit of five bounds both.

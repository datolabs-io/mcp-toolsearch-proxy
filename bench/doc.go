// Package bench measures what a client spends with and without
// mcp-toolsearch-proxy in front of a large MCP server, as Go benchmarks.
//
// BenchmarkContext measures what a client loads before the model does
// anything: the upstream's whole tool list, the same deferred behind
// Anthropic's tool search, or the proxy's tools and guidance, and what one
// search costs on top. Bytes always; tokens too when ANTHROPIC_API_KEY is
// set, which the free count_tokens endpoint needs.
//
// BenchmarkAgent runs the same tasks against a model three ways (every
// tool loaded, Anthropic's tool search, the proxy) and reports how often
// the right tool was called, in how many turns, at what cost. Its requests
// are billed, so it runs only with BENCH_AGENT=1 and ANTHROPIC_API_KEY set.
//
// Both run on made-up catalogs: the evaluation set's tools, padded with
// distractors generated from data/domains.json, served by an MCP server in
// the process, with the proxy built from this repository in front. Nothing
// here touches a real tool catalog.
package bench

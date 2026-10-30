# 6. A static embedding model, run in process

Date: 2026-10-28 · Status: accepted

## Context

Lexical search finds tools by the words in their descriptions. It cannot match a question to a tool described in
other words, or in another language: tool descriptions are mostly English, and questions are often not. Semantic
search needs an embedding model. A remote embedding service would add a dependency, a credential and a network call
to every search; a transformer run in process needs cgo or is too slow on a CPU, and too large for a small service.

## Decision

The proxy runs a model2vec static model in pure Go: a vector per piece of the tokenizer's vocabulary, averaged over a
text's pieces. The default, potion-multilingual-128M (MIT), is distilled from BGE-M3 and covers 101 languages. Its
tokenizer is implemented here and tested against the reference implementation, and its weights are held as 8-bit
integers. `fetch-model` downloads a pinned revision and checks its digests; the server never downloads. Lexical and
semantic rankings are fused, and without the model the proxy searches lexically.

## Consequences

Search needs no service and no network, embeds a query in microseconds and works across languages, at about 130 MB
of memory and a 530 MB download. Its quality is below a transformer's; the evaluation set measures it, and a remote
embedder can be added behind the same interface if one is needed.

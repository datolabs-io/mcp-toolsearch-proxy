// Package search ranks tools against a query written in natural language.
//
// Ranking is lexical, with BM25 over words folded and stemmed for English and
// Lithuanian, and, given vectors from an embedding model, semantic too; the
// two rankings are merged by reciprocal rank fusion. Without vectors, or when
// the model fails, search is lexical alone.
package search

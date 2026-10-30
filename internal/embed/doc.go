// Package embed turns text into vectors whose cosine similarity reflects
// meaning, for semantic search.
//
// Static runs a model2vec model in process, in pure Go. Cache keeps the
// vectors of texts already embedded, so that a catalog of tools is embedded
// once, whoever lists it.
package embed

package search

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// BM25 parameters: k1 is how quickly repeating a word stops adding to a
// tool's score, b how much a long description is held against it.
const (
	k1 = 1.2
	b  = 0.75
	// smoothing keeps the weight of a term every document has above zero.
	smoothing = 0.5
)

// Field weights: a word in a tool's name says more about it than one in its
// title, which says more than one in its description or its parameters.
const (
	weightName  = 3
	weightTitle = 2
	weightBody  = 1
)

// rrfK damps reciprocal rank fusion: the larger it is, the less the very
// first places of either ranking dominate the merged one. 60 is the value
// the method was published with and works without tuning.
const rrfK = 60

// Param is one parameter of a tool's input.
type Param struct {
	Name        string
	Description string
}

// Doc is a tool as search sees it.
type Doc struct {
	Name        string
	Title       string
	Description string
	Params      []Param
}

// Text is what an embedding model is given for d: its name, with the
// separators an API name uses read as spaces, its title, its description
// and its parameters.
func (d Doc) Text() string {
	var sb strings.Builder
	sb.WriteString(strings.NewReplacer("_", " ", "-", " ").Replace(d.Name))
	for _, s := range []string{d.Title, d.Description} {
		if s != "" {
			sb.WriteString("\n")
			sb.WriteString(s)
		}
	}
	for _, p := range d.Params {
		sb.WriteString("\n")
		sb.WriteString(p.Name)
		if p.Description != "" {
			sb.WriteString(": ")
			sb.WriteString(p.Description)
		}
	}
	return sb.String()
}

// Hit is a document of an index, by position, and its score in a ranking.
type Hit struct {
	Doc   int
	Score float64
}

// Index ranks a fixed set of documents. It is safe for concurrent use.
type Index struct {
	docs []Doc
	tf   []map[string]float64 // weighted term frequencies, per document
	lens []float64            // weighted lengths
	df   map[string]int       // documents each term appears in
	avg  float64              // average weighted length
	vecs [][]float32          // unit vectors, per document; nil without
}

// NewIndex indexes docs. vecs, when not nil, holds an embedding of each
// document's Text, in the same order; a nil entry leaves that document out
// of semantic ranking.
func NewIndex(docs []Doc, vecs [][]float32) *Index {
	ix := &Index{
		docs: docs,
		tf:   make([]map[string]float64, len(docs)),
		lens: make([]float64, len(docs)),
		df:   map[string]int{},
	}
	var total float64
	for i, d := range docs {
		tf := map[string]float64{}
		add := func(s string, w float64) {
			for _, tok := range Tokens(s) {
				tf[tok] += w
				ix.lens[i] += w
			}
		}
		add(d.Name, weightName)
		add(d.Title, weightTitle)
		add(d.Description, weightBody)
		for _, p := range d.Params {
			add(p.Name, weightBody)
			add(p.Description, weightBody)
		}
		for tok := range tf {
			ix.df[tok]++
		}
		ix.tf[i] = tf
		total += ix.lens[i]
	}
	if len(docs) > 0 {
		ix.avg = total / float64(len(docs))
	}
	if len(vecs) == len(docs) {
		ix.vecs = make([][]float32, len(vecs))
		for i, v := range vecs {
			ix.vecs[i] = Normalize(v)
		}
	}
	return ix
}

// Len is the number of documents.
func (ix *Index) Len() int { return len(ix.docs) }

// Doc returns document i.
func (ix *Index) Doc(i int) Doc { return ix.docs[i] }

// Semantic reports whether the index holds vectors to rank by.
func (ix *Index) Semantic() bool { return ix.vecs != nil }

// Lexical ranks the documents by BM25 against query and returns the k best
// with a score above zero.
func (ix *Index) Lexical(query string, k int) []Hit {
	terms := Tokens(query)
	if len(terms) == 0 || len(ix.docs) == 0 {
		return nil
	}
	n := float64(len(ix.docs))
	var hits []Hit
	for i, tf := range ix.tf {
		var score float64
		for _, term := range terms {
			f := tf[term]
			if f == 0 {
				continue
			}
			df := float64(ix.df[term])
			idf := math.Log(1 + (n-df+smoothing)/(df+smoothing))
			score += idf * f * (k1 + 1) / (f + k1*(1-b+b*ix.lens[i]/ix.avg))
		}
		if score > 0 {
			hits = append(hits, Hit{Doc: i, Score: score})
		}
	}
	return top(hits, k)
}

// Similar ranks the documents by cosine similarity to q, an embedding of
// the query, and returns the k most similar. It returns nil when the index
// holds no vectors or q has another dimension.
func (ix *Index) Similar(q []float32, k int) []Hit {
	if ix.vecs == nil || len(q) == 0 {
		return nil
	}
	q = Normalize(q)
	hits := make([]Hit, 0, len(ix.vecs))
	for i, v := range ix.vecs {
		if len(v) != len(q) {
			continue
		}
		var dot float64
		for j := range v {
			dot += float64(v[j]) * float64(q[j])
		}
		hits = append(hits, Hit{Doc: i, Score: dot})
	}
	return top(hits, k)
}

// Fuse merges rankings by reciprocal rank fusion: a document scores
// 1/(rrfK+rank) for each ranking it appears in, so one placed well in both
// beats one placed first in only one. It returns the k best.
func Fuse(k int, rankings ...[]Hit) []Hit {
	scores := map[int]float64{}
	for _, r := range rankings {
		for rank, h := range r {
			scores[h.Doc] += 1 / float64(rrfK+rank+1)
		}
	}
	hits := make([]Hit, 0, len(scores))
	for doc, s := range scores {
		hits = append(hits, Hit{Doc: doc, Score: s})
	}
	return top(hits, k)
}

// top sorts hits by score, best first and ties by position, and keeps k.
func top(hits []Hit, k int) []Hit {
	slices.SortFunc(hits, func(a, c Hit) int {
		if a.Score != c.Score {
			return cmp.Compare(c.Score, a.Score)
		}
		return cmp.Compare(a.Doc, c.Doc)
	})
	if k >= 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// Normalize returns v scaled to unit length; a zero vector is returned as
// it is.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	norm := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * norm
	}
	return out
}

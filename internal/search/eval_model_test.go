//go:build model

package search_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/embed"
	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/search"
)

// TestSemanticRecall measures semantic and hybrid ranking on the evaluation
// set with the static model in TOOLSEARCH_MODEL_DIR (see embed's tests).
func TestSemanticRecall(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("TOOLSEARCH_MODEL_DIR")
	if dir == "" {
		t.Skip("TOOLSEARCH_MODEL_DIR is not set")
	}
	m, err := embed.LoadStatic(dir, "static")
	require.NoError(t, err)
	docs, queries := evalSet(t)
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Text()
	}
	vecs, err := m.Embed(t.Context(), texts)
	require.NoError(t, err)
	ix := search.NewIndex(docs, vecs)
	similar := func(q string) []search.Hit {
		v, verr := m.Embed(t.Context(), []string{q})
		require.NoError(t, verr)
		return ix.Similar(v[0], 5)
	}
	hybrid := func(q string) []search.Hit {
		v, verr := m.Embed(t.Context(), []string{q})
		require.NoError(t, verr)
		return search.Fuse(5, ix.Lexical(q, 50), ix.Similar(v[0], 50))
	}

	sem, hyb := recall(ix, queries, similar), recall(ix, queries, hybrid)
	t.Logf("semantic recall@5: %v", sem)
	t.Logf("hybrid recall@5: %v", hyb)

	require.GreaterOrEqual(t, hyb["en"], hybridFloorEN)
	require.GreaterOrEqual(t, hyb["lt"], hybridFloorLT)
}

// Hybrid recall@5 floors with potion-multilingual-128M.
const (
	hybridFloorEN = 0.78
	hybridFloorLT = 0.72
)

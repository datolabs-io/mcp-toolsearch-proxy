//go:build model

package embed_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/embed"
)

// golden holds what the reference model2vec implementation gives for the
// model in TOOLSEARCH_MODEL_DIR.
type golden struct {
	Cases []struct {
		Text   string    `json:"text"`
		IDs    []int32   `json:"ids"`
		Vector []float32 `json:"vector"`
	} `json:"cases"`
}

// TestStaticMatchesReference compares the tokenizer and the embeddings with
// those of the reference implementation. Run it with the model downloaded:
//
//	mcp-toolsearch-proxy fetch-model -dir /tmp/model
//	TOOLSEARCH_MODEL_DIR=/tmp/model go test -tags model ./internal/embed/
func TestStaticMatchesReference(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("TOOLSEARCH_MODEL_DIR")
	if dir == "" {
		t.Skip("TOOLSEARCH_MODEL_DIR is not set")
	}
	raw, err := os.ReadFile("testdata/potion-multilingual-128M.json")
	require.NoError(t, err)
	var g golden
	require.NoError(t, json.Unmarshal(raw, &g))
	m, err := embed.LoadStatic(dir, "potion-multilingual-128M")
	require.NoError(t, err)

	for _, c := range g.Cases {
		assert.Equal(t, c.IDs, append([]int32{}, m.Tokens(c.Text)...), "pieces of %q", c.Text)
		vecs, eerr := m.Embed(t.Context(), []string{c.Text})
		require.NoError(t, eerr)
		if len(c.IDs) == 0 {
			continue
		}
		var dot float64
		for i := range c.Vector {
			dot += float64(c.Vector[i]) * float64(vecs[0][i])
		}
		assert.Greater(t, dot, 0.999, "cosine to the reference vector of %q", c.Text)
	}
}

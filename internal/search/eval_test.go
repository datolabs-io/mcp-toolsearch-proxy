package search_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/search"
)

// evalTool and evalQuery are the evaluation set in testdata/eval: a catalog
// of made-up tools and questions people ask, in English and Lithuanian,
// each with the tool that answers it.
type evalTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Params      []search.Param `json:"params"`
}

type evalQuery struct {
	Lang  string `json:"lang"`
	Query string `json:"query"`
	Want  string `json:"want"`
}

// evalSet reads the evaluation set.
func evalSet(t testing.TB) ([]search.Doc, []evalQuery) {
	t.Helper()
	var tools []evalTool
	var queries []evalQuery
	for path, v := range map[string]any{"testdata/eval/catalog.json": &tools, "testdata/eval/queries.json": &queries} {
		raw, err := os.ReadFile(path) //nolint:gosec // fixed test data
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, v))
	}
	docs := make([]search.Doc, 0, len(tools))
	for _, tl := range tools {
		docs = append(docs, search.Doc{Name: tl.Name, Description: tl.Description, Params: tl.Params})
	}
	return docs, queries
}

// recall is the share of queries, by language, whose tool rank returns.
func recall(ix *search.Index, queries []evalQuery, rank func(q string) []search.Hit) map[string]float64 {
	found, total := map[string]int{}, map[string]int{}
	for _, q := range queries {
		total[q.Lang]++
		for _, h := range rank(q.Query) {
			if ix.Doc(h.Doc).Name == q.Want {
				found[q.Lang]++
				break
			}
		}
	}
	out := map[string]float64{}
	for lang, n := range total {
		out[lang] = float64(found[lang]) / float64(n)
	}
	return out
}

// TestLexicalRecall guards lexical ranking on the evaluation set: a change
// to tokenizing or stemming that loses queries fails here. The floors are
// what lexical search reaches today; raise them as it improves.
func TestLexicalRecall(t *testing.T) {
	t.Parallel()
	docs, queries := evalSet(t)
	ix := search.NewIndex(docs, nil)

	got := recall(ix, queries, func(q string) []search.Hit { return ix.Lexical(q, 5) })
	t.Logf("lexical recall@5: %v", got)

	require.GreaterOrEqual(t, got["en"], lexicalFloorEN)
	require.GreaterOrEqual(t, got["lt"], lexicalFloorLT)
}

// Recall@5 floors on the evaluation set. Lexical search finds few
// Lithuanian queries, since the tools are described in English: that is
// what semantic search is for (see eval_model_test.go).
const (
	lexicalFloorEN = 0.50
	lexicalFloorLT = 0.0
)

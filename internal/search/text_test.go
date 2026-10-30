package search_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/search"
)

func TestTokensSplitsNamesAndCase(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"get", "power", "flow", "now"}, search.Tokens("get_powerFlow-now"))
}

func TestTokensFoldsDiacritics(t *testing.T) {
	t.Parallel()

	assert.Equal(t, search.Tokens("sildymas"), search.Tokens("Šildymas"))
	assert.Equal(t, search.Tokens("zinute"), search.Tokens("žinutė"))
}

func TestTokensDropsStopWords(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"weather"}, search.Tokens("what is the weather"))
	assert.Equal(t, []string{"or"}, search.Tokens("koks yra oras"))
}

func TestStemMatchesInflections(t *testing.T) {
	t.Parallel()

	for _, group := range [][]string{
		{"sildymas", "sildymo", "sildymui", "sildyma", "sildyme"},
		{"irenginys", "irenginio", "irenginiai", "irenginiu", "irenginiuose"},
		{"list", "lists", "listing"},
		{"device", "devices"},
		{"battery", "batteries"},
		{"oras", "oro", "orui", "ora"},
	} {
		want := search.Stem(group[0])
		for _, w := range group[1:] {
			assert.Equal(t, want, search.Stem(w), "%s and %s", group[0], w)
		}
	}
}

func TestStemKeepsShortWordsAndNumbers(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "vlan600", search.Stem("vlan600"))
	assert.Equal(t, "ok", search.Stem("ok"))
	assert.Equal(t, search.Stem("oras"), search.Stem("oro"))
	assert.Equal(t, search.Stem("log"), search.Stem("logs"))
}

func FuzzTokens(f *testing.F) {
	for _, s := range []string{"", "get_powerFlow", "Šildymas", "ąčęėįšųūž", "a__b", "\xff\xfe"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, tok := range search.Tokens(s) {
			if tok == "" {
				t.Fatalf("empty token from %q", s)
			}
		}
	})
}

func TestSuffixesLongestFirst(t *testing.T) {
	t.Parallel()

	// Stem takes the first ending that fits, so a longer one listed after a
	// shorter one it ends with would never be taken.
	assert.Equal(t, "irengin", search.Stem("irenginiuose"))
	assert.Equal(t, "nam", search.Stem("namuose"))
}

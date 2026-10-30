package search_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/search"
)

func docs() []search.Doc {
	return []search.Doc{
		{Name: "weather_get_forecast", Description: "Hourly and daily weather forecast for a place."},
		{Name: "router_list_dhcp_leases", Description: "Addresses the router has leased to devices."},
		{Name: "calendar_create_event", Title: "Create event", Description: "Add an event to a calendar.",
			Params: []search.Param{{Name: "start", Description: "When the event starts"}}},
		{Name: "heating_set_temperature", Description: "Šildymo temperatūra kambaryje."},
	}
}

func names(ix *search.Index, hits []search.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, ix.Doc(h.Doc).Name)
	}
	return out
}

func TestLexicalFindsByNameAndDescription(t *testing.T) {
	t.Parallel()
	ix := search.NewIndex(docs(), nil)

	assert.Equal(t, "router_list_dhcp_leases", names(ix, ix.Lexical("which devices got an address", 1))[0])
	assert.Equal(t, "weather_get_forecast", names(ix, ix.Lexical("forecast", 1))[0])
}

func TestLexicalMatchesLithuanianWithoutDiacritics(t *testing.T) {
	t.Parallel()
	ix := search.NewIndex(docs(), nil)

	assert.Equal(t, []string{"heating_set_temperature"}, names(ix, ix.Lexical("sildymas", 5)))
}

func TestLexicalWeighsNamesAboveDescriptions(t *testing.T) {
	t.Parallel()
	ix := search.NewIndex([]search.Doc{
		{Name: "notes_search", Description: "Find a calendar entry mentioned in a note."},
		{Name: "calendar_search", Description: "Find entries."},
	}, nil)

	assert.Equal(t, "calendar_search", names(ix, ix.Lexical("calendar", 2))[0])
}

func TestLexicalReturnsNothingForUnknownWords(t *testing.T) {
	t.Parallel()
	ix := search.NewIndex(docs(), nil)

	assert.Empty(t, ix.Lexical("xyzzy", 5))
	assert.Empty(t, ix.Lexical("the", 5), "stop words alone match nothing")
	assert.Empty(t, search.NewIndex(nil, nil).Lexical("weather", 5))
}

func TestSimilarRanksByCosine(t *testing.T) {
	t.Parallel()
	ix := search.NewIndex(docs(), [][]float32{{1, 0}, {0, 1}, {0.7, 0.7}, {-1, 0}})

	require.True(t, ix.Semantic())
	assert.Equal(
		t,
		[]string{"weather_get_forecast", "calendar_create_event"},
		names(ix, ix.Similar([]float32{2, 0.1}, 2)),
	)
}

func TestSimilarWithoutVectors(t *testing.T) {
	t.Parallel()
	ix := search.NewIndex(docs(), nil)

	assert.False(t, ix.Semantic())
	assert.Nil(t, ix.Similar([]float32{1, 0}, 3))
}

func TestSimilarSkipsVectorsOfAnotherDimension(t *testing.T) {
	t.Parallel()
	ix := search.NewIndex(docs()[:2], [][]float32{{1, 0, 0}, {0, 1}})

	assert.Equal(t, []string{"router_list_dhcp_leases"}, names(ix, ix.Similar([]float32{0, 1}, 5)))
}

func TestFusePrefersAgreement(t *testing.T) {
	t.Parallel()

	lexical := []search.Hit{{Doc: 1}, {Doc: 2}, {Doc: 3}}
	semantic := []search.Hit{{Doc: 4}, {Doc: 2}, {Doc: 1}}

	fused := search.Fuse(3, lexical, semantic)

	require.Len(t, fused, 3)
	assert.ElementsMatch(t, []int{1, 2}, []int{fused[0].Doc, fused[1].Doc})
}

func TestFuseOfOneRankingKeepsItsOrder(t *testing.T) {
	t.Parallel()

	fused := search.Fuse(-1, []search.Hit{{Doc: 3}, {Doc: 1}, {Doc: 2}})

	assert.Equal(t, []int{3, 1, 2}, []int{fused[0].Doc, fused[1].Doc, fused[2].Doc})
}

func TestDocText(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "calendar create event\nCreate event\nAdd an event to a calendar.\nstart: When the event starts",
		docs()[2].Text())
}

func TestNormalize(t *testing.T) {
	t.Parallel()

	assert.InDeltaSlice(t, []float32{0.6, 0.8}, search.Normalize([]float32{3, 4}), 1e-6)
	assert.Equal(t, []float32{0, 0}, search.Normalize([]float32{0, 0}))
}

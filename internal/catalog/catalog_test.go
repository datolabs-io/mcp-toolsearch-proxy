package catalog_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"weak"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/catalog"
)

func tools() []*mcp.Tool {
	return []*mcp.Tool{
		{Name: "weather_get_forecast", Description: "Weather forecast for a place."},
		{Name: "router_list_dhcp_leases", Description: "Addresses leased to devices.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"server": map[string]any{"description": "DHCP server"}},
		}},
		{Name: "calendar_create_event", Title: "Create event", Description: "Add an event to a calendar."},
	}
}

func searchNames(c *catalog.Catalog, emb catalog.Embedder, q string, k int) ([]string, string) {
	found, mode := c.Search(context.Background(), emb, q, k)
	out := make([]string, 0, len(found))
	for _, t := range found {
		out = append(out, t.Name)
	}
	return out, mode
}

// axes embeds a text along the axis of the first of its words it knows.
type axes struct {
	words map[string]int
	err   error
	calls atomic.Int32
}

func (a *axes) Embed(_ context.Context, texts []string) ([][]float32, error) {
	a.calls.Add(1)
	if a.err != nil {
		return nil, a.err
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, 3)
		for w, axis := range a.words {
			if containsWord(text, w) {
				v[axis] = 1
				break
			}
		}
		out[i] = v
	}
	return out, nil
}

func containsWord(text, w string) bool {
	for i := 0; i+len(w) <= len(text); i++ {
		if text[i:i+len(w)] == w {
			return true
		}
	}
	return false
}

func TestSearchLexically(t *testing.T) {
	t.Parallel()
	c, err := catalog.New(t.Context(), tools(), nil)
	require.NoError(t, err)

	got, mode := searchNames(c, nil, "dhcp server", 5)

	assert.Equal(t, []string{"router_list_dhcp_leases"}, got)
	assert.Equal(t, catalog.ModeLexical, mode)
	assert.False(t, c.Semantic())
}

func TestSearchFusesSemanticRanking(t *testing.T) {
	t.Parallel()
	emb := &axes{words: map[string]int{"forecast": 0, "rain": 0, "leased": 1, "calendar": 2}}
	c, err := catalog.New(t.Context(), tools(), emb)
	require.NoError(t, err)

	got, mode := searchNames(c, emb, "will it rain", 1)

	assert.Equal(t, []string{"weather_get_forecast"}, got, "no word in common, found by meaning")
	assert.Equal(t, catalog.ModeHybrid, mode)
}

func TestSearchFallsBackWhenTheQueryCannotBeEmbedded(t *testing.T) {
	t.Parallel()
	emb := &axes{words: map[string]int{"forecast": 0}}
	c, err := catalog.New(t.Context(), tools(), emb)
	require.NoError(t, err)
	emb.err = errors.New("down")

	got, mode := searchNames(c, emb, "forecast", 3)

	assert.Equal(t, []string{"weather_get_forecast"}, got)
	assert.Equal(t, catalog.ModeLexical, mode)
}

// shortQuery embeds the tools as axes does, and a query as a vector of
// another length, which no tool's vector can be compared with.
type shortQuery struct{ axes }

func (s *shortQuery) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 1 {
		return [][]float32{{1}}, nil
	}
	return s.axes.Embed(ctx, texts)
}

// A semantic ranking that finds nothing leaves the search lexical, and it
// says so: the mode is how a broken model shows in the metrics.
func TestSearchIsLexicalWhenTheSemanticRankingFindsNothing(t *testing.T) {
	t.Parallel()
	emb := &shortQuery{axes{words: map[string]int{"forecast": 0}}}
	c, err := catalog.New(t.Context(), tools(), emb)
	require.NoError(t, err)
	require.True(t, c.Semantic())

	got, mode := searchNames(c, emb, "forecast", 3)

	assert.Equal(t, []string{"weather_get_forecast"}, got)
	assert.Equal(t, catalog.ModeLexical, mode)
}

func TestNewKeepsLexicalSearchWhenEmbeddingFails(t *testing.T) {
	t.Parallel()

	c, err := catalog.New(t.Context(), tools(), &axes{err: errors.New("down")})

	require.Error(t, err)
	require.NotNil(t, c)
	assert.False(t, c.Semantic())
	got, _ := searchNames(c, nil, "forecast", 3)
	assert.Equal(t, []string{"weather_get_forecast"}, got)
}

func TestTool(t *testing.T) {
	t.Parallel()
	c, err := catalog.New(t.Context(), tools(), nil)
	require.NoError(t, err)

	got, ok := c.Tool("calendar_create_event")
	require.True(t, ok)
	assert.Equal(t, "Create event", got.Title)
	_, ok = c.Tool("nope")
	assert.False(t, ok)
	assert.Equal(t, 3, c.Len())
}

func TestDocReadsParameters(t *testing.T) {
	t.Parallel()

	d := catalog.Doc(tools()[1])

	require.Len(t, d.Params, 1)
	assert.Equal(t, "server", d.Params[0].Name)
	assert.Equal(t, "DHCP server", d.Params[0].Description)
}

// lister counts the lists it serves, per caller.
type lister struct {
	mu    sync.Mutex
	calls map[string]int
	err   error
	gate  chan struct{}
}

func (l *lister) list(_ context.Context, auth string) ([]*mcp.Tool, error) {
	if l.gate != nil {
		<-l.gate
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls == nil {
		l.calls = map[string]int{}
	}
	l.calls[auth]++
	if l.err != nil {
		return nil, l.err
	}
	if auth == "Bearer small" {
		return tools()[:1], nil
	}
	return tools(), nil
}

func (l *lister) count(auth string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[auth]
}

func TestStoreKeepsACatalogPerCaller(t *testing.T) {
	t.Parallel()
	l := &lister{}
	s := catalog.NewStore(catalog.Tools(l.list), nil)

	big, err := s.Get(t.Context(), "Bearer big", false)
	require.NoError(t, err)
	small, err := s.Get(t.Context(), "Bearer small", false)
	require.NoError(t, err)
	again, err := s.Get(t.Context(), "Bearer big", false)
	require.NoError(t, err)

	assert.Equal(t, 3, big.Len())
	assert.Equal(t, 1, small.Len())
	assert.Same(t, big, again)
	assert.Equal(t, 1, l.count("Bearer big"))
	assert.Equal(t, 2, s.Len())
}

// clock is a test clock that moves only when told to.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Unix(1_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (l *lister) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func TestStoreRefreshesAnExpiredCatalogInTheBackground(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{TTL: time.Minute, Now: clk.Now})
	first, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)

	clk.advance(2 * time.Minute)
	got, err := s.Get(t.Context(), "Bearer a", false)

	require.NoError(t, err)
	assert.Same(t, first, got, "the expired catalog is served at once")
	require.Eventually(t, func() bool { return l.count("Bearer a") == 2 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		c, _ := s.Get(t.Context(), "Bearer a", false)
		return c != first
	}, time.Second, time.Millisecond, "the refreshed catalog replaces it")
	assert.Equal(t, 2, l.count("Bearer a"))
}

func TestStoreServesThePreviousCatalogWhenARefreshFails(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	var mu sync.Mutex
	var reported []error
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{
		TTL: time.Minute, Now: clk.Now,
		OnBuild: func(_ int, _ time.Duration, err error) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, err)
		},
	})
	first, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	l.fail(errors.New("gateway down"))

	clk.advance(2 * time.Minute)
	for range 5 {
		got, getErr := s.Get(t.Context(), "Bearer a", false)
		require.NoError(t, getErr)
		assert.Same(t, first, got)
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reported) == 2
	}, time.Second, time.Millisecond, "the failed refresh is reported")
	for range 5 {
		got, getErr := s.Get(t.Context(), "Bearer a", false)
		require.NoError(t, getErr)
		assert.Same(t, first, got, "the old catalog is kept")
	}

	assert.Equal(t, 2, l.count("Bearer a"), "one refresh per backoff window")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, reported, 2)
	assert.Error(t, reported[1], "the failure is reported")
}

func TestStoreBacksOffAFailingLister(t *testing.T) {
	t.Parallel()
	l := &lister{err: errors.New("gateway down")}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{Now: clk.Now})

	for range 5 {
		_, err := s.Get(t.Context(), "Bearer a", false)
		require.ErrorContains(t, err, "gateway down")
	}
	assert.Equal(t, 1, l.count("Bearer a"), "one list per backoff window")

	clk.advance(time.Minute)
	_, err := s.Get(t.Context(), "Bearer a", false)
	require.Error(t, err)
	assert.Equal(t, 2, l.count("Bearer a"), "listed again once the window passed")

	_, err = s.Get(t.Context(), "Bearer b", false)
	require.Error(t, err)
	assert.Equal(t, 1, l.count("Bearer b"), "each caller backs off on its own")

	l.fail(nil)
	clk.advance(10 * time.Minute)
	_, err = s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err, "a success ends the backoff")
}

func TestStoreStopsServingACatalogPastItsMaxAge(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{TTL: time.Minute, Now: clk.Now})
	_, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	l.fail(errors.New("gateway down"))

	clk.advance(6 * time.Minute)
	_, err = s.Get(t.Context(), "Bearer a", false)

	require.ErrorContains(t, err, "gateway down", "the default bound is 6 TTLs")
	_, ok := s.Cached("Bearer a")
	assert.False(t, ok, "a catalog past its max age is dropped")
	assert.Zero(t, s.Len())
}

// A catalog past its max age is never served again, so it is dropped
// rather than kept until a new caller needs its room.
func TestStoreDropsCatalogsPastTheirMaxAge(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{
		TTL: time.Minute, MaxAge: 2 * time.Minute, MaxSize: 4, Now: clk.Now,
	})
	_, err := s.Get(t.Context(), "Bearer old", false)
	require.NoError(t, err)
	clk.advance(90 * time.Second)
	_, err = s.Get(t.Context(), "Bearer new", false)
	require.NoError(t, err)
	require.Equal(t, 2, s.Len())

	clk.advance(time.Minute)

	assert.Equal(t, 1, s.Len(), "the older one is past its max age")
	_, ok := s.Cached("Bearer old")
	assert.False(t, ok)
	_, ok = s.Cached("Bearer new")
	assert.True(t, ok)
}

// A catalog past its max age is let go even when no request comes after it.
func TestStoreLetsGoOfCatalogsNobodyAsksFor(t *testing.T) {
	t.Parallel()
	l := &lister{}
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{
		TTL: 10 * time.Millisecond, MaxAge: 50 * time.Millisecond,
	})
	// The test keeps only a weak pointer to the catalog.
	kept := func() weak.Pointer[catalog.Catalog] {
		c, err := s.Get(t.Context(), "Bearer a", false)
		require.NoError(t, err)
		return weak.Make(c)
	}()

	assert.Eventually(t, func() bool {
		runtime.GC()
		return kept.Value() == nil
	}, 5*time.Second, 100*time.Millisecond, "the catalog is released without another request")
}

func TestStoreKeepsSixteenCatalogsByDefault(t *testing.T) {
	t.Parallel()
	l := &lister{}
	s := catalog.NewStore(catalog.Tools(l.list), nil)

	for i := range 20 {
		_, err := s.Get(t.Context(), fmt.Sprintf("Bearer %d", i), false)
		require.NoError(t, err)
	}

	assert.Equal(t, 16, s.Len())
}

func TestStoreHonoursAMaxAge(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{
		TTL: time.Minute, MaxAge: 90 * time.Second, Now: clk.Now,
	})
	_, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	l.fail(errors.New("gateway down"))

	clk.advance(80 * time.Second)
	_, err = s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	clk.advance(20 * time.Second)
	_, err = s.Get(t.Context(), "Bearer a", false)
	require.Error(t, err)
}

func TestStoreListsAgainWhenAskedOnlyPastTheFloor(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{Now: clk.Now})
	first, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)

	for range 10 {
		got, getErr := s.Get(t.Context(), "Bearer a", true)
		require.NoError(t, getErr)
		assert.Same(t, first, got)
	}
	assert.Equal(t, 1, l.count("Bearer a"), "a recent catalog is not listed again")

	clk.advance(catalog.DefaultMinRelist)
	got, err := s.Get(t.Context(), "Bearer a", true)
	require.NoError(t, err)
	assert.NotSame(t, first, got)
	assert.Equal(t, 2, l.count("Bearer a"))
}

func TestStoreFallsBackWhenAFreshListFails(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{Now: clk.Now})
	first, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	l.fail(errors.New("gateway down"))
	clk.advance(catalog.DefaultMinRelist)

	got, err := s.Get(t.Context(), "Bearer a", true)

	require.NoError(t, err)
	assert.Same(t, first, got)
	assert.Equal(t, 2, l.count("Bearer a"))
}

func TestStoreTriesAFreshListOncePerFloorEvenWhenItFails(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{Now: clk.Now})
	_, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	l.fail(errors.New("gateway down"))

	clk.advance(catalog.DefaultMinRelist)
	for range 5 {
		_, err = s.Get(t.Context(), "Bearer a", true)
		require.NoError(t, err)
		clk.advance(6 * time.Second)
	}

	assert.Equal(t, 2, l.count("Bearer a"), "the backoff ending is not enough: the floor counts from the failure")
}

func TestStoreDoesNotFallBackPastTheMaxAge(t *testing.T) {
	t.Parallel()
	clk := newClock()
	var fail atomic.Bool
	list := func(context.Context, string) (catalog.Listing, error) {
		if !fail.Load() {
			return catalog.Listing{Tools: tools()}, nil
		}
		clk.advance(time.Minute) // a slow list, that fails
		return catalog.Listing{}, errors.New("gateway down")
	}
	s := catalog.NewStore(list, &catalog.StoreOptions{TTL: time.Minute, MaxAge: 2 * time.Minute, Now: clk.Now})
	_, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	fail.Store(true)

	clk.advance(90 * time.Second)
	_, err = s.Get(t.Context(), "Bearer a", true)

	require.ErrorContains(t, err, "gateway down", "the catalog kept aged past the bound while listing")
}

func TestStoreReportsTheAgeOfWhatItServes(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	var mu sync.Mutex
	var ages []time.Duration
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{
		Now: clk.Now, OnServe: func(age time.Duration) { mu.Lock(); defer mu.Unlock(); ages = append(ages, age) },
	})

	_, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	clk.advance(time.Minute)
	_, err = s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []time.Duration{0, time.Minute}, ages)
}

func TestStoreSharesConcurrentBuilds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := &lister{gate: make(chan struct{})}
		s := catalog.NewStore(catalog.Tools(l.list), nil)

		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				_, err := s.Get(t.Context(), "Bearer a", false)
				assert.NoError(t, err)
			})
		}
		// Every request is blocked now: one in the listing, the others
		// waiting for it.
		synctest.Wait()
		close(l.gate)
		wg.Wait()

		assert.Equal(t, 1, l.count("Bearer a"))
	})
}

func TestStoreDropsTheOldest(t *testing.T) {
	t.Parallel()
	l := &lister{}
	now := time.Unix(0, 0)
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{MaxSize: 2, Now: func() time.Time {
		now = now.Add(time.Second)
		return now
	}})

	for _, auth := range []string{"Bearer 1", "Bearer 2", "Bearer 3"} {
		_, err := s.Get(t.Context(), auth, false)
		require.NoError(t, err)
	}
	_, err := s.Get(t.Context(), "Bearer 3", false)
	require.NoError(t, err)

	assert.Equal(t, 2, s.Len())
	assert.Equal(t, 1, l.count("Bearer 3"))
}

func TestStoreReportsBuilds(t *testing.T) {
	t.Parallel()
	list := func(context.Context, string) ([]*mcp.Tool, error) {
		time.Sleep(10 * time.Millisecond)
		return nil, errors.New("unauthorized")
	}
	var got []error
	var took []time.Duration
	s := catalog.NewStore(
		catalog.Tools(list),
		&catalog.StoreOptions{OnBuild: func(_ int, d time.Duration, err error) {
			got, took = append(got, err), append(took, d)
		}},
	)

	_, err := s.Get(t.Context(), "Bearer a", false)

	require.Error(t, err)
	require.Len(t, got, 1)
	require.Error(t, got[0])
	assert.GreaterOrEqual(t, took[0], 10*time.Millisecond, "the build's duration is reported")
	assert.Equal(t, 0, s.Len(), "a failed build is not kept")
}

func TestStoreReportsTheLargestCatalogItServes(t *testing.T) {
	t.Parallel()
	l := &lister{}
	clk := newClock()
	s := catalog.NewStore(catalog.Tools(l.list), &catalog.StoreOptions{TTL: time.Minute, Now: clk.Now})
	assert.Equal(t, 0, s.MaxTools())

	_, err := s.Get(t.Context(), "Bearer small", false)
	require.NoError(t, err)
	assert.Equal(t, 1, s.MaxTools())
	_, err = s.Get(t.Context(), "Bearer big", false)
	require.NoError(t, err)
	_, err = s.Get(t.Context(), "Bearer small", false)
	require.NoError(t, err)
	assert.Equal(t, 3, s.MaxTools(), "whichever caller listed last")

	clk.advance(6 * time.Minute)
	assert.Equal(t, 0, s.MaxTools(), "a catalog past its max age is not served, so it does not count")
}

func TestDocKeepsParametersBesideBooleanSchemas(t *testing.T) {
	t.Parallel()

	d := catalog.Doc(&mcp.Tool{Name: "x", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"payload": true,
			"city":    map[string]any{"description": "City name"},
			"note":    map[string]any{"title": "A note"},
		},
	}})

	require.Len(t, d.Params, 3)
	assert.Equal(t, "city", d.Params[0].Name)
	assert.Equal(t, "City name", d.Params[0].Description)
	assert.Equal(t, "A note", d.Params[1].Description, "the title stands in for a missing description")
	assert.Equal(t, "payload", d.Params[2].Name, "a boolean schema still names a parameter")
	assert.Empty(t, d.Params[2].Description)
}

func TestDocWithoutUsableSchema(t *testing.T) {
	t.Parallel()

	for _, schema := range []any{nil, true, "not a schema", map[string]any{"properties": []any{1}}} {
		assert.Empty(t, catalog.Doc(&mcp.Tool{Name: "x", InputSchema: schema}).Params, "%v", schema)
	}
}

func TestCachedNeverLists(t *testing.T) {
	t.Parallel()
	l := &lister{}
	now := time.Unix(0, 0)
	s := catalog.NewStore(
		catalog.Tools(l.list),
		&catalog.StoreOptions{TTL: time.Minute, Now: func() time.Time { return now }},
	)

	_, ok := s.Cached("Bearer a")
	require.False(t, ok)
	_, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	now = now.Add(5 * time.Minute)
	c, ok := s.Cached("Bearer a")

	require.True(t, ok, "a catalog past its TTL is still returned")
	assert.Equal(t, 3, c.Len())
	assert.Equal(t, 1, l.count("Bearer a"))
}

func TestNewKeepsOneEntryForARepeatedName(t *testing.T) {
	t.Parallel()
	first := &mcp.Tool{Name: "dup", Description: "First."}
	second := &mcp.Tool{Name: "dup", Description: "Second.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}
	listed := append(tools(), first, second, &mcp.Tool{Name: "dup"})

	c, err := catalog.New(t.Context(), listed, nil)

	require.NoError(t, err)
	assert.Equal(t, 4, c.Len(), "a repeated name counts once")
	got, ok := c.Tool("dup")
	require.True(t, ok)
	assert.Same(t, first, got, "the first listed stands for the name")
	assert.Len(t, c.Copies("dup"), 3)
	assert.Len(t, c.Copies("weather_get_forecast"), 1)
	assert.Empty(t, c.Copies("nope"))
	assert.Equal(t, map[string]int{"dup": 3}, c.Ambiguous())
	found, _ := searchNames(c, nil, "dup", 10)
	assert.Equal(t, []string{"dup"}, found, "search shows the name once")
}

func TestStoreWarnsOfRepeatedNamesOncePerBuild(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	list := catalog.Tools(func(context.Context, string) ([]*mcp.Tool, error) {
		return []*mcp.Tool{{Name: "dup"}, {Name: "dup"}, {Name: "solo"}}, nil
	})
	s := catalog.NewStore(list, &catalog.StoreOptions{Logger: logger})

	_, err := s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)
	_, err = s.Get(t.Context(), "Bearer a", false)
	require.NoError(t, err)

	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, "level=WARN"), out)
	assert.Contains(t, out, "dup=2")
	assert.NotContains(t, out, "solo")
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/singleflight"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/search"
)

// Defaults for a Store.
const (
	DefaultTTL = 10 * time.Minute
	// DefaultMaxSize is how many callers' catalogs are kept. A catalog of
	// a thousand tools takes several MB, a richer one tens; every new
	// token is a new caller.
	DefaultMaxSize = 16
	// DefaultMaxAgeTTLs is how many TTLs old a catalog may get, while
	// listing it again fails, before it is no longer served.
	DefaultMaxAgeTTLs = 6
	// DefaultMinRelist is how old a catalog must be before a request for a
	// fresh one, as for a tool name it lacks, lists the tools again.
	DefaultMinRelist = 30 * time.Second
)

// Backoff after a failed build: the caller's tools are not listed again
// for failBackoff, doubling with each failure in a row up to maxBackoff.
const (
	failBackoff = 5 * time.Second
	maxBackoff  = time.Minute
	// maxShift bounds the doubling, so the shift cannot overflow.
	maxShift = 8
)

// candidates is how many tools each ranking hands to fusion.
const candidates = 50

// Listing is what the upstream offers a caller: the tools, and the
// instructions the upstream server gave for them.
type Listing struct {
	Tools        []*mcp.Tool
	Instructions string
}

// Lister lists what the caller with the given Authorization header value
// may use.
type Lister func(ctx context.Context, authorization string) (Listing, error)

// Tools makes a Lister of a function that lists tools without
// instructions.
func Tools(list func(ctx context.Context, authorization string) ([]*mcp.Tool, error)) Lister {
	return func(ctx context.Context, authorization string) (Listing, error) {
		tools, err := list(ctx, authorization)
		return Listing{Tools: tools}, err
	}
}

// Embedder turns texts into vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Catalog is the tools one caller may use, indexed for search.
type Catalog struct {
	tools []*mcp.Tool
	// instructions are the upstream server's, as it gave them.
	instructions string
	byName       map[string]*mcp.Tool
	// copies holds every tool listed under a name the upstream lists more
	// than once, in the order listed.
	copies map[string][]*mcp.Tool
	ix     *search.Index
	built  time.Time
}

// Copies returns every tool the upstream lists under name: one, several
// when the name is ambiguous, or none when no tool has it. Which of several
// a call reaches is up to the upstream.
func (c *Catalog) Copies(name string) []*mcp.Tool {
	if copies, ok := c.copies[name]; ok {
		return copies
	}
	if t, ok := c.byName[name]; ok {
		return []*mcp.Tool{t}
	}
	return nil
}

// Ambiguous returns the names the upstream lists more than once, with how
// many times each.
func (c *Catalog) Ambiguous() map[string]int {
	out := make(map[string]int, len(c.copies))
	for name, copies := range c.copies {
		out[name] = len(copies)
	}
	return out
}

// Instructions are the upstream server's instructions, as it gave them.
func (c *Catalog) Instructions() string { return c.instructions }

// Len is the number of tools.
func (c *Catalog) Len() int { return len(c.tools) }

// Tool returns the tool called name.
func (c *Catalog) Tool(name string) (*mcp.Tool, bool) {
	t, ok := c.byName[name]
	return t, ok
}

// Names returns the names of the catalog's tools, each once, in no
// particular order.
func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.byName))
	for name := range c.byName {
		names = append(names, name)
	}
	return names
}

// Semantic reports whether the catalog's tools have vectors to search by.
func (c *Catalog) Semantic() bool { return c.ix.Semantic() }

// Search modes, as Search reports them.
const (
	ModeHybrid  = "hybrid"
	ModeLexical = "lexical"
)

// Search returns the k tools that best match query, best first, and the
// mode it searched in. With vectors and an embedder it ranks both lexically
// and semantically and fuses the two; without, or when the query cannot be
// embedded or ranked by meaning, lexically alone.
func (c *Catalog) Search(ctx context.Context, emb Embedder, query string, k int) ([]*mcp.Tool, string) {
	lexical := c.ix.Lexical(query, candidates)
	hits, mode := lexical, ModeLexical
	if c.ix.Semantic() && emb != nil {
		// A ranking that finds nothing, as for a vector of the wrong
		// length, leaves the search lexical, and it is reported so.
		if vecs, err := emb.Embed(ctx, []string{query}); err == nil && len(vecs) == 1 {
			if similar := c.ix.Similar(vecs[0], candidates); len(similar) > 0 {
				hits, mode = search.Fuse(-1, lexical, similar), ModeHybrid
			}
		}
	}
	if len(hits) > k {
		hits = hits[:k]
	}
	out := make([]*mcp.Tool, 0, len(hits))
	for _, h := range hits {
		out = append(out, c.tools[h.Doc])
	}
	return out, mode
}

// maxAmbiguousLogged bounds the names one warning lists.
const maxAmbiguousLogged = 20

// warnAmbiguous logs one warning naming the tools the upstream lists more
// than once and how often, and nothing else of them.
func (c *Catalog) warnAmbiguous(ctx context.Context, logger *slog.Logger) {
	if len(c.copies) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(c.copies))
	attrs := make([]any, 0, min(len(names), maxAmbiguousLogged)+1)
	for _, name := range names[:min(len(names), maxAmbiguousLogged)] {
		attrs = append(attrs, slog.Int(name, len(c.copies[name])))
	}
	logger.WarnContext(ctx, "the upstream lists tool names more than once", "names", len(names),
		slog.Group("ambiguous", attrs...))
}

// New indexes tools, embedding them with emb when it is not nil. A failure
// to embed leaves the catalog searchable lexically, and is returned
// alongside it. A name listed more than once is indexed once, as its first
// tool, and is ambiguous (see Copies): the upstream decides which a call
// reaches, so the catalog cannot tell which annotations apply.
func New(ctx context.Context, tools []*mcp.Tool, emb Embedder) (*Catalog, error) {
	c := &Catalog{byName: make(map[string]*mcp.Tool, len(tools)), copies: map[string][]*mcp.Tool{}, built: time.Now()}
	unique := make([]*mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if first, seen := c.byName[t.Name]; seen {
			if _, ok := c.copies[t.Name]; !ok {
				c.copies[t.Name] = []*mcp.Tool{first}
			}
			c.copies[t.Name] = append(c.copies[t.Name], t)
			continue
		}
		c.byName[t.Name] = t
		unique = append(unique, t)
	}
	tools = unique
	c.tools = tools
	docs := make([]search.Doc, len(tools))
	texts := make([]string, len(tools))
	for i, t := range tools {
		docs[i] = Doc(t)
		texts[i] = docs[i].Text()
	}
	var vecs [][]float32
	var err error
	if emb != nil && len(tools) > 0 {
		if vecs, err = emb.Embed(ctx, texts); err != nil {
			vecs, err = nil, fmt.Errorf("catalog: embed tools: %w", err)
		} else if len(vecs) != len(texts) {
			vecs, err = nil, errors.New("catalog: embed tools: wrong number of vectors")
		}
	}
	c.ix = search.NewIndex(docs, vecs)
	return c, err
}

// Store keeps a catalog per caller, built from that caller's own list of
// tools, since what each may use differs. Callers are told apart by a hash
// of their whole Authorization header value, never by a claim inside it: an
// unverified claim could be forged to read another caller's catalog. A new
// token therefore builds a new catalog. It is safe for concurrent use.
//
// A catalog older than the TTL is still served while it is listed again in
// the background, and kept when that fails, up to the max age; past it, it
// is dropped, by a timer when no request comes, so a token no longer used
// does not hold memory. A failed
// build is remembered with a backoff, so that an upstream that is down is
// not listed again on every request.
type Store struct {
	list      Lister
	emb       Embedder
	ttl       time.Duration
	maxAge    time.Duration
	minRelist time.Duration
	maxSize   int
	now       func() time.Time
	onBuild   func(tools int, took time.Duration, err error)
	onServe   func(age time.Duration)
	logger    *slog.Logger

	mu         sync.Mutex
	entries    map[string]*Catalog
	failures   map[string]failure
	refreshing map[string]bool
	group      singleflight.Group
	// expiry sweeps the store when its oldest catalog passes the max age,
	// so a catalog no request asks for again is still let go; nil when
	// none is due.
	expiry *time.Timer
}

// failure is a caller's last failed build.
type failure struct {
	err error
	// streak is how many builds in a row failed.
	streak int
	// at is when it failed; until, when the tools may be listed again.
	at, until time.Time
}

// StoreOptions configures a Store. A nil *StoreOptions is valid.
type StoreOptions struct {
	// Embedder embeds the tools for semantic search; nil searches
	// lexically only.
	Embedder Embedder
	// TTL is how long a catalog is used before it is listed again; zero
	// means DefaultTTL. An older one is still served while it is listed
	// again in the background.
	TTL time.Duration
	// MaxAge is how old a catalog may get, while listing it again fails,
	// before it is no longer served; zero means DefaultMaxAgeTTLs times
	// the TTL, and less than the TTL means the TTL.
	MaxAge time.Duration
	// MinRelist is how old a catalog must be before a request for a fresh
	// one lists the tools again; zero means DefaultMinRelist.
	MinRelist time.Duration
	// MaxSize is how many callers' catalogs are kept; zero means
	// DefaultMaxSize. The oldest is dropped to make room, and any past
	// MaxAge is dropped regardless.
	MaxSize int
	// OnBuild, if set, is told of every build: the number of tools, how
	// long the build took, and the error that failed it (no tools) or left
	// it lexical only.
	OnBuild func(tools int, took time.Duration, err error)
	// OnServe, if set, is told the age of every catalog Get returns.
	OnServe func(age time.Duration)
	// Now returns the time; nil means time.Now.
	Now func() time.Time
	// Logger receives a warning for each build whose listing repeats a
	// tool name, with the names and counts only; nil discards.
	Logger *slog.Logger
}

// NewStore returns a store whose catalogs list builds.
func NewStore(list Lister, opts *StoreOptions) *Store {
	var o StoreOptions
	if opts != nil {
		o = *opts
	}
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.MaxAge == 0 {
		o.MaxAge = DefaultMaxAgeTTLs * o.TTL
	}
	o.MaxAge = max(o.MaxAge, o.TTL)
	if o.MinRelist <= 0 {
		o.MinRelist = DefaultMinRelist
	}
	if o.MaxSize <= 0 {
		o.MaxSize = DefaultMaxSize
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.OnServe == nil {
		o.OnServe = func(time.Duration) {}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Store{
		list: list, emb: o.Embedder, ttl: o.TTL, maxAge: o.MaxAge, minRelist: o.MinRelist, maxSize: o.MaxSize,
		now: o.Now, onBuild: o.OnBuild, onServe: o.OnServe, logger: o.Logger,
		entries: map[string]*Catalog{}, failures: map[string]failure{}, refreshing: map[string]bool{},
	}
}

// Embedder is the store's embedder, or nil.
//
//nolint:ireturn // whatever embedder the store was given
func (s *Store) Embedder() Embedder { return s.emb }

// Get returns the catalog of the caller whose Authorization header value is
// authorization. A catalog older than the TTL is returned at once and
// listed again in the background; one older than the max age is no longer
// returned, and is listed again first. With fresh set, as for a tool name
// the catalog lacks, the tools are listed again first, unless the catalog
// is younger than the minimum, and the catalog kept is returned when that
// fails. A caller whose last build failed is not listed again until its
// backoff ends. Concurrent requests of one caller share a build.
func (s *Store) Get(ctx context.Context, authorization string, fresh bool) (*Catalog, error) {
	key := identity(authorization)
	now := s.now()
	s.mu.Lock()
	s.sweep(now)
	c, ok := s.entries[key]
	var age time.Duration
	if ok {
		age = now.Sub(c.built)
		ok = age < s.maxAge
	}
	f, failed := s.failures[key]
	backoff := failed && now.Before(f.until)
	s.mu.Unlock()

	// A fresh list is tried at most once per minimum, failed or not.
	relist := fresh && age >= s.minRelist && (!failed || now.Sub(f.at) >= s.minRelist)
	switch {
	case ok && relist && !backoff:
		nc, err := s.build(ctx, key, authorization, now)
		if err == nil {
			s.onServe(0)
			return nc, nil
		}
		// The list may have taken a while, and the catalog kept aged past
		// the bound meanwhile.
		if age = s.now().Sub(c.built); age >= s.maxAge {
			return nil, err
		}
		s.onServe(age)
		return c, nil
	case ok:
		if age >= s.ttl && !backoff {
			s.refresh(ctx, key, authorization, now)
		}
		s.onServe(age)
		return c, nil
	case backoff:
		return nil, fmt.Errorf("catalog: %w (not listed again for %s)", f.err, f.until.Sub(now).Round(time.Second))
	}
	nc, err := s.build(ctx, key, authorization, now)
	if err != nil {
		return nil, err
	}
	s.onServe(0)
	return nc, nil
}

// Cached returns the catalog kept for the caller whose Authorization header
// value is authorization, however far past its TTL, up to the max age,
// without listing anything.
func (s *Store) Cached(authorization string) (*Catalog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(s.now())
	c, ok := s.entries[identity(authorization)]
	return c, ok
}

// Len is how many catalogs the store keeps.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(s.now())
	return len(s.entries)
}

// MaxTools is how many tools the largest catalog the store would serve
// holds: of the catalogs kept, those younger than the max age. Unlike the
// last build's count, it does not change with which caller listed last.
func (s *Store) MaxTools() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.entries {
		if now.Sub(c.built) < s.maxAge {
			n = max(n, c.Len())
		}
	}
	return n
}

// sweep drops the catalogs past the max age, which are never served again:
// the next request of their caller lists the tools anew. s.mu must be held.
func (s *Store) sweep(now time.Time) {
	for k, c := range s.entries {
		if now.Sub(c.built) >= s.maxAge {
			delete(s.entries, k)
		}
	}
}

// refresh lists the caller's tools again in the background, unless that is
// already under way.
func (s *Store) refresh(ctx context.Context, key, authorization string, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshing[key] {
		return
	}
	s.refreshing[key] = true
	ctx = context.WithoutCancel(ctx)
	go func() {
		_, _ = s.build(ctx, key, authorization, since)
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.refreshing, key)
	}()
}

// build lists the caller's tools and keeps the catalog made of them, or
// remembers that it failed. When another build finished or failed since
// the caller decided to list, at since or later, its result answers
// instead: a clock that has not moved in between, coarse or a test's,
// gives that result the very time since.
func (s *Store) build(ctx context.Context, key, authorization string, since time.Time) (*Catalog, error) {
	v, err, _ := s.group.Do(key, func() (any, error) {
		s.mu.Lock()
		last, built := s.entries[key]
		f, failed := s.failures[key]
		s.mu.Unlock()
		switch {
		case built && !last.built.Before(since):
			return last, nil
		case failed && !f.at.Before(since):
			return nil, f.err
		}
		// The build outlives a caller that gives up, so the others waiting
		// on it still get it.
		bctx := context.WithoutCancel(ctx)
		start := time.Now()
		listing, lerr := s.list(bctx, authorization)
		if lerr != nil {
			s.report(0, start, lerr)
			s.fail(key, lerr)
			return nil, lerr
		}
		c, eerr := New(bctx, listing.Tools, s.emb)
		c.warnAmbiguous(bctx, s.logger)
		c.instructions = listing.Instructions
		c.built = s.now()
		s.report(len(listing.Tools), start, eerr)
		s.put(key, c)
		return c, nil
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	c, ok := v.(*Catalog)
	if !ok {
		return nil, errors.New("catalog: build returned no catalog")
	}
	return c, nil
}

func (s *Store) report(n int, start time.Time, err error) {
	if s.onBuild != nil {
		s.onBuild(n, time.Since(start), err)
	}
}

// fail remembers that the caller's build failed with err, and backs off,
// twice as long with each failure in a row.
func (s *Store) fail(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.failures[key]
	if !ok && len(s.failures) >= s.maxSize {
		var oldest string
		for k, e := range s.failures {
			if oldest == "" || e.until.Before(s.failures[oldest].until) {
				oldest = k
			}
		}
		delete(s.failures, oldest)
	}
	f.err, f.streak, f.at = err, f.streak+1, s.now()
	f.until = f.at.Add(min(failBackoff<<(min(f.streak, maxShift)-1), maxBackoff))
	s.failures[key] = f
}

func (s *Store) put(key string, c *Catalog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, key)
	s.sweep(s.now())
	if _, ok := s.entries[key]; !ok && len(s.entries) >= s.maxSize {
		var oldest string
		for k, e := range s.entries {
			if oldest == "" || e.built.Before(s.entries[oldest].built) {
				oldest = k
			}
		}
		delete(s.entries, oldest)
	}
	s.entries[key] = c
	s.scheduleSweep()
}

// scheduleSweep sets the timer that sweeps the store when its oldest
// catalog passes the max age, unless one is set or there is none. s.mu must
// be held.
func (s *Store) scheduleSweep() {
	if s.expiry != nil || len(s.entries) == 0 {
		return
	}
	var oldest time.Time
	for _, c := range s.entries {
		if oldest.IsZero() || c.built.Before(oldest) {
			oldest = c.built
		}
	}
	s.expiry = time.AfterFunc(oldest.Add(s.maxAge).Sub(s.now()), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.expiry = nil
		s.sweep(s.now())
		s.scheduleSweep()
	})
}

// identity is the key of a caller: a hash, so the store holds no token.
func identity(authorization string) string {
	sum := sha256.Sum256([]byte(authorization))
	return hex.EncodeToString(sum[:])
}

// Doc returns what search reads of t: its name, title and description,
// and the names and descriptions of its input's top-level properties.
func Doc(t *mcp.Tool) search.Doc {
	d := search.Doc{Name: t.Name, Title: t.Title, Description: t.Description}
	if d.Title == "" && t.Annotations != nil {
		d.Title = t.Annotations.Title
	}
	d.Params = params(t.InputSchema)
	return d
}

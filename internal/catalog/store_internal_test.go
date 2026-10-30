package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A caller reads the store, decides to list, and calls build; another build
// of the same caller can finish in between. With a clock that has not moved
// since, its result carries the very time the caller decided at, and must
// still answer instead of a second listing.

func TestBuildAnswersWithAFailureRecordedWhenTheCallerDecided(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	calls := 0
	s := NewStore(Tools(func(context.Context, string) ([]*mcp.Tool, error) {
		calls++
		return nil, errors.New("gateway down")
	}), &StoreOptions{Now: func() time.Time { return now }})
	key := identity("Bearer a")
	_, err := s.build(t.Context(), key, "Bearer a", now)
	require.Error(t, err)

	_, err = s.build(t.Context(), key, "Bearer a", now)

	require.ErrorContains(t, err, "gateway down", "the failure answers")
	assert.Equal(t, 1, calls, "not listed again")
}

func TestBuildAnswersWithACatalogBuiltWhenTheCallerDecided(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	calls := 0
	s := NewStore(Tools(func(context.Context, string) ([]*mcp.Tool, error) {
		calls++
		return []*mcp.Tool{{Name: "weather_get_forecast"}}, nil
	}), &StoreOptions{Now: func() time.Time { return now }})
	key := identity("Bearer a")
	first, err := s.build(t.Context(), key, "Bearer a", now)
	require.NoError(t, err)

	got, err := s.build(t.Context(), key, "Bearer a", now)

	require.NoError(t, err)
	assert.Same(t, first, got, "the catalog answers")
	assert.Equal(t, 1, calls, "not listed again")
}

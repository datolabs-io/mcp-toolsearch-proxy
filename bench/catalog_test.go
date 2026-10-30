package bench

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvalCoreAndTasks(t *testing.T) {
	t.Parallel()
	core, tasks, err := loadEval()
	require.NoError(t, err)

	require.Len(t, core, 50)
	assert.NotEmpty(t, tasks)
	names := map[string]Tool{}
	for _, tl := range core {
		names[tl.Name] = tl
	}
	for _, task := range tasks {
		assert.Contains(t, names, task.Want, "every task's tool is in the core")
	}
	assert.True(t, names["weather_get_forecast"].ReadOnly)
	assert.False(t, names["mail_send_message"].ReadOnly)
	assert.False(t, names["tasks_complete"].ReadOnly)
	assert.Equal(t, []string{"location"}, names["weather_get_forecast"].Schema["required"])
}

// A catalog holds the core and as many distractors as its size asks, the
// same ones every time, and none of them can be taken for a core tool.
func TestCatalogSizes(t *testing.T) {
	t.Parallel()
	core, _, err := loadEval()
	require.NoError(t, err)
	domains, err := loadDomains()
	require.NoError(t, err)
	corePrefixes := map[string]bool{}
	for _, tl := range core {
		corePrefixes[strings.SplitN(tl.Name, "_", 2)[0]] = true
	}

	for _, n := range []int{50, 250, 1000, 1490} {
		cat, cerr := catalog(core, domains, n)
		require.NoError(t, cerr)
		require.Len(t, cat, n)
		again, aerr := catalog(core, domains, n)
		require.NoError(t, aerr)
		assert.Equal(t, cat, again, "deterministic")

		seen := map[string]bool{}
		for _, tl := range cat {
			assert.False(t, seen[tl.Name], tl.Name)
			seen[tl.Name] = true
			assert.Equal(t, "object", tl.Schema["type"], tl.Name)
		}
		for _, tl := range core {
			assert.True(t, seen[tl.Name], "core tool %s kept", tl.Name)
		}
		for name := range seen {
			if !isCore(core, name) {
				assert.False(t, corePrefixes[strings.SplitN(name, "_", 2)[0]], name)
			}
		}
	}

	_, err = catalog(core, domains, 49)
	require.Error(t, err, "smaller than the core")
	_, err = catalog(core, domains, 1491)
	require.Error(t, err, "more than the distractors")
}

func TestDistractorsReadLikeTools(t *testing.T) {
	t.Parallel()
	domains, err := loadDomains()
	require.NoError(t, err)
	all := distractors(domains)

	byName := map[string]Tool{}
	for _, tl := range all {
		byName[tl.Name] = tl
	}
	get := byName["fleet_get_vehicle"]
	assert.True(t, get.ReadOnly)
	assert.Contains(t, get.Description, "vehicle")
	assert.Equal(t, []string{"id"}, get.Schema["required"])
	del := byName["firewall_delete_nat_rule"]
	assert.False(t, del.ReadOnly)
	assert.Contains(t, del.Description, "NAT rule")
	assert.Contains(t, byName, "marketing_create_a_b_test")
}

func isCore(core []Tool, name string) bool {
	for _, tl := range core {
		if tl.Name == name {
			return true
		}
	}
	return false
}

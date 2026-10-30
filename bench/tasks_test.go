package bench

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tasks are picked the same every time, from both languages.
func TestSelectTasks(t *testing.T) {
	t.Parallel()
	_, tasks, err := loadEval()
	require.NoError(t, err)

	got := selectTasks(tasks, 10)
	require.Len(t, got, 10)
	assert.Equal(t, got, selectTasks(tasks, 10))
	langs := map[string]int{}
	for _, task := range got {
		langs[task.Lang]++
	}
	assert.Equal(t, map[string]int{"en": 5, "lt": 5}, langs)
	assert.Len(t, selectTasks(tasks, 1000), len(tasks))
}

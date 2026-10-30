package bench

import (
	"math/rand/v2"
	"slices"
)

// taskSeed fixes the order tasks are picked in.
const taskSeed = 3

// selectTasks is n of tasks, as many of each language as can be, in a fixed
// random order.
func selectTasks(tasks []Task, n int) []Task {
	byLang := map[string][]Task{}
	var langs []string
	for _, task := range tasks {
		if _, ok := byLang[task.Lang]; !ok {
			langs = append(langs, task.Lang)
		}
		byLang[task.Lang] = append(byLang[task.Lang], task)
	}
	slices.Sort(langs)
	r := rand.New(rand.NewPCG(taskSeed, taskSeed)) //nolint:gosec // a fixed order, not a secret
	for _, lang := range langs {
		ts := byLang[lang]
		r.Shuffle(len(ts), func(i, j int) { ts[i], ts[j] = ts[j], ts[i] })
	}
	var out []Task
	for i := 0; len(out) < min(n, len(tasks)); i++ {
		for _, lang := range langs {
			if i < len(byLang[lang]) && len(out) < n {
				out = append(out, byLang[lang][i])
			}
		}
	}
	return out
}

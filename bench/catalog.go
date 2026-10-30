package bench

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Tool is a made-up tool: its name, what it says it does, whether it only
// reads, and its input schema.
type Tool struct {
	Name        string
	Description string
	ReadOnly    bool
	Schema      map[string]any
}

// Task is a question someone asks, in a language, with the tool that
// answers it.
type Task struct {
	Lang  string `json:"lang"`
	Query string `json:"query"`
	Want  string `json:"want"`
}

// domain is a made-up system of distractor tools: six per noun.
type domain struct {
	Domain string `json:"domain"`
	System string `json:"system"`
	Nouns  []struct {
		Name string `json:"name"` // plural, as in tool names
		One  string `json:"one"`  // one of them, in words
	} `json:"nouns"`
}

// schemaType is JSON Schema's type keyword.
const schemaType = "type"

// Argument names that tools share, and the seed of the order distractors
// are picked in.
const (
	argQuery    = "query"
	argFields   = "fields"
	catalogSeed = 1
)

// The repository whose proxy is measured, as seen from this module, and
// its evaluation set.
const (
	repoRoot = ".."
	evalDir  = repoRoot + "/internal/search/testdata/eval"
)

//go:embed data/domains.json
var domainsJSON []byte

// writeVerbs are the core tools' verbs that create, change or delete
// something; any other core tool only reads.
var writeVerbs = []string{ //nolint:gochecknoglobals // fixed data
	"add", "append", "complete", "create", "log", "post", "refund", "reset", "rollback", "send", "share", "upload",
}

// loadEval reads the evaluation set: its tools, given schemas whose first
// parameter is required, and its questions.
func loadEval() ([]Tool, []Task, error) {
	var tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Params      []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"params"`
	}
	var tasks []Task
	for file, v := range map[string]any{"catalog.json": &tools, "queries.json": &tasks} {
		raw, err := os.ReadFile(filepath.Join(evalDir, file)) //nolint:gosec // the repository's own files
		if err != nil {
			return nil, nil, fmt.Errorf("read evaluation set: %w", err)
		}
		if err = json.Unmarshal(raw, v); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", file, err)
		}
	}
	core := make([]Tool, 0, len(tools))
	for _, tl := range tools {
		props := map[string]any{}
		var required []string
		for i, p := range tl.Params {
			props[p.Name] = prop("string", p.Description)
			if i == 0 {
				required = append(required, p.Name)
			}
		}
		verbs := strings.Split(tl.Name, "_")[1:]
		readOnly := !slices.ContainsFunc(verbs, func(v string) bool { return slices.Contains(writeVerbs, v) })
		core = append(
			core,
			Tool{Name: tl.Name, Description: tl.Description, ReadOnly: readOnly, Schema: schema(props, required)},
		)
	}
	return core, tasks, nil
}

// loadDomains reads the distractor domains.
func loadDomains() ([]domain, error) {
	var ds []domain
	if err := json.Unmarshal(domainsJSON, &ds); err != nil {
		return nil, fmt.Errorf("parse domains: %w", err)
	}
	return ds, nil
}

// distractors are every tool the domains make: list, get, search, create,
// update and delete for each noun.
func distractors(ds []domain) []Tool {
	str := func(desc string) map[string]any { return prop("string", desc) }
	limit := prop("integer", "The most results to return")
	limit["minimum"] = 1
	var out []Tool
	for _, d := range ds {
		for _, n := range d.Nouns {
			one := strings.NewReplacer(" ", "_", "/", "_").Replace(strings.ToLower(n.One))
			plural := strings.ReplaceAll(n.Name, "_", " ")
			id := str("The " + n.One + "'s ID")
			fields := str("The fields to set, as JSON")
			out = append(
				out,
				Tool{
					d.Domain + "_list_" + n.Name,
					fmt.Sprintf("List the %s in %s, newest first.", plural, d.System),
					true,
					schema(map[string]any{"limit": limit, "status": str("Only those with this status")}, nil),
				},
				Tool{
					d.Domain + "_get_" + one,
					fmt.Sprintf("Get one %s from %s by its ID, with all its details.", n.One, d.System),
					true,
					schema(map[string]any{"id": id}, []string{"id"}),
				},
				Tool{
					d.Domain + "_search_" + n.Name,
					fmt.Sprintf("Search the %s in %s by text.", plural, d.System),
					true,
					schema(map[string]any{argQuery: str("Words to look for"), "limit": limit}, []string{argQuery}),
				},
				Tool{
					d.Domain + "_create_" + one,
					fmt.Sprintf("Create a %s in %s.", n.One, d.System),
					false,
					schema(
						map[string]any{"name": str("The " + n.One + "'s name"), argFields: fields},
						[]string{"name"},
					),
				},
				Tool{
					d.Domain + "_update_" + one,
					fmt.Sprintf("Change the fields of a %s in %s.", n.One, d.System),
					false,
					schema(map[string]any{"id": id, argFields: fields}, []string{"id", argFields}),
				},
				Tool{d.Domain + "_delete_" + one, fmt.Sprintf("Delete a %s from %s for good.", n.One, d.System), false,
					schema(map[string]any{"id": id}, []string{"id"})},
			)
		}
	}
	return out
}

// catalog is the core and n-len(core) distractors, chosen in a fixed
// random order so a larger catalog holds every tool of a smaller one, and
// sorted by name, as gateways list them.
func catalog(core []Tool, ds []domain, n int) ([]Tool, error) {
	pool := distractors(ds)
	if n < len(core) || n > len(core)+len(pool) {
		return nil, fmt.Errorf("a catalog holds %d to %d tools, not %d", len(core), len(core)+len(pool), n)
	}
	r := rand.New(rand.NewPCG(catalogSeed, catalogSeed)) //nolint:gosec // a fixed order, not a secret
	r.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	out := append(slices.Clone(core), pool[:n-len(core)]...)
	slices.SortFunc(out, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// prop is a property of type typ, described as desc.
func prop(typ, desc string) map[string]any {
	return map[string]any{schemaType: typ, "description": desc}
}

// schema is an object schema with props, of which required must be given.
func schema(props map[string]any, required []string) map[string]any {
	s := map[string]any{schemaType: "object", "properties": props}
	if required != nil {
		s["required"] = required
	}
	return s
}

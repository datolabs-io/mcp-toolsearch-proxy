package catalog

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/search"
)

// params returns the top-level properties of schema, a tool's input
// schema in whatever form the SDK decoded it, sorted by name. Each property
// is read on its own: one that is a boolean schema, such as "payload": true,
// keeps its name without a description, and one that cannot be read at all
// loses nothing but its own description.
func params(schema any) []search.Param {
	if schema == nil {
		return nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	out := make([]search.Param, 0, len(s.Properties))
	for name, prop := range s.Properties {
		out = append(out, search.Param{Name: name, Description: describe(prop)})
	}
	slices.SortFunc(out, func(a, b search.Param) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// describe returns the description of a property schema, or its title when
// it has none, or nothing for a boolean schema.
func describe(prop json.RawMessage) string {
	var p struct {
		Description string `json:"description"`
		Title       string `json:"title"`
	}
	if json.Unmarshal(prop, &p) != nil {
		return ""
	}
	if p.Description != "" {
		return p.Description
	}
	return p.Title
}

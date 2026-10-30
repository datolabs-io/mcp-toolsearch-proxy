//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Clients differ in how much of JSON Schema they accept, and some drop a
// server's whole tool list over one boolean schema. The gateway may rewrite
// schemas on the way, so they are checked as it serves them.
func TestToolSchemasThroughTheGatewayHoldNoBooleanSchemas(t *testing.T) {
	t.Parallel()

	res, err := connect(t, env(t, "E2E_SEARCH_URL"), token(t, "alice")).ListTools(t.Context(), nil)

	require.NoError(t, err)
	require.NotEmpty(t, res.Tools)
	for _, tl := range res.Tools {
		for kind, s := range map[string]any{"input": tl.InputSchema, "output": tl.OutputSchema} {
			if s == nil {
				continue
			}
			raw, merr := json.Marshal(s)
			require.NoError(t, merr)
			var v any
			require.NoError(t, json.Unmarshal(raw, &v))
			assert.Empty(t, booleanSchemas(v, "$"), "%s schema of %s", kind, tl.Name)
		}
	}
}

// booleanSchemas returns the paths in a decoded JSON Schema where a
// subschema is a boolean, under any keyword that holds subschemas;
// `additionalProperties: false` alone is allowed, as the usual closed object.
// It is a copy of the walker of the server's unit tests, which test it.
func booleanSchemas(v any, path string) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	sub := func(p string, s any) {
		if _, isBool := s.(bool); isBool {
			out = append(out, p)
			return
		}
		out = append(out, booleanSchemas(s, p)...)
	}
	list := func(key string, l []any) {
		for i, s := range l {
			sub(fmt.Sprintf("%s.%s[%d]", path, key, i), s)
		}
	}
	for _, key := range []string{
		"additionalItems", "contains", "not", "if", "then", "else",
		"propertyNames", "unevaluatedItems", "unevaluatedProperties",
	} {
		if s, has := m[key]; has {
			sub(path+"."+key, s)
		}
	}
	// items is a schema, or before draft 2020-12 a list of them.
	if s, has := m["items"]; has {
		if l, isList := s.([]any); isList {
			list("items", l)
		} else {
			sub(path+".items", s)
		}
	}
	if s, has := m["additionalProperties"]; has && s != false {
		sub(path+".additionalProperties", s)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if l, has := m[key].([]any); has {
			list(key, l)
		}
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if set, has := m[key].(map[string]any); has {
			for name, s := range set {
				sub(path+"."+key+"."+name, s)
			}
		}
	}
	// Before draft 2019-09, dependencies held schemas and lists of names.
	if set, has := m["dependencies"].(map[string]any); has {
		for name, s := range set {
			if _, names := s.([]any); !names {
				sub(path+".dependencies."+name, s)
			}
		}
	}
	return out
}

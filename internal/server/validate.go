package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// The check of a call's arguments against the tool's input schema is
// deliberately shallow: whether the required properties are there, and
// whether each property given has a type and value its schema allows, at
// the top level only. Local $refs, anyOf, oneOf and allOf are followed for
// those keywords alone; nothing else, at any depth, is checked.
//
// Real servers publish schemas that their own code reads more loosely than
// the schema says: zod writes additionalProperties: false and strips unknown
// keys at run time, OpenAPI conversions write nullable: true, drafts differ
// on $ref beside other keywords, and format or pattern assertions differ
// between implementations. A full validation would refuse calls those
// servers accept. The mistakes a model makes most, a property missing or
// misnamed, a wrong type or a value outside an enum, are all at the top
// level, and the refusal returns the whole schema for the rest.

// JSON Schema types.
const (
	typeNull    = "null"
	typeBoolean = "boolean"
	typeInteger = "integer"
	typeNumber  = "number"
	typeString  = "string"
	typeArray   = "array"
	typeObject  = "object"
)

// maxProblems bounds the problems a refusal lists.
const maxProblems = 10

// maxRefDepth bounds how many $refs in a row are followed.
const maxRefDepth = 8

// argumentProblems returns what keeps args, a call's arguments as sent (a
// JSON object, or empty), from fitting schema, a tool's input schema as its
// server lists it. It returns nothing when they fit, and when the schema
// cannot be read, resolved or checked: a sloppy schema never refuses a call.
// unknown are the keys given that the schema does not name, which are not
// a problem on their own.
func argumentProblems(schema any, args json.RawMessage) ([]string, []string) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, nil
	}
	var problems, unknown []string
	var doc any
	var root jsonschema.Schema
	if json.Unmarshal(raw, &doc) != nil || json.Unmarshal(raw, &root) != nil {
		return nil, nil
	}
	// A schema the library cannot resolve, as with a remote $ref or a
	// pattern it cannot compile, is not checked at all.
	if _, resolveErr := root.Resolve(nil); resolveErr != nil {
		return nil, nil
	}
	if (root.Type != "" && root.Type != typeObject) || (root.Types != nil && !slices.Contains(root.Types, typeObject)) {
		return nil, nil
	}
	// A root that is a $ref, whose siblings drafts read differently, and a
	// schema with resources of its own, whose $refs are relative to them,
	// are not checked: only the root's own keywords and pointers into the
	// whole document are read here.
	if root.Ref != "" || root.DynamicRef != "" || nestedID(doc, true) {
		return nil, nil
	}
	values := map[string]any{}
	if len(args) > 0 {
		if json.Unmarshal(args, &values) != nil {
			return nil, nil
		}
	}
	for _, name := range root.Required {
		if _, ok := values[name]; !ok {
			problems = append(problems, fmt.Sprintf("%q is required and missing", name))
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		prop, ok := root.Properties[key]
		if !ok {
			// Keys patternProperties may name are not reported.
			if len(root.Properties) > 0 && len(root.PatternProperties) == 0 {
				unknown = append(unknown, key)
			}
			continue
		}
		if p := propertyProblem(doc, prop, key, values[key]); p != "" {
			problems = append(problems, p)
		}
	}
	if len(problems) > maxProblems {
		problems = append(problems[:maxProblems], "and more")
	}
	return problems, unknown
}

// propertyProblem says what keeps value from fitting prop, the schema of
// the property key, or nothing when it fits or cannot be checked.
func propertyProblem(doc any, prop *jsonschema.Schema, key string, value any) string {
	shallow := reduce(doc, prop, 0)
	if shallow == nil {
		return ""
	}
	resolved, err := shallow.Resolve(nil)
	if err != nil {
		return ""
	}
	if resolved.Validate(value) == nil {
		return ""
	}
	types, enum := allowed(shallow)
	got := jsonType(value)
	switch {
	case len(types) > 0 && !typeAllowed(got, types):
		return fmt.Sprintf("%q is %s, but must be %s", key, article(got), strings.Join(types, " or "))
	case len(enum) > 0:
		return fmt.Sprintf("%q is %s, but must be one of %s", key, shortJSON(value), strings.Join(enum, ", "))
	default:
		return fmt.Sprintf("%q does not fit its schema", key)
	}
}

// reduce returns the part of s that the check reads: type, nullable, enum
// and const, through local $refs and anyOf, oneOf and allOf. It returns nil
// when that part allows any value. oneOf becomes anyOf, since its branches,
// reduced, may overlap where the whole ones do not.
func reduce(doc any, s *jsonschema.Schema, depth int) *jsonschema.Schema {
	if s == nil || depth > maxRefDepth {
		return nil
	}
	if s.Ref != "" {
		target := localRef(doc, s.Ref)
		if target == nil {
			return nil
		}
		// Keywords beside a $ref are left alone, as draft 7 reads them.
		return reduce(doc, target, depth+1)
	}
	out := &jsonschema.Schema{}
	constrained := false
	types := s.Types
	if s.Type != "" {
		types = []string{s.Type}
	}
	nullable, _ := s.Extra["nullable"].(bool)
	if len(types) > 0 {
		out.Types = slices.Clone(types)
		if nullable && !slices.Contains(out.Types, typeNull) {
			out.Types = append(out.Types, typeNull)
		}
		constrained = true
	}
	if len(s.Enum) > 0 {
		out.Enum = slices.Clone(s.Enum)
		if nullable {
			out.Enum = append(out.Enum, nil)
		}
		constrained = true
	}
	if s.Const != nil && !nullable {
		out.Const = s.Const
		constrained = true
	}
	for _, branches := range [][]*jsonschema.Schema{s.AnyOf, s.OneOf} {
		if union := reduceAll(doc, branches, depth); union != nil {
			if nullable {
				union = append(union, &jsonschema.Schema{Type: typeNull})
			}
			out.AllOf = append(out.AllOf, &jsonschema.Schema{AnyOf: union})
			constrained = true
		}
	}
	for _, branch := range s.AllOf {
		if r := reduce(doc, branch, depth+1); r != nil {
			out.AllOf = append(out.AllOf, r)
			constrained = true
		}
	}
	if !constrained {
		return nil
	}
	return out
}

// reduceAll reduces each of branches, or returns nil when any of them
// allows any value, as their union then does.
func reduceAll(doc any, branches []*jsonschema.Schema, depth int) []*jsonschema.Schema {
	if len(branches) == 0 {
		return nil
	}
	out := make([]*jsonschema.Schema, 0, len(branches))
	for _, b := range branches {
		r := reduce(doc, b, depth+1)
		if r == nil {
			return nil
		}
		out = append(out, r)
	}
	return out
}

// nameKeywords hold subschemas under names rather than keywords.
var nameKeywords = map[string]bool{ //nolint:gochecknoglobals // a constant set
	"properties": true, "$defs": true, "definitions": true, "patternProperties": true,
	"dependentSchemas": true,
}

// nestedID reports whether v, a decoded schema, holds an $id or id below
// its root, which is v when root is set.
func nestedID(v any, root bool) bool {
	switch x := v.(type) {
	case map[string]any:
		if !root && (isString(x["$id"]) || isString(x["id"])) {
			return true
		}
		for key, sub := range x {
			if m, ok := sub.(map[string]any); ok && nameKeywords[key] {
				// The keys are names, so a property may be called id.
				for _, s := range m {
					if nestedID(s, false) {
						return true
					}
				}
			} else if nestedID(sub, false) {
				return true
			}
		}
	case []any:
		for _, sub := range x {
			if nestedID(sub, false) {
				return true
			}
		}
	}
	return false
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

// localRef returns the subschema of doc, the whole input schema, that ref
// points to, when ref is a JSON pointer into doc such as "#/$defs/Color";
// nil for any other.
func localRef(doc any, ref string) *jsonschema.Schema {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	v := doc
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok = m[part]; !ok {
			return nil
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var s jsonschema.Schema
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// allowed collects the types and enum values s and its anyOf branches
// allow, for a refusal's text.
func allowed(s *jsonschema.Schema) ([]string, []string) {
	var types, enum []string
	var walk func(*jsonschema.Schema)
	walk = func(s *jsonschema.Schema) {
		for _, t := range s.Types {
			if !slices.Contains(types, t) {
				types = append(types, t)
			}
		}
		for _, v := range s.Enum {
			enum = append(enum, shortJSON(v))
		}
		if s.Const != nil {
			enum = append(enum, shortJSON(*s.Const))
		}
		for _, sub := range s.AllOf {
			walk(sub)
		}
		for _, sub := range s.AnyOf {
			walk(sub)
		}
	}
	walk(s)
	if len(enum) > maxProblems {
		enum = append(enum[:maxProblems], "…")
	}
	return types, enum
}

// typeAllowed reports whether a value of JSON type got fits types, where
// an integer is also a number.
func typeAllowed(got string, types []string) bool {
	return slices.Contains(types, got) || (got == typeInteger && slices.Contains(types, typeNumber))
}

// jsonType is the JSON Schema type of v, as encoding/json decodes it.
func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return typeNull
	case bool:
		return typeBoolean
	case float64:
		if x == float64(int64(x)) {
			return typeInteger
		}
		return typeNumber
	case string:
		return typeString
	case []any:
		return typeArray
	default:
		return typeObject
	}
}

// article names a JSON type with its article, as in "an integer".
func article(t string) string {
	switch t {
	case typeNull:
		return typeNull
	case typeInteger, typeObject, typeArray:
		return "an " + t
	default:
		return "a " + t
	}
}

// maxShown bounds a value a refusal shows.
const maxShown = 64

// shortJSON is v as JSON, cut at maxShown bytes.
func shortJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	if len(raw) > maxShown {
		return string(raw[:maxShown]) + "…"
	}
	return string(raw)
}

// refusal is the text of a call refused because its arguments do not fit
// the input schema of the tool name.
func refusal(name string, schema any, problems, unknown []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "the arguments do not fit the input schema of %s, so it was not called:\n", name)
	for _, p := range problems {
		b.WriteString("- " + p + "\n")
	}
	if len(unknown) > 0 {
		if len(unknown) > maxProblems {
			unknown = append(unknown[:maxProblems:maxProblems], "…")
		}
		quoted := make([]string, len(unknown))
		for i, k := range unknown {
			quoted[i] = fmt.Sprintf("%q", truncate(k))
		}
		b.WriteString("- the schema names no " + strings.Join(quoted, ", ") + "\n")
	}
	b.WriteString(schemaNote(name, schema))
	return b.String()
}

// schemaNote gives the input schema of the tool name, when it is small
// enough, so that a call can be fixed without describe_tool.
func schemaNote(name string, schema any) string {
	raw, err := json.Marshal(schema)
	if err != nil || schema == nil {
		return fmt.Sprintf("Read the input schema of %s with %s.", name, ToolDescribe)
	}
	if len(raw) > maxSearchSchema {
		return fmt.Sprintf("The input schema of %s is too large to include here (%d bytes); read it with %s.",
			name, len(raw), ToolDescribe)
	}
	return fmt.Sprintf("Input schema of %s: %s", name, raw)
}

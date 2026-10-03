package ghfake

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// The fake's response and webhook SHAPES come from GitHub's own OpenAPI
// description (schema/openapi-subset.json — the operations and webhooks the
// fake serves, copied verbatim from github/rest-api-description; see
// schema/extract_schemas.py for the source, tag and regeneration).
//
// Every object the fake emits is built by Shape: the schema's REQUIRED fields
// are filled type-correctly from the schema itself (nullable → null, enum →
// its first value, object → its own required fields, recursively), and the
// model's real values are deep-merged over that. So no response is missing a
// field GitHub documents as always present, and Validate — run over every
// response and delivery in the package's tests — checks the model's values
// against the same schema.

//go:embed schema/openapi-subset.json
var openapiJSON []byte

type openapiDoc struct {
	Source     string                    `json:"source"`
	Operations map[string]map[string]any `json:"operations"`
	Webhooks   map[string]any            `json:"webhooks"`
	Schemas    map[string]any            `json:"schemas"`
}

var (
	docOnce sync.Once
	doc     openapiDoc
)

func spec() *openapiDoc {
	docOnce.Do(func() {
		if err := json.Unmarshal(openapiJSON, &doc); err != nil {
			panic("ghfake: vendored OpenAPI subset does not parse: " + err.Error())
		}
	})
	return &doc
}

// SchemaSource names the vendored OpenAPI description (for logs and docs).
func SchemaSource() string { return spec().Source }

// componentRef is "#/components/schemas/<name>".
func componentRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// OperationSchema returns the response schema of a REST operation ("GET
// /repos/{owner}/{repo}/pulls/{pull_number}") for a status code, and whether
// the operation is in the vendored subset at all. A nil schema with ok=true
// is a documented empty body (204).
func OperationSchema(op string, status int) (schema any, ok bool) {
	codes, found := spec().Operations[op]
	if !found {
		return nil, false
	}
	s, ok := codes[fmt.Sprint(status)]
	return s, ok
}

// WebhookSchema returns a webhook's payload schema by its OpenAPI name
// ("pull-request-review-submitted").
func WebhookSchema(name string) (any, bool) {
	s, ok := spec().Webhooks[name]
	return s, ok
}

func resolve(node any) any {
	for i := 0; i < 64; i++ {
		m, ok := node.(map[string]any)
		if !ok {
			return node
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return node
		}
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		next, found := spec().Schemas[name]
		if !found {
			panic("ghfake: schema reference not vendored: " + ref)
		}
		node = next
	}
	return node
}

// types lists a schema's JSON types (3.1 `type` may be a string or a list).
func types(s map[string]any) []string {
	switch t := s["type"].(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func has(xs []string, x string) bool {
	for _, e := range xs {
		if e == x {
			return true
		}
	}
	return false
}

// Shape builds a value for schema from over: walking the schema and the
// value together, every object — at any depth, including one under a
// nullable reference — gets its documented required fields (from over where
// over has them, else schema-derived defaults), and over's own fields are
// kept (shaped by their property schemas where the schema has them).
func Shape(schema any, over map[string]any) map[string]any {
	v := shapeWith(schema, over, 0)
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// ShapeOf is Shape for a named component schema ("pull-request").
func ShapeOf(name string, over map[string]any) map[string]any {
	return Shape(componentRef(name), over)
}

func merge(dst, over map[string]any) map[string]any {
	for k, v := range over {
		if om, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = merge(dm, om)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// alternatives lists a node's anyOf/oneOf alternatives (nil when it has none).
func alternatives(s map[string]any) []any {
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := s[key].([]any); ok && len(alts) > 0 {
			return alts
		}
	}
	return nil
}

func shapeWith(node, v any, depth int) any {
	if depth > 32 {
		return v
	}
	s, ok := resolve(node).(map[string]any)
	if !ok {
		return v
	}
	if v == nil {
		return nil
	}
	if all, ok := s["allOf"].([]any); ok {
		m, isMap := v.(map[string]any)
		if !isMap {
			return v
		}
		out := map[string]any{}
		for _, a := range all {
			if sm, ok := shapeWith(a, m, depth+1).(map[string]any); ok {
				merge(out, sm)
			}
		}
		return out
	}
	if alts := alternatives(s); alts != nil {
		// The alternative of the value's own JSON type: an object fills
		// from the first object alternative, a list from the first array.
		want := jsonKind(v)
		for _, a := range alts {
			as, _ := resolve(a).(map[string]any)
			if as == nil {
				continue
			}
			ts := types(as)
			if has(ts, want) || (want == "object" && (as["properties"] != nil || as["allOf"] != nil)) || (len(ts) == 0 && want == "object" && alternatives(as) != nil) {
				return shapeWith(a, v, depth+1)
			}
		}
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if props == nil {
			return x
		}
		out := map[string]any{}
		for k, e := range x {
			if ps, ok := props[k]; ok {
				out[k] = shapeWith(ps, e, depth+1)
			} else {
				out[k] = e
			}
		}
		req, _ := s["required"].([]any)
		for _, r := range req {
			name, _ := r.(string)
			if _, present := out[name]; present {
				continue
			}
			if ps, ok := props[name]; ok {
				out[name] = fill(ps, depth+1)
			} else {
				out[name] = nil
			}
		}
		return out
	case []any:
		it, ok := s["items"]
		if !ok {
			return x
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = shapeWith(it, e, depth+1)
		}
		return out
	}
	return v
}

// jsonKind is a Go value's JSON type name.
func jsonKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int64, float64:
		return "integer"
	}
	return "object"
}

// fill is the schema-derived default for one node.
func fill(node any, depth int) any {
	if depth > 24 {
		return nil
	}
	s, ok := resolve(node).(map[string]any)
	if !ok {
		return nil
	}
	if c, ok := s["const"]; ok {
		return c
	}
	if all, ok := s["allOf"].([]any); ok {
		out := map[string]any{}
		for _, a := range all {
			if m, ok := fill(a, depth+1).(map[string]any); ok {
				merge(out, m)
			}
		}
		return out
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := s[key].([]any); ok && len(alts) > 0 {
			for _, a := range alts {
				if as, ok := resolve(a).(map[string]any); ok && has(types(as), "null") && len(types(as)) == 1 {
					return nil // a nullable alternative: GitHub sends null
				}
			}
			return fill(alts[0], depth+1)
		}
	}
	ts := types(s)
	if has(ts, "null") {
		return nil
	}
	if e, ok := s["enum"].([]any); ok && len(e) > 0 {
		return e[0]
	}
	t := ""
	if len(ts) > 0 {
		t = ts[0]
	} else if _, ok := s["properties"]; ok {
		t = "object"
	}
	switch t {
	case "string":
		switch s["format"] {
		case "date-time":
			return "2026-01-01T00:00:00Z"
		case "uri":
			return "https://api.github.com/"
		}
		return ""
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		out := map[string]any{}
		props, _ := s["properties"].(map[string]any)
		req, _ := s["required"].([]any)
		for _, r := range req {
			name, _ := r.(string)
			if ps, ok := props[name]; ok {
				out[name] = fill(ps, depth+1)
			} else {
				out[name] = nil
			}
		}
		return out
	}
	return nil
}

// Validate checks value (as decoded from JSON) against schema and returns
// every violation with its path; empty means valid. It covers what a shape
// check needs: types (with 3.1 null), required properties, nested
// properties, array items, enums, $ref, allOf (all), anyOf/oneOf (at least
// one alternative — GitHub's oneOfs overlap, so exclusivity is not checked).
// Formats, bounds and additionalProperties are not checked.
func Validate(schema, value any) []string {
	var errs []string
	validate(schema, value, "$", &errs, 0)
	return errs
}

func validate(node, v any, path string, errs *[]string, depth int) {
	if depth > 48 {
		return
	}
	s, ok := resolve(node).(map[string]any)
	if !ok || len(s) == 0 {
		return
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, a := range all {
			validate(a, v, path, errs, depth+1)
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		alts, ok := s[key].([]any)
		if !ok {
			continue
		}
		var best []string
		for i, a := range alts {
			var e []string
			validate(a, v, path, &e, depth+1)
			if len(e) == 0 {
				best = nil
				break
			}
			if i == 0 || len(e) < len(best) {
				best = e
			}
		}
		*errs = append(*errs, best...)
	}
	if c, ok := s["const"]; ok && fmt.Sprint(c) != fmt.Sprint(v) {
		*errs = append(*errs, fmt.Sprintf("%s: want const %v, got %v", path, c, v))
	}
	ts := types(s)
	if len(ts) > 0 && !typeOK(ts, v) {
		*errs = append(*errs, fmt.Sprintf("%s: want %v, got %s", path, ts, jsonType(v)))
		return
	}
	if e, ok := s["enum"].([]any); ok && v != nil {
		found := false
		for _, x := range e {
			if fmt.Sprint(x) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			*errs = append(*errs, fmt.Sprintf("%s: %v is not one of %v", path, v, e))
		}
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if name, _ := r.(string); name != "" {
					if _, present := x[name]; !present {
						*errs = append(*errs, fmt.Sprintf("%s: missing required %q", path, name))
					}
				}
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k]; ok {
				validate(ps, x[k], path+"."+k, errs, depth+1)
			}
		}
	case []any:
		if it, ok := s["items"]; ok {
			for i, e := range x {
				validate(it, e, fmt.Sprintf("%s[%d]", path, i), errs, depth+1)
			}
		}
	}
}

func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == float64(int64(x)) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func typeOK(ts []string, v any) bool {
	got := jsonType(v)
	for _, t := range ts {
		if t == got || (t == "number" && got == "integer") {
			return true
		}
	}
	return false
}

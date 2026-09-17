package enginekit

import (
	"fmt"
	"sort"
)

// StepVars turns a step's env: map into named variable bindings for the jq and
// yq engines — each entry becomes a $NAME variable whose value is the entry's
// string, exactly the way `jq --arg NAME value` does. It is the one place the
// two engines agree on what a variable name may be and in what order the
// bindings are applied, so a program compiles identically under either.
//
// The returned names are BARE (no leading "$") and the values are aligned
// position-for-position: names[i] binds to values[i]. Both slices are sorted by
// name, which makes gojq's positional (variables, values) pairing deterministic
// regardless of Go's randomized map iteration — the jq engine hands the names
// to gojq.WithVariables and the values to Code.Run in this same order, and the
// yq engine seeds each name/value onto the evaluation context.
//
// A key that is not a valid variable identifier is rejected with a clear error
// rather than silently dropped. The accepted shape — a letter or underscore
// followed by letters, digits or underscores — is the INTERSECTION of what
// gojq and yqlib each accept as a variable name (gojq requires a leading
// letter/underscore; yqlib's lexer also allows digits and hyphens, which gojq
// does not), so any name that passes here is legal in both engines. Values are
// unconstrained — any string an env: entry can hold is a valid variable value.
//
// Values are always strings, mirroring `jq --arg` (not `--argjson`): a program
// that wants a number, bool, or nested structure converts explicitly, e.g. jq's
// `($COUNT | tonumber)` / `($JSON | fromjson)` or yq's `($JSON | from_json)`.
// This keeps the binding unambiguous — "3" is the string "3", never the number
// 3 — instead of guessing a type from the text.
func StepVars(env map[string]string) (names []string, values []string, err error) {
	if len(env) == 0 {
		return nil, nil, nil
	}
	names = make([]string, 0, len(env))
	for name := range env {
		if !validVarName(name) {
			return nil, nil, fmt.Errorf("invalid variable name %q: a step's env: key used as a $variable must be a letter or _ followed by letters, digits or _", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	values = make([]string, len(names))
	for i, name := range names {
		values[i] = env[name]
	}
	return names, values, nil
}

// validVarName reports whether s is a legal jq/yq variable name (without the
// leading "$"): a letter or underscore, then letters, digits or underscores.
func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

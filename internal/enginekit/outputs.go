package enginekit

// WrapValue is the OUTPUT CONTRACT the four in-process engines share, ported
// verbatim from conductor's internal/code/code.go wrapValue. An engine's
// snippet produces one Go value; this decides what that value means as a
// step's outputs:
//
//   - nil (no return, an empty script) -> {} (no outputs; a step that only
//     had side effects shouldn't have to produce any)
//   - an object -> that object, verbatim (its keys become named outputs)
//   - anything else (a number, a string, a list) -> {"value": v} (it is a
//     result, just not a set of named ones, so it becomes the step's single
//     `value` output rather than being rejected)
//
// It is deliberately NOT internal/code's ParseOutputs. That one parses a host
// interpreter's raw STDOUT (`use: cli`, `run: go`, run: python/ruby/…) and so
// has two more cases these engines cannot reach — blank text, and text that
// isn't JSON at all. js/go-embed/risor/lua hand back a typed value rather than
// a byte stream, so they end at the three cases above, exactly as they did
// in-binary.
func WrapValue(v any) map[string]any {
	switch t := v.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return t
	default:
		return map[string]any{"value": v}
	}
}

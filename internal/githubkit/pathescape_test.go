package githubkit

import "testing"

// §17: the ref/label/id interpolations escape; the three content-path ones
// did not. A `path` carrying `?`, `#`, or `..` reached the API as
// structure rather than as a name, so a caller-supplied path could address
// a different endpoint or a different file than the one it named.
func TestEscapePathEscapesPerSegment(t *testing.T) {
	tests := map[string]string{
		"docs/README.md": "docs/README.md",    // separators survive
		"a/b c.md":       "a/b%20c.md",        // spaces
		"a/q?ref=other":  "a/q%3Fref=other",   // cannot start a query
		"a/frag#x":       "a/frag%23x",        // cannot start a fragment
		"..%2f..%2fetc":  "..%252f..%252fetc", // no double-decode
		"a/../b":         "a/../b",            // dots are a path concern, kept literal
	}
	for in, want := range tests {
		if got := escapePath(in); got != want {
			t.Errorf("escapePath(%q) = %q, want %q", in, got, want)
		}
	}
}

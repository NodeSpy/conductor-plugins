package enginekit

import (
	"fmt"
	"os"
	"strings"
)

// LoadCode resolves a step's code: source. A "file:" prefix loads the
// referenced file's text (a leading "~/" is expanded); anything else is
// returned unchanged — it is inline source. This mirrors conductor core's
// `code: file:<path>` convention so a code step can point at a script file
// instead of inlining it, and every text engine that calls LoadCode honors the
// same spelling.
//
// The "file:" prefix is required (rather than guessing a bare string is a path)
// so a short inline program — a jq `keys`, a lua `return 1` — is never mistaken
// for a filename.
func LoadCode(code string) (string, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(code), "file:")
	if !ok {
		return code, nil
	}
	path := expandHome(strings.TrimSpace(rest))
	if path == "" {
		return "", fmt.Errorf("code: file: needs a path")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("code: reading %q: %w", path, err)
	}
	return string(b), nil
}

// expandHome replaces a leading "~/" (or a bare "~") with the user's home dir.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + strings.TrimPrefix(p, "~")
		}
	}
	return p
}

package ghsource

import "testing"

func TestAPIBaseURL(t *testing.T) {
	// Unset (production) → public API, unchanged behavior.
	t.Setenv("PC_GITHUB_API_BASE", "")
	if got := apiBaseURL(); got != "https://api.github.com" {
		t.Fatalf("default apiBaseURL = %q, want public API", got)
	}
	// Overridden (hermetic harness) → mock endpoint, trailing slash trimmed.
	t.Setenv("PC_GITHUB_API_BASE", "http://mock-github:8080/")
	if got := apiBaseURL(); got != "http://mock-github:8080" {
		t.Fatalf("overridden apiBaseURL = %q, want mock endpoint without trailing slash", got)
	}
}

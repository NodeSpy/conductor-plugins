package githubkit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ghHostsToken reads the login `gh auth login` saved in gh's hosts.yml: the
// fallback when `gh auth token` itself fails. gh can refuse to run with a
// read-only config dir that lacks config.yml (it tries to write one), which is
// how it is mounted in conductor's plugin jail; reading the file it would have
// read needs nothing more than what the plugin already declares (gh's config
// dir and GH_CONFIG_DIR/XDG_CONFIG_HOME/GH_HOST). A login kept in a system
// keyring is not in the file, and is not found here.
func ghHostsToken() (string, error) {
	dir := ghConfigDir()
	if dir == "" {
		return "", fmt.Errorf("no gh config dir")
	}
	b, err := os.ReadFile(filepath.Join(dir, "hosts.yml"))
	if err != nil {
		return "", err
	}
	type login struct {
		OAuthToken string `yaml:"oauth_token"`
	}
	var hosts map[string]struct {
		OAuthToken string           `yaml:"oauth_token"`
		User       string           `yaml:"user"`
		Users      map[string]login `yaml:"users"`
	}
	if err := yaml.Unmarshal(b, &hosts); err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Join(dir, "hosts.yml"), err)
	}
	host := strings.TrimSpace(os.Getenv("GH_HOST"))
	if host == "" {
		host = "github.com"
	}
	h, ok := hosts[host]
	if !ok {
		return "", fmt.Errorf("gh has no login for %s", host)
	}
	if tok := strings.TrimSpace(h.OAuthToken); tok != "" {
		return tok, nil
	}
	if tok := strings.TrimSpace(h.Users[h.User].OAuthToken); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("gh's login for %s is not in hosts.yml (a keyring login)", host)
}

// ghConfigDir is where gh keeps its config: GH_CONFIG_DIR, else
// $XDG_CONFIG_HOME/gh, else ~/.config/gh.
func ghConfigDir() string {
	if d := os.Getenv("GH_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "gh")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gh")
}

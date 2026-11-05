// Command conductor-git-land is a verb-only conductor connector that lands
// commits made in another repository — a workspace an agent committed in —
// on a remote branch that may have moved since they were made: keep copies
// them out under a ref, replay re-creates them on the branch's new head
// without a working tree or hooks, and push sends the result, fast-forward
// only (land.go). Built ONLY against the public SDK.
//
// It is its own connector, not verbs on the git connector, because conductor
// bundles a connector type named git (a sandboxed agent's git.push) and a
// plugin cannot provide a bundled type.
//
// Credentials are the git connector's, materialized per invocation rather
// than baked into argv:
//
//   - SSH auth (ssh_key / ssh_key_path) is delivered via GIT_SSH_COMMAND — an
//     inline ssh_key is written to a 0600 temp file for the duration of the
//     call and removed on return; ssh_key_path is used as-is.
//   - HTTPS auth (username / token) is delivered via a GIT_ASKPASS helper
//     script (0700 temp file, removed on return) plus GIT_TERMINAL_PROMPT=0 —
//     the secret is written to a file, never to argv or the process list.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type landPlugin struct{}

func (landPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "git-land",
		Desc: "git-land: land commits made in another repository (a workspace) on a remote branch that moved since — keep them under a ref, replay them onto the branch's new head without a working tree or hooks (reporting the conflicting paths when they do not apply), and push the result fast-forward only. Configurable per-connection SSH key or HTTPS token credentials. Shells out to the git CLI.",
		Connection: plugin.Schema{
			"dir":                      {Type: "string", Required: true, Desc: "the repository commits are kept and replayed in (created bare when missing); never a workspace"},
			"binary":                   {Type: "string", Desc: "override the git binary path (default git)"},
			"ssh_key":                  {Type: "string", Desc: "inline SSH private key (PEM). Written to a 0600 temp file for the call and removed on return."},
			"ssh_key_path":             {Type: "string", Desc: "path to an existing SSH private key file (used as-is; takes precedence over ssh_key)"},
			"strict_host_key_checking": {Type: "string", Enum: []string{"yes", "no", "accept-new"}, Desc: "ssh -o StrictHostKeyChecking=<value>"},
			"known_hosts":              {Type: "string", Desc: "ssh -o UserKnownHostsFile=<path>"},
			"username":                 {Type: "string", Desc: "HTTPS credential username; -c credential.username=<username> (paired with token)"},
			"token":                    {Type: "string", Desc: "HTTPS credential token/password, delivered via a GIT_ASKPASS temp script — never placed in argv"},
			"sign":                     {Type: "boolean", Desc: "sign the commits replay writes (git commit-tree -S, with the signing key set in config)"},
			"config":                   {Type: "map", Desc: "additional -c key=value flags (sorted), applied before every subcommand — e.g. user.signingkey and gpg.format for sign"},
			"env":                      {Type: "map", Desc: "default process environment for every invocation"},
			"timeout":                  {Type: "duration", Desc: "default per-verb timeout (default 10m); a verb's timeout option overrides it"},
		},
		Verbs: landVerbs(),
		Capabilities: plugin.Capabilities{
			Commands: []string{"git", "ssh"},
			Spawns:   true,
			// No declared egress, as the git connector: git's own transport
			// reaches the remote URL / SSH host.
			Egress: []string{},
		},
	}
}

// stdOutputs is the request-response shape every verb shares.
func stdOutputs() plugin.Schema {
	return plugin.Schema{
		"stdout":    {Type: "string"},
		"stderr":    {Type: "string"},
		"exit_code": {Type: "integer"},
	}
}

func withOutputs(extra plugin.Schema) plugin.Schema {
	out := stdOutputs()
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// landConn is the resolved connection config for one invocation.
type landConn struct {
	dir                   string
	binary                string
	sshKeyPath            string
	sshKeyInline          string
	strictHostKeyChecking string
	knownHosts            string
	username              string
	token                 string
	sign                  bool
	config                map[string]string
	env                   map[string]string
	timeout               time.Duration
}

func parseConn(m map[string]any) (landConn, error) {
	c := landConn{
		dir:                   str(m["dir"]),
		binary:                strOr(m["binary"], "git"),
		sshKeyPath:            str(m["ssh_key_path"]),
		sshKeyInline:          str(m["ssh_key"]),
		strictHostKeyChecking: str(m["strict_host_key_checking"]),
		knownHosts:            str(m["known_hosts"]),
		username:              str(m["username"]),
		token:                 str(m["token"]),
		sign:                  boolv(m["sign"]),
		config:                strMap(m["config"]),
		env:                   strMap(m["env"]),
		timeout:               10 * time.Minute,
	}
	if d, err := toDuration(m["timeout"]); err != nil {
		return c, fmt.Errorf("connection.timeout: %w", err)
	} else if d > 0 {
		c.timeout = d
	}
	return c, nil
}

// configFlags builds the sorted `-c key=value` flags that precede every
// subcommand.
func configFlags(config map[string]string) []string {
	keys := make([]string, 0, len(config))
	for k := range config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var a []string
	for _, k := range keys {
		a = append(a, "-c", k+"="+config[k])
	}
	return a
}

// sshCommand renders the GIT_SSH_COMMAND value for a materialized key file
// plus the optional host-key options; the value is parsed by a shell, so the
// paths are single-quoted.
func sshCommand(keyPath, strictHostKeyChecking, knownHosts string) string {
	parts := []string{"ssh", "-o", "IdentitiesOnly=yes", "-i", shQuote(keyPath)}
	if strictHostKeyChecking != "" {
		parts = append(parts, "-o", "StrictHostKeyChecking="+strictHostKeyChecking)
	}
	if knownHosts != "" {
		parts = append(parts, "-o", "UserKnownHostsFile="+shQuote(knownHosts))
	}
	return strings.Join(parts, " ")
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (landPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	// Materialize credentials for THIS invocation only; always cleaned up.
	var cleanup []string
	defer func() {
		for _, p := range cleanup {
			os.Remove(p)
		}
	}()
	extraEnv := map[string]string{}
	flags := configFlags(conn.config)
	sshKeyPath := conn.sshKeyPath
	if sshKeyPath == "" && conn.sshKeyInline != "" {
		f, werr := writeTempFile("conductor-git-land-key-*", conn.sshKeyInline, 0o600)
		if werr != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "ssh_key: "+werr.Error())
		}
		sshKeyPath = f
		cleanup = append(cleanup, f)
	}
	if sshKeyPath != "" {
		extraEnv["GIT_SSH_COMMAND"] = sshCommand(sshKeyPath, conn.strictHostKeyChecking, conn.knownHosts)
	}
	if conn.token != "" {
		askpass, werr := writeAskpass(conn.token)
		if werr != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "token: "+werr.Error())
		}
		cleanup = append(cleanup, askpass)
		extraEnv["GIT_ASKPASS"] = askpass
		extraEnv["GIT_TERMINAL_PROMPT"] = "0"
		if conn.username != "" {
			flags = append(flags, "-c", "credential.username="+conn.username)
		}
	}

	timeout := conn.timeout
	if d, derr := toDuration(o["timeout"]); derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": options.timeout: "+derr.Error())
	} else if d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := invokeLand(gitRunner{ctx: ctx, binary: conn.binary, dir: conn.dir, env: procEnv(conn.env, extraEnv), flags: flags, sign: conn.sign}, req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, err
	}
	return plugin.InvokeResult{Outputs: res}, nil
}

// runGitIn runs git with stdin: a non-zero exit is DATA (exit_code), only a
// failure to start the process (missing binary, timeout) is an error. No
// shell is involved.
func runGitIn(ctx context.Context, binary, dir string, env, args []string, stdin string) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			return nil, err
		}
	}
	return map[string]any{"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": exit}, nil
}

// writeTempFile writes content to a new temp file with the given permissions,
// returning its path. The caller removes it.
func writeTempFile(pattern, content string, perm os.FileMode) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if err := os.Chmod(path, perm); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// writeAskpass writes a GIT_ASKPASS helper that prints the token whatever
// the prompt — credential.username answers the username prompt and
// GIT_TERMINAL_PROMPT=0 disables any other fallback.
func writeAskpass(token string) (string, error) {
	script := "#!/bin/sh\nprintf '%s\\n' " + shQuote(token) + "\n"
	return writeTempFile("conductor-git-land-askpass-*", script, 0o700)
}

// procEnv is the parent's environment, then the connection's env map, then
// the invocation's overrides, each sorted, so later entries win.
func procEnv(env, extra map[string]string) []string {
	out := os.Environ()
	out = append(out, sortedAssignments(env)...)
	out = append(out, sortedAssignments(extra)...)
	return out
}

func sortedAssignments(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func main() {
	if err := plugin.Serve(landPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-git-land: %v\n", err)
		os.Exit(1)
	}
}

// --- option helpers (stdlib only) ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
}

func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = fmt.Sprintf("%v", val)
	}
	return out
}

// toDuration parses a timeout: a Go duration string ("10m"), or a number of
// seconds. Zero/absent → 0 (use the default).
func toDuration(v any) (time.Duration, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case string:
		if x == "" {
			return 0, nil
		}
		return time.ParseDuration(x)
	case float64:
		return time.Duration(x * float64(time.Second)), nil
	case int:
		return time.Duration(x) * time.Second, nil
	case int64:
		return time.Duration(x) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %v", v)
}

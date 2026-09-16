// Command conductor-git is a verb-only conductor connector (#59) that drives
// the `git` CLI directly — clone/init/fetch/pull/push, the working-tree verbs
// (checkout, switch, add, commit, status, diff, log), branch/tag/remote/stash
// management, merge/rebase/reset, the read-only rev_parse/ls_remote/show, and
// a `cli` escape hatch for any subcommand a first-class verb does not cover.
// Built ONLY against the public SDK.
//
// This connector's distinguishing feature is CONFIGURABLE CREDENTIALS on the
// connection, materialized safely per invocation rather than baked into argv:
//
//   - SSH auth (ssh_key / ssh_key_path) is delivered via GIT_SSH_COMMAND — an
//     inline ssh_key is written to a 0600 temp file for the duration of the
//     call and removed on return (defer); ssh_key_path is used as-is.
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

type gitPlugin struct{}

func (gitPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "git",
		Desc: "git: clone/init/fetch/pull/push, working-tree verbs (checkout, switch, add, commit, status, diff, log), branch/tag/remote/stash, merge/rebase/reset, rev_parse/ls_remote/show, and a cli escape hatch. Configurable per-connection SSH key or HTTPS token credentials, materialized safely per invocation. Shells out to the git CLI.",
		Connection: plugin.Schema{
			"dir":                      {Type: "string", Desc: "working directory for every invocation (git -C equivalent via cmd.Dir)"},
			"binary":                   {Type: "string", Desc: "override the git binary path (default git)"},
			"ssh_key":                  {Type: "string", Desc: "inline SSH private key (PEM). Written to a 0600 temp file for the call and removed on return."},
			"ssh_key_path":             {Type: "string", Desc: "path to an existing SSH private key file (used as-is; takes precedence over ssh_key)"},
			"strict_host_key_checking": {Type: "string", Enum: []string{"yes", "no", "accept-new"}, Desc: "ssh -o StrictHostKeyChecking=<value>"},
			"known_hosts":              {Type: "string", Desc: "ssh -o UserKnownHostsFile=<path>"},
			"username":                 {Type: "string", Desc: "HTTPS credential username; -c credential.username=<username> (paired with token)"},
			"token":                    {Type: "string", Desc: "HTTPS credential token/password, delivered via a GIT_ASKPASS temp script — never placed in argv"},
			"user_name":                {Type: "string", Desc: "-c user.name=<value>"},
			"user_email":               {Type: "string", Desc: "-c user.email=<value>"},
			"config":                   {Type: "map", Desc: "additional -c key=value flags (sorted), applied before every subcommand"},
			"env":                      {Type: "map", Desc: "default process environment for every invocation"},
			"timeout":                  {Type: "duration", Desc: "default per-verb timeout (default 10m); a verb's timeout option overrides it"},
		},
		Verbs: gitVerbs(),
		Capabilities: plugin.Capabilities{
			Commands: []string{"git", "ssh"},
			Spawns:   true,
			// No declared egress: git's own transport (the remote URL / SSH
			// host) reaches the network on the default, un-isolated path —
			// same reasoning as the docker connector's docker_host. Under OS
			// isolation a connector instance's `network:` can only narrow.
			Egress: []string{},
		},
	}
}

// stdOutputs is the request-response shape every verb shares: a non-zero exit
// is DATA (the caller inspects exit_code), not an invocation error.
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

func gitVerbs() []plugin.Verb {
	out := stdOutputs()
	return []plugin.Verb{
		{
			Name: "clone", Desc: "clone a repository",
			Options: plugin.Schema{
				"url":           {Type: "string", Required: true, Scope: "repo"},
				"dir":           {Type: "string", Desc: "destination directory"},
				"branch":        {Type: "string", Desc: "--branch"},
				"depth":         {Type: "integer", Desc: "--depth"},
				"single_branch": {Type: "boolean", Desc: "--single-branch"},
				"recursive":     {Type: "boolean", Desc: "--recursive"},
				"bare":          {Type: "boolean", Desc: "--bare"},
			},
			Outputs: out,
		},
		{
			Name: "init", Desc: "create a new repository",
			Options: plugin.Schema{
				"dir":  {Type: "string", Desc: "directory to initialize (default .)"},
				"bare": {Type: "boolean", Desc: "--bare"},
			},
			Outputs: out,
		},
		{
			Name: "fetch", Desc: "download objects and refs from a remote",
			Options: plugin.Schema{
				"remote":  {Type: "string", Scope: "repo"},
				"refspec": {Type: "string"},
				"all":     {Type: "boolean", Desc: "--all"},
				"prune":   {Type: "boolean", Desc: "--prune"},
				"tags":    {Type: "boolean", Desc: "--tags"},
				"depth":   {Type: "integer", Desc: "--depth"},
			},
			Outputs: out,
		},
		{
			Name: "pull", Desc: "fetch and integrate a remote branch",
			Options: plugin.Schema{
				"remote":  {Type: "string", Scope: "repo"},
				"branch":  {Type: "string"},
				"rebase":  {Type: "boolean", Desc: "--rebase"},
				"ff_only": {Type: "boolean", Desc: "--ff-only"},
			},
			Outputs: out,
		},
		{
			Name: "push", Desc: "update remote refs",
			Options: plugin.Schema{
				"remote":           {Type: "string", Scope: "repo"},
				"refspec":          {Type: "string", Desc: "refspec or branch to push (alias: branch)"},
				"branch":           {Type: "string", Desc: "alias for refspec"},
				"tags":             {Type: "boolean", Desc: "--tags"},
				"force":            {Type: "boolean", Desc: "--force"},
				"force_with_lease": {Type: "boolean", Desc: "--force-with-lease (takes precedence over force)"},
				"set_upstream":     {Type: "boolean", Desc: "-u"},
				"delete":           {Type: "boolean", Desc: "--delete"},
			},
			Outputs: out,
		},
		{
			Name: "checkout", Desc: "switch branches or restore working-tree files",
			Options: plugin.Schema{
				"ref":    {Type: "string", Required: true},
				"create": {Type: "boolean", Desc: "-b create a new branch named ref"},
				"force":  {Type: "boolean", Desc: "-f"},
				"track":  {Type: "boolean", Desc: "--track"},
			},
			Outputs: out,
		},
		{
			Name: "switch", Desc: "switch branches",
			Options: plugin.Schema{
				"ref":    {Type: "string", Required: true},
				"create": {Type: "boolean", Desc: "-c create a new branch named ref"},
			},
			Outputs: out,
		},
		{
			Name: "add", Desc: "stage file contents",
			Options: plugin.Schema{
				"paths": {Type: "list", Desc: "paths to stage"},
				"all":   {Type: "boolean", Desc: "-A stage everything"},
			},
			Outputs: out,
		},
		{
			Name: "commit", Desc: "record staged changes",
			Options: plugin.Schema{
				"message":     {Type: "string", Desc: "-m (required unless amend)"},
				"all":         {Type: "boolean", Desc: "-a"},
				"allow_empty": {Type: "boolean", Desc: "--allow-empty"},
				"amend":       {Type: "boolean", Desc: "--amend"},
				"author":      {Type: "string", Desc: "--author"},
			},
			Outputs: out,
		},
		{
			Name: "status",
			Desc: "show working-tree status (parsed into files[] and branch)",
			Outputs: withOutputs(plugin.Schema{
				"files":  {Type: "list", Desc: "[{status, path}]"},
				"branch": {Type: "string"},
			}),
		},
		{
			Name: "log", Desc: "show commit history",
			Options: plugin.Schema{
				"max_count": {Type: "integer", Desc: "-n"},
				"oneline":   {Type: "boolean", Desc: "--oneline"},
				"format":    {Type: "string", Desc: "--format="},
				"since":     {Type: "string", Desc: "--since="},
				"paths":     {Type: "list", Desc: "-- <paths> to restrict history to"},
			},
			Outputs: out,
		},
		{
			Name: "diff", Desc: "show changes",
			Options: plugin.Schema{
				"cached":    {Type: "boolean", Desc: "--cached"},
				"name_only": {Type: "boolean", Desc: "--name-only"},
				"stat":      {Type: "boolean", Desc: "--stat"},
				"paths":     {Type: "list", Desc: "-- <paths> to restrict the diff to"},
			},
			Outputs: out,
		},
		{
			Name: "branch", Desc: "list, create, delete, or rename branches",
			Options: plugin.Schema{
				"subcommand":  {Type: "string", Required: true, Enum: []string{"list", "create", "delete", "rename"}},
				"name":        {Type: "string", Desc: "branch name (create/delete/rename)"},
				"new_name":    {Type: "string", Desc: "new branch name (rename)"},
				"start_point": {Type: "string", Desc: "starting commit/branch (create)"},
				"force":       {Type: "boolean", Desc: "-D instead of -d (delete)"},
			},
			Outputs: out,
		},
		{
			Name: "tag", Desc: "list, create, or delete tags",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Required: true, Enum: []string{"list", "create", "delete"}},
				"name":       {Type: "string", Desc: "tag name (create/delete)"},
				"message":    {Type: "string", Desc: "-m (create, makes it annotated)"},
				"ref":        {Type: "string", Desc: "commit to tag (create, default HEAD)"},
				"force":      {Type: "boolean", Desc: "-f (create)"},
			},
			Outputs: out,
		},
		{
			Name: "merge", Desc: "join two or more development histories",
			Options: plugin.Schema{
				"ref":     {Type: "string", Required: true},
				"no_ff":   {Type: "boolean", Desc: "--no-ff"},
				"ff_only": {Type: "boolean", Desc: "--ff-only"},
				"message": {Type: "string", Desc: "-m"},
			},
			Outputs: out,
		},
		{
			Name: "rebase", Desc: "reapply commits on top of another base",
			Options: plugin.Schema{
				"upstream":   {Type: "string", Desc: "required unless subcommand is abort/continue"},
				"onto":       {Type: "string", Desc: "--onto"},
				"subcommand": {Type: "string", Enum: []string{"abort", "continue"}},
			},
			Outputs: out,
		},
		{
			Name: "reset", Desc: "reset current HEAD to a state",
			Options: plugin.Schema{
				"mode": {Type: "string", Enum: []string{"soft", "mixed", "hard"}, Desc: "--<mode>"},
				"ref":  {Type: "string"},
			},
			Outputs: out,
		},
		{
			Name:    "rev_parse",
			Desc:    "resolve a ref to a SHA (parsed into sha)",
			Options: plugin.Schema{"ref": {Type: "string", Required: true}},
			Outputs: withOutputs(plugin.Schema{"sha": {Type: "string"}}),
		},
		{
			Name:    "ls_remote",
			Desc:    "list references of a remote (parsed into refs[])",
			Options: plugin.Schema{"remote_or_url": {Type: "string", Required: true, Scope: "repo"}},
			Outputs: withOutputs(plugin.Schema{"refs": {Type: "list", Desc: "[{sha, ref}]"}}),
		},
		{
			Name: "remote", Desc: "manage tracked remotes",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Required: true, Enum: []string{"add", "remove", "set_url", "list"}},
				"name":       {Type: "string", Desc: "remote name (add/remove/set_url)"},
				"url":        {Type: "string", Desc: "remote URL (add/set_url)"},
			},
			Outputs: out,
		},
		{
			Name:    "config_get",
			Desc:    "read a git config value",
			Options: plugin.Schema{"key": {Type: "string", Required: true}},
			Outputs: out,
		},
		{
			Name:    "config_set",
			Desc:    "write a git config value",
			Options: plugin.Schema{"key": {Type: "string", Required: true}, "value": {Type: "string", Required: true}},
			Outputs: out,
		},
		{
			Name: "stash", Desc: "stash working-tree changes",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Enum: []string{"push", "pop", "list", "drop", "apply", "show"}, Desc: "default push"},
				"message":    {Type: "string", Desc: "-m (push)"},
			},
			Outputs: out,
		},
		{
			Name: "clean", Desc: "remove untracked files from the working tree",
			Options: plugin.Schema{
				"force":   {Type: "boolean", Desc: "-f"},
				"dirs":    {Type: "boolean", Desc: "-d"},
				"dry_run": {Type: "boolean", Desc: "-n (takes precedence over force)"},
			},
			Outputs: out,
		},
		{
			Name: "show", Desc: "show an object (commit, tag, ...)",
			Options: plugin.Schema{
				"ref":    {Type: "string"},
				"format": {Type: "string", Desc: "--format="},
			},
			Outputs: out,
		},
		{
			Name:    "cli",
			Desc:    "run any git subcommand: git <args…>",
			Usage:   "escape hatch for a subcommand without a first-class verb",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after the binary and -c flags"}},
			Outputs: out,
		},
	}
}

// gitConn is the resolved connection config for one invocation.
type gitConn struct {
	dir                   string
	binary                string
	sshKeyPath            string
	sshKeyInline          string
	strictHostKeyChecking string
	knownHosts            string
	username              string
	token                 string
	userName              string
	userEmail             string
	config                map[string]string
	env                   map[string]string
	timeout               time.Duration
}

func parseConn(m map[string]any) (gitConn, error) {
	c := gitConn{
		dir:                   str(m["dir"]),
		binary:                strOr(m["binary"], "git"),
		sshKeyPath:            str(m["ssh_key_path"]),
		sshKeyInline:          str(m["ssh_key"]),
		strictHostKeyChecking: str(m["strict_host_key_checking"]),
		knownHosts:            str(m["known_hosts"]),
		username:              str(m["username"]),
		token:                 str(m["token"]),
		userName:              str(m["user_name"]),
		userEmail:             str(m["user_email"]),
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

// globalConfigFlags builds the `-c key=value` flags that PRECEDE the
// subcommand: identity (user.name/user.email) then the arbitrary config map,
// sorted for deterministic argv. Pure and hermetically testable.
func globalConfigFlags(userName, userEmail string, config map[string]string) []string {
	var a []string
	if userName != "" {
		a = append(a, "-c", "user.name="+userName)
	}
	if userEmail != "" {
		a = append(a, "-c", "user.email="+userEmail)
	}
	keys := make([]string, 0, len(config))
	for k := range config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-c", k+"="+config[k])
	}
	return a
}

// sshCommand renders the GIT_SSH_COMMAND value for a materialized key file
// plus the optional host-key options. Pure — takes a resolved key path, never
// touches the filesystem. The value is parsed by a shell, so the key/known-
// hosts paths are single-quoted.
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

func (gitPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	args, err := verbArgs(req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	// Materialize credentials for THIS invocation only; always cleaned up.
	var cleanup []string
	defer func() {
		for _, p := range cleanup {
			os.Remove(p)
		}
	}()

	extraEnv := map[string]string{}
	configFlags := globalConfigFlags(conn.userName, conn.userEmail, conn.config)

	sshKeyPath := conn.sshKeyPath
	if sshKeyPath == "" && conn.sshKeyInline != "" {
		f, werr := writeTempFile("conductor-git-key-*", conn.sshKeyInline, 0o600)
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
			configFlags = append(configFlags, "-c", "credential.username="+conn.username)
		}
	}

	full := make([]string, 0, len(configFlags)+len(args))
	full = append(full, configFlags...)
	full = append(full, args...)

	timeout := conn.timeout
	if d, derr := toDuration(o["timeout"]); derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": options.timeout: "+derr.Error())
	} else if d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := runGit(ctx, conn.binary, conn.dir, procEnv(conn.env, extraEnv), full)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	enrich(req.Verb, o, res)
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary and the `-c` config flags. Pure
// and hermetically testable — no process is spawned here.
func verbArgs(verb string, o map[string]any) ([]string, error) {
	switch verb {
	case "clone":
		return cloneArgs(o)
	case "init":
		return initArgs(o)
	case "fetch":
		return fetchArgs(o)
	case "pull":
		return pullArgs(o)
	case "push":
		return pushArgs(o)
	case "checkout":
		return checkoutArgs(o)
	case "switch":
		return switchArgs(o)
	case "add":
		return addArgs(o)
	case "commit":
		return commitArgs(o)
	case "status":
		return statusArgs(o), nil
	case "log":
		return logArgs(o), nil
	case "diff":
		return diffArgs(o), nil
	case "branch":
		return branchArgs(o)
	case "tag":
		return tagArgs(o)
	case "merge":
		return mergeArgs(o)
	case "rebase":
		return rebaseArgs(o)
	case "reset":
		return resetArgs(o), nil
	case "rev_parse":
		return revParseArgs(o)
	case "ls_remote":
		return lsRemoteArgs(o)
	case "remote":
		return remoteArgs(o)
	case "config_get":
		return configGetArgs(o)
	case "config_set":
		return configSetArgs(o)
	case "stash":
		return stashArgs(o)
	case "clean":
		return cleanArgs(o), nil
	case "show":
		return showArgs(o), nil
	case "cli":
		return cliArgs(o)
	}
	return nil, fmt.Errorf("unknown verb")
}

func cloneArgs(o map[string]any) ([]string, error) {
	url := str(o["url"])
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}
	a := []string{"clone"}
	if boolv(o["bare"]) {
		a = append(a, "--bare")
	}
	if b := str(o["branch"]); b != "" {
		a = append(a, "--branch", b)
	}
	if d := intStr(o["depth"]); d != "" {
		a = append(a, "--depth", d)
	}
	if boolv(o["single_branch"]) {
		a = append(a, "--single-branch")
	}
	if boolv(o["recursive"]) {
		a = append(a, "--recursive")
	}
	a = append(a, url)
	if dir := str(o["dir"]); dir != "" {
		a = append(a, dir)
	}
	return a, nil
}

func initArgs(o map[string]any) ([]string, error) {
	a := []string{"init"}
	if boolv(o["bare"]) {
		a = append(a, "--bare")
	}
	if dir := str(o["dir"]); dir != "" {
		a = append(a, dir)
	}
	return a, nil
}

func fetchArgs(o map[string]any) ([]string, error) {
	a := []string{"fetch"}
	if boolv(o["all"]) {
		a = append(a, "--all")
	}
	if boolv(o["prune"]) {
		a = append(a, "--prune")
	}
	if boolv(o["tags"]) {
		a = append(a, "--tags")
	}
	if d := intStr(o["depth"]); d != "" {
		a = append(a, "--depth", d)
	}
	if r := str(o["remote"]); r != "" {
		a = append(a, r)
		if rs := str(o["refspec"]); rs != "" {
			a = append(a, rs)
		}
	}
	return a, nil
}

func pullArgs(o map[string]any) ([]string, error) {
	a := []string{"pull"}
	if boolv(o["rebase"]) {
		a = append(a, "--rebase")
	}
	if boolv(o["ff_only"]) {
		a = append(a, "--ff-only")
	}
	if r := str(o["remote"]); r != "" {
		a = append(a, r)
		if b := str(o["branch"]); b != "" {
			a = append(a, b)
		}
	}
	return a, nil
}

func pushArgs(o map[string]any) ([]string, error) {
	a := []string{"push"}
	if boolv(o["force_with_lease"]) {
		a = append(a, "--force-with-lease")
	} else if boolv(o["force"]) {
		a = append(a, "--force")
	}
	if boolv(o["set_upstream"]) {
		a = append(a, "-u")
	}
	if boolv(o["delete"]) {
		a = append(a, "--delete")
	}
	if boolv(o["tags"]) {
		a = append(a, "--tags")
	}
	if r := str(o["remote"]); r != "" {
		a = append(a, r)
		ref := str(o["refspec"])
		if ref == "" {
			ref = str(o["branch"])
		}
		if ref != "" {
			a = append(a, ref)
		}
	}
	return a, nil
}

func checkoutArgs(o map[string]any) ([]string, error) {
	ref := str(o["ref"])
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	a := []string{"checkout"}
	if boolv(o["force"]) {
		a = append(a, "-f")
	}
	if boolv(o["track"]) {
		a = append(a, "--track")
	}
	if boolv(o["create"]) {
		a = append(a, "-b")
	}
	a = append(a, ref)
	return a, nil
}

func switchArgs(o map[string]any) ([]string, error) {
	ref := str(o["ref"])
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	a := []string{"switch"}
	if boolv(o["create"]) {
		a = append(a, "-c")
	}
	a = append(a, ref)
	return a, nil
}

func addArgs(o map[string]any) ([]string, error) {
	a := []string{"add"}
	if boolv(o["all"]) {
		return append(a, "-A"), nil
	}
	paths := strList(o["paths"])
	if len(paths) == 0 {
		return nil, fmt.Errorf("paths is required (or set all)")
	}
	return append(a, paths...), nil
}

func commitArgs(o map[string]any) ([]string, error) {
	a := []string{"commit"}
	if boolv(o["all"]) {
		a = append(a, "-a")
	}
	if boolv(o["amend"]) {
		a = append(a, "--amend")
	}
	if boolv(o["allow_empty"]) {
		a = append(a, "--allow-empty")
	}
	if author := str(o["author"]); author != "" {
		a = append(a, "--author", author)
	}
	msg := str(o["message"])
	if msg == "" && !boolv(o["amend"]) {
		return nil, fmt.Errorf("message is required")
	}
	if msg != "" {
		a = append(a, "-m", msg)
	}
	return a, nil
}

func statusArgs(o map[string]any) []string {
	return []string{"status", "--porcelain=v1", "-b"}
}

func logArgs(o map[string]any) []string {
	a := []string{"log"}
	if n := intStr(o["max_count"]); n != "" {
		a = append(a, "-n", n)
	}
	if boolv(o["oneline"]) {
		a = append(a, "--oneline")
	}
	if f := str(o["format"]); f != "" {
		a = append(a, "--format="+f)
	}
	if s := str(o["since"]); s != "" {
		a = append(a, "--since="+s)
	}
	if paths := strList(o["paths"]); len(paths) > 0 {
		a = append(a, "--")
		a = append(a, paths...)
	}
	return a
}

func diffArgs(o map[string]any) []string {
	a := []string{"diff"}
	if boolv(o["cached"]) {
		a = append(a, "--cached")
	}
	if boolv(o["name_only"]) {
		a = append(a, "--name-only")
	}
	if boolv(o["stat"]) {
		a = append(a, "--stat")
	}
	if paths := strList(o["paths"]); len(paths) > 0 {
		a = append(a, "--")
		a = append(a, paths...)
	}
	return a
}

func branchArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	name := str(o["name"])
	switch sub {
	case "list":
		return []string{"branch", "--list"}, nil
	case "create":
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		a := []string{"branch", name}
		if sp := str(o["start_point"]); sp != "" {
			a = append(a, sp)
		}
		return a, nil
	case "delete":
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		flag := "-d"
		if boolv(o["force"]) {
			flag = "-D"
		}
		return []string{"branch", flag, name}, nil
	case "rename":
		newName := str(o["new_name"])
		if newName == "" {
			return nil, fmt.Errorf("new_name is required")
		}
		a := []string{"branch", "-m"}
		if name != "" {
			a = append(a, name)
		}
		a = append(a, newName)
		return a, nil
	}
	return nil, fmt.Errorf("subcommand must be list, create, delete, or rename")
}

func tagArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	name := str(o["name"])
	switch sub {
	case "list":
		return []string{"tag", "--list"}, nil
	case "create":
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		a := []string{"tag"}
		if boolv(o["force"]) {
			a = append(a, "-f")
		}
		if m := str(o["message"]); m != "" {
			a = append(a, "-m", m)
		}
		a = append(a, name)
		if ref := str(o["ref"]); ref != "" {
			a = append(a, ref)
		}
		return a, nil
	case "delete":
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		return []string{"tag", "-d", name}, nil
	}
	return nil, fmt.Errorf("subcommand must be list, create, or delete")
}

func mergeArgs(o map[string]any) ([]string, error) {
	ref := str(o["ref"])
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	a := []string{"merge"}
	if boolv(o["no_ff"]) {
		a = append(a, "--no-ff")
	}
	if boolv(o["ff_only"]) {
		a = append(a, "--ff-only")
	}
	if m := str(o["message"]); m != "" {
		a = append(a, "-m", m)
	}
	a = append(a, ref)
	return a, nil
}

func rebaseArgs(o map[string]any) ([]string, error) {
	switch str(o["subcommand"]) {
	case "abort":
		return []string{"rebase", "--abort"}, nil
	case "continue":
		return []string{"rebase", "--continue"}, nil
	case "":
		upstream := str(o["upstream"])
		if upstream == "" {
			return nil, fmt.Errorf("upstream is required")
		}
		a := []string{"rebase"}
		if onto := str(o["onto"]); onto != "" {
			a = append(a, "--onto", onto)
		}
		a = append(a, upstream)
		return a, nil
	}
	return nil, fmt.Errorf("subcommand must be abort or continue")
}

func resetArgs(o map[string]any) []string {
	a := []string{"reset"}
	if mode := str(o["mode"]); mode != "" {
		a = append(a, "--"+mode)
	}
	if ref := str(o["ref"]); ref != "" {
		a = append(a, ref)
	}
	return a
}

func revParseArgs(o map[string]any) ([]string, error) {
	ref := str(o["ref"])
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	return []string{"rev-parse", ref}, nil
}

func lsRemoteArgs(o map[string]any) ([]string, error) {
	target := str(o["remote_or_url"])
	if target == "" {
		return nil, fmt.Errorf("remote_or_url is required")
	}
	return []string{"ls-remote", target}, nil
}

func remoteArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	name := str(o["name"])
	url := str(o["url"])
	switch sub {
	case "list":
		return []string{"remote", "-v"}, nil
	case "add":
		if name == "" || url == "" {
			return nil, fmt.Errorf("name and url are required")
		}
		return []string{"remote", "add", name, url}, nil
	case "remove":
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		return []string{"remote", "remove", name}, nil
	case "set_url":
		if name == "" || url == "" {
			return nil, fmt.Errorf("name and url are required")
		}
		return []string{"remote", "set-url", name, url}, nil
	}
	return nil, fmt.Errorf("subcommand must be add, remove, set_url, or list")
}

func configGetArgs(o map[string]any) ([]string, error) {
	key := str(o["key"])
	if key == "" {
		return nil, fmt.Errorf("key is required")
	}
	return []string{"config", "--get", key}, nil
}

func configSetArgs(o map[string]any) ([]string, error) {
	key := str(o["key"])
	value := str(o["value"])
	if key == "" {
		return nil, fmt.Errorf("key is required")
	}
	if value == "" {
		return nil, fmt.Errorf("value is required")
	}
	return []string{"config", key, value}, nil
}

func stashArgs(o map[string]any) ([]string, error) {
	sub := strOr(o["subcommand"], "push")
	switch sub {
	case "push":
		a := []string{"stash", "push"}
		if m := str(o["message"]); m != "" {
			a = append(a, "-m", m)
		}
		return a, nil
	case "pop", "list", "drop", "apply", "show":
		return []string{"stash", sub}, nil
	}
	return nil, fmt.Errorf("subcommand must be push, pop, list, drop, apply, or show")
}

func cleanArgs(o map[string]any) []string {
	a := []string{"clean"}
	if boolv(o["dry_run"]) {
		a = append(a, "-n")
	} else if boolv(o["force"]) {
		a = append(a, "-f")
	}
	if boolv(o["dirs"]) {
		a = append(a, "-d")
	}
	return a
}

func showArgs(o map[string]any) []string {
	a := []string{"show"}
	if f := str(o["format"]); f != "" {
		a = append(a, "--format="+f)
	}
	if ref := str(o["ref"]); ref != "" {
		a = append(a, ref)
	}
	return a
}

func cliArgs(o map[string]any) ([]string, error) {
	args := strList(o["args"])
	if len(args) == 0 {
		return nil, fmt.Errorf("args is required")
	}
	return args, nil
}

// enrich adds verb-specific structured outputs, only when the command
// succeeded (exit_code 0) so we never parse an error stream.
func enrich(verb string, o, res map[string]any) {
	if res["exit_code"] != 0 {
		return
	}
	stdout, _ := res["stdout"].(string)
	switch verb {
	case "status":
		files, branch := parseStatusPorcelain(stdout)
		res["files"] = files
		res["branch"] = branch
	case "rev_parse":
		res["sha"] = strings.TrimSpace(stdout)
	case "ls_remote":
		res["refs"] = parseLsRemote(stdout)
	}
}

// parseStatusPorcelain parses `git status --porcelain=v1 -b` output into the
// current branch name and a list of {status, path} entries.
func parseStatusPorcelain(s string) ([]map[string]any, string) {
	branch := ""
	files := []map[string]any{}
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			b := strings.TrimPrefix(line, "## ")
			if i := strings.Index(b, "..."); i >= 0 {
				b = b[:i]
			}
			if i := strings.Index(b, " ["); i >= 0 {
				b = b[:i]
			}
			branch = b
			continue
		}
		if len(line) < 3 {
			continue
		}
		files = append(files, map[string]any{"status": line[:2], "path": strings.TrimSpace(line[3:])})
	}
	return files, branch
}

// parseLsRemote parses `git ls-remote` output (tab-separated "sha\tref" lines)
// into a list of {sha, ref} entries.
func parseLsRemote(s string) []map[string]any {
	out := []map[string]any{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		out = append(out, map[string]any{"sha": parts[0], "ref": parts[1]})
	}
	return out
}

// runGit spawns the git binary with the full argv (config flags + verb args)
// and captures the result. A non-zero exit is returned as exit_code, not an
// error; only a failure to start the process (missing binary, timeout) is an
// error. No shell is involved — exec.CommandContext runs the binary directly.
func runGit(ctx context.Context, binary, dir string, env, args []string) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
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
// returning its path. The caller is responsible for removing it.
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

// writeAskpass writes a GIT_ASKPASS helper script that prints the token to
// stdout, regardless of the prompt it is invoked with — safe because
// credential.username (when set) already answers the username prompt and
// GIT_TERMINAL_PROMPT=0 disables any other interactive fallback. The token
// never appears in argv or the process list.
func writeAskpass(token string) (string, error) {
	script := "#!/bin/sh\nprintf '%s\\n' " + shQuote(token) + "\n"
	return writeTempFile("conductor-git-askpass-*", script, 0o700)
}

// procEnv builds the child process environment: the parent's environment,
// then the connection's env map (sorted), then invocation-specific overrides
// (GIT_SSH_COMMAND, GIT_ASKPASS, ...) also sorted, so later entries win.
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
	if err := plugin.Serve(gitPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-git: %v\n", err)
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

// intStr renders an integer-ish option as a string flag value ("" if absent).
func intStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%d", int64(x))
	case string:
		return x
	}
	return fmt.Sprintf("%v", v)
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

// strList accepts a string, []string, or []any and returns a non-empty slice.
func strList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s := fmt.Sprintf("%v", e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// toDuration parses a timeout option: a Go duration string ("10m"), or a
// number interpreted as seconds. Zero/absent → 0 (use the default).
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

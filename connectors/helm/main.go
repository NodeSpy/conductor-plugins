// Command conductor-helm is a verb-only conductor connector that drives Helm 3
// releases by shelling out to the `helm` CLI. It exposes the release lifecycle
// as verbs — install/upgrade/uninstall/rollback/list/status/history/
// get_values/template/pull/repo_add/repo_update/test/lint — plus a `cli`
// escape hatch for any subcommand a first-class verb does not cover. Built
// ONLY against the public SDK.
//
// Cluster access is reached through Helm's OWN kubeconfig plumbing rather
// than any re-implementation: set `kubeconfig: /path/to/config` and/or
// `kube_context: <name>`. `namespace` scopes every invocation with `-n`.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type helmPlugin struct{}

func (helmPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "helm",
		Desc: "Helm 3: install, upgrade, and manage releases as verbs (install, upgrade, uninstall, rollback, list, status, history, get_values, template, pull, repo_add, repo_update, test, lint, cli). kubeconfig/kube_context/namespace scope every invocation. Shells out to the helm CLI.",
		Connection: plugin.Schema{
			"kubeconfig":   {Type: "string", Desc: "--kubeconfig path"},
			"kube_context": {Type: "string", Desc: "--kube-context name"},
			"namespace":    {Type: "string", Desc: "-n namespace"},
			"binary":       {Type: "string", Desc: "override the CLI binary path (default helm)"},
			"env":          {Type: "map", Desc: "default process environment for every invocation"},
			"timeout":      {Type: "duration", Desc: "default per-verb timeout (default 10m); a verb's timeout option overrides it"},
		},
		Verbs:        helmVerbs(),
		Capabilities: plugin.Capabilities{Commands: []string{"helm"}, Spawns: true},
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

// chartOptions are the install/upgrade/template shared release options.
func chartOptions() plugin.Schema {
	return plugin.Schema{
		"name":             {Type: "string", Required: true},
		"chart":            {Type: "string", Required: true},
		"version":          {Type: "string", Desc: "--version"},
		"values":           {Type: "list", Desc: "-f value file(s), repeated"},
		"set":              {Type: "any", Desc: "--set overrides: map (key=value, sorted) or list of raw KEY=VALUE"},
		"set_string":       {Type: "any", Desc: "--set-string overrides: map (sorted) or list of raw KEY=VALUE"},
		"create_namespace": {Type: "boolean", Desc: "--create-namespace"},
		"wait":             {Type: "boolean", Desc: "--wait"},
		"atomic":           {Type: "boolean", Desc: "--atomic"},
		"dry_run":          {Type: "boolean", Desc: "--dry-run"},
		"timeout":          {Type: "duration", Desc: "--timeout (Helm's own wait timeout, distinct from the process timeout)"},
		"repo":             {Type: "string", Desc: "--repo chart repository URL"},
	}
}

func helmVerbs() []plugin.Verb {
	out := stdOutputs()
	return []plugin.Verb{
		{
			Name: "install", Desc: "install a chart as a new release",
			Usage:   "helm install <name> <chart> [flags]",
			Options: chartOptions(),
			Outputs: out,
		},
		{
			Name: "upgrade", Desc: "upgrade a release, installing it first if it doesn't exist",
			Usage: "helm upgrade <name> <chart> [flags]",
			Options: func() plugin.Schema {
				s := chartOptions()
				s["install"] = plugin.Field{Type: "boolean", Desc: "--install (upgrade --install)"}
				s["reuse_values"] = plugin.Field{Type: "boolean", Desc: "--reuse-values"}
				s["force"] = plugin.Field{Type: "boolean", Desc: "--force"}
				return s
			}(),
			Outputs: out,
		},
		{
			Name: "uninstall", Desc: "uninstall a release",
			Options: plugin.Schema{
				"name":         {Type: "string", Required: true},
				"keep_history": {Type: "boolean", Desc: "--keep-history"},
				"wait":         {Type: "boolean", Desc: "--wait"},
				"timeout":      {Type: "duration", Desc: "--timeout"},
			},
			Outputs: out,
		},
		{
			Name: "rollback", Desc: "roll back a release to a previous revision",
			Options: plugin.Schema{
				"name":     {Type: "string", Required: true},
				"revision": {Type: "integer", Desc: "revision to roll back to (positional); omit for the previous one"},
				"wait":     {Type: "boolean", Desc: "--wait"},
				"timeout":  {Type: "duration", Desc: "--timeout"},
			},
			Outputs: out,
		},
		{
			Name: "list", Desc: "list releases (parsed into releases[])",
			Options: plugin.Schema{
				"all":            {Type: "boolean", Desc: "-a include uninstalled/failed"},
				"all_namespaces": {Type: "boolean", Desc: "-A across every namespace"},
				"filter":         {Type: "string", Desc: "--filter regex on release name"},
			},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "releases": {Type: "list"}},
		},
		{
			Name: "status", Desc: "show the status of a release (parsed into status)",
			Options: plugin.Schema{
				"name":     {Type: "string", Required: true},
				"revision": {Type: "integer", Desc: "--revision"},
			},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "status": {Type: "any"}},
		},
		{
			Name: "history", Desc: "show a release's revision history (parsed into history[])",
			Options: plugin.Schema{"name": {Type: "string", Required: true}},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "history": {Type: "list"}},
		},
		{
			Name: "get_values", Desc: "fetch a release's values (parsed into values)",
			Options: plugin.Schema{
				"name": {Type: "string", Required: true},
				"all":  {Type: "boolean", Desc: "-a include computed defaults"},
			},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "values": {Type: "any"}},
		},
		{
			Name: "template", Desc: "render chart templates locally without installing",
			Options: plugin.Schema{
				"name":      {Type: "string", Required: true},
				"chart":     {Type: "string", Required: true},
				"values":    {Type: "list", Desc: "-f value file(s), repeated"},
				"set":       {Type: "any", Desc: "--set overrides: map (sorted) or list of raw KEY=VALUE"},
				"version":   {Type: "string", Desc: "--version"},
				"show_only": {Type: "list", Desc: "-s only render these templates"},
			},
			Outputs: out,
		},
		{
			Name: "pull", Desc: "download a chart to the local filesystem",
			Options: plugin.Schema{
				"chart":       {Type: "string", Required: true},
				"version":     {Type: "string", Desc: "--version"},
				"destination": {Type: "string", Desc: "-d destination directory"},
				"untar":       {Type: "boolean", Desc: "--untar"},
				"repo":        {Type: "string", Desc: "--repo chart repository URL"},
			},
			Outputs: out,
		},
		{
			Name: "repo_add", Desc: "add a chart repository",
			Usage: "helm repo add <name> <url> [flags]",
			Options: plugin.Schema{
				"name":         {Type: "string", Required: true},
				"url":          {Type: "string", Required: true},
				"username":     {Type: "string", Desc: "--username"},
				"password":     {Type: "string", Desc: "--password"},
				"force_update": {Type: "boolean", Desc: "--force-update"},
			},
			Outputs: out,
		},
		{
			Name: "repo_update", Desc: "update the local chart repository cache",
			Usage:   "helm repo update [name…]",
			Options: plugin.Schema{"names": {Type: "list", Desc: "repository name(s) to update; omit to update all"}},
			Outputs: out,
		},
		{
			Name: "test", Desc: "run a release's Helm tests",
			Options: plugin.Schema{
				"name":    {Type: "string", Required: true},
				"timeout": {Type: "duration", Desc: "--timeout"},
			},
			Outputs: out,
		},
		{
			Name: "lint", Desc: "lint a chart directory for issues",
			Options: plugin.Schema{
				"chart":  {Type: "string", Required: true, Desc: "chart path (positional)"},
				"values": {Type: "list", Desc: "-f value file(s), repeated"},
				"strict": {Type: "boolean", Desc: "--strict"},
			},
			Outputs: out,
		},
		{
			Name: "cli", Desc: "run any helm subcommand: helm <args…>",
			Usage:   "escape hatch for a subcommand without a first-class verb",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after the binary"}},
			Outputs: out,
		},
	}
}

// helmConn is the resolved connection config for one invocation.
type helmConn struct {
	binary      string
	kubeconfig  string
	kubeContext string
	namespace   string
	env         map[string]string
	timeout     time.Duration
}

func parseConn(m map[string]any) (helmConn, error) {
	c := helmConn{
		kubeconfig:  str(m["kubeconfig"]),
		kubeContext: str(m["kube_context"]),
		namespace:   str(m["namespace"]),
		binary:      strOr(m["binary"], "helm"),
		env:         strMap(m["env"]),
		timeout:     10 * time.Minute,
	}
	if d, err := toDuration(m["timeout"]); err != nil {
		return c, fmt.Errorf("connection.timeout: %w", err)
	} else if d > 0 {
		c.timeout = d
	}
	return c, nil
}

// connFlags are the top-level helm flags that precede any subcommand.
func (c helmConn) connFlags() []string {
	var a []string
	if c.kubeconfig != "" {
		a = append(a, "--kubeconfig", c.kubeconfig)
	}
	if c.kubeContext != "" {
		a = append(a, "--kube-context", c.kubeContext)
	}
	if c.namespace != "" {
		a = append(a, "-n", c.namespace)
	}
	return a
}

func (c helmConn) procEnv() []string {
	env := os.Environ()
	keys := make([]string, 0, len(c.env))
	for k := range c.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+c.env[k])
	}
	return env
}

func (helmPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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

	timeout := conn.timeout
	if d, derr := toDuration(o["timeout"]); derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": options.timeout: "+derr.Error())
	} else if d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := runHelm(ctx, conn, args)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	enrich(req.Verb, o, res)
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary and the connection flags. Pure and
// hermetically testable — no process is spawned here.
func verbArgs(verb string, o map[string]any) ([]string, error) {
	switch verb {
	case "install":
		return installArgs(o)
	case "upgrade":
		return upgradeArgs(o)
	case "uninstall":
		return uninstallArgs(o)
	case "rollback":
		return rollbackArgs(o)
	case "list":
		return listArgs(o), nil
	case "status":
		return statusArgs(o)
	case "history":
		return historyArgs(o)
	case "get_values":
		return getValuesArgs(o)
	case "template":
		return templateArgs(o)
	case "pull":
		return pullArgs(o)
	case "repo_add":
		return repoAddArgs(o)
	case "repo_update":
		return append([]string{"repo", "update"}, strList(o["names"])...), nil
	case "test":
		return testArgs(o)
	case "lint":
		return lintArgs(o)
	case "cli":
		args := strList(o["args"])
		if len(args) == 0 {
			return nil, fmt.Errorf("args is required")
		}
		return args, nil
	}
	return nil, fmt.Errorf("unknown verb")
}

// chartFlags renders the install/upgrade/template flags shared by all three:
// version, values files, set/set_string overrides, repo. Positional
// name/chart are appended by the caller.
func chartFlags(o map[string]any) []string {
	var a []string
	if v := str(o["version"]); v != "" {
		a = append(a, "--version", v)
	}
	a = append(a, repeatFlag("-f", strList(o["values"]))...)
	a = append(a, setFlags("--set", o["set"])...)
	a = append(a, setFlags("--set-string", o["set_string"])...)
	if r := str(o["repo"]); r != "" {
		a = append(a, "--repo", r)
	}
	return a
}

func installArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	chart := str(o["chart"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if chart == "" {
		return nil, fmt.Errorf("chart is required")
	}
	a := []string{"install", name, chart}
	if boolv(o["create_namespace"]) {
		a = append(a, "--create-namespace")
	}
	if boolv(o["wait"]) {
		a = append(a, "--wait")
	}
	if boolv(o["atomic"]) {
		a = append(a, "--atomic")
	}
	if boolv(o["dry_run"]) {
		a = append(a, "--dry-run")
	}
	if t := str(o["timeout"]); t != "" {
		a = append(a, "--timeout", t)
	}
	a = append(a, chartFlags(o)...)
	return a, nil
}

func upgradeArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	chart := str(o["chart"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if chart == "" {
		return nil, fmt.Errorf("chart is required")
	}
	a := []string{"upgrade", name, chart}
	if boolv(o["install"]) {
		a = append(a, "--install")
	}
	if boolv(o["reuse_values"]) {
		a = append(a, "--reuse-values")
	}
	if boolv(o["force"]) {
		a = append(a, "--force")
	}
	if boolv(o["create_namespace"]) {
		a = append(a, "--create-namespace")
	}
	if boolv(o["wait"]) {
		a = append(a, "--wait")
	}
	if boolv(o["atomic"]) {
		a = append(a, "--atomic")
	}
	if boolv(o["dry_run"]) {
		a = append(a, "--dry-run")
	}
	if t := str(o["timeout"]); t != "" {
		a = append(a, "--timeout", t)
	}
	a = append(a, chartFlags(o)...)
	return a, nil
}

func uninstallArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a := []string{"uninstall", name}
	if boolv(o["keep_history"]) {
		a = append(a, "--keep-history")
	}
	if boolv(o["wait"]) {
		a = append(a, "--wait")
	}
	if t := str(o["timeout"]); t != "" {
		a = append(a, "--timeout", t)
	}
	return a, nil
}

// rollbackArgs handles `rollback` — the release name, then an optional
// positional revision, then flags.
func rollbackArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a := []string{"rollback", name}
	if r := intStr(o["revision"]); r != "" {
		a = append(a, r)
	}
	if boolv(o["wait"]) {
		a = append(a, "--wait")
	}
	if t := str(o["timeout"]); t != "" {
		a = append(a, "--timeout", t)
	}
	return a, nil
}

func listArgs(o map[string]any) []string {
	a := []string{"list"}
	if boolv(o["all"]) {
		a = append(a, "-a")
	}
	if boolv(o["all_namespaces"]) {
		a = append(a, "-A")
	}
	if f := str(o["filter"]); f != "" {
		a = append(a, "--filter", f)
	}
	return append(a, "-o", "json")
}

func statusArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a := []string{"status", name}
	if r := intStr(o["revision"]); r != "" {
		a = append(a, "--revision", r)
	}
	return append(a, "-o", "json"), nil
}

func historyArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	return []string{"history", name, "-o", "json"}, nil
}

func getValuesArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a := []string{"get", "values", name}
	if boolv(o["all"]) {
		a = append(a, "-a")
	}
	return append(a, "-o", "json"), nil
}

func templateArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	chart := str(o["chart"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if chart == "" {
		return nil, fmt.Errorf("chart is required")
	}
	a := []string{"template", name, chart}
	a = append(a, repeatFlag("-f", strList(o["values"]))...)
	a = append(a, setFlags("--set", o["set"])...)
	if v := str(o["version"]); v != "" {
		a = append(a, "--version", v)
	}
	a = append(a, repeatFlag("-s", strList(o["show_only"]))...)
	return a, nil
}

func pullArgs(o map[string]any) ([]string, error) {
	chart := str(o["chart"])
	if chart == "" {
		return nil, fmt.Errorf("chart is required")
	}
	a := []string{"pull", chart}
	if v := str(o["version"]); v != "" {
		a = append(a, "--version", v)
	}
	if d := str(o["destination"]); d != "" {
		a = append(a, "-d", d)
	}
	if boolv(o["untar"]) {
		a = append(a, "--untar")
	}
	if r := str(o["repo"]); r != "" {
		a = append(a, "--repo", r)
	}
	return a, nil
}

func repoAddArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	url := str(o["url"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}
	a := []string{"repo", "add", name, url}
	if u := str(o["username"]); u != "" {
		a = append(a, "--username", u)
	}
	if p := str(o["password"]); p != "" {
		a = append(a, "--password", p)
	}
	if boolv(o["force_update"]) {
		a = append(a, "--force-update")
	}
	return a, nil
}

func testArgs(o map[string]any) ([]string, error) {
	name := str(o["name"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a := []string{"test", name}
	if t := str(o["timeout"]); t != "" {
		a = append(a, "--timeout", t)
	}
	return a, nil
}

func lintArgs(o map[string]any) ([]string, error) {
	chart := str(o["chart"])
	if chart == "" {
		return nil, fmt.Errorf("chart is required")
	}
	a := []string{"lint", chart}
	a = append(a, repeatFlag("-f", strList(o["values"]))...)
	if boolv(o["strict"]) {
		a = append(a, "--strict")
	}
	return a, nil
}

// enrich adds verb-specific structured outputs, only when the command
// succeeded (exit_code 0) so we never parse an error stream.
func enrich(verb string, o, res map[string]any) {
	ok := res["exit_code"] == 0
	if !ok {
		return
	}
	stdout, _ := res["stdout"].(string)
	switch verb {
	case "list":
		if v, ok := jsonAny(stdout).([]any); ok {
			out := make([]map[string]any, 0, len(v))
			for _, e := range v {
				if m, ok := e.(map[string]any); ok {
					out = append(out, m)
				}
			}
			res["releases"] = out
		}
	case "status":
		if v := jsonAny(stdout); v != nil {
			res["status"] = v
		}
	case "history":
		if v, ok := jsonAny(stdout).([]any); ok {
			out := make([]map[string]any, 0, len(v))
			for _, e := range v {
				if m, ok := e.(map[string]any); ok {
					out = append(out, m)
				}
			}
			res["history"] = out
		}
	case "get_values":
		if v := jsonAny(stdout); v != nil {
			res["values"] = v
		}
	}
}

// runHelm spawns the helm binary with connFlags + args and captures the
// result. A non-zero exit is returned as exit_code, not an error; only a
// failure to start the process (missing binary, timeout) is an error.
func runHelm(ctx context.Context, conn helmConn, args []string) (map[string]any, error) {
	full := append(conn.connFlags(), args...)
	cmd := exec.CommandContext(ctx, conn.binary, full...)
	cmd.Env = conn.procEnv()
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

func main() {
	if err := plugin.Serve(helmPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-helm: %v\n", err)
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
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatInt(int64(x), 10)
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

// repeatFlag emits `flag val` for each value.
func repeatFlag(flag string, vals []string) []string {
	if len(vals) == 0 {
		return nil
	}
	out := make([]string, 0, len(vals)*2)
	for _, v := range vals {
		out = append(out, flag, v)
	}
	return out
}

// kvFlags emits `flag KEY=VALUE` for each map entry, keys sorted for
// deterministic argv.
func kvFlags(flag string, v any) []string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out, flag, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return out
}

// setFlags renders --set/--set-string overrides from a map (KEY=VALUE,
// sorted) or a list of raw KEY=VALUE strings, like docker bake's setFlags.
func setFlags(flag string, v any) []string {
	switch v.(type) {
	case map[string]any:
		return kvFlags(flag, v)
	default:
		return repeatFlag(flag, strList(v))
	}
}

// jsonAny parses a JSON document (object or array) into a generic value; nil
// on failure so a caller can leave the output unset.
func jsonAny(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
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

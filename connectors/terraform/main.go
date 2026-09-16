// Command conductor-terraform is a verb-only conductor connector that drives
// Terraform (and its drop-in siblings OpenTofu and Terragrunt) runs by
// shelling out to the corresponding CLI. It exposes the standard workflow as
// verbs — init/validate/plan/apply/destroy/output/show/fmt/workspace/state/
// import/refresh/providers/version — plus a `cli` escape hatch for any
// subcommand a first-class verb does not cover. Built ONLY against the
// public SDK.
//
// The connection's `engine` selects which CLI is driven: "terraform"
// (default) or "tofu" — both 100% argv-compatible, including the GLOBAL
// `-chdir=DIR` flag, which must precede the subcommand
// (`terraform -chdir=DIR plan ...`) and is emitted as a connFlag ahead of
// every verb's argv, exactly like docker's --context — or "terragrunt",
// which has no -chdir flag at all: chdir is instead applied as the spawned
// process's working directory (cmd.Dir). Terragrunt also understands a
// `run_all` option on plan/apply/destroy/refresh/output/init, prefixing the
// subcommand with `run-all`.
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
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type terraformPlugin struct{}

func (terraformPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "terraform",
		Desc: "Terraform / OpenTofu / Terragrunt: run the standard workflow as verbs (init, validate, plan, apply, destroy, output, show, fmt, workspace, state, import, refresh, providers, version, cli). Shells out to the selected engine's CLI; -chdir targets a working directory for terraform/tofu, while terragrunt (which has no -chdir) runs with its process working directory set instead.",
		Connection: plugin.Schema{
			"engine":  {Type: "string", Enum: []string{"terraform", "tofu", "terragrunt"}, Desc: "which CLI to drive (default terraform); tofu is argv-identical to terraform, terragrunt has no -chdir and instead runs with cmd.Dir set to chdir"},
			"chdir":   {Type: "string", Desc: "working directory — for terraform/tofu, emitted as the global -chdir=<dir> flag before the subcommand; for terragrunt, used as the spawned process's working directory (no -chdir flag exists)"},
			"binary":  {Type: "string", Desc: "override the CLI binary path (default: the engine name — terraform, tofu, or terragrunt)"},
			"env":     {Type: "map", Desc: "process environment for every invocation, e.g. TF_VAR_*, AWS_*"},
			"timeout": {Type: "duration", Desc: "default per-verb timeout (default 10m); a verb's timeout option overrides it"},
		},
		Verbs:        terraformVerbs(),
		Capabilities: plugin.Capabilities{Commands: []string{"terraform"}, Spawns: true},
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

func terraformVerbs() []plugin.Verb {
	out := stdOutputs()
	withOutputs := plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "outputs": {Type: "any", Desc: "parsed -json output, when json is used"}}
	withResult := plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "result": {Type: "any", Desc: "parsed -json output"}}
	return []plugin.Verb{
		{
			Name: "init", Desc: "initialize a working directory (backend, providers, modules)",
			Options: plugin.Schema{
				"backend_config": {Type: "list", Desc: "-backend-config=<path-or-key=value> (repeatable)"},
				"upgrade":        {Type: "boolean", Desc: "-upgrade"},
				"reconfigure":    {Type: "boolean", Desc: "-reconfigure"},
				"no_color":       {Type: "boolean", Desc: "-no-color"},
				"run_all":        {Type: "boolean", Desc: "terragrunt only: prefix the subcommand with run-all"},
			},
			Outputs: out,
		},
		{
			Name: "validate", Desc: "validate the configuration in the working directory",
			Options: plugin.Schema{
				"json":     {Type: "boolean", Desc: "-json"},
				"no_color": {Type: "boolean", Desc: "-no-color"},
			},
			Outputs: out,
		},
		{
			Name: "plan", Desc: "compute an execution plan",
			Usage: "always runs with -input=false (non-interactive)",
			Options: plugin.Schema{
				"out":               {Type: "string", Desc: "-out=<path> save the plan"},
				"var":               {Type: "map", Desc: "-var 'k=v' (keys sorted)"},
				"var_files":         {Type: "list", Desc: "-var-file=<path> (repeatable)"},
				"target":            {Type: "list", Desc: "-target=<addr> (repeatable)"},
				"destroy":           {Type: "boolean", Desc: "-destroy"},
				"refresh_only":      {Type: "boolean", Desc: "-refresh-only"},
				"detailed_exitcode": {Type: "boolean", Desc: "-detailed-exitcode"},
				"json":              {Type: "boolean", Desc: "-json"},
				"no_color":          {Type: "boolean", Desc: "-no-color"},
				"run_all":           {Type: "boolean", Desc: "terragrunt only: prefix the subcommand with run-all"},
			},
			Outputs: out,
		},
		{
			Name: "apply", Desc: "apply a plan or the current configuration",
			Usage: "auto_approve defaults to true; always runs with -input=false",
			Options: plugin.Schema{
				"plan_file":    {Type: "string", Desc: "a saved plan file (positional)"},
				"auto_approve": {Type: "boolean", Desc: "-auto-approve (default true)"},
				"var":          {Type: "map", Desc: "-var 'k=v' (keys sorted)"},
				"var_files":    {Type: "list", Desc: "-var-file=<path> (repeatable)"},
				"target":       {Type: "list", Desc: "-target=<addr> (repeatable)"},
				"json":         {Type: "boolean", Desc: "-json"},
				"no_color":     {Type: "boolean", Desc: "-no-color"},
				"run_all":      {Type: "boolean", Desc: "terragrunt only: prefix the subcommand with run-all"},
			},
			Outputs: out,
		},
		{
			Name: "destroy", Desc: "destroy all resources managed by the configuration",
			Usage: "auto_approve defaults to true; always runs with -input=false",
			Options: plugin.Schema{
				"auto_approve": {Type: "boolean", Desc: "-auto-approve (default true)"},
				"var":          {Type: "map", Desc: "-var 'k=v' (keys sorted)"},
				"var_files":    {Type: "list", Desc: "-var-file=<path> (repeatable)"},
				"target":       {Type: "list", Desc: "-target=<addr> (repeatable)"},
				"json":         {Type: "boolean", Desc: "-json"},
				"no_color":     {Type: "boolean", Desc: "-no-color"},
				"run_all":      {Type: "boolean", Desc: "terragrunt only: prefix the subcommand with run-all"},
			},
			Outputs: out,
		},
		{
			Name: "output", Desc: "read an output value from the current state (parsed into outputs)",
			Options: plugin.Schema{
				"name":    {Type: "string", Desc: "a single output name (positional)"},
				"json":    {Type: "boolean", Desc: "-json (default true)"},
				"raw":     {Type: "boolean", Desc: "-raw (mutually exclusive with json)"},
				"run_all": {Type: "boolean", Desc: "terragrunt only: prefix the subcommand with run-all"},
			},
			Outputs: withOutputs,
		},
		{
			Name: "show", Desc: "show a state or plan file in human/JSON form (parsed into result)",
			Options: plugin.Schema{
				"path": {Type: "string", Desc: "a state or plan file (positional)"},
				"json": {Type: "boolean", Desc: "-json (default true)"},
			},
			Outputs: withResult,
		},
		{
			Name: "fmt", Desc: "rewrite configuration files to canonical style",
			Options: plugin.Schema{
				"check":     {Type: "boolean", Desc: "-check"},
				"diff":      {Type: "boolean", Desc: "-diff"},
				"recursive": {Type: "boolean", Desc: "-recursive"},
				"write":     {Type: "boolean", Desc: "-write=false when set to false (default true)"},
			},
			Outputs: out,
		},
		{
			Name: "workspace", Desc: "manage terraform workspaces",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Required: true, Enum: []string{"list", "select", "new", "delete", "show"}},
				"name":       {Type: "string", Desc: "workspace name — required for select/new/delete (positional)"},
			},
			Outputs: out,
		},
		{
			Name: "state", Desc: "advanced state management",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Required: true, Enum: []string{"list", "show", "rm", "mv", "pull", "push"}},
				"args":       {Type: "list", Desc: "trailing positional arguments for the subcommand"},
			},
			Outputs: out,
		},
		{
			Name: "import", Desc: "import an existing resource into the state",
			Options: plugin.Schema{
				"address":   {Type: "string", Required: true, Desc: "resource address (positional)"},
				"id":        {Type: "string", Required: true, Desc: "provider-specific resource id (positional)"},
				"var":       {Type: "map", Desc: "-var 'k=v' (keys sorted)"},
				"var_files": {Type: "list", Desc: "-var-file=<path> (repeatable)"},
			},
			Outputs: out,
		},
		{
			Name: "refresh", Desc: "reconcile state with real infrastructure",
			Usage: "always runs with -input=false",
			Options: plugin.Schema{
				"var":       {Type: "map", Desc: "-var 'k=v' (keys sorted)"},
				"var_files": {Type: "list", Desc: "-var-file=<path> (repeatable)"},
				"target":    {Type: "list", Desc: "-target=<addr> (repeatable)"},
				"no_color":  {Type: "boolean", Desc: "-no-color"},
				"run_all":   {Type: "boolean", Desc: "terragrunt only: prefix the subcommand with run-all"},
			},
			Outputs: out,
		},
		{
			Name: "providers", Desc: "print the provider dependency tree",
			Options: plugin.Schema{},
			Outputs: out,
		},
		{
			Name: "version", Desc: "print the terraform and provider versions",
			Options: plugin.Schema{"json": {Type: "boolean", Desc: "-json"}},
			Outputs: out,
		},
		{
			Name: "cli", Desc: "run any terraform subcommand: terraform -chdir=<dir> <args…>",
			Usage:   "escape hatch for a subcommand without a first-class verb",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after -chdir"}},
			Outputs: out,
		},
	}
}

// terraformConn is the resolved connection config for one invocation.
type terraformConn struct {
	engine  string
	binary  string
	chdir   string
	env     map[string]string
	timeout time.Duration
}

func parseConn(m map[string]any) (terraformConn, error) {
	engine := strOr(m["engine"], "terraform")
	switch engine {
	case "terraform", "tofu", "terragrunt":
	default:
		return terraformConn{}, fmt.Errorf("connection.engine: invalid value %q (want terraform, tofu, or terragrunt)", engine)
	}
	c := terraformConn{
		engine:  engine,
		binary:  strOr(m["binary"], engine),
		chdir:   str(m["chdir"]),
		env:     strMap(m["env"]),
		timeout: 10 * time.Minute,
	}
	if d, err := toDuration(m["timeout"]); err != nil {
		return c, fmt.Errorf("connection.timeout: %w", err)
	} else if d > 0 {
		c.timeout = d
	}
	return c, nil
}

// connFlags is the GLOBAL -chdir flag, which must precede the subcommand.
// terragrunt has no -chdir flag at all: its chdir is instead applied as the
// spawned process's working directory (see runTerraform).
func (c terraformConn) connFlags() []string {
	if c.chdir != "" && c.engine != "terragrunt" {
		return []string{"-chdir=" + c.chdir}
	}
	return nil
}

func (c terraformConn) procEnv() []string {
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

func (terraformPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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

	res, err := runTerraform(ctx, conn, args)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	enrich(req.Verb, o, res)
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary and the -chdir connection flag.
// Pure and hermetically testable — no process is spawned here.
func verbArgs(verb string, o map[string]any) ([]string, error) {
	switch verb {
	case "init":
		return initArgs(o), nil
	case "validate":
		return validateArgs(o), nil
	case "plan":
		return planArgs(o), nil
	case "apply":
		return applyArgs(o), nil
	case "destroy":
		return destroyArgs(o), nil
	case "output":
		return outputArgs(o), nil
	case "show":
		return showArgs(o), nil
	case "fmt":
		return fmtArgs(o), nil
	case "workspace":
		return workspaceArgs(o)
	case "state":
		return stateArgs(o)
	case "import":
		return importArgs(o)
	case "refresh":
		return refreshArgs(o), nil
	case "providers":
		return []string{"providers"}, nil
	case "version":
		return versionArgs(o), nil
	case "cli":
		args := strList(o["args"])
		if len(args) == 0 {
			return nil, fmt.Errorf("args is required")
		}
		return args, nil
	}
	return nil, fmt.Errorf("unknown verb")
}

// runAllPrefix prepends "run-all" to a subcommand's argv when run_all is set
// — a terragrunt-only option (terragrunt run-all <subcommand> ...); other
// engines simply never set it.
func runAllPrefix(o map[string]any, a []string) []string {
	if boolv(o["run_all"]) {
		return append([]string{"run-all"}, a...)
	}
	return a
}

func initArgs(o map[string]any) []string {
	a := []string{"init"}
	a = append(a, eqFlags("-backend-config=", strList(o["backend_config"]))...)
	if boolv(o["upgrade"]) {
		a = append(a, "-upgrade")
	}
	if boolv(o["reconfigure"]) {
		a = append(a, "-reconfigure")
	}
	if boolv(o["no_color"]) {
		a = append(a, "-no-color")
	}
	return runAllPrefix(o, a)
}

func validateArgs(o map[string]any) []string {
	a := []string{"validate"}
	if boolv(o["json"]) {
		a = append(a, "-json")
	}
	if boolv(o["no_color"]) {
		a = append(a, "-no-color")
	}
	return a
}

// planFlags builds the flags shared by plan/apply/destroy/refresh: var,
// var_files, target, json, no_color. input=false is always appended by the
// caller since every one of these runs non-interactively.
func planFlags(o map[string]any) []string {
	var a []string
	a = append(a, varFlags(o["var"])...)
	a = append(a, eqFlags("-var-file=", strList(o["var_files"]))...)
	a = append(a, eqFlags("-target=", strList(o["target"]))...)
	return a
}

func planArgs(o map[string]any) []string {
	a := []string{"plan"}
	if out := str(o["out"]); out != "" {
		a = append(a, "-out="+out)
	}
	a = append(a, planFlags(o)...)
	if boolv(o["destroy"]) {
		a = append(a, "-destroy")
	}
	if boolv(o["refresh_only"]) {
		a = append(a, "-refresh-only")
	}
	if boolv(o["detailed_exitcode"]) {
		a = append(a, "-detailed-exitcode")
	}
	if boolv(o["json"]) {
		a = append(a, "-json")
	}
	if boolv(o["no_color"]) {
		a = append(a, "-no-color")
	}
	a = append(a, "-input=false")
	return runAllPrefix(o, a)
}

func applyArgs(o map[string]any) []string {
	a := []string{"apply"}
	if boolOr(o["auto_approve"], true) {
		a = append(a, "-auto-approve")
	}
	a = append(a, planFlags(o)...)
	if boolv(o["json"]) {
		a = append(a, "-json")
	}
	if boolv(o["no_color"]) {
		a = append(a, "-no-color")
	}
	a = append(a, "-input=false")
	if pf := str(o["plan_file"]); pf != "" {
		a = append(a, pf)
	}
	return runAllPrefix(o, a)
}

func destroyArgs(o map[string]any) []string {
	a := []string{"destroy"}
	if boolOr(o["auto_approve"], true) {
		a = append(a, "-auto-approve")
	}
	a = append(a, planFlags(o)...)
	if boolv(o["json"]) {
		a = append(a, "-json")
	}
	if boolv(o["no_color"]) {
		a = append(a, "-no-color")
	}
	a = append(a, "-input=false")
	return runAllPrefix(o, a)
}

func outputArgs(o map[string]any) []string {
	a := []string{"output"}
	if boolv(o["raw"]) {
		a = append(a, "-raw")
	} else if boolOr(o["json"], true) {
		a = append(a, "-json")
	}
	if n := str(o["name"]); n != "" {
		a = append(a, n)
	}
	return runAllPrefix(o, a)
}

func showArgs(o map[string]any) []string {
	a := []string{"show"}
	if boolOr(o["json"], true) {
		a = append(a, "-json")
	}
	if p := str(o["path"]); p != "" {
		a = append(a, p)
	}
	return a
}

func fmtArgs(o map[string]any) []string {
	a := []string{"fmt"}
	if boolv(o["check"]) {
		a = append(a, "-check")
	}
	if boolv(o["diff"]) {
		a = append(a, "-diff")
	}
	if boolv(o["recursive"]) {
		a = append(a, "-recursive")
	}
	if !boolOr(o["write"], true) {
		a = append(a, "-write=false")
	}
	return a
}

func workspaceArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	if sub == "" {
		return nil, fmt.Errorf("subcommand is required")
	}
	a := []string{"workspace", sub}
	name := str(o["name"])
	switch sub {
	case "select", "new", "delete":
		if name == "" {
			return nil, fmt.Errorf("name is required for workspace %s", sub)
		}
		a = append(a, name)
	default:
		if name != "" {
			a = append(a, name)
		}
	}
	return a, nil
}

func stateArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	if sub == "" {
		return nil, fmt.Errorf("subcommand is required")
	}
	a := []string{"state", sub}
	a = append(a, strList(o["args"])...)
	return a, nil
}

func importArgs(o map[string]any) ([]string, error) {
	addr := str(o["address"])
	id := str(o["id"])
	if addr == "" {
		return nil, fmt.Errorf("address is required")
	}
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	a := []string{"import"}
	a = append(a, varFlags(o["var"])...)
	a = append(a, eqFlags("-var-file=", strList(o["var_files"]))...)
	a = append(a, addr, id)
	return a, nil
}

func refreshArgs(o map[string]any) []string {
	a := []string{"refresh"}
	a = append(a, planFlags(o)...)
	if boolv(o["no_color"]) {
		a = append(a, "-no-color")
	}
	a = append(a, "-input=false")
	return runAllPrefix(o, a)
}

func versionArgs(o map[string]any) []string {
	a := []string{"version"}
	if boolv(o["json"]) {
		a = append(a, "-json")
	}
	return a
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
	case "output":
		if !boolv(o["raw"]) && boolOr(o["json"], true) {
			if v := jsonAny(stdout); v != nil {
				res["outputs"] = v
			}
		}
	case "show":
		if boolOr(o["json"], true) {
			if v := jsonAny(stdout); v != nil {
				res["result"] = v
			}
		}
	}
}

// runTerraform spawns the terraform binary with connFlags + args and
// captures the result. A non-zero exit is returned as exit_code, not an
// error; only a failure to start the process (missing binary, timeout) is an
// error.
func runTerraform(ctx context.Context, conn terraformConn, args []string) (map[string]any, error) {
	full := append(conn.connFlags(), args...)
	cmd := exec.CommandContext(ctx, conn.binary, full...)
	if conn.engine == "terragrunt" && conn.chdir != "" {
		cmd.Dir = conn.chdir
	}
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
	if err := plugin.Serve(terraformPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-terraform: %v\n", err)
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

// boolOr returns d when v is unset (nil), otherwise the parsed bool — used
// for options that default to true (auto_approve, output/show's json, fmt's
// write).
func boolOr(v any, d bool) bool {
	if v == nil {
		return d
	}
	return boolv(v)
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

// eqFlags emits a single `flag+val` token per value (flag already includes
// the trailing "="), e.g. -var-file=, -target=, -backend-config=.
func eqFlags(flag string, vals []string) []string {
	if len(vals) == 0 {
		return nil
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, flag+v)
	}
	return out
}

// varFlags emits `-var KEY=VALUE` for each map entry, keys sorted for
// deterministic argv.
func varFlags(v any) []string {
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
		out = append(out, "-var", fmt.Sprintf("%s=%v", k, m[k]))
	}
	return out
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

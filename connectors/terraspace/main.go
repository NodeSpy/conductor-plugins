// Command conductor-terraspace is a verb-only conductor connector that drives
// Terraspace (https://terraspace.cloud) by shelling out to the `terraspace`
// CLI. Terraspace wraps Terraform/OpenTofu with a per-STACK command model —
// most commands take the form `terraspace <cmd> <STACK> [flags]` — plus an
// `all` prefix that repeats a command across every stack
// (`terraspace all <cmd>`). It exposes that workflow as verbs — up/down/plan/
// all_up/all_down/all_plan/output/logs/list/new/import/console/state/build/
// clean/fmt/validate/test/info — plus a `cli` escape hatch for any subcommand
// a first-class verb does not cover. Built ONLY against the public SDK.
//
// The target environment is selected by the TS_ENV environment variable, NOT
// a CLI flag — the connection's `ts_env` sets it for every invocation.
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
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type terraspacePlugin struct{}

func (terraspacePlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "terraspace",
		Desc: "Terraspace: drive the per-stack Terraform/OpenTofu workflow as verbs (up, down, plan, all_up, all_down, all_plan, output, logs, list, new, import, console, state, build, clean, fmt, validate, test, info, cli). Shells out to the terraspace CLI; TS_ENV selects the environment.",
		Connection: plugin.Schema{
			"binary":  {Type: "string", Desc: "override the CLI binary path (default terraspace)"},
			"dir":     {Type: "string", Desc: "working directory for the spawned process (a Terraspace project root)"},
			"ts_env":  {Type: "string", Desc: "sets TS_ENV for every invocation — selects the terraspace environment"},
			"env":     {Type: "map", Desc: "extra process environment for every invocation"},
			"timeout": {Type: "duration", Desc: "default per-verb timeout (default 10m); a verb's timeout option overrides it"},
		},
		Verbs:        terraspaceVerbs(),
		Capabilities: plugin.Capabilities{Commands: []string{"terraspace", "terraform", "tofu"}, Spawns: true, Egress: []string{}},
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

func terraspaceVerbs() []plugin.Verb {
	out := stdOutputs()
	return []plugin.Verb{
		{
			Name: "up", Desc: "provision a stack",
			Usage: "yes defaults to true (non-interactive -y); extra_args are appended last",
			Options: plugin.Schema{
				"stack":      {Type: "string", Required: true, Scope: "stack"},
				"yes":        {Type: "boolean", Desc: "-y (default true)"},
				"extra_args": {Type: "list", Desc: "raw flags appended after -y"},
			},
			Outputs: out,
		},
		{
			Name: "down", Desc: "tear down a stack",
			Usage: "yes defaults to true (non-interactive -y); extra_args are appended last",
			Options: plugin.Schema{
				"stack":      {Type: "string", Required: true, Scope: "stack"},
				"yes":        {Type: "boolean", Desc: "-y (default true)"},
				"extra_args": {Type: "list", Desc: "raw flags appended after -y"},
			},
			Outputs: out,
		},
		{
			Name: "plan", Desc: "compute an execution plan for a stack",
			Options: plugin.Schema{
				"stack":      {Type: "string", Required: true, Scope: "stack"},
				"out":        {Type: "string", Desc: "--out <path> save the plan"},
				"extra_args": {Type: "list", Desc: "raw flags appended after --out"},
			},
			Outputs: out,
		},
		{
			Name: "all_up", Desc: "provision every stack",
			Usage: "yes defaults to true (non-interactive -y); extra_args are appended last",
			Options: plugin.Schema{
				"yes":        {Type: "boolean", Desc: "-y (default true)"},
				"extra_args": {Type: "list", Desc: "raw flags appended after -y"},
			},
			Outputs: out,
		},
		{
			Name: "all_down", Desc: "tear down every stack",
			Usage: "yes defaults to true (non-interactive -y); extra_args are appended last",
			Options: plugin.Schema{
				"yes":        {Type: "boolean", Desc: "-y (default true)"},
				"extra_args": {Type: "list", Desc: "raw flags appended after -y"},
			},
			Outputs: out,
		},
		{
			Name: "all_plan", Desc: "compute an execution plan for every stack",
			Options: plugin.Schema{
				"extra_args": {Type: "list", Desc: "raw flags appended last"},
			},
			Outputs: out,
		},
		{
			Name: "output", Desc: "read a stack's output value(s)",
			Options: plugin.Schema{
				"stack": {Type: "string", Required: true, Scope: "stack"},
				"name":  {Type: "string", Desc: "a single output name (positional, optional)"},
			},
			Outputs: out,
		},
		{
			Name: "logs", Desc: "fetch a stack's (or all stacks') logs",
			Options: plugin.Schema{
				"stack":  {Type: "string", Scope: "stack", Desc: "optional; omit for every stack"},
				"follow": {Type: "boolean", Desc: "-f"},
			},
			Outputs: out,
		},
		{
			Name: "list", Desc: "list all stacks",
			Options: plugin.Schema{},
			Outputs: out,
		},
		{
			Name: "new", Desc: "scaffold a new module, project, or stack",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Required: true, Desc: "e.g. module, project, stack"},
				"name":       {Type: "string", Required: true},
				"extra_args": {Type: "list", Desc: "raw flags appended last"},
			},
			Outputs: out,
		},
		{
			Name: "import", Desc: "import an existing resource into a stack's state",
			Options: plugin.Schema{
				"stack":   {Type: "string", Required: true, Scope: "stack"},
				"address": {Type: "string", Required: true, Desc: "resource address (positional)"},
				"id":      {Type: "string", Required: true, Desc: "provider-specific resource id (positional)"},
			},
			Outputs: out,
		},
		{
			Name: "console", Desc: "open an interactive console for a stack",
			Options: plugin.Schema{
				"stack": {Type: "string", Required: true, Scope: "stack"},
			},
			Outputs: out,
		},
		{
			Name: "state", Desc: "advanced state management for a stack",
			Options: plugin.Schema{
				"stack": {Type: "string", Required: true, Scope: "stack"},
				"args":  {Type: "list", Desc: "trailing positional arguments, e.g. [list], [show, addr]"},
			},
			Outputs: out,
		},
		{
			Name: "build", Desc: "build the Terraform/OpenTofu configuration for a stack (or all stacks) without applying",
			Options: plugin.Schema{
				"stack": {Type: "string", Scope: "stack", Desc: "optional; omit for every stack"},
			},
			Outputs: out,
		},
		{
			Name: "clean", Desc: "remove generated build artifacts",
			Options: plugin.Schema{
				"target": {Type: "string", Enum: []string{"cache", "all", "logs"}, Desc: "optional; defaults to terraspace's own default"},
			},
			Outputs: out,
		},
		{
			Name: "fmt", Desc: "rewrite configuration files to canonical style",
			Options: plugin.Schema{},
			Outputs: out,
		},
		{
			Name: "validate", Desc: "validate a stack's configuration",
			Options: plugin.Schema{
				"stack": {Type: "string", Required: true, Scope: "stack"},
			},
			Outputs: out,
		},
		{
			Name: "test", Desc: "run the project's test suite",
			Options: plugin.Schema{},
			Outputs: out,
		},
		{
			Name: "info", Desc: "print project/environment info",
			Options: plugin.Schema{},
			Outputs: out,
		},
		{
			Name: "cli", Desc: "run any terraspace subcommand: terraspace <args…>",
			Usage:   "escape hatch for a subcommand without a first-class verb",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv"}},
			Outputs: out,
		},
	}
}

// terraspaceConn is the resolved connection config for one invocation.
type terraspaceConn struct {
	binary  string
	dir     string
	tsEnv   string
	env     map[string]string
	timeout time.Duration
}

func parseConn(m map[string]any) (terraspaceConn, error) {
	c := terraspaceConn{
		binary:  strOr(m["binary"], "terraspace"),
		dir:     str(m["dir"]),
		tsEnv:   str(m["ts_env"]),
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

// procEnv builds the spawned process's environment: the parent's environ,
// plus the connection's env map (sorted for determinism), plus TS_ENV — which
// is how Terraspace selects its target environment (there is no CLI flag).
func (c terraspaceConn) procEnv() []string {
	env := os.Environ()
	keys := make([]string, 0, len(c.env))
	for k := range c.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+c.env[k])
	}
	if c.tsEnv != "" {
		env = append(env, "TS_ENV="+c.tsEnv)
	}
	return env
}

func (terraspacePlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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

	res, err := runTerraspace(ctx, conn, args)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary. Pure and hermetically
// testable — no process is spawned here.
func verbArgs(verb string, o map[string]any) ([]string, error) {
	switch verb {
	case "up":
		return stackCmdArgs("up", o)
	case "down":
		return stackCmdArgs("down", o)
	case "plan":
		return planArgs(o)
	case "all_up":
		return allCmdArgs("up", o), nil
	case "all_down":
		return allCmdArgs("down", o), nil
	case "all_plan":
		return append([]string{"all", "plan"}, strList(o["extra_args"])...), nil
	case "output":
		return outputArgs(o)
	case "logs":
		return logsArgs(o), nil
	case "list":
		return []string{"list"}, nil
	case "new":
		return newArgs(o)
	case "import":
		return importArgs(o)
	case "console":
		return consoleArgs(o)
	case "state":
		return stateArgs(o)
	case "build":
		return buildArgs(o), nil
	case "clean":
		return cleanArgs(o), nil
	case "fmt":
		return []string{"fmt"}, nil
	case "validate":
		return validateArgs(o)
	case "test":
		return []string{"test"}, nil
	case "info":
		return []string{"info"}, nil
	case "cli":
		args := strList(o["args"])
		if len(args) == 0 {
			return nil, fmt.Errorf("args is required")
		}
		return args, nil
	}
	return nil, fmt.Errorf("unknown verb")
}

// stackCmdArgs builds `<cmd> <stack> [-y] [extra_args…]` — shared by up/down.
// yes defaults to true (non-interactive); extra_args are appended last.
func stackCmdArgs(cmd string, o map[string]any) ([]string, error) {
	stack := str(o["stack"])
	if stack == "" {
		return nil, fmt.Errorf("stack is required")
	}
	a := []string{cmd, stack}
	if boolOr(o["yes"], true) {
		a = append(a, "-y")
	}
	a = append(a, strList(o["extra_args"])...)
	return a, nil
}

// allCmdArgs builds `all <cmd> [-y] [extra_args…]` — shared by all_up/all_down.
func allCmdArgs(cmd string, o map[string]any) []string {
	a := []string{"all", cmd}
	if boolOr(o["yes"], true) {
		a = append(a, "-y")
	}
	a = append(a, strList(o["extra_args"])...)
	return a
}

func planArgs(o map[string]any) ([]string, error) {
	stack := str(o["stack"])
	if stack == "" {
		return nil, fmt.Errorf("stack is required")
	}
	a := []string{"plan", stack}
	if out := str(o["out"]); out != "" {
		a = append(a, "--out", out)
	}
	a = append(a, strList(o["extra_args"])...)
	return a, nil
}

func outputArgs(o map[string]any) ([]string, error) {
	stack := str(o["stack"])
	if stack == "" {
		return nil, fmt.Errorf("stack is required")
	}
	a := []string{"output", stack}
	if n := str(o["name"]); n != "" {
		a = append(a, n)
	}
	return a, nil
}

// logsArgs builds `logs [stack] [-f]`; stack is optional (omit for every
// stack's logs).
func logsArgs(o map[string]any) []string {
	a := []string{"logs"}
	if stack := str(o["stack"]); stack != "" {
		a = append(a, stack)
	}
	if boolv(o["follow"]) {
		a = append(a, "-f")
	}
	return a
}

func newArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	if sub == "" {
		return nil, fmt.Errorf("subcommand is required")
	}
	name := str(o["name"])
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a := []string{"new", sub, name}
	a = append(a, strList(o["extra_args"])...)
	return a, nil
}

func importArgs(o map[string]any) ([]string, error) {
	stack := str(o["stack"])
	if stack == "" {
		return nil, fmt.Errorf("stack is required")
	}
	addr := str(o["address"])
	if addr == "" {
		return nil, fmt.Errorf("address is required")
	}
	id := str(o["id"])
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	return []string{"import", stack, addr, id}, nil
}

func consoleArgs(o map[string]any) ([]string, error) {
	stack := str(o["stack"])
	if stack == "" {
		return nil, fmt.Errorf("stack is required")
	}
	return []string{"console", stack}, nil
}

func stateArgs(o map[string]any) ([]string, error) {
	stack := str(o["stack"])
	if stack == "" {
		return nil, fmt.Errorf("stack is required")
	}
	a := []string{"state", stack}
	a = append(a, strList(o["args"])...)
	return a, nil
}

// buildArgs builds `build [stack]`; stack is optional (omit to build every
// stack).
func buildArgs(o map[string]any) []string {
	a := []string{"build"}
	if stack := str(o["stack"]); stack != "" {
		a = append(a, stack)
	}
	return a
}

// cleanArgs builds `clean [target]`; target is optional.
func cleanArgs(o map[string]any) []string {
	a := []string{"clean"}
	if t := str(o["target"]); t != "" {
		a = append(a, t)
	}
	return a
}

func validateArgs(o map[string]any) ([]string, error) {
	stack := str(o["stack"])
	if stack == "" {
		return nil, fmt.Errorf("stack is required")
	}
	return []string{"validate", stack}, nil
}

// runTerraspace spawns the terraspace binary with args and captures the
// result. cmd.Dir is set from the connection's `dir` when set. A non-zero
// exit is returned as exit_code, not an error; only a failure to start the
// process (missing binary, timeout) is an error.
func runTerraspace(ctx context.Context, conn terraspaceConn, args []string) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, conn.binary, args...)
	cmd.Env = conn.procEnv()
	if conn.dir != "" {
		cmd.Dir = conn.dir
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

func main() {
	if err := plugin.Serve(terraspacePlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-terraspace: %v\n", err)
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
// for options that default to true (up/down/all_up/all_down's yes).
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

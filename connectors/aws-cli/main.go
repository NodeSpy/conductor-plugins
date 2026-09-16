// Command conductor-aws-cli is a verb-only conductor connector (type: aws-cli)
// that drives the AWS CLI (`aws`) by shelling out to it. AWS's surface is unbounded, so the design
// is generic-first + first-class conveniences: `run` covers ANY AWS CLI
// service/operation pair, while `s3`, `lambda_invoke`, `sts_identity`, and
// `cli` give ergonomic shapes to the operations most triggers actually need.
//
// Credentials come from the AMBIENT AWS profile/environment (the standard aws
// CLI credential chain: env vars, `~/.aws/credentials`, an instance role, SSO
// cache, …) — never from this connector. `profile`/`region`/`env` only select
// WHICH ambient credentials to use; they do not carry secrets themselves.
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

type awsPlugin struct{}

func (awsPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "aws-cli",
		Desc: "AWS CLI: a generic `run` (any service/operation) plus first-class conveniences for s3, lambda invoke, and sts identity, and a `cli` escape hatch. Shells out to the aws CLI; credentials come from the ambient AWS profile/environment, not from this connector.",
		Connection: plugin.Schema{
			"profile":      {Type: "string", Desc: "--profile (selects an ambient credential profile; does not carry secrets)"},
			"region":       {Type: "string", Desc: "--region"},
			"output":       {Type: "string", Enum: []string{"json", "yaml", "yaml-stream", "text", "table"}, Desc: "--output (default json); ignored by s3 and cli"},
			"endpoint_url": {Type: "string", Desc: "--endpoint-url (e.g. a LocalStack or VPC endpoint)"},
			"binary":       {Type: "string", Desc: "override the CLI binary path (default aws)"},
			"env":          {Type: "map", Desc: "default process environment for every invocation, e.g. AWS_PROFILE/AWS_ACCESS_KEY_ID"},
			"timeout":      {Type: "duration", Desc: "default per-verb timeout (default 10m); a verb's timeout option overrides it"},
		},
		Verbs:        awsVerbs(),
		Capabilities: plugin.Capabilities{Commands: []string{"aws"}, Spawns: true},
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

func awsVerbs() []plugin.Verb {
	out := stdOutputs()
	withResult := plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "result": {Type: "any", Desc: "parsed JSON stdout (only when exit_code == 0)"}}
	return []plugin.Verb{
		{
			Name: "run", Desc: "run any AWS CLI service/operation: aws <service> <operation> [params…]",
			Usage: "the generic verb — covers any AWS API the CLI exposes, when no first-class verb fits",
			Options: plugin.Schema{
				"service":   {Type: "string", Required: true, Desc: "e.g. s3api, ec2, dynamodb"},
				"operation": {Type: "string", Required: true, Desc: "e.g. list-buckets, describe-instances"},
				"params": {Type: "map", Desc: "for each key: emits --<key> <value>; true emits a bare --<key>; " +
					"false is skipped; a list value repeats --<key> for each element"},
				"args": {Type: "list", Desc: "raw trailing argv, appended after params"},
			},
			Outputs: withResult,
		},
		{
			Name: "s3", Desc: "aws s3 <subcommand>: cp/sync/mv/rm/ls/mb/rb",
			Usage: "high-level S3 object/bucket operations (the s3 CLI, not s3api)",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Required: true, Enum: []string{"cp", "sync", "mv", "rm", "ls", "mb", "rb"}},
				"src":        {Type: "string", Desc: "source path or s3:// uri", Scope: "path"},
				"dst":        {Type: "string", Desc: "destination path or s3:// uri (cp/sync/mv)", Scope: "path"},
				"recursive":  {Type: "boolean", Desc: "--recursive"},
				"delete":     {Type: "boolean", Desc: "--delete (sync)"},
				"exclude":    {Type: "list", Desc: "--exclude pattern(s), repeated"},
				"include":    {Type: "list", Desc: "--include pattern(s), repeated"},
				"dryrun":     {Type: "boolean", Desc: "--dryrun"},
				"extra_args": {Type: "list", Desc: "raw flags inserted before the source/destination"},
			},
			Outputs: out,
		},
		{
			Name: "lambda_invoke", Desc: "invoke a Lambda function and capture its response payload",
			Options: plugin.Schema{
				"function":        {Type: "string", Required: true, Scope: "function", Desc: "--function-name"},
				"payload":         {Type: "any", Desc: "--payload: a JSON string, or an object marshaled to one"},
				"invocation_type": {Type: "string", Enum: []string{"RequestResponse", "Event", "DryRun"}, Desc: "--invocation-type"},
				"log_type":        {Type: "string", Enum: []string{"None", "Tail"}, Desc: "--log-type"},
				"qualifier":       {Type: "string", Desc: "--qualifier (version or alias)"},
			},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "response": {Type: "any", Desc: "parsed invocation response body"}},
		},
		{
			Name: "sts_identity", Desc: "aws sts get-caller-identity — who these credentials resolve to",
			Options: plugin.Schema{},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "identity": {Type: "any", Desc: "parsed identity document (Account/Arn/UserId)"}},
		},
		{
			Name: "cli", Desc: "run any aws subcommand: aws <args…>",
			Usage:   "escape hatch for anything without a first-class verb",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after the binary"}},
			Outputs: out,
		},
	}
}

// awsConn is the resolved connection config for one invocation.
type awsConn struct {
	binary      string
	profile     string
	region      string
	output      string
	endpointURL string
	env         map[string]string
	timeout     time.Duration
}

func parseConn(m map[string]any) (awsConn, error) {
	c := awsConn{
		binary:      strOr(m["binary"], "aws"),
		profile:     str(m["profile"]),
		region:      str(m["region"]),
		output:      strOr(m["output"], "json"),
		endpointURL: str(m["endpoint_url"]),
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

// connFlags are the top-level CLI flags that precede any service/operation.
func (c awsConn) connFlags() []string {
	var f []string
	if c.profile != "" {
		f = append(f, "--profile", c.profile)
	}
	if c.region != "" {
		f = append(f, "--region", c.region)
	}
	if c.endpointURL != "" {
		f = append(f, "--endpoint-url", c.endpointURL)
	}
	return f
}

func (c awsConn) procEnv() []string {
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

func (awsPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	args, err := verbArgs(req.Verb, o, conn.output)
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

	res, err := runAWS(ctx, conn, args)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	enrich(req.Verb, res)
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary and the connection flags. Pure and
// hermetically testable — no process is spawned here. output is the resolved
// connection output format ("json" by default); s3 and cli ignore it since
// s3's own CLI has no --output and cli is a raw escape hatch.
func verbArgs(verb string, o map[string]any, output string) ([]string, error) {
	switch verb {
	case "run":
		return runArgs(o, output)
	case "s3":
		return s3Args(o)
	case "lambda_invoke":
		return lambdaInvokeArgs(o, output)
	case "sts_identity":
		return stsIdentityArgs(output), nil
	case "cli":
		return cliArgs(o)
	}
	return nil, fmt.Errorf("unknown verb")
}

// runArgs is the generic verb: aws <service> <operation> [params…] [args…]
// --output <output>. params/args order is deterministic (params sorted by
// key, then raw args, then --output last) so the same options always produce
// the same argv.
func runArgs(o map[string]any, output string) ([]string, error) {
	svc := str(o["service"])
	if svc == "" {
		return nil, fmt.Errorf("service is required")
	}
	op := str(o["operation"])
	if op == "" {
		return nil, fmt.Errorf("operation is required")
	}
	a := []string{svc, op}
	a = append(a, paramFlags(o["params"])...)
	a = append(a, strList(o["args"])...)
	a = append(a, "--output", strOr(output, "json"))
	return a, nil
}

// s3Args builds `aws s3 <subcommand>` — the high-level S3 CLI, which has no
// --output flag, so none is appended.
func s3Args(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	if sub == "" {
		return nil, fmt.Errorf("subcommand is required")
	}
	a := []string{"s3", sub}
	if boolv(o["recursive"]) {
		a = append(a, "--recursive")
	}
	if boolv(o["delete"]) {
		a = append(a, "--delete")
	}
	a = append(a, repeatFlag("--exclude", strList(o["exclude"]))...)
	a = append(a, repeatFlag("--include", strList(o["include"]))...)
	if boolv(o["dryrun"]) {
		a = append(a, "--dryrun")
	}
	a = append(a, strList(o["extra_args"])...)
	if src := str(o["src"]); src != "" {
		a = append(a, src)
	}
	if dst := str(o["dst"]); dst != "" {
		a = append(a, dst)
	}
	return a, nil
}

// lambdaInvokeArgs builds `aws lambda invoke`. The CLI writes the response
// BODY to an outfile positional arg rather than stdout; we use /dev/stdout as
// that outfile so the body still comes back on the captured stdout stream.
func lambdaInvokeArgs(o map[string]any, output string) ([]string, error) {
	fn := str(o["function"])
	if fn == "" {
		return nil, fmt.Errorf("function is required")
	}
	a := []string{"lambda", "invoke", "--function-name", fn}
	if p, ok := o["payload"]; ok && p != nil {
		s, err := payloadString(p)
		if err != nil {
			return nil, fmt.Errorf("payload: %w", err)
		}
		if s != "" {
			a = append(a, "--payload", s)
		}
	}
	if it := str(o["invocation_type"]); it != "" {
		a = append(a, "--invocation-type", it)
	}
	if lt := str(o["log_type"]); lt != "" {
		a = append(a, "--log-type", lt)
	}
	if q := str(o["qualifier"]); q != "" {
		a = append(a, "--qualifier", q)
	}
	a = append(a, "--output", strOr(output, "json"))
	// The outfile MUST be the final positional argument.
	a = append(a, "/dev/stdout")
	return a, nil
}

// stsIdentityArgs takes no options: aws sts get-caller-identity --output <output>.
func stsIdentityArgs(output string) []string {
	return []string{"sts", "get-caller-identity", "--output", strOr(output, "json")}
}

func cliArgs(o map[string]any) ([]string, error) {
	args := strList(o["args"])
	if len(args) == 0 {
		return nil, fmt.Errorf("args is required")
	}
	return args, nil
}

// payloadString renders a lambda_invoke payload option as the JSON string the
// --payload flag wants: a string is passed through verbatim (the caller may
// have already JSON-encoded it); anything else is marshaled.
func payloadString(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// enrich adds verb-specific structured outputs, only when the command
// succeeded (exit_code 0) so we never try to parse an error stream as JSON.
func enrich(verb string, res map[string]any) {
	if res["exit_code"] != 0 {
		return
	}
	stdout, _ := res["stdout"].(string)
	switch verb {
	case "run":
		if v := jsonAny(stdout); v != nil {
			res["result"] = v
		}
	case "lambda_invoke":
		if v := jsonAny(stdout); v != nil {
			res["response"] = v
		}
	case "sts_identity":
		if v := jsonAny(stdout); v != nil {
			res["identity"] = v
		}
	}
}

// runAWS spawns the aws binary with connFlags + args and captures the result.
// A non-zero exit is returned as exit_code, not an error; only a failure to
// start the process (missing binary, timeout) is an error. exec.CommandContext
// is used directly — no shell, so there is no quoting/injection surface.
func runAWS(ctx context.Context, conn awsConn, args []string) (map[string]any, error) {
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
	if err := plugin.Serve(awsPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-aws: %v\n", err)
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

// paramFlags renders run's generic `params` map into flags, keys sorted for
// deterministic argv: true emits a bare --<key>; false is skipped; a list
// value repeats --<key> for each element; anything else emits --<key> <val>.
func paramFlags(v any) []string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		flag := "--" + k
		switch val := m[k].(type) {
		case bool:
			if val {
				out = append(out, flag)
			}
		case nil:
			// skip
		case []any, []string:
			out = append(out, repeatFlag(flag, strList(val))...)
		default:
			out = append(out, flag, fmt.Sprintf("%v", val))
		}
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

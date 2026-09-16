// Command conductor-libation is a verb-only conductor connector (#59) that
// drives Libation (https://getlibation.com) by shelling out to its CLI,
// `LibationCli`, to pull DRM-free M4B copies of an Audible library. It exposes
// the subcommands a scheduled download pass needs — scan/export/liberate/
// set_status/search/list_accounts — plus a `cli` escape hatch for the rest
// (convert, get-setting, upload, version). Built ONLY against the public SDK.
//
// This is the DOWNLOAD half of the NodeSpy/audiobookshelf-import pipeline
// (scan → export → liberate). The IMPORT half is the separate `audiobookshelf`
// connector; a conductor workflow composes the two, which is why nothing here
// knows anything about Audiobookshelf.
//
// Downloads are slow — a long title is tens of gigabytes and Audible throttles —
// so the default per-verb timeout is 60m rather than the minutes a normal CLI
// connector allows. A non-zero exit is DATA (`liberate` returns non-zero when
// Audible refuses to license one title of many, and the pass still made
// progress), never an RPC error.
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
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type libationPlugin struct{}

func (libationPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "libation",
		Desc: "Libation: download DRM-free copies of an Audible library by shelling to the LibationCli CLI (scan, export, liberate, set_status, search, list_accounts, cli). The download half of an Audible→Audiobookshelf pipeline; compose it with the audiobookshelf connector for the import half.",
		Connection: plugin.Schema{
			"binary":  {Type: "string", Desc: "LibationCli binary path (default LibationCli)"},
			"dir":     {Type: "string", Desc: "working directory for every invocation"},
			"env":     {Type: "map", Desc: "process environment — LIBATION_FILES_DIR selects Libation's config/database directory"},
			"timeout": {Type: "duration", Desc: "default per-verb timeout (default 60m — downloads are slow); a verb's timeout option overrides it"},
		},
		Verbs: libationVerbs(),
		// Every verb is a LibationCli shell-out. Libation itself reaches
		// Audible and its CDN; this connector dials nothing directly, so it
		// declares no egress and the operator scopes the network the
		// subprocess gets with the instance's `network:`.
		Capabilities: plugin.Capabilities{Commands: []string{"LibationCli"}, Spawns: true},
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

func libationVerbs() []plugin.Verb {
	out := stdOutputs()
	return []plugin.Verb{
		{
			Name: "scan", Desc: "index the Audible library: ask Audible what is new",
			Usage: "the cheap first step of a download pass; it downloads nothing",
			Options: plugin.Schema{
				"accounts": plugin.Field{Type: "list", Scope: "account", Desc: "account ids or nicknames to scan (default: every configured account)"},
			},
			Outputs: out,
		},
		{
			Name: "export", Desc: "write the library manifest to a file (--json/--csv/--xlsx)",
			Usage: "the manifest is how a workflow learns each book's BookStatus (Liberated or not)",
			Options: plugin.Schema{
				"path":   {Type: "string", Required: true, Scope: "path", Desc: "-p file to write"},
				"format": {Type: "string", Enum: []string{"json", "csv", "xlsx"}, Desc: "-j / -c / -x (default json)"},
				"asins":  {Type: "list", Desc: "limit the export to these product ids (positional)"},
				"parse":  {Type: "boolean", Desc: "read the written json back into the library output"},
			},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"},
				"library": {Type: "list", Desc: "the parsed manifest, with parse: true and format json"},
				"count":   {Type: "integer", Desc: "number of records in library"}},
		},
		{
			Name: "liberate", Desc: "download and decrypt pending books (and their PDFs)",
			Usage: "the slow step: name asins to fetch exactly those, or use limit_books to bound a backlog",
			Options: plugin.Schema{
				"asins":       {Type: "list", Desc: "-i product id(s) to liberate; default = every un-liberated title"},
				"pdf":         {Type: "boolean", Desc: "--pdf only download PDFs"},
				"force":       {Type: "boolean", Desc: "--force re-download a title already liberated"},
				"limit_books": {Type: "integer", Desc: "--limit-books stop the run after this many books"},
				"limit_mb":    {Type: "integer", Desc: "--limit-mb stop the run after about this many MB"},
				"limit_gb":    {Type: "integer", Desc: "--limit-gb stop the run after about this many GB"},
			},
			Outputs: out,
		},
		{
			Name: "set_status", Desc: "mark books downloaded / download-pending in Libation's database",
			Usage: "seeding: tell Libation a book is already held so a fresh database does not re-download the whole library",
			Options: plugin.Schema{
				"status": {Type: "string", Required: true, Enum: []string{"downloaded", "pending"}, Desc: "--downloaded / --download-pending"},
				"asins":  {Type: "list", Desc: "limit to these product ids (positional); default = the whole library"},
				"force":  {Type: "boolean", Desc: "--force set the status without looking for the audio file"},
			},
			Outputs: out,
		},
		{
			Name: "search", Desc: "search the local library (Lucene query)",
			Options: plugin.Schema{
				"query": {Type: "string", Required: true, Desc: "Lucene search string (positional)"},
				"count": {Type: "integer", Desc: "-n results per page (0 = all)"},
				"bare":  {Type: "boolean", Desc: "--bare print one ASIN per line, no titles"},
			},
			Outputs: out,
		},
		{
			Name: "list_accounts", Desc: "list the configured Audible accounts and whether their credentials still work",
			Usage:   "the health check: an account whose stored credentials expired makes every scan fail",
			Options: plugin.Schema{"bare": {Type: "boolean", Desc: "--bare tab-separated values, no table borders"}},
			Outputs: out,
		},
		{
			Name: "cli", Desc: "run any LibationCli subcommand: LibationCli <args…>",
			Usage:   "escape hatch for a subcommand without a first-class verb (convert, get-setting, upload, version, …)",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after the binary"}},
			Outputs: out,
		},
	}
}

// libationConn is the resolved connection config for one invocation.
type libationConn struct {
	binary  string
	dir     string
	env     map[string]string
	timeout time.Duration
}

func parseConn(m map[string]any) (libationConn, error) {
	c := libationConn{
		binary:  strOr(m["binary"], "LibationCli"),
		dir:     str(m["dir"]),
		env:     strMap(m["env"]),
		timeout: 60 * time.Minute,
	}
	if d, err := toDuration(m["timeout"]); err != nil {
		return c, fmt.Errorf("connection.timeout: %w", err)
	} else if d > 0 {
		c.timeout = d
	}
	return c, nil
}

func (c libationConn) procEnv() []string {
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

func (libationPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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

	res, err := runLibation(ctx, conn, args)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	enrich(req.Verb, o, res)
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary. Pure and hermetically testable —
// no process is spawned here.
func verbArgs(verb string, o map[string]any) ([]string, error) {
	switch verb {
	case "scan":
		// Accounts are positional; no account means every configured one.
		return append([]string{"scan"}, strList(o["accounts"])...), nil
	case "export":
		return exportArgs(o)
	case "liberate":
		return liberateArgs(o)
	case "set_status":
		return setStatusArgs(o)
	case "search":
		return searchArgs(o)
	case "list_accounts":
		a := []string{"list-accounts"}
		if boolv(o["bare"]) {
			a = append(a, "--bare")
		}
		return a, nil
	case "cli":
		args := strList(o["args"])
		if len(args) == 0 {
			return nil, fmt.Errorf("args is required")
		}
		return args, nil
	}
	return nil, fmt.Errorf("unknown verb")
}

// exportArgs builds `export -p <path> -j|-c|-x [asins…]`. LibationCli requires
// the path and infers the format from the extension when no flag is given; the
// flag is always emitted so the format is never accidental.
func exportArgs(o map[string]any) ([]string, error) {
	path := str(o["path"])
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	a := []string{"export", "-p", path}
	switch f := strOr(o["format"], "json"); f {
	case "json":
		a = append(a, "-j")
	case "csv":
		a = append(a, "-c")
	case "xlsx":
		a = append(a, "-x")
	default:
		return nil, fmt.Errorf("format must be json, csv or xlsx, got %q", f)
	}
	return append(a, strList(o["asins"])...), nil
}

// liberateArgs builds `liberate [-i asin]… [--pdf] [--force] [--limit-*]`.
// Naming the asins is what the reference pipeline does, so a title Audible has
// already refused is never re-attempted.
func liberateArgs(o map[string]any) ([]string, error) {
	a := []string{"liberate"}
	a = append(a, repeatFlag("-i", strList(o["asins"]))...)
	if boolv(o["pdf"]) {
		a = append(a, "--pdf")
	}
	if boolv(o["force"]) {
		a = append(a, "--force")
	}
	// The three run limits are mutually exclusive in LibationCli itself
	// (distinct option sets) — catch it here rather than spending a process to
	// be told so.
	limits := [][2]string{{"--limit-books", intStr(o["limit_books"])}, {"--limit-mb", intStr(o["limit_mb"])}, {"--limit-gb", intStr(o["limit_gb"])}}
	set := 0
	for _, l := range limits {
		if l[1] != "" {
			set++
		}
	}
	if set > 1 {
		return nil, fmt.Errorf("limit_books, limit_mb and limit_gb are mutually exclusive")
	}
	for _, l := range limits {
		if l[1] != "" {
			a = append(a, l[0], l[1])
		}
	}
	return a, nil
}

// setStatusArgs builds `set-status --downloaded|--download-pending [--force]
// [asins…]`. The status is the whole point of the verb, so it is required
// rather than defaulted — defaulting it would make a typo silently flip the
// library the wrong way.
func setStatusArgs(o map[string]any) ([]string, error) {
	a := []string{"set-status"}
	switch s := str(o["status"]); s {
	case "downloaded":
		a = append(a, "--downloaded")
	case "pending":
		a = append(a, "--download-pending")
	case "":
		return nil, fmt.Errorf("status is required")
	default:
		return nil, fmt.Errorf("status must be downloaded or pending, got %q", s)
	}
	if boolv(o["force"]) {
		a = append(a, "--force")
	}
	return append(a, strList(o["asins"])...), nil
}

// searchArgs builds `search [-n N] [--bare] <query>` — the query is positional
// and comes last.
func searchArgs(o map[string]any) ([]string, error) {
	q := str(o["query"])
	if q == "" {
		return nil, fmt.Errorf("query is required")
	}
	a := []string{"search"}
	if n := intStr(o["count"]); n != "" {
		a = append(a, "-n", n)
	}
	if boolv(o["bare"]) {
		a = append(a, "--bare")
	}
	return append(a, q), nil
}

// enrich adds verb-specific structured outputs, only when the command succeeded
// (exit_code 0) so we never parse a half-written file.
//
// LibationCli writes its JSON to the export PATH rather than to stdout, so
// `export` with parse: true reads that file back — opt-in, because a large
// library is a multi-megabyte manifest most workflows would rather hand to the
// next step by path.
func enrich(verb string, o, res map[string]any) {
	if verb != "export" || res["exit_code"] != 0 || !boolv(o["parse"]) {
		return
	}
	if strOr(o["format"], "json") != "json" {
		return
	}
	b, err := os.ReadFile(str(o["path"]))
	if err != nil {
		return
	}
	var lib []any
	if err := json.Unmarshal(b, &lib); err != nil {
		return
	}
	res["library"] = lib
	res["count"] = len(lib)
}

// runLibation spawns LibationCli with args and captures the result. A non-zero
// exit is returned as exit_code, not an error — liberate exits non-zero when
// Audible refuses one title of many, and that pass still made progress. Only a
// failure to start the process (missing binary, timeout) is an error.
func runLibation(ctx context.Context, conn libationConn, args []string) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, conn.binary, args...)
	cmd.Dir = conn.dir
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
	if err := plugin.Serve(libationPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-libation: %v\n", err)
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

// toDuration parses a timeout option: a Go duration string ("60m"), or a number
// interpreted as seconds. Zero/absent → 0 (use the default).
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

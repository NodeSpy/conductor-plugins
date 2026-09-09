// Command conductor-paseo is the paseo runtime as a standalone external
// conductor plugin (issue #59 / docs/design/paseo-runtime-plugin.md). It
// exposes, as verbs, exactly the paseo-daemon operations
// internal/dispatch.Backend needs — launching an agent turn, listing/
// inspecting agents, archiving an agent or workspace, creating a worktree or
// plain workspace, listing workspaces, cloning a repo, sending a follow-up,
// and waiting for an agent to go idle — each implemented by shelling to the
// `paseo` CLI against paseo's own persistent, multi-agent daemon (the same
// CLI cliBackend drives directly; this plugin is an alternate, opt-in
// transport for the identical operations, not a different feature set).
//
// This is deliberately NOT a `kind: runtime` (ACP) plugin: as
// docs/design/paseo-runtime-plugin.md explains, paseo's daemon-wide
// dedup/reaper/hold semantics have no ACP equivalent (one subprocess would
// have to BE one session, and paseo is the opposite — a stateless CLI client
// against a long-lived daemon). It is a `kind: connector`-shaped verb plugin,
// invoked by internal/dispatch's rpcBackend via the ordinary plugin.invoke
// RPC — no new wire protocol.
//
// Built ONLY against the public SDK (pkg/plugin) + os/exec + the standard
// library — no other conductor package.
//
// Connection (per verb call):
//
//	paseo_bin: "paseo"   # override the paseo binary path (default "paseo"; tests point this at a stub)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// Verb names — mirror internal/dispatch.Backend's method set 1:1.
const (
	verbRun              = "run"
	verbListAgents       = "list_agents"
	verbInspect          = "inspect"
	verbArchiveAgent     = "archive_agent"
	verbArchiveWorkspace = "archive_workspace"
	verbCreateWorktree   = "create_worktree"
	verbCreateWorkspace  = "create_workspace"
	verbListWorkspaces   = "list_workspaces"
	verbClone            = "clone"
	verbSend             = "send"
	verbWait             = "wait"
)

type paseoPlugin struct{}

func (paseoPlugin) Describe() plugin.Decl {
	strArg := func(desc string) plugin.Field { return plugin.Field{Type: "string", Desc: desc} }
	return plugin.Decl{
		Type: "paseo",
		Desc: "paseo runtime: the daemon operations internal/dispatch.Backend needs, over the plugin RPC instead of a direct CLI shell-out (#59).",
		Connection: plugin.Schema{
			"paseo_bin": strArg("override the paseo binary path (default \"paseo\")"),
		},
		Verbs: []plugin.Verb{
			{
				Name: verbRun, Desc: "launch (or re-launch) a coding agent turn: `paseo run <args...>`",
				Options: plugin.Schema{
					"args": {Type: "list", Required: true, Desc: "the full `paseo run` argument list (everything after `run` itself)"},
				},
				Outputs: plugin.Schema{
					"output":  {Type: "string"},
					"agentId": {Type: "string"},
				},
			},
			{
				Name: verbListAgents, Desc: "list non-archived agents: `paseo ls --json [--label k=v ...]`",
				Options: plugin.Schema{
					"labels": {Type: "map", Desc: "exact-match label filters"},
				},
				Outputs: plugin.Schema{"agents": {Type: "list"}},
			},
			{
				Name: verbInspect, Desc: "inspect one agent: `paseo inspect <id> --json`",
				Options: plugin.Schema{"id": {Type: "string", Required: true}},
				Outputs: plugin.Schema{
					"cwd": {Type: "string"}, "lastUsage": {Type: "string"},
					"updatedAt": {Type: "string"}, "createdAt": {Type: "string"},
				},
			},
			{
				Name: verbArchiveAgent, Desc: "soft-delete one agent: `paseo archive <id>`",
				Options: plugin.Schema{"id": {Type: "string", Required: true}},
			},
			{
				Name: verbArchiveWorkspace, Desc: "soft-delete a workspace (reclaims any worktree it owns): `paseo workspace archive <id>`",
				Options: plugin.Schema{"id": {Type: "string", Required: true}},
			},
			{
				Name: verbCreateWorktree, Desc: "create an isolated PR/branch worktree workspace: `paseo workspace create --mode ...`",
				Options: plugin.Schema{
					"isolation": {Type: "string", Required: true},
					"path":      {Type: "string", Required: true},
					"strategy":  {Type: "string", Required: true, Enum: []string{"checkout-pr", "branch-off"}},
					"prNumber":  {Type: "integer"},
					"forge":     {Type: "string"},
					"newBranch": {Type: "string"},
					"baseRef":   {Type: "string"},
				},
				Outputs: plugin.Schema{"workspaceId": {Type: "string"}, "cwd": {Type: "string"}},
			},
			{
				Name: verbCreateWorkspace, Desc: "create a plain (non-worktree) workspace: `paseo workspace create --isolation ...`",
				Options: plugin.Schema{
					"isolation": {Type: "string", Required: true},
					"path":      {Type: "string", Required: true},
					"title":     {Type: "string"},
				},
				Outputs: plugin.Schema{"workspaceId": {Type: "string"}},
			},
			{
				Name: verbListWorkspaces, Desc: "list every workspace: `paseo workspace ls --json`",
				Outputs: plugin.Schema{"workspaces": {Type: "list"}},
			},
			{
				Name: verbClone, Desc: "clone a repo and register it with paseo: `paseo clone`",
				Options: plugin.Schema{
					"repo": {Type: "string", Required: true}, "dir": {Type: "string", Required: true},
					"protocol": {Type: "string"},
				},
			},
			{
				Name: verbSend, Desc: "queue a follow-up prompt to a live agent: `paseo send <id> <prompt> [--json]`",
				Options: plugin.Schema{
					"id": {Type: "string", Required: true}, "prompt": {Type: "string", Required: true},
					"json": {Type: "boolean"},
				},
				Outputs: plugin.Schema{"output": {Type: "string"}},
			},
			{
				Name: verbWait, Desc: "block until an agent goes idle: `paseo wait <id>`",
				Options: plugin.Schema{"id": {Type: "string", Required: true}},
			},
		},
	}
}

func (p paseoPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	bin := strOr(req.Connection["paseo_bin"], "paseo")
	opts := req.Options
	switch req.Verb {
	case verbRun:
		return p.run(bin, opts)
	case verbListAgents:
		return p.listAgents(bin, opts)
	case verbInspect:
		return p.inspect(bin, opts)
	case verbArchiveAgent:
		return p.archiveAgent(bin, opts)
	case verbArchiveWorkspace:
		return p.archiveWorkspace(bin, opts)
	case verbCreateWorktree:
		return p.createWorktree(bin, opts)
	case verbCreateWorkspace:
		return p.createWorkspace(bin, opts)
	case verbListWorkspaces:
		return p.listWorkspaces(bin)
	case verbClone:
		return p.clone(bin, opts)
	case verbSend:
		return p.send(bin, opts)
	case verbWait:
		return p.wait(bin, opts)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unknown verb "+req.Verb)
	}
}

// run shells `paseo run <args...>`, returning stdout and the best-effort
// parsed agent id. A failure carries the same stdout-JSON-error-else-stderr
// detail cliBackend's paseoErrDetail extracts, so a caller doing its own
// transient-error retry (rpcBackend) sees the same signal text.
func (p paseoPlugin) run(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	args := asStringSlice(opts["args"])
	out, stderr, err := runCmd(bin, args...)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(out, stderr, err))
	}
	return plugin.InvokeResult{Outputs: map[string]any{
		"output": string(out), "agentId": parseAgentID(out),
	}}, nil
}

func (p paseoPlugin) listAgents(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	args := []string{"ls", "--json"}
	args = append(args, sortedLabelArgs(asStringMap(opts["labels"]))...)
	out, stderr, err := runCmd(bin, args...)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(out, stderr, err))
	}
	var agents []map[string]any
	if json.Unmarshal(out, &agents) != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "list_agents: unparseable output")
	}
	return plugin.InvokeResult{Outputs: map[string]any{"agents": agents}}, nil
}

func (p paseoPlugin) inspect(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	id := asString(opts["id"])
	out, stderr, err := runCmd(bin, "inspect", id, "--json")
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(out, stderr, err))
	}
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "inspect: unparseable output")
	}
	return plugin.InvokeResult{Outputs: map[string]any{
		"cwd": m["Cwd"], "lastUsage": m["LastUsage"], "updatedAt": m["UpdatedAt"], "createdAt": m["CreatedAt"],
	}}, nil
}

func (p paseoPlugin) archiveAgent(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	_, stderr, err := runCmd(bin, "archive", asString(opts["id"]))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(nil, stderr, err))
	}
	return plugin.InvokeResult{}, nil
}

func (p paseoPlugin) archiveWorkspace(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	_, stderr, err := runCmd(bin, "workspace", "archive", asString(opts["id"]))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(nil, stderr, err))
	}
	return plugin.InvokeResult{}, nil
}

func (p paseoPlugin) createWorktree(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	strat := asString(opts["strategy"])
	args := []string{"workspace", "create", "--isolation", asString(opts["isolation"]),
		"--path", asString(opts["path"]), "--mode", strat, "--json"}
	switch strat {
	case "checkout-pr":
		args = append(args, "--pr-number", fmt.Sprintf("%d", asInt(opts["prNumber"])), "--forge", asString(opts["forge"]))
	case "branch-off":
		args = append(args, "--new-branch", asString(opts["newBranch"]))
		if base := asString(opts["baseRef"]); base != "" {
			args = append(args, "--base", base)
		}
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "create_worktree: unexpected strategy "+strat)
	}
	out, stderr, err := runCmd(bin, args...)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(out, stderr, err))
	}
	var w struct {
		WorkspaceID string `json:"workspaceId"`
		Cwd         string `json:"cwd"`
	}
	if json.Unmarshal(out, &w) != nil || w.WorkspaceID == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "create_worktree: unparseable output: "+strings.TrimSpace(string(out)))
	}
	return plugin.InvokeResult{Outputs: map[string]any{"workspaceId": w.WorkspaceID, "cwd": w.Cwd}}, nil
}

func (p paseoPlugin) createWorkspace(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	args := []string{"workspace", "create", "--isolation", asString(opts["isolation"]), "--path", asString(opts["path"])}
	if title := asString(opts["title"]); title != "" {
		args = append(args, "--title", title)
	}
	args = append(args, "--json")
	out, stderr, err := runCmd(bin, args...)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(out, stderr, err))
	}
	var w struct {
		WorkspaceID string `json:"workspaceId"`
		ID          string `json:"id"`
	}
	if json.Unmarshal(out, &w) != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "create_workspace: unparseable output")
	}
	id := w.WorkspaceID
	if id == "" {
		id = w.ID
	}
	return plugin.InvokeResult{Outputs: map[string]any{"workspaceId": id}}, nil
}

func (p paseoPlugin) listWorkspaces(bin string) (plugin.InvokeResult, error) {
	out, stderr, err := runCmd(bin, "workspace", "ls", "--json")
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(out, stderr, err))
	}
	var wl []map[string]any
	if json.Unmarshal(out, &wl) != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "list_workspaces: unparseable output")
	}
	return plugin.InvokeResult{Outputs: map[string]any{"workspaces": wl}}, nil
}

func (p paseoPlugin) clone(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	proto := asString(opts["protocol"])
	if proto == "" {
		proto = "ssh"
	}
	out, stderr, err := runCmd(bin, "clone", asString(opts["repo"]), "--dir", asString(opts["dir"]), "--protocol", proto, "--json")
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(out, stderr, err))
	}
	return plugin.InvokeResult{}, nil
}

func (p paseoPlugin) send(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	args := []string{"send", asString(opts["id"]), asString(opts["prompt"])}
	if b, _ := opts["json"].(bool); b {
		args = append(args, "--json")
	}
	out, stderr, err := runCmd(bin, args...)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(out, stderr, err))
	}
	return plugin.InvokeResult{Outputs: map[string]any{"output": string(out)}}, nil
}

func (p paseoPlugin) wait(bin string, opts map[string]any) (plugin.InvokeResult, error) {
	_, stderr, err := runCmd(bin, "wait", asString(opts["id"]))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, cliErrDetail(nil, stderr, err))
	}
	return plugin.InvokeResult{}, nil
}

// --- exec + coercion helpers ---

// runCmd runs the paseo CLI and returns (stdout, stderr, error) — err is the
// raw *exec.ExitError (or launch error), never wrapped, so cliErrDetail can
// extract the same structured/stderr detail cliBackend's paseoErrDetail does.
func runCmd(bin string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.Command(bin, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, runErr := cmd.Output()
	return out, errBuf.Bytes(), runErr
}

// cliErrDetail extracts a human-readable reason from a failed paseo CLI call,
// mirroring internal/dispatch's paseoErrDetail: prefer the --json error
// object on stdout, else stderr, else the exec error itself.
func cliErrDetail(stdout, stderr []byte, err error) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(stdout, &e) == nil && e.Error.Message != "" {
		if e.Error.Code != "" {
			return e.Error.Code + ": " + e.Error.Message
		}
		return e.Error.Message
	}
	if s := strings.TrimSpace(string(stderr)); s != "" {
		return s
	}
	if err != nil {
		return err.Error()
	}
	return "paseo: unknown error"
}

// parseAgentID best-effort extracts an agent id from `paseo run --json`
// output (mirrors internal/dispatch's parseAgentID).
func parseAgentID(out []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		return ""
	}
	for _, k := range []string{"id", "agentId", "agent_id", "agentID"} {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// sortedLabelArgs renders a label map as sorted `--label k=v` argv pairs
// (deterministic; paseo AND-filters exact-match labels regardless of order).
func sortedLabelArgs(labels map[string]string) []string {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		args = append(args, "--label", k+"="+labels[k])
	}
	return args
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func asStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			out = append(out, asString(e))
		}
		return out
	default:
		return nil
	}
}

func asStringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = asString(val)
	}
	return out
}

func main() {
	if err := plugin.Serve(paseoPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-paseo: %v\n", err)
		os.Exit(1)
	}
}

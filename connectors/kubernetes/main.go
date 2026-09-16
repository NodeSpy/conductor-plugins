// Command conductor-kubernetes is a verb-only conductor connector (#59) that
// drives a Kubernetes cluster by shelling out to the `kubectl` CLI. It exposes
// the manifest lifecycle as verbs — apply/delete/get/describe/logs/exec/
// rollout/scale/patch/create/label/annotate/wait/top/cordon/uncordon/drain/cp —
// plus a `cli` escape hatch for any subcommand a first-class verb does not
// cover. Built ONLY against the public SDK.
//
// The cluster/namespace to talk to is selected the same way kubectl always
// does: `kubeconfig` (--kubeconfig), `context` (--context), and `namespace`
// (-n, applied as a per-verb default when a verb doesn't set its own).
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type kubernetesPlugin struct{}

func (kubernetesPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "kubernetes",
		Desc: "Kubernetes: drive a cluster's manifest lifecycle as verbs (apply, delete, get, describe, logs, exec, rollout, scale, patch, create, label, annotate, wait, top, cordon, uncordon, drain, cp, cli). Shells out to the kubectl CLI.",
		Connection: plugin.Schema{
			"kubeconfig": {Type: "string", Desc: "--kubeconfig path"},
			"context":    {Type: "string", Desc: "--context <name>"},
			"namespace":  {Type: "string", Desc: "-n default namespace, applied when a verb doesn't set its own"},
			"binary":     {Type: "string", Desc: "override the CLI binary path (default kubectl)"},
			"env":        {Type: "map", Desc: "default process environment for every invocation"},
			"timeout":    {Type: "duration", Desc: "default per-verb timeout (default 10m); a verb's timeout option overrides it"},
		},
		Verbs:        kubernetesVerbs(),
		Capabilities: plugin.Capabilities{Commands: []string{"kubectl"}, Spawns: true},
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

func kubernetesVerbs() []plugin.Verb {
	out := stdOutputs()
	withResult := plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "result": {Type: "any", Desc: "parsed --output json"}}
	return []plugin.Verb{
		{
			Name: "apply", Desc: "apply one or more manifests",
			Usage: "declaratively create/update resources from files or an inline manifest",
			Options: plugin.Schema{
				"filename":    {Type: "list", Desc: "-f each (files, dirs, or URLs)"},
				"manifest":    {Type: "string", Desc: "inline YAML/JSON, piped to stdin with -f -"},
				"recursive":   {Type: "boolean", Desc: "-R recurse into directories in filename"},
				"prune":       {Type: "boolean", Desc: "--prune"},
				"server_side": {Type: "boolean", Desc: "--server-side"},
				"force":       {Type: "boolean", Desc: "--force"},
				"namespace":   {Type: "string", Scope: "namespace", Desc: "-n override for this call"},
				"output":      {Type: "string", Enum: []string{"json", "yaml"}, Desc: "-o (parsed into result when json)"},
			},
			Outputs: withResult,
		},
		{
			Name: "delete", Desc: "delete resources",
			Options: plugin.Schema{
				"resource":         {Type: "string", Scope: "resource", Desc: "e.g. pod, deployment"},
				"name":             {Type: "string", Desc: "resource name"},
				"filename":         {Type: "list", Desc: "-f each"},
				"selector":         {Type: "string", Desc: "-l label selector"},
				"all":              {Type: "boolean", Desc: "--all"},
				"grace_period":     {Type: "integer", Desc: "--grace-period"},
				"force":            {Type: "boolean", Desc: "--force"},
				"ignore_not_found": {Type: "boolean", Desc: "--ignore-not-found"},
				"namespace":        {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "get", Desc: "list/read resources (parsed into result when output json)",
			Options: plugin.Schema{
				"resource":       {Type: "string", Required: true, Scope: "resource", Desc: "e.g. pods"},
				"name":           {Type: "string", Desc: "resource name"},
				"selector":       {Type: "string", Desc: "-l label selector"},
				"all_namespaces": {Type: "boolean", Desc: "-A"},
				"field_selector": {Type: "string", Desc: "--field-selector"},
				"output":         {Type: "string", Enum: []string{"json", "yaml", "wide", "name"}, Desc: "-o (default json)"},
				"namespace":      {Type: "string", Scope: "namespace"},
			},
			Outputs: withResult,
		},
		{
			Name: "describe", Desc: "human-readable details on one or more resources",
			Options: plugin.Schema{
				"resource":  {Type: "string", Required: true, Scope: "resource"},
				"name":      {Type: "string"},
				"selector":  {Type: "string", Desc: "-l"},
				"namespace": {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "logs", Desc: "fetch a pod's logs",
			Options: plugin.Schema{
				"pod":            {Type: "string", Required: true, Scope: "resource"},
				"container":      {Type: "string", Desc: "-c"},
				"tail":           {Type: "integer", Desc: "--tail"},
				"since":          {Type: "string", Desc: "--since"},
				"previous":       {Type: "boolean", Desc: "-p"},
				"all_containers": {Type: "boolean", Desc: "--all-containers"},
				"namespace":      {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "exec", Desc: "run a command in a running pod",
			Options: plugin.Schema{
				"pod":       {Type: "string", Required: true, Scope: "resource"},
				"container": {Type: "string", Desc: "-c"},
				"command":   {Type: "list", Required: true, Desc: "argv after --"},
				"stdin":     {Type: "boolean", Desc: "-i"},
				"tty":       {Type: "boolean", Desc: "-t"},
				"namespace": {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "rollout", Desc: "manage a rollout (status/restart/undo/pause/resume/history)",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Required: true, Enum: []string{"status", "restart", "undo", "pause", "resume", "history"}},
				"resource":   {Type: "string", Required: true, Scope: "resource", Desc: "e.g. deployment/web"},
				"namespace":  {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "scale", Desc: "scale a resource's replica count",
			Options: plugin.Schema{
				"resource":  {Type: "string", Required: true, Scope: "resource"},
				"replicas":  {Type: "integer", Required: true, Desc: "--replicas"},
				"namespace": {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "patch", Desc: "patch a resource",
			Options: plugin.Schema{
				"resource":  {Type: "string", Required: true, Scope: "resource"},
				"name":      {Type: "string", Required: true},
				"patch":     {Type: "string", Required: true, Desc: "the patch body"},
				"type":      {Type: "string", Enum: []string{"strategic", "merge", "json"}, Desc: "--type"},
				"namespace": {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "create", Desc: "create a resource from a file/manifest, or raw args",
			Options: plugin.Schema{
				"filename":   {Type: "list", Desc: "-f each"},
				"manifest":   {Type: "string", Desc: "inline YAML/JSON, piped to stdin with -f -"},
				"extra_args": {Type: "list", Desc: "raw args for `kubectl create <args>`"},
				"namespace":  {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "label", Desc: "add/update labels on a resource",
			Options: plugin.Schema{
				"resource":  {Type: "string", Required: true, Scope: "resource"},
				"name":      {Type: "string", Required: true},
				"pairs":     {Type: "map", Required: true, Desc: "key=value args"},
				"overwrite": {Type: "boolean", Desc: "--overwrite"},
				"namespace": {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "annotate", Desc: "add/update annotations on a resource",
			Options: plugin.Schema{
				"resource":  {Type: "string", Required: true, Scope: "resource"},
				"name":      {Type: "string", Required: true},
				"pairs":     {Type: "map", Required: true, Desc: "key=value args"},
				"overwrite": {Type: "boolean", Desc: "--overwrite"},
				"namespace": {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "wait", Desc: "wait on a condition",
			Options: plugin.Schema{
				"resource":  {Type: "string", Required: true, Scope: "resource"},
				"name":      {Type: "string"},
				"selector":  {Type: "string", Desc: "-l"},
				"for":       {Type: "string", Required: true, Desc: "--for, e.g. condition=Ready"},
				"timeout":   {Type: "string", Desc: "--timeout"},
				"namespace": {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "top", Desc: "resource usage (pods/nodes)",
			Options: plugin.Schema{
				"subcommand": {Type: "string", Required: true, Enum: []string{"pods", "nodes"}},
				"name":       {Type: "string"},
				"selector":   {Type: "string", Desc: "-l"},
				"namespace":  {Type: "string", Scope: "namespace"},
			},
			Outputs: out,
		},
		{
			Name: "cordon", Desc: "mark a node unschedulable",
			Options: plugin.Schema{"node": {Type: "string", Required: true, Scope: "resource"}},
			Outputs: out,
		},
		{
			Name: "uncordon", Desc: "mark a node schedulable",
			Options: plugin.Schema{"node": {Type: "string", Required: true, Scope: "resource"}},
			Outputs: out,
		},
		{
			Name: "drain", Desc: "drain a node for maintenance",
			Options: plugin.Schema{
				"node":                 {Type: "string", Required: true, Scope: "resource"},
				"ignore_daemonsets":    {Type: "boolean", Desc: "--ignore-daemonsets"},
				"delete_emptydir_data": {Type: "boolean", Desc: "--delete-emptydir-data"},
				"force":                {Type: "boolean", Desc: "--force"},
			},
			Outputs: out,
		},
		{
			Name: "cp", Desc: "copy files to/from a pod",
			Options: plugin.Schema{
				"src":       {Type: "string", Required: true},
				"dst":       {Type: "string", Required: true},
				"container": {Type: "string", Desc: "-c"},
			},
			Outputs: out,
		},
		{
			Name: "cli", Desc: "run any kubectl subcommand: kubectl <args…>",
			Usage:   "escape hatch for a subcommand without a first-class verb",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after the binary"}},
			Outputs: out,
		},
	}
}

// kubernetesConn is the resolved connection config for one invocation.
type kubernetesConn struct {
	binary     string
	kubeconfig string
	context    string
	namespace  string
	env        map[string]string
	timeout    time.Duration
}

func parseConn(m map[string]any) (kubernetesConn, error) {
	c := kubernetesConn{
		binary:     strOr(m["binary"], "kubectl"),
		kubeconfig: str(m["kubeconfig"]),
		context:    str(m["context"]),
		namespace:  str(m["namespace"]),
		env:        strMap(m["env"]),
		timeout:    10 * time.Minute,
	}
	if d, err := toDuration(m["timeout"]); err != nil {
		return c, fmt.Errorf("connection.timeout: %w", err)
	} else if d > 0 {
		c.timeout = d
	}
	return c, nil
}

// connFlags are the top-level kubectl flags that precede any subcommand.
func (c kubernetesConn) connFlags() []string {
	var a []string
	if c.kubeconfig != "" {
		a = append(a, "--kubeconfig", c.kubeconfig)
	}
	if c.context != "" {
		a = append(a, "--context", c.context)
	}
	if c.namespace != "" {
		a = append(a, "-n", c.namespace)
	}
	return a
}

func (c kubernetesConn) procEnv() []string {
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

func (kubernetesPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	args, stdin, err := verbArgs(req.Verb, o)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+err.Error())
	}

	// A verb's own `namespace` option overrides the connection default; the
	// connection flags are built once the effective namespace is known.
	if ns := str(o["namespace"]); ns != "" {
		conn.namespace = ns
	}

	timeout := conn.timeout
	if d, derr := toDuration(o["timeout"]); derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": options.timeout: "+derr.Error())
	} else if d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := runKubectl(ctx, conn, args, stdin)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	enrich(req.Verb, o, res)
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary and the connection flags, plus an
// optional stdin payload (apply/create -f -). Pure and hermetically testable
// — no process is spawned here.
func verbArgs(verb string, o map[string]any) (args []string, stdin string, err error) {
	switch verb {
	case "apply":
		return applyArgs(o)
	case "delete":
		a, e := deleteArgs(o)
		return a, "", e
	case "get":
		a, e := getArgs(o)
		return a, "", e
	case "describe":
		a, e := describeArgs(o)
		return a, "", e
	case "logs":
		a, e := logsArgs(o)
		return a, "", e
	case "exec":
		a, e := execArgs(o)
		return a, "", e
	case "rollout":
		a, e := rolloutArgs(o)
		return a, "", e
	case "scale":
		a, e := scaleArgs(o)
		return a, "", e
	case "patch":
		a, e := patchArgs(o)
		return a, "", e
	case "create":
		return createArgs(o)
	case "label":
		a, e := labelAnnotateArgs("label", o)
		return a, "", e
	case "annotate":
		a, e := labelAnnotateArgs("annotate", o)
		return a, "", e
	case "wait":
		a, e := waitArgs(o)
		return a, "", e
	case "top":
		a, e := topArgs(o)
		return a, "", e
	case "cordon":
		node := str(o["node"])
		if node == "" {
			return nil, "", fmt.Errorf("node is required")
		}
		return []string{"cordon", node}, "", nil
	case "uncordon":
		node := str(o["node"])
		if node == "" {
			return nil, "", fmt.Errorf("node is required")
		}
		return []string{"uncordon", node}, "", nil
	case "drain":
		a, e := drainArgs(o)
		return a, "", e
	case "cp":
		a, e := cpArgs(o)
		return a, "", e
	case "cli":
		a := strList(o["args"])
		if len(a) == 0 {
			return nil, "", fmt.Errorf("args is required")
		}
		return a, "", nil
	}
	return nil, "", fmt.Errorf("unknown verb")
}

func applyArgs(o map[string]any) ([]string, string, error) {
	files := strList(o["filename"])
	manifest := str(o["manifest"])
	if len(files) == 0 && manifest == "" {
		return nil, "", fmt.Errorf("filename or manifest is required")
	}
	a := []string{"apply"}
	a = append(a, repeatFlag("-f", files)...)
	stdin := ""
	if manifest != "" {
		a = append(a, "-f", "-")
		stdin = manifest
	}
	if boolv(o["recursive"]) {
		a = append(a, "-R")
	}
	if boolv(o["prune"]) {
		a = append(a, "--prune")
	}
	if boolv(o["server_side"]) {
		a = append(a, "--server-side")
	}
	if boolv(o["force"]) {
		a = append(a, "--force")
	}
	if out := str(o["output"]); out == "json" {
		a = append(a, "-o", "json")
	}
	return a, stdin, nil
}

func deleteArgs(o map[string]any) ([]string, error) {
	resource := str(o["resource"])
	name := str(o["name"])
	files := strList(o["filename"])
	if resource == "" && len(files) == 0 {
		return nil, fmt.Errorf("resource or filename is required")
	}
	a := []string{"delete"}
	a = append(a, repeatFlag("-f", files)...)
	if resource != "" {
		a = append(a, resource)
		if name != "" {
			a = append(a, name)
		}
	}
	if s := str(o["selector"]); s != "" {
		a = append(a, "-l", s)
	}
	if boolv(o["all"]) {
		a = append(a, "--all")
	}
	if n := intStr(o["grace_period"]); n != "" {
		a = append(a, "--grace-period", n)
	}
	if boolv(o["force"]) {
		a = append(a, "--force")
	}
	if boolv(o["ignore_not_found"]) {
		a = append(a, "--ignore-not-found")
	}
	return a, nil
}

func getArgs(o map[string]any) ([]string, error) {
	resource := str(o["resource"])
	if resource == "" {
		return nil, fmt.Errorf("resource is required")
	}
	a := []string{"get", resource}
	if n := str(o["name"]); n != "" {
		a = append(a, n)
	}
	if s := str(o["selector"]); s != "" {
		a = append(a, "-l", s)
	}
	if boolv(o["all_namespaces"]) {
		a = append(a, "-A")
	}
	if fs := str(o["field_selector"]); fs != "" {
		a = append(a, "--field-selector", fs)
	}
	out := strOr(o["output"], "json")
	return append(a, "-o", out), nil
}

func describeArgs(o map[string]any) ([]string, error) {
	resource := str(o["resource"])
	if resource == "" {
		return nil, fmt.Errorf("resource is required")
	}
	a := []string{"describe", resource}
	if n := str(o["name"]); n != "" {
		a = append(a, n)
	}
	if s := str(o["selector"]); s != "" {
		a = append(a, "-l", s)
	}
	return a, nil
}

func logsArgs(o map[string]any) ([]string, error) {
	pod := str(o["pod"])
	if pod == "" {
		return nil, fmt.Errorf("pod is required")
	}
	a := []string{"logs", pod}
	if c := str(o["container"]); c != "" {
		a = append(a, "-c", c)
	}
	if n := intStr(o["tail"]); n != "" {
		a = append(a, "--tail", n)
	}
	if s := str(o["since"]); s != "" {
		a = append(a, "--since", s)
	}
	if boolv(o["previous"]) {
		a = append(a, "-p")
	}
	if boolv(o["all_containers"]) {
		a = append(a, "--all-containers")
	}
	return a, nil
}

func execArgs(o map[string]any) ([]string, error) {
	pod := str(o["pod"])
	if pod == "" {
		return nil, fmt.Errorf("pod is required")
	}
	cmd := strList(o["command"])
	if len(cmd) == 0 {
		return nil, fmt.Errorf("command is required")
	}
	a := []string{"exec"}
	if boolv(o["stdin"]) {
		a = append(a, "-i")
	}
	if boolv(o["tty"]) {
		a = append(a, "-t")
	}
	if c := str(o["container"]); c != "" {
		a = append(a, "-c", c)
	}
	a = append(a, pod, "--")
	a = append(a, cmd...)
	return a, nil
}

func rolloutArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	if sub == "" {
		return nil, fmt.Errorf("subcommand is required")
	}
	resource := str(o["resource"])
	if resource == "" {
		return nil, fmt.Errorf("resource is required")
	}
	return []string{"rollout", sub, resource}, nil
}

func scaleArgs(o map[string]any) ([]string, error) {
	resource := str(o["resource"])
	if resource == "" {
		return nil, fmt.Errorf("resource is required")
	}
	n := intStr(o["replicas"])
	if n == "" {
		return nil, fmt.Errorf("replicas is required")
	}
	return []string{"scale", resource, "--replicas", n}, nil
}

func patchArgs(o map[string]any) ([]string, error) {
	resource := str(o["resource"])
	name := str(o["name"])
	patch := str(o["patch"])
	if resource == "" || name == "" || patch == "" {
		return nil, fmt.Errorf("resource, name, and patch are required")
	}
	a := []string{"patch", resource, name, "-p", patch}
	if t := str(o["type"]); t != "" {
		a = append(a, "--type", t)
	}
	return a, nil
}

func createArgs(o map[string]any) ([]string, string, error) {
	files := strList(o["filename"])
	manifest := str(o["manifest"])
	extra := strList(o["extra_args"])
	if len(files) == 0 && manifest == "" && len(extra) == 0 {
		return nil, "", fmt.Errorf("filename, manifest, or extra_args is required")
	}
	a := []string{"create"}
	a = append(a, repeatFlag("-f", files)...)
	stdin := ""
	if manifest != "" {
		a = append(a, "-f", "-")
		stdin = manifest
	}
	a = append(a, extra...)
	return a, stdin, nil
}

func labelAnnotateArgs(verb string, o map[string]any) ([]string, error) {
	resource := str(o["resource"])
	name := str(o["name"])
	if resource == "" || name == "" {
		return nil, fmt.Errorf("resource and name are required")
	}
	pairs := kvArgs(o["pairs"])
	if len(pairs) == 0 {
		return nil, fmt.Errorf("pairs is required")
	}
	a := []string{verb, resource, name}
	a = append(a, pairs...)
	if boolv(o["overwrite"]) {
		a = append(a, "--overwrite")
	}
	return a, nil
}

func waitArgs(o map[string]any) ([]string, error) {
	resource := str(o["resource"])
	if resource == "" {
		return nil, fmt.Errorf("resource is required")
	}
	forCond := str(o["for"])
	if forCond == "" {
		return nil, fmt.Errorf("for is required")
	}
	a := []string{"wait", resource}
	if n := str(o["name"]); n != "" {
		a = append(a, n)
	}
	if s := str(o["selector"]); s != "" {
		a = append(a, "-l", s)
	}
	a = append(a, "--for", forCond)
	if t := str(o["timeout"]); t != "" {
		a = append(a, "--timeout", t)
	}
	return a, nil
}

func topArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	if sub == "" {
		return nil, fmt.Errorf("subcommand is required")
	}
	a := []string{"top", sub}
	if n := str(o["name"]); n != "" {
		a = append(a, n)
	}
	if s := str(o["selector"]); s != "" {
		a = append(a, "-l", s)
	}
	return a, nil
}

func drainArgs(o map[string]any) ([]string, error) {
	node := str(o["node"])
	if node == "" {
		return nil, fmt.Errorf("node is required")
	}
	a := []string{"drain", node}
	if boolv(o["ignore_daemonsets"]) {
		a = append(a, "--ignore-daemonsets")
	}
	if boolv(o["delete_emptydir_data"]) {
		a = append(a, "--delete-emptydir-data")
	}
	if boolv(o["force"]) {
		a = append(a, "--force")
	}
	return a, nil
}

func cpArgs(o map[string]any) ([]string, error) {
	src := str(o["src"])
	dst := str(o["dst"])
	if src == "" || dst == "" {
		return nil, fmt.Errorf("src and dst are required")
	}
	a := []string{"cp", src, dst}
	if c := str(o["container"]); c != "" {
		a = append(a, "-c", c)
	}
	return a, nil
}

// enrich adds verb-specific structured outputs, only when the command
// succeeded (exit_code 0) so we never parse an error stream.
func enrich(verb string, o, res map[string]any) {
	ok := res["exit_code"] == 0
	stdout, _ := res["stdout"].(string)
	switch verb {
	case "apply":
		if ok && str(o["output"]) == "json" {
			if v := jsonAny(stdout); v != nil {
				res["result"] = v
			}
		}
	case "get":
		if ok && strOr(o["output"], "json") == "json" {
			if v := jsonAny(stdout); v != nil {
				res["result"] = v
			}
		}
	}
}

// runKubectl spawns the kubectl binary with connFlags + args and captures the
// result. A non-zero exit is returned as exit_code, not an error; only a
// failure to start the process (missing binary, timeout) is an error.
func runKubectl(ctx context.Context, conn kubernetesConn, args []string, stdin string) (map[string]any, error) {
	full := append(conn.connFlags(), args...)
	cmd := exec.CommandContext(ctx, conn.binary, full...)
	cmd.Env = conn.procEnv()
	if stdin != "" {
		cmd.Stdin = io.NopCloser(strings.NewReader(stdin))
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
	if err := plugin.Serve(kubernetesPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-kubernetes: %v\n", err)
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

// kvArgs renders a map into bare `key=value` positional args (sorted), for
// label/annotate which take them without a preceding flag.
func kvArgs(v any) []string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return out
}

// jsonAny parses a JSON document (object or array) into a generic value; nil on
// failure so a caller can leave the output unset.
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

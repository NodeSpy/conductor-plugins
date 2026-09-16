// Command conductor-docker is a verb-only conductor connector (#59) that drives
// the local or remote container engine by shelling out to the `docker` (or
// `podman`) CLI. It exposes the container lifecycle as verbs — run/exec/build/
// pull/push/ps/images/logs/stop/start/rm/inspect — plus `compose`, `buildx`,
// and `bake`, and a `cli` escape hatch for any subcommand a first-class verb
// does not cover. Built ONLY against the public SDK.
//
// Remote engines are reached through docker's OWN remoting rather than any SSH
// re-implementation: set `docker_host: ssh://user@host` (or tcp://, unix://),
// or `context: <name>`. `engine: podman` swaps the binary (podman's CLI is
// docker-compatible); `buildx`/`bake` are docker-only and refuse podman.
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

type dockerPlugin struct{}

func (dockerPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "docker",
		Desc: "Docker/Podman: run containers and the full engine lifecycle as verbs (run, exec, build, pull, push, ps, logs, stop, start, rm, inspect, compose, buildx, bake, cli). Local by default; docker_host/context reach a remote engine. Shells out to the docker (or podman) CLI.",
		Connection: plugin.Schema{
			"engine":      {Type: "string", Enum: []string{"docker", "podman"}, Desc: "container engine (default docker)"},
			"binary":      {Type: "string", Desc: "override the CLI binary path (default = engine name)"},
			"docker_host": {Type: "string", Desc: "sets DOCKER_HOST — ssh://user@host, tcp://host:2376, unix:///path.sock"},
			"context":     {Type: "string", Desc: "docker --context <name> (uses ~/.docker/contexts)"},
			"tls_verify":  {Type: "boolean", Desc: "sets DOCKER_TLS_VERIFY=1 (TLS to a tcp:// engine)"},
			"cert_path":   {Type: "string", Desc: "sets DOCKER_CERT_PATH (client certs for a tcp:// engine)"},
			"env":         {Type: "map", Desc: "default process environment for every invocation"},
			"timeout":     {Type: "duration", Desc: "default per-verb timeout (default 10m); a verb's timeout option overrides it"},
		},
		Verbs:        dockerVerbs(),
		Capabilities: plugin.Capabilities{Commands: []string{"docker", "podman"}, Spawns: true},
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

func dockerVerbs() []plugin.Verb {
	out := stdOutputs()
	withID := plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "container_id": {Type: "string", Desc: "set when detach is true"}}
	withImage := plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "image": {Type: "string", Desc: "the built tag"}}
	return []plugin.Verb{
		{
			Name: "run", Desc: "run a container from an image",
			Usage: "start a container: use detach for a service, remove for a one-shot job",
			Options: plugin.Schema{
				"image":      {Type: "string", Required: true},
				"cmd":        {Type: "list", Desc: "argv passed to the container"},
				"env":        {Type: "map", Desc: "-e KEY=VALUE container env"},
				"volumes":    {Type: "list", Desc: "-v mounts, e.g. /src:/dst"},
				"ports":      {Type: "list", Desc: "-p publishes, e.g. 8080:80"},
				"workdir":    {Type: "string", Desc: "-w working directory"},
				"name":       {Type: "string", Desc: "--name"},
				"detach":     {Type: "boolean", Desc: "-d run in background; stdout is the container id"},
				"remove":     {Type: "boolean", Desc: "--rm remove on exit"},
				"network":    {Type: "string", Desc: "--network"},
				"entrypoint": {Type: "string", Desc: "--entrypoint"},
				"user":       {Type: "string", Desc: "--user"},
				"platform":   {Type: "string", Desc: "--platform"},
				"pull":       {Type: "string", Enum: []string{"always", "missing", "never"}, Desc: "--pull policy"},
				"extra_args": {Type: "list", Desc: "raw flags inserted before the image"},
			},
			Outputs: withID,
		},
		{
			Name: "exec", Desc: "run a command in a running container",
			Options: plugin.Schema{
				"container": {Type: "string", Required: true, Scope: "container"},
				"cmd":       {Type: "list", Required: true, Desc: "argv to execute"},
				"env":       {Type: "map"},
				"workdir":   {Type: "string"},
				"user":      {Type: "string"},
				"detach":    {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "build", Desc: "build an image from a context directory",
			Options: plugin.Schema{
				"context":    {Type: "string", Desc: "build context (default .)"},
				"dockerfile": {Type: "string", Desc: "-f path to the Dockerfile"},
				"tag":        {Type: "string", Desc: "-t name:tag"},
				"build_args": {Type: "map", Desc: "--build-arg KEY=VALUE"},
				"target":     {Type: "string", Desc: "--target stage"},
				"platform":   {Type: "string"},
				"pull":       {Type: "boolean", Desc: "--pull always fetch a newer base"},
				"no_cache":   {Type: "boolean", Desc: "--no-cache"},
				"extra_args": {Type: "list"},
			},
			Outputs: withImage,
		},
		{
			Name: "pull", Desc: "pull an image",
			Options: plugin.Schema{"image": {Type: "string", Required: true}, "platform": {Type: "string"}},
			Outputs: out,
		},
		{
			Name: "push", Desc: "push an image",
			Options: plugin.Schema{"image": {Type: "string", Required: true}},
			Outputs: out,
		},
		{
			Name: "ps", Desc: "list containers (parsed into containers[])",
			Options: plugin.Schema{
				"all":    {Type: "boolean", Desc: "-a include stopped"},
				"filter": {Type: "list", Desc: "--filter key=value entries"},
				"limit":  {Type: "integer", Desc: "-n last N"},
			},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "containers": {Type: "list"}},
		},
		{
			Name: "images", Desc: "list images (parsed into images[])",
			Options: plugin.Schema{"all": {Type: "boolean"}, "filter": {Type: "list"}},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "images": {Type: "list"}},
		},
		{
			Name: "logs", Desc: "fetch a container's logs",
			Options: plugin.Schema{
				"container":  {Type: "string", Required: true, Scope: "container"},
				"tail":       {Type: "string", Desc: "--tail N (or all)"},
				"since":      {Type: "string", Desc: "--since"},
				"timestamps": {Type: "boolean", Desc: "-t"},
			},
			Outputs: out,
		},
		{
			Name: "stop", Desc: "stop one or more containers",
			Options: plugin.Schema{"container": {Type: "any", Required: true, Scope: "container", Desc: "name or list of names"}, "timeout": {Type: "integer", Desc: "-t seconds before kill"}},
			Outputs: out,
		},
		{
			Name: "start", Desc: "start one or more stopped containers",
			Options: plugin.Schema{"container": {Type: "any", Required: true, Scope: "container"}},
			Outputs: out,
		},
		{
			Name: "rm", Desc: "remove one or more containers",
			Options: plugin.Schema{"container": {Type: "any", Required: true, Scope: "container"}, "force": {Type: "boolean", Desc: "-f"}, "volumes": {Type: "boolean", Desc: "-v anonymous volumes"}},
			Outputs: out,
		},
		{
			Name: "inspect", Desc: "low-level info on containers/images (parsed into inspected[])",
			Options: plugin.Schema{"target": {Type: "any", Required: true, Desc: "name or list"}, "type": {Type: "string", Enum: []string{"container", "image", "network", "volume"}}},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "inspected": {Type: "list"}},
		},
		{
			Name: "compose", Desc: "docker compose subcommand (up/down/ps/logs/build/…)",
			Usage: "orchestrate a compose project; global flags (files/project) precede the subcommand",
			Options: plugin.Schema{
				"subcommand":        {Type: "string", Required: true, Enum: []string{"up", "down", "start", "stop", "restart", "ps", "logs", "build", "pull", "push", "config", "run", "exec", "create", "rm", "kill", "pause", "unpause"}},
				"files":             {Type: "list", Desc: "-f compose file(s)"},
				"project":           {Type: "string", Desc: "-p project name"},
				"project_directory": {Type: "string", Desc: "--project-directory"},
				"env_files":         {Type: "list", Desc: "--env-file(s)"},
				"profiles":          {Type: "list", Desc: "--profile(s)"},
				"services":          {Type: "list", Desc: "target services (appended last)"},
				"detach":            {Type: "boolean", Desc: "-d (up/run/exec)"},
				"build":             {Type: "boolean", Desc: "--build (up)"},
				"remove_orphans":    {Type: "boolean", Desc: "--remove-orphans (up/down)"},
				"volumes":           {Type: "boolean", Desc: "-v (down)"},
				"tail":              {Type: "string", Desc: "--tail (logs)"},
				"since":             {Type: "string", Desc: "--since (logs)"},
				"timestamps":        {Type: "boolean", Desc: "-t (logs)"},
				"no_cache":          {Type: "boolean", Desc: "--no-cache (build)"},
				"pull":              {Type: "boolean", Desc: "--pull (build)"},
				"json":              {Type: "boolean", Desc: "--format json (ps/config), parsed into containers/config"},
				"extra_args":        {Type: "list"},
			},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "containers": {Type: "list", Desc: "parsed ps --format json"}, "config": {Type: "any", Desc: "parsed config --format json"}},
		},
		{
			Name: "buildx", Desc: "docker buildx build (multi-platform, push/load, cache) — docker only",
			Options: plugin.Schema{
				"context":    {Type: "string", Desc: "build context (default .)"},
				"dockerfile": {Type: "string", Desc: "-f"},
				"tag":        {Type: "string", Desc: "-t"},
				"platform":   {Type: "list", Desc: "--platform (repeatable)"},
				"push":       {Type: "boolean", Desc: "--push"},
				"load":       {Type: "boolean", Desc: "--load"},
				"builder":    {Type: "string", Desc: "--builder"},
				"cache_from": {Type: "list", Desc: "--cache-from (repeatable)"},
				"cache_to":   {Type: "list", Desc: "--cache-to (repeatable)"},
				"output":     {Type: "string", Desc: "--output"},
				"build_args": {Type: "map"},
				"target":     {Type: "string"},
				"no_cache":   {Type: "boolean"},
				"pull":       {Type: "boolean"},
				"extra_args": {Type: "list"},
			},
			Outputs: withImage,
		},
		{
			Name: "bake", Desc: "docker buildx bake (build targets from an HCL/JSON/compose file) — docker only",
			Options: plugin.Schema{
				"files":      {Type: "list", Desc: "-f bake file(s)"},
				"targets":    {Type: "list", Desc: "target(s) to build (appended last)"},
				"set":        {Type: "any", Desc: "--set overrides: map (key=value) or list of raw KEY=VALUE"},
				"push":       {Type: "boolean"},
				"load":       {Type: "boolean"},
				"print":      {Type: "boolean", Desc: "--print the resolved definition (parsed into metadata)"},
				"builder":    {Type: "string"},
				"no_cache":   {Type: "boolean"},
				"pull":       {Type: "boolean"},
				"extra_args": {Type: "list"},
			},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"}, "metadata": {Type: "any", Desc: "parsed --print output"}},
		},
		{
			Name: "cli", Desc: "run any engine subcommand: docker <args…>",
			Usage:   "escape hatch for a subcommand without a first-class verb",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after the binary"}},
			Outputs: out,
		},
	}
}

// dockerConn is the resolved connection config for one invocation.
type dockerConn struct {
	binary     string
	engine     string
	dockerHost string
	context    string
	tlsVerify  bool
	certPath   string
	env        map[string]string
	timeout    time.Duration
}

func parseConn(m map[string]any) (dockerConn, error) {
	c := dockerConn{
		engine:     strOr(m["engine"], "docker"),
		dockerHost: str(m["docker_host"]),
		context:    str(m["context"]),
		tlsVerify:  boolv(m["tls_verify"]),
		certPath:   str(m["cert_path"]),
		env:        strMap(m["env"]),
		timeout:    10 * time.Minute,
	}
	if c.engine != "docker" && c.engine != "podman" {
		return c, fmt.Errorf("engine must be docker or podman, got %q", c.engine)
	}
	c.binary = strOr(m["binary"], c.engine)
	if d, err := toDuration(m["timeout"]); err != nil {
		return c, fmt.Errorf("connection.timeout: %w", err)
	} else if d > 0 {
		c.timeout = d
	}
	return c, nil
}

// connFlags are the top-level engine flags that precede any subcommand.
func (c dockerConn) connFlags() []string {
	if c.context != "" {
		return []string{"--context", c.context}
	}
	return nil
}

func (c dockerConn) procEnv() []string {
	env := os.Environ()
	keys := make([]string, 0, len(c.env))
	for k := range c.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+c.env[k])
	}
	if c.dockerHost != "" {
		env = append(env, "DOCKER_HOST="+c.dockerHost)
	}
	if c.tlsVerify {
		env = append(env, "DOCKER_TLS_VERIFY=1")
	}
	if c.certPath != "" {
		env = append(env, "DOCKER_CERT_PATH="+c.certPath)
	}
	return env
}

func (dockerPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	// buildx/bake are docker-only — refuse before spawning podman.
	if (req.Verb == "buildx" || req.Verb == "bake") && conn.engine != "docker" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+" requires engine docker (podman has no buildx)")
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

	res, err := runDocker(ctx, conn, args)
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
	case "run":
		return runArgs(o)
	case "exec":
		return execArgs(o)
	case "build":
		return buildArgs(o)
	case "pull":
		img := str(o["image"])
		if img == "" {
			return nil, fmt.Errorf("image is required")
		}
		return append(append([]string{"pull"}, repeatFlag("--platform", strList(o["platform"]))...), img), nil
	case "push":
		img := str(o["image"])
		if img == "" {
			return nil, fmt.Errorf("image is required")
		}
		return []string{"push", img}, nil
	case "ps":
		return psArgs(o), nil
	case "images":
		return imagesArgs(o), nil
	case "logs":
		return logsArgs(o)
	case "stop":
		return stopArgs(o)
	case "start":
		names := strList(o["container"])
		if len(names) == 0 {
			return nil, fmt.Errorf("container is required")
		}
		return append([]string{"start"}, names...), nil
	case "rm":
		return rmArgs(o)
	case "inspect":
		return inspectArgs(o)
	case "compose":
		return composeArgs(o)
	case "buildx":
		return buildxArgs(o)
	case "bake":
		return bakeArgs(o)
	case "cli":
		args := strList(o["args"])
		if len(args) == 0 {
			return nil, fmt.Errorf("args is required")
		}
		return args, nil
	}
	return nil, fmt.Errorf("unknown verb")
}

func runArgs(o map[string]any) ([]string, error) {
	img := str(o["image"])
	if img == "" {
		return nil, fmt.Errorf("image is required")
	}
	a := []string{"run"}
	if boolv(o["remove"]) {
		a = append(a, "--rm")
	}
	if boolv(o["detach"]) {
		a = append(a, "-d")
	}
	if n := str(o["name"]); n != "" {
		a = append(a, "--name", n)
	}
	a = append(a, kvFlags("-e", o["env"])...)
	a = append(a, repeatFlag("-v", strList(o["volumes"]))...)
	a = append(a, repeatFlag("-p", strList(o["ports"]))...)
	if w := str(o["workdir"]); w != "" {
		a = append(a, "-w", w)
	}
	if u := str(o["user"]); u != "" {
		a = append(a, "--user", u)
	}
	if n := str(o["network"]); n != "" {
		a = append(a, "--network", n)
	}
	if e := str(o["entrypoint"]); e != "" {
		a = append(a, "--entrypoint", e)
	}
	if p := str(o["platform"]); p != "" {
		a = append(a, "--platform", p)
	}
	if p := str(o["pull"]); p != "" {
		a = append(a, "--pull", p)
	}
	a = append(a, strList(o["extra_args"])...)
	a = append(a, img)
	a = append(a, strList(o["cmd"])...)
	return a, nil
}

func execArgs(o map[string]any) ([]string, error) {
	name := str(o["container"])
	if name == "" {
		return nil, fmt.Errorf("container is required")
	}
	cmd := strList(o["cmd"])
	if len(cmd) == 0 {
		return nil, fmt.Errorf("cmd is required")
	}
	a := []string{"exec"}
	if boolv(o["detach"]) {
		a = append(a, "-d")
	}
	a = append(a, kvFlags("-e", o["env"])...)
	if w := str(o["workdir"]); w != "" {
		a = append(a, "-w", w)
	}
	if u := str(o["user"]); u != "" {
		a = append(a, "--user", u)
	}
	a = append(a, name)
	a = append(a, cmd...)
	return a, nil
}

func buildArgs(o map[string]any) ([]string, error) {
	a := []string{"build"}
	if f := str(o["dockerfile"]); f != "" {
		a = append(a, "-f", f)
	}
	if t := str(o["tag"]); t != "" {
		a = append(a, "-t", t)
	}
	a = append(a, kvFlags("--build-arg", o["build_args"])...)
	if t := str(o["target"]); t != "" {
		a = append(a, "--target", t)
	}
	if p := str(o["platform"]); p != "" {
		a = append(a, "--platform", p)
	}
	if boolv(o["pull"]) {
		a = append(a, "--pull")
	}
	if boolv(o["no_cache"]) {
		a = append(a, "--no-cache")
	}
	a = append(a, strList(o["extra_args"])...)
	a = append(a, strOr(o["context"], "."))
	return a, nil
}

func psArgs(o map[string]any) []string {
	a := []string{"ps"}
	if boolv(o["all"]) {
		a = append(a, "-a")
	}
	a = append(a, repeatFlag("--filter", strList(o["filter"]))...)
	if n := intStr(o["limit"]); n != "" {
		a = append(a, "-n", n)
	}
	return append(a, "--format", "{{json .}}")
}

func imagesArgs(o map[string]any) []string {
	a := []string{"images"}
	if boolv(o["all"]) {
		a = append(a, "-a")
	}
	a = append(a, repeatFlag("--filter", strList(o["filter"]))...)
	return append(a, "--format", "{{json .}}")
}

func logsArgs(o map[string]any) ([]string, error) {
	name := str(o["container"])
	if name == "" {
		return nil, fmt.Errorf("container is required")
	}
	a := []string{"logs"}
	if t := str(o["tail"]); t != "" {
		a = append(a, "--tail", t)
	}
	if s := str(o["since"]); s != "" {
		a = append(a, "--since", s)
	}
	if boolv(o["timestamps"]) {
		a = append(a, "-t")
	}
	return append(a, name), nil
}

func rmArgs(o map[string]any) ([]string, error) {
	names := strList(o["container"])
	if len(names) == 0 {
		return nil, fmt.Errorf("container is required")
	}
	a := []string{"rm"}
	if boolv(o["force"]) {
		a = append(a, "-f")
	}
	if boolv(o["volumes"]) {
		a = append(a, "-v")
	}
	return append(a, names...), nil
}

// stopArgs handles `stop` — an optional -t timeout then the container names.
func stopArgs(o map[string]any) ([]string, error) {
	names := strList(o["container"])
	if len(names) == 0 {
		return nil, fmt.Errorf("container is required")
	}
	a := []string{"stop"}
	if n := intStr(o["timeout"]); n != "" {
		a = append(a, "-t", n)
	}
	return append(a, names...), nil
}

func inspectArgs(o map[string]any) ([]string, error) {
	targets := strList(o["target"])
	if len(targets) == 0 {
		return nil, fmt.Errorf("target is required")
	}
	a := []string{"inspect"}
	if t := str(o["type"]); t != "" {
		a = append(a, "--type", t)
	}
	return append(a, targets...), nil
}

func composeArgs(o map[string]any) ([]string, error) {
	sub := str(o["subcommand"])
	if sub == "" {
		return nil, fmt.Errorf("subcommand is required")
	}
	a := []string{"compose"}
	// Global compose flags precede the subcommand.
	a = append(a, repeatFlag("-f", strList(o["files"]))...)
	if p := str(o["project"]); p != "" {
		a = append(a, "-p", p)
	}
	if d := str(o["project_directory"]); d != "" {
		a = append(a, "--project-directory", d)
	}
	a = append(a, repeatFlag("--env-file", strList(o["env_files"]))...)
	a = append(a, repeatFlag("--profile", strList(o["profiles"]))...)
	a = append(a, sub)
	// Curated subcommand flags, applied only where the subcommand accepts them.
	switch sub {
	case "up", "run", "exec":
		if boolv(o["detach"]) {
			a = append(a, "-d")
		}
	}
	if sub == "up" && boolv(o["build"]) {
		a = append(a, "--build")
	}
	switch sub {
	case "up", "down":
		if boolv(o["remove_orphans"]) {
			a = append(a, "--remove-orphans")
		}
	}
	if sub == "down" && boolv(o["volumes"]) {
		a = append(a, "-v")
	}
	if sub == "logs" {
		if t := str(o["tail"]); t != "" {
			a = append(a, "--tail", t)
		}
		if s := str(o["since"]); s != "" {
			a = append(a, "--since", s)
		}
		if boolv(o["timestamps"]) {
			a = append(a, "-t")
		}
	}
	if sub == "build" {
		if boolv(o["no_cache"]) {
			a = append(a, "--no-cache")
		}
		if boolv(o["pull"]) {
			a = append(a, "--pull")
		}
	}
	if (sub == "ps" || sub == "config") && boolv(o["json"]) {
		a = append(a, "--format", "json")
	}
	a = append(a, strList(o["extra_args"])...)
	a = append(a, strList(o["services"])...)
	return a, nil
}

func buildxArgs(o map[string]any) ([]string, error) {
	a := []string{"buildx", "build"}
	if b := str(o["builder"]); b != "" {
		a = append(a, "--builder", b)
	}
	if f := str(o["dockerfile"]); f != "" {
		a = append(a, "-f", f)
	}
	if t := str(o["tag"]); t != "" {
		a = append(a, "-t", t)
	}
	a = append(a, repeatFlag("--platform", strList(o["platform"]))...)
	if boolv(o["push"]) {
		a = append(a, "--push")
	}
	if boolv(o["load"]) {
		a = append(a, "--load")
	}
	a = append(a, repeatFlag("--cache-from", strList(o["cache_from"]))...)
	a = append(a, repeatFlag("--cache-to", strList(o["cache_to"]))...)
	if out := str(o["output"]); out != "" {
		a = append(a, "--output", out)
	}
	a = append(a, kvFlags("--build-arg", o["build_args"])...)
	if t := str(o["target"]); t != "" {
		a = append(a, "--target", t)
	}
	if boolv(o["no_cache"]) {
		a = append(a, "--no-cache")
	}
	if boolv(o["pull"]) {
		a = append(a, "--pull")
	}
	a = append(a, strList(o["extra_args"])...)
	a = append(a, strOr(o["context"], "."))
	return a, nil
}

func bakeArgs(o map[string]any) ([]string, error) {
	a := []string{"buildx", "bake"}
	a = append(a, repeatFlag("-f", strList(o["files"]))...)
	if b := str(o["builder"]); b != "" {
		a = append(a, "--builder", b)
	}
	a = append(a, setFlags(o["set"])...)
	if boolv(o["push"]) {
		a = append(a, "--push")
	}
	if boolv(o["load"]) {
		a = append(a, "--load")
	}
	if boolv(o["print"]) {
		a = append(a, "--print")
	}
	if boolv(o["no_cache"]) {
		a = append(a, "--no-cache")
	}
	if boolv(o["pull"]) {
		a = append(a, "--pull")
	}
	a = append(a, strList(o["extra_args"])...)
	a = append(a, strList(o["targets"])...)
	return a, nil
}

// enrich adds verb-specific structured outputs, only when the command
// succeeded (exit_code 0) so we never parse an error stream.
func enrich(verb string, o, res map[string]any) {
	ok := res["exit_code"] == 0
	stdout, _ := res["stdout"].(string)
	switch verb {
	case "run":
		if boolv(o["detach"]) && ok {
			res["container_id"] = strings.TrimSpace(stdout)
		}
	case "build", "buildx":
		if t := str(o["tag"]); t != "" && ok {
			res["image"] = t
		}
	case "ps":
		if ok {
			res["containers"] = jsonLines(stdout)
		}
	case "images":
		if ok {
			res["images"] = jsonLines(stdout)
		}
	case "inspect":
		if ok {
			if v := jsonAny(stdout); v != nil {
				res["inspected"] = v
			}
		}
	case "compose":
		if !ok {
			return
		}
		switch str(o["subcommand"]) {
		case "ps":
			if boolv(o["json"]) {
				res["containers"] = composePS(stdout)
			}
		case "config":
			if boolv(o["json"]) {
				if v := jsonAny(stdout); v != nil {
					res["config"] = v
				}
			}
		}
	case "bake":
		if boolv(o["print"]) && ok {
			if v := jsonAny(stdout); v != nil {
				res["metadata"] = v
			}
		}
	}
}

// runDocker spawns the engine binary with connFlags + args and captures the
// result. A non-zero exit is returned as exit_code, not an error; only a
// failure to start the process (missing binary, timeout) is an error.
func runDocker(ctx context.Context, conn dockerConn, args []string) (map[string]any, error) {
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
	if err := plugin.Serve(dockerPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-docker: %v\n", err)
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

// setFlags renders bake's --set overrides from a map (KEY=VALUE, sorted) or a
// list of raw KEY=VALUE strings.
func setFlags(v any) []string {
	switch v.(type) {
	case map[string]any:
		return kvFlags("--set", v)
	default:
		return repeatFlag("--set", strList(v))
	}
}

// jsonLines parses newline-delimited `{{json .}}` output into objects,
// skipping blank or unparseable lines.
func jsonLines(s string) []map[string]any {
	out := []map[string]any{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			out = append(out, m)
		}
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

// composePS parses `docker compose ps --format json`, which is a JSON array on
// newer engines and newline-delimited objects on older ones.
func composePS(s string) []map[string]any {
	if v, ok := jsonAny(s).([]any); ok {
		out := make([]map[string]any, 0, len(v))
		for _, e := range v {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return jsonLines(s)
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

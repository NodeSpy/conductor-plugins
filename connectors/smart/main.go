// Command conductor-smart is a conductor connector (#59) that reports disk
// S.M.A.R.T. health by shelling out to `smartctl` (smartmontools). It exposes
// the tool as verbs — scan/info/health/attributes/all/capabilities/test/log —
// plus a `cli` escape hatch, and a POLL SOURCE that watches a set of devices
// and emits a `health` event when one fails its self-assessment. Built ONLY
// against the public SDK + connector-kit.
//
// Two smartctl facts shape this connector:
//
//   - Its exit status is a BITMASK, not a success/failure flag: bit 3 (8) means
//     "the disk is failing", which is exactly the case an operator cares about.
//     A non-zero exit is therefore DATA — returned as exit_code, with the JSON
//     still parsed — never an invocation error.
//   - Reading a device needs raw access, so `sudo: true` prepends sudo rather
//     than requiring the daemon itself to run as root.
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
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type smartPlugin struct{}

func (smartPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "smart",
		Desc: "Disk S.M.A.R.T. health via smartctl (smartmontools): scan, info, health, attributes, all, capabilities, test, log, cli — plus a poll source that emits a health event when a device fails its self-assessment. smartctl's exit status is a bitmask, so a non-zero exit is data, not an error.",
		Connection: plugin.Schema{
			"binary":        {Type: "string", Desc: "smartctl binary path (default smartctl)"},
			"sudo":          {Type: "boolean", Desc: "prepend sudo — reading a device needs raw access"},
			"devices":       {Type: "list", Desc: "devices the source polls, e.g. [/dev/sda, /dev/nvme0]; empty = discover with --scan-open"},
			"poll_interval": {Type: "duration", Desc: "source poll period (default 5m)"},
			"env":           {Type: "map", Desc: "default process environment for every invocation"},
			"timeout":       {Type: "duration", Desc: "default per-verb timeout (default 2m); a verb's timeout option overrides it"},
		},
		Verbs: smartVerbs(),
		Events: []plugin.Event{
			{
				Name: "health", Desc: "a device FAILED its S.M.A.R.T. self-assessment",
				Filters: plugin.Schema{
					"devices": {Type: "list", Desc: "match if the device is one of these"},
					"device":  {Type: "string", Desc: "match if the device equals this"},
					"models":  {Type: "list", Desc: "match if model_name is one of these"},
					"model":   {Type: "string", Desc: "match if model_name equals this"},
				},
				Context: plugin.Schema{
					"device":              {Type: "string"},
					"model_name":          {Type: "string"},
					"serial_number":       {Type: "string"},
					"passed":              {Type: "boolean"},
					"temperature_c":       {Type: "integer"},
					"power_on_hours":      {Type: "integer"},
					"reallocated_sectors": {Type: "integer"},
					"health":              {Type: "string"},
				},
			},
		},
		// Every verb is a smartctl shell-out, optionally through sudo. It
		// dials nothing.
		Capabilities: plugin.Capabilities{Commands: []string{"smartctl", "sudo"}, Spawns: true},
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

// jsonOutputs is stdOutputs plus the parsed `--json` document. Every verb but
// `cli` runs with --json, so every verb but `cli` carries it.
func jsonOutputs() plugin.Schema {
	s := stdOutputs()
	s["result"] = plugin.Field{Type: "any", Desc: "the parsed --json document"}
	return s
}

func smartVerbs() []plugin.Verb {
	out := jsonOutputs()
	dev := plugin.Field{Type: "string", Required: true, Scope: "device", Desc: "device node, e.g. /dev/sda"}
	return []plugin.Verb{
		{
			Name: "scan", Desc: "enumerate the devices smartctl can see",
			Usage:   "discover device nodes before pointing the other verbs at one",
			Options: plugin.Schema{"open": {Type: "boolean", Desc: "--scan-open (open each device to identify it) instead of --scan"}},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"},
				"result": {Type: "any", Desc: "the parsed --json document"}, "devices": {Type: "list", Desc: "the scanned device names"}},
		},
		{
			Name: "info", Desc: "device identity: model, serial, firmware, capacity (-i)",
			Options: plugin.Schema{"device": dev},
			Outputs: out,
		},
		{
			Name: "health", Desc: "the overall-health self-assessment (-H)",
			Usage:   "the cheap check: passed is false when the drive predicts its own failure",
			Options: plugin.Schema{"device": dev},
			Outputs: plugin.Schema{"stdout": {Type: "string"}, "stderr": {Type: "string"}, "exit_code": {Type: "integer"},
				"result": {Type: "any", Desc: "the parsed --json document"}, "passed": {Type: "boolean", Desc: "smart_status.passed"}},
		},
		{
			Name: "attributes", Desc: "the vendor S.M.A.R.T. attribute table (-A)",
			Options: plugin.Schema{"device": dev},
			Outputs: out,
		},
		{
			Name: "all", Desc: "everything smartctl knows about the device (-a, or -x with extended)",
			Options: plugin.Schema{
				"device":   dev,
				"extended": {Type: "boolean", Desc: "-x (adds device-statistics and SCSI/SATA logs) instead of -a"},
			},
			Outputs: out,
		},
		{
			Name: "capabilities", Desc: "the device's S.M.A.R.T. capabilities and test timings (-c)",
			Options: plugin.Schema{"device": dev},
			Outputs: out,
		},
		{
			Name: "test", Desc: "start a self-test (-t)",
			Usage: "starts the test and returns immediately; poll `log` with type selftest for the result",
			Options: plugin.Schema{
				"device": dev,
				"type":   {Type: "string", Required: true, Enum: []string{"short", "long", "conveyance", "offline"}, Desc: "-t <type>"},
			},
			Outputs: out,
		},
		{
			Name: "log", Desc: "read a device log (-l), e.g. error or selftest",
			Options: plugin.Schema{
				"device": dev,
				"type":   {Type: "string", Desc: "-l <type> — error (default), selftest, devstat, scttemp, …"},
			},
			Outputs: out,
		},
		{
			Name: "cli", Desc: "run smartctl with raw arguments: smartctl <args…>",
			Usage:   "escape hatch for a flag without a first-class verb; --json is NOT added",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after the binary"}},
			Outputs: stdOutputs(),
		},
	}
}

// smartConn is the resolved connection config for one invocation or source.
type smartConn struct {
	binary       string
	sudo         bool
	devices      []string
	pollInterval time.Duration
	env          map[string]string
	timeout      time.Duration
}

func parseConn(m map[string]any) (smartConn, error) {
	c := smartConn{
		binary:       strOr(m["binary"], "smartctl"),
		sudo:         boolv(m["sudo"]),
		devices:      strList(m["devices"]),
		pollInterval: 5 * time.Minute,
		env:          strMap(m["env"]),
		timeout:      2 * time.Minute,
	}
	if d, err := toDuration(m["timeout"]); err != nil {
		return c, fmt.Errorf("connection.timeout: %w", err)
	} else if d > 0 {
		c.timeout = d
	}
	if d, err := toDuration(m["poll_interval"]); err != nil {
		return c, fmt.Errorf("connection.poll_interval: %w", err)
	} else if d > 0 {
		c.pollInterval = d
	}
	return c, nil
}

// argv is the full command line: the binary and its arguments, prefixed with
// sudo when the operator asked for it. Pure, so the sudo contract is testable.
func (c smartConn) argv(args []string) []string {
	head := []string{c.binary}
	if c.sudo {
		head = []string{"sudo", c.binary}
	}
	return append(head, args...)
}

func (c smartConn) procEnv() []string {
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

func (smartPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
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

	res, err := runSmartctl(ctx, conn, args)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	enrich(req.Verb, res)
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary. Every verb but `cli` is prefixed
// with --json so the output is structured; `cli` is raw by definition. Pure and
// hermetically testable — no process is spawned here.
func verbArgs(verb string, o map[string]any) ([]string, error) {
	if verb == "cli" {
		args := strList(o["args"])
		if len(args) == 0 {
			return nil, fmt.Errorf("args is required")
		}
		return args, nil
	}
	var flags []string
	switch verb {
	case "scan":
		flags = []string{"--scan"}
		if boolv(o["open"]) {
			flags = []string{"--scan-open"}
		}
	case "info":
		return deviceArgs(o, "-i")
	case "health":
		return deviceArgs(o, "-H")
	case "attributes":
		return deviceArgs(o, "-A")
	case "capabilities":
		return deviceArgs(o, "-c")
	case "all":
		if boolv(o["extended"]) {
			return deviceArgs(o, "-x")
		}
		return deviceArgs(o, "-a")
	case "test":
		t := str(o["type"])
		if t == "" {
			return nil, fmt.Errorf("type is required")
		}
		return deviceArgs(o, "-t", t)
	case "log":
		return deviceArgs(o, "-l", strOr(o["type"], "error"))
	default:
		return nil, fmt.Errorf("unknown verb")
	}
	return append([]string{"--json"}, flags...), nil
}

// deviceArgs is the shape every per-device verb shares: --json, the flag (and
// its value, where it takes one), then the device node last.
func deviceArgs(o map[string]any, flags ...string) ([]string, error) {
	device := str(o["device"])
	if device == "" {
		return nil, fmt.Errorf("device is required")
	}
	a := append([]string{"--json"}, flags...)
	return append(a, device), nil
}

// enrich parses the --json document into `result` and adds the per-verb
// convenience outputs. Unlike a normal CLI connector this parses REGARDLESS of
// exit_code: smartctl's exit status is a bitmask, and the failing-disk case
// (bit 3) is precisely the one whose JSON the caller needs.
func enrich(verb string, res map[string]any) {
	if verb == "cli" {
		return
	}
	doc := jsonDoc(str(res["stdout"]))
	if doc == nil {
		return
	}
	res["result"] = doc
	switch verb {
	case "scan":
		res["devices"] = scanDevices(doc)
	case "health":
		if passed, ok := smartPassed(doc); ok {
			res["passed"] = passed
		}
	}
}

// runSmartctl spawns smartctl (via sudo when configured) and captures the
// result. A non-zero exit is returned as exit_code, not an error; only a
// failure to start the process (missing binary, timeout) is an error.
func runSmartctl(ctx context.Context, conn smartConn, args []string) (map[string]any, error) {
	full := conn.argv(args)
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
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

// --- source: poll each device, emit when one is failing ---

// StartSource polls every configured device (or every device --scan-open finds)
// once per poll_interval and emits a `health` event for each one that fails its
// self-assessment. Dedup is on device+status, so a disk that has been failing
// for a week emits ONCE, not once per cycle.
func (smartPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	conn, err := parseConn(req.Config)
	if err != nil {
		return fmt.Errorf("smart: %w", err)
	}
	dedup := sourcekit.NewDedup(1024)
	fmt.Fprintf(os.Stderr, "smart[%s]: polling %s every %s\n", req.Instance, deviceDesc(conn.devices), conn.pollInterval)
	for {
		if ctx.Err() != nil {
			return nil
		}
		devices, err := pollDevices(ctx, conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "smart[%s]: scan: %v\n", req.Instance, err)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}
		for _, device := range devices {
			doc, err := runJSON(ctx, conn, []string{"--json", "-H", "-A", device})
			if err != nil {
				fmt.Fprintf(os.Stderr, "smart[%s]: %s: %v\n", req.Instance, device, err)
				continue
			}
			ev, ok := healthEvent(device, doc)
			if !ok {
				continue
			}
			if !dedup.Add(str(ev["dedup"])) {
				continue
			}
			_ = emit(ev)
		}
		if !sleepCtx(ctx, conn.pollInterval) {
			return nil
		}
	}
}

// backoff is how long the source waits after a failed scan before retrying, so
// a missing smartctl or a revoked sudo doesn't become a hot loop.
const backoff = 30 * time.Second

// healthEvent is the source's WHOLE decision, kept pure so it is testable
// without a disk: given one device's parsed `smartctl --json -H -A` document it
// reports whether that device is unhealthy and, if so, builds the event.
//
// A device with no smart_status at all (SMART disabled, an enclosure smartctl
// cannot interrogate) is NOT an alert — it is a device we know nothing about.
func healthEvent(device string, doc map[string]any) (map[string]any, bool) {
	passed, ok := smartPassed(doc)
	if !ok || passed {
		return nil, false
	}
	model, serial := gs(doc, "model_name"), gs(doc, "serial_number")
	title := "smart: " + device + " FAILED its self-assessment"
	if model != "" {
		title += " (" + model + ")"
	}
	return map[string]any{
		"event": "health",
		"kind":  "health",
		"title": title,
		"dedup": device + "\x00FAILED",
		"context": map[string]any{
			"device": device, "model_name": model, "serial_number": serial,
			"passed":         false,
			"temperature_c":  gn(doc, "temperature", "current"),
			"power_on_hours": gn(doc, "power_on_time", "hours"),
			// 0 on a device with no such attribute (NVMe has none).
			"reallocated_sectors": reallocatedSectors(doc),
			"health":              "FAILED",
			// Plural aliases so the documented filter vocabulary
			// (filters: {devices/models: [...]}) matches against the daemon's
			// generic list-contains filter evaluator — the same convention the
			// sentry and email plugins use.
			"devices": device, "models": model,
		},
	}, true
}

// pollDevices is the source's device list: the configured one, or whatever
// --scan-open finds when none was configured.
func pollDevices(ctx context.Context, conn smartConn) ([]string, error) {
	if len(conn.devices) > 0 {
		return conn.devices, nil
	}
	doc, err := runJSON(ctx, conn, []string{"--json", "--scan-open"})
	if err != nil {
		return nil, err
	}
	return scanDevices(doc), nil
}

// runJSON runs one smartctl command under the connection timeout and returns
// the parsed document. A non-zero exit is expected and ignored (bitmask); only
// a failure to run, or output that is not JSON, is an error.
func runJSON(ctx context.Context, conn smartConn, args []string) (map[string]any, error) {
	cctx, cancel := context.WithTimeout(ctx, conn.timeout)
	defer cancel()
	res, err := runSmartctl(cctx, conn, args)
	if err != nil {
		return nil, err
	}
	doc := jsonDoc(str(res["stdout"]))
	if doc == nil {
		return nil, fmt.Errorf("no JSON output (exit %v): %s", res["exit_code"], strings.TrimSpace(str(res["stderr"])))
	}
	return doc, nil
}

func deviceDesc(devices []string) string {
	if len(devices) == 0 {
		return "every device --scan-open finds"
	}
	return strings.Join(devices, ", ")
}

func main() {
	if err := plugin.Serve(smartPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-smart: %v\n", err)
		os.Exit(1)
	}
}

// --- smartctl JSON helpers (stdlib only) ---

// smartPassed reads smart_status.passed, reporting whether the field was
// present at all — absent is "unknown", which is not the same as "failing".
func smartPassed(doc map[string]any) (bool, bool) {
	p, ok := gm(doc, "smart_status")["passed"].(bool)
	return p, ok
}

// scanDevices pulls the device names out of a `--scan`/`--scan-open` document.
func scanDevices(doc map[string]any) []string {
	out := []string{}
	for _, e := range gl(doc, "devices") {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if n := gs(m, "name"); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// reallocatedSectors is ATA attribute 5 (Reallocated_Sector_Ct) — the classic
// "this disk is dying" counter. NVMe devices have no attribute table; they
// report 0.
func reallocatedSectors(doc map[string]any) int64 {
	for _, e := range gl(gm(doc, "ata_smart_attributes"), "table") {
		m, ok := e.(map[string]any)
		if !ok || gn(m, "id") != 5 {
			continue
		}
		return gn(m, "raw", "value")
	}
	return 0
}

func gs(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func gm(m map[string]any, k string) map[string]any {
	sub, _ := m[k].(map[string]any)
	return sub
}

func gl(m map[string]any, k string) []any {
	l, _ := m[k].([]any)
	return l
}

// gn walks nested objects and returns the number at path, 0 when any step is
// missing or not a number.
func gn(m map[string]any, path ...string) int64 {
	var cur any = m
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = obj[k]
	}
	switch x := cur.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
}

// jsonDoc parses a smartctl --json document; nil when the output is empty or
// not an object, so a caller can leave `result` unset.
func jsonDoc(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// sleepCtx sleeps d or returns early (false) if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
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

// toDuration parses a timeout option: a Go duration string ("2m"), or a number
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

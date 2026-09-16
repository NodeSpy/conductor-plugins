// Command conductor-ffmpeg is a verb-only conductor connector that drives
// local media transcoding/inspection by shelling out to the `ffmpeg` (and
// `ffprobe`) CLI. It exposes the common operations as verbs — transcode,
// extract_audio, thumbnail, extract_frames, trim, scale, to_gif, concat,
// remux, overlay, probe — plus a `cli` escape hatch for any ffmpeg
// invocation a first-class verb does not cover. Built ONLY against the
// public SDK.
//
// ffmpeg logs its progress to stderr (stdout is reserved for `-f data`/pipe
// output we do not use here), so both streams are captured and returned;
// stdout is inspected only by `probe`, which asks ffprobe for JSON.
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

type ffmpegPlugin struct{}

func (ffmpegPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "ffmpeg",
		Desc: "ffmpeg/ffprobe: transcode, extract, trim, scale, and inspect media as verbs (transcode, extract_audio, thumbnail, extract_frames, trim, scale, to_gif, concat, remux, overlay, probe, cli). Shells out to the ffmpeg/ffprobe CLI.",
		Connection: plugin.Schema{
			"binary":            {Type: "string", Desc: "override the ffmpeg binary path (default ffmpeg)"},
			"ffprobe_binary":    {Type: "string", Desc: "override the ffprobe binary path (default ffprobe)"},
			"dir":               {Type: "string", Desc: "working directory for the process (relative input/output paths resolve here)"},
			"env":               {Type: "map", Desc: "default process environment for every invocation"},
			"timeout":           {Type: "duration", Desc: "default per-verb timeout (default 30m); a verb's timeout option overrides it"},
			"overwrite_default": {Type: "boolean", Desc: "default -y behavior when a verb does not set its own overwrite option (default true)"},
		},
		Verbs:        ffmpegVerbs(),
		Capabilities: plugin.Capabilities{Commands: []string{"ffmpeg", "ffprobe"}, Spawns: true, Egress: []string{}},
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

func withResult() plugin.Schema {
	return plugin.Schema{
		"stdout":    {Type: "string"},
		"stderr":    {Type: "string"},
		"exit_code": {Type: "integer"},
		"result":    {Type: "any", Desc: "parsed ffprobe JSON (format + streams)"},
	}
}

func ffmpegVerbs() []plugin.Verb {
	out := stdOutputs()
	return []plugin.Verb{
		{
			Name: "transcode", Desc: "transcode media with full control over codecs, filters, and timing",
			Usage: "the general-purpose verb; reach for a narrower verb (trim/scale/extract_audio/…) when it fits",
			Options: plugin.Schema{
				"input":         {Type: "string", Desc: "single input path (use inputs for multiple -i)"},
				"inputs":        {Type: "list", Desc: "multiple input paths, each its own -i"},
				"output":        {Type: "string", Required: true},
				"video_codec":   {Type: "string", Desc: "-c:v"},
				"audio_codec":   {Type: "string", Desc: "-c:a"},
				"video_bitrate": {Type: "string", Desc: "-b:v"},
				"audio_bitrate": {Type: "string", Desc: "-b:a"},
				"format":        {Type: "string", Desc: "-f"},
				"fps":           {Type: "string", Desc: "-r"},
				"vf":            {Type: "string", Desc: "-vf"},
				"af":            {Type: "string", Desc: "-af"},
				"preset":        {Type: "string", Desc: "-preset"},
				"crf":           {Type: "string", Desc: "-crf"},
				"pix_fmt":       {Type: "string", Desc: "-pix_fmt"},
				"start":         {Type: "string", Desc: "-ss; placed before -i when seek_input is true, else after (output seek)"},
				"seek_input":    {Type: "boolean", Desc: "place -ss before the first -i (fast input seek) instead of after"},
				"duration":      {Type: "string", Desc: "-t"},
				"to":            {Type: "string", Desc: "-to"},
				"map":           {Type: "list", Desc: "-map, repeated"},
				"metadata":      {Type: "map", Desc: "-metadata key=value, one per entry"},
				"threads":       {Type: "integer", Desc: "-threads"},
				"overwrite":     {Type: "boolean", Desc: "override the connection's overwrite_default for this call"},
				"extra_args":    {Type: "list", Desc: "raw flags inserted before the output"},
			},
			Outputs: out,
		},
		{
			Name: "extract_audio", Desc: "pull the audio track out of a media file",
			Options: plugin.Schema{
				"input":         {Type: "string", Required: true},
				"output":        {Type: "string", Required: true},
				"audio_codec":   {Type: "string", Desc: "-c:a (default copy)"},
				"audio_bitrate": {Type: "string", Desc: "-b:a"},
				"overwrite":     {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "thumbnail", Desc: "grab a single frame as an image",
			Options: plugin.Schema{
				"input":     {Type: "string", Required: true},
				"output":    {Type: "string", Required: true},
				"time":      {Type: "string", Desc: "-ss (input seek) position of the frame"},
				"size":      {Type: "string", Desc: "-s WxH"},
				"overwrite": {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "extract_frames", Desc: "extract a sequence of frames to an image pattern",
			Options: plugin.Schema{
				"input":          {Type: "string", Required: true},
				"output_pattern": {Type: "string", Required: true, Desc: "e.g. frame-%04d.png"},
				"fps":            {Type: "string", Desc: "sampling rate, folded into -vf fps=N"},
				"size":           {Type: "string", Desc: "WxH, folded into -vf scale=WxH"},
				"overwrite":      {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "trim", Desc: "cut a clip out of a media file",
			Options: plugin.Schema{
				"input":     {Type: "string", Required: true},
				"output":    {Type: "string", Required: true},
				"start":     {Type: "string", Desc: "-ss"},
				"end":       {Type: "string", Desc: "-to (wins over duration if both set)"},
				"duration":  {Type: "string", Desc: "-t"},
				"copy":      {Type: "boolean", Desc: "-c copy (default true; set false to re-encode)"},
				"overwrite": {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "scale", Desc: "resize video",
			Options: plugin.Schema{
				"input":     {Type: "string", Required: true},
				"output":    {Type: "string", Required: true},
				"width":     {Type: "any", Desc: "-vf scale=width:height (default -1, preserve aspect)"},
				"height":    {Type: "any", Desc: "-vf scale=width:height (default -1, preserve aspect)"},
				"overwrite": {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "to_gif", Desc: "convert a clip to an animated GIF",
			Options: plugin.Schema{
				"input":     {Type: "string", Required: true},
				"output":    {Type: "string", Required: true},
				"fps":       {Type: "any", Desc: "default 12"},
				"width":     {Type: "any", Desc: "default 480; height is always -1 (preserve aspect)"},
				"overwrite": {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "concat", Desc: "concatenate multiple inputs into one output",
			Usage: "same codec inputs → leave reencode false (stream copy); mixed inputs → reencode true",
			Options: plugin.Schema{
				"inputs":    {Type: "list", Required: true},
				"output":    {Type: "string", Required: true},
				"reencode":  {Type: "boolean", Desc: "re-encode instead of -c copy (needed when inputs are not stream-compatible)"},
				"overwrite": {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "remux", Desc: "change container without re-encoding (-c copy)",
			Options: plugin.Schema{
				"input":     {Type: "string", Required: true},
				"output":    {Type: "string", Required: true},
				"overwrite": {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "overlay", Desc: "overlay one video/image on top of another",
			Options: plugin.Schema{
				"input":     {Type: "string", Required: true},
				"overlay":   {Type: "string", Required: true, Desc: "second -i, drawn on top"},
				"output":    {Type: "string", Required: true},
				"position":  {Type: "string", Desc: "x:y for -filter_complex overlay=x:y (default 0:0)"},
				"overwrite": {Type: "boolean"},
			},
			Outputs: out,
		},
		{
			Name: "probe", Desc: "inspect a media file's format and streams (ffprobe)",
			Options: plugin.Schema{
				"input": {Type: "string", Required: true},
			},
			Outputs: withResult(),
		},
		{
			Name: "cli", Desc: "run any ffmpeg invocation: ffmpeg <args…>",
			Usage:   "escape hatch for an invocation without a first-class verb",
			Options: plugin.Schema{"args": {Type: "list", Required: true, Desc: "raw argv after the binary"}},
			Outputs: out,
		},
	}
}

// ffmpegConn is the resolved connection config for one invocation.
type ffmpegConn struct {
	binary           string
	ffprobeBinary    string
	dir              string
	env              map[string]string
	timeout          time.Duration
	overwriteDefault bool
}

func parseConn(m map[string]any) (ffmpegConn, error) {
	c := ffmpegConn{
		binary:           strOr(m["binary"], "ffmpeg"),
		ffprobeBinary:    strOr(m["ffprobe_binary"], "ffprobe"),
		dir:              str(m["dir"]),
		env:              strMap(m["env"]),
		timeout:          30 * time.Minute,
		overwriteDefault: true,
	}
	if v, ok := m["overwrite_default"]; ok {
		c.overwriteDefault = boolv(v)
	}
	if d, err := toDuration(m["timeout"]); err != nil {
		return c, fmt.Errorf("connection.timeout: %w", err)
	} else if d > 0 {
		c.timeout = d
	}
	return c, nil
}

func (c ffmpegConn) procEnv() []string {
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

func (ffmpegPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn, err := parseConn(req.Connection)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	// Inject the connection's overwrite default so verbArgs (pure, opts-only)
	// can decide -y from opts alone.
	if _, ok := o["overwrite"]; !ok {
		o["overwrite"] = conn.overwriteDefault
	}

	var args []string
	var binPath string
	var cleanup func()

	switch req.Verb {
	case "probe":
		a, perr := probeArgs(o)
		if perr != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+perr.Error())
		}
		args, binPath = a, conn.ffprobeBinary
	case "concat":
		inputs := strList(o["inputs"])
		if len(inputs) == 0 {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "concat: inputs is required")
		}
		if str(o["output"]) == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "concat: output is required")
		}
		listfile, werr := writeConcatList(inputs)
		if werr != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "concat: "+werr.Error())
		}
		cleanup = func() { os.Remove(listfile) }
		args, binPath = concatArgs(o, listfile), conn.binary
	default:
		a, verr := verbArgs(req.Verb, o)
		if verr != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": "+verr.Error())
		}
		args, binPath = a, conn.binary
	}
	if cleanup != nil {
		defer cleanup()
	}

	timeout := conn.timeout
	if d, derr := toDuration(o["timeout"]); derr != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, req.Verb+": options.timeout: "+derr.Error())
	} else if d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := runFFmpeg(ctx, binPath, args, conn.dir, conn.procEnv())
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": "+err.Error())
	}
	enrich(req.Verb, o, res)
	return plugin.InvokeResult{Outputs: res}, nil
}

// verbArgs builds the argv AFTER the binary for every verb except concat
// (which needs a listfile path — see concatArgs). Pure and hermetically
// testable — no process is spawned here.
func verbArgs(verb string, o map[string]any) ([]string, error) {
	switch verb {
	case "transcode":
		return transcodeArgs(o)
	case "extract_audio":
		return extractAudioArgs(o)
	case "thumbnail":
		return thumbnailArgs(o)
	case "extract_frames":
		return extractFramesArgs(o)
	case "trim":
		return trimArgs(o)
	case "scale":
		return scaleArgs(o)
	case "to_gif":
		return toGifArgs(o)
	case "remux":
		return remuxArgs(o)
	case "overlay":
		return overlayArgs(o)
	case "probe":
		return probeArgs(o)
	case "cli":
		args := strList(o["args"])
		if len(args) == 0 {
			return nil, fmt.Errorf("args is required")
		}
		return args, nil
	case "concat":
		return nil, fmt.Errorf("concat requires a listfile; use concatArgs")
	}
	return nil, fmt.Errorf("unknown verb")
}

// overwriteFlag renders -y unless opts explicitly set overwrite:false.
func overwriteFlag(o map[string]any) []string {
	if v, ok := o["overwrite"]; ok && !boolv(v) {
		return nil
	}
	return []string{"-y"}
}

// inputList merges the singular "input" and plural "inputs" options.
func inputList(o map[string]any) []string {
	list := strList(o["input"])
	list = append(list, strList(o["inputs"])...)
	return list
}

func transcodeArgs(o map[string]any) ([]string, error) {
	inputs := inputList(o)
	if len(inputs) == 0 {
		return nil, fmt.Errorf("input or inputs is required")
	}
	output := str(o["output"])
	if output == "" {
		return nil, fmt.Errorf("output is required")
	}
	seekInput := boolv(o["seek_input"])
	start := str(o["start"])

	a := overwriteFlag(o)
	for i, in := range inputs {
		if i == 0 && seekInput && start != "" {
			a = append(a, "-ss", start)
		}
		a = append(a, "-i", in)
	}
	if v := str(o["video_codec"]); v != "" {
		a = append(a, "-c:v", v)
	}
	if v := str(o["audio_codec"]); v != "" {
		a = append(a, "-c:a", v)
	}
	if v := str(o["video_bitrate"]); v != "" {
		a = append(a, "-b:v", v)
	}
	if v := str(o["audio_bitrate"]); v != "" {
		a = append(a, "-b:a", v)
	}
	if v := str(o["format"]); v != "" {
		a = append(a, "-f", v)
	}
	if v := str(o["fps"]); v != "" {
		a = append(a, "-r", v)
	}
	if v := str(o["vf"]); v != "" {
		a = append(a, "-vf", v)
	}
	if v := str(o["af"]); v != "" {
		a = append(a, "-af", v)
	}
	if v := str(o["preset"]); v != "" {
		a = append(a, "-preset", v)
	}
	if v := str(o["crf"]); v != "" {
		a = append(a, "-crf", v)
	}
	if v := str(o["pix_fmt"]); v != "" {
		a = append(a, "-pix_fmt", v)
	}
	if !seekInput && start != "" {
		a = append(a, "-ss", start)
	}
	if v := str(o["duration"]); v != "" {
		a = append(a, "-t", v)
	}
	if v := str(o["to"]); v != "" {
		a = append(a, "-to", v)
	}
	a = append(a, repeatFlag("-map", strList(o["map"]))...)
	a = append(a, kvFlags("-metadata", o["metadata"])...)
	if t := intStr(o["threads"]); t != "" {
		a = append(a, "-threads", t)
	}
	a = append(a, strList(o["extra_args"])...)
	a = append(a, output)
	return a, nil
}

func extractAudioArgs(o map[string]any) ([]string, error) {
	input, output := str(o["input"]), str(o["output"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	if output == "" {
		return nil, fmt.Errorf("output is required")
	}
	a := overwriteFlag(o)
	a = append(a, "-i", input, "-vn", "-c:a", strOr(o["audio_codec"], "copy"))
	if br := str(o["audio_bitrate"]); br != "" {
		a = append(a, "-b:a", br)
	}
	return append(a, output), nil
}

func thumbnailArgs(o map[string]any) ([]string, error) {
	input, output := str(o["input"]), str(o["output"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	if output == "" {
		return nil, fmt.Errorf("output is required")
	}
	a := overwriteFlag(o)
	if t := str(o["time"]); t != "" {
		a = append(a, "-ss", t)
	}
	a = append(a, "-i", input, "-frames:v", "1")
	if s := str(o["size"]); s != "" {
		a = append(a, "-s", s)
	}
	return append(a, output), nil
}

func extractFramesArgs(o map[string]any) ([]string, error) {
	input, pattern := str(o["input"]), str(o["output_pattern"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	if pattern == "" {
		return nil, fmt.Errorf("output_pattern is required")
	}
	a := overwriteFlag(o)
	a = append(a, "-i", input)
	var parts []string
	if fps := str(o["fps"]); fps != "" {
		parts = append(parts, "fps="+fps)
	}
	if size := str(o["size"]); size != "" {
		parts = append(parts, "scale="+size)
	}
	if len(parts) > 0 {
		a = append(a, "-vf", strings.Join(parts, ","))
	}
	return append(a, pattern), nil
}

func trimArgs(o map[string]any) ([]string, error) {
	input, output := str(o["input"]), str(o["output"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	if output == "" {
		return nil, fmt.Errorf("output is required")
	}
	a := overwriteFlag(o)
	if s := str(o["start"]); s != "" {
		a = append(a, "-ss", s)
	}
	a = append(a, "-i", input)
	if e := str(o["end"]); e != "" {
		a = append(a, "-to", e)
	} else if d := str(o["duration"]); d != "" {
		a = append(a, "-t", d)
	}
	copyStream := true
	if v, ok := o["copy"]; ok {
		copyStream = boolv(v)
	}
	if copyStream {
		a = append(a, "-c", "copy")
	}
	return append(a, output), nil
}

func scaleArgs(o map[string]any) ([]string, error) {
	input, output := str(o["input"]), str(o["output"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	if output == "" {
		return nil, fmt.Errorf("output is required")
	}
	w := valStr(o["width"], "-1")
	h := valStr(o["height"], "-1")
	a := overwriteFlag(o)
	a = append(a, "-i", input, "-vf", "scale="+w+":"+h, output)
	return a, nil
}

func toGifArgs(o map[string]any) ([]string, error) {
	input, output := str(o["input"]), str(o["output"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	if output == "" {
		return nil, fmt.Errorf("output is required")
	}
	fps := valStr(o["fps"], "12")
	width := valStr(o["width"], "480")
	a := overwriteFlag(o)
	vf := fmt.Sprintf("fps=%s,scale=%s:-1:flags=lanczos", fps, width)
	a = append(a, "-i", input, "-vf", vf, output)
	return a, nil
}

// concatArgs builds the concat argv given an already-materialized listfile
// path. Kept separate from verbArgs (and pure/testable) because writing the
// listfile itself is a side effect that belongs in Invoke.
func concatArgs(o map[string]any, listfile string) []string {
	a := overwriteFlag(o)
	a = append(a, "-f", "concat", "-safe", "0", "-i", listfile)
	if !boolv(o["reencode"]) {
		a = append(a, "-c", "copy")
	}
	return append(a, str(o["output"]))
}

// writeConcatList materializes ffmpeg's concat-demuxer list file: one
// `file '<path>'` line per input, single quotes escaped. The caller is
// responsible for removing it (defer) once the invocation completes.
func writeConcatList(inputs []string) (string, error) {
	f, err := os.CreateTemp("", "conductor-ffmpeg-concat-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, in := range inputs {
		escaped := strings.ReplaceAll(in, "'", `'\''`)
		if _, err := fmt.Fprintf(f, "file '%s'\n", escaped); err != nil {
			os.Remove(f.Name())
			return "", err
		}
	}
	return f.Name(), nil
}

func remuxArgs(o map[string]any) ([]string, error) {
	input, output := str(o["input"]), str(o["output"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	if output == "" {
		return nil, fmt.Errorf("output is required")
	}
	a := overwriteFlag(o)
	return append(a, "-i", input, "-c", "copy", output), nil
}

func overlayArgs(o map[string]any) ([]string, error) {
	input, overlay, output := str(o["input"]), str(o["overlay"]), str(o["output"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	if overlay == "" {
		return nil, fmt.Errorf("overlay is required")
	}
	if output == "" {
		return nil, fmt.Errorf("output is required")
	}
	pos := strOr(o["position"], "0:0")
	a := overwriteFlag(o)
	a = append(a, "-i", input, "-i", overlay, "-filter_complex", "overlay="+pos, output)
	return a, nil
}

// probeArgs builds the ffprobe argv. No -y/overwrite handling — ffprobe
// never writes files.
func probeArgs(o map[string]any) ([]string, error) {
	input := str(o["input"])
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}
	return []string{"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", input}, nil
}

// enrich adds verb-specific structured outputs, only when the command
// succeeded (exit_code 0) so we never parse an error stream.
func enrich(verb string, o, res map[string]any) {
	if res["exit_code"] != 0 {
		return
	}
	if verb == "probe" {
		stdout, _ := res["stdout"].(string)
		if v := jsonAny(stdout); v != nil {
			res["result"] = v
		}
	}
}

// runFFmpeg spawns the binary with args in dir and captures the result. A
// non-zero exit is returned as exit_code, not an error; only a failure to
// start the process (missing binary, timeout) is an error. No shell is
// involved — exec.CommandContext runs the binary directly.
func runFFmpeg(ctx context.Context, binary string, args []string, dir string, env []string) (map[string]any, error) {
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

func main() {
	if err := plugin.Serve(ffmpegPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-ffmpeg: %v\n", err)
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

// valStr stringifies an int/float/string option, defaulting to d when the
// value is absent or empty.
func valStr(v any, d string) string {
	switch x := v.(type) {
	case nil:
		return d
	case string:
		if x == "" {
			return d
		}
		return x
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%d", int64(x))
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

// kvFlags emits `flag key=value` for each map entry, keys sorted for
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

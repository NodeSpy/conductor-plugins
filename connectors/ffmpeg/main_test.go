package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestVerbArgs pins the exact argv each verb builds — the whole contract with
// the ffmpeg/ffprobe CLI, proven without spawning anything.
func TestVerbArgs(t *testing.T) {
	cases := []struct {
		name string
		verb string
		opts map[string]any
		want []string
	}{
		{
			name: "transcode full, input seek",
			verb: "transcode",
			opts: map[string]any{
				"input": "in.mp4", "output": "out.mkv",
				"video_codec": "libx264", "audio_codec": "aac",
				"video_bitrate": "2M", "audio_bitrate": "128k",
				"format": "matroska", "fps": "30", "vf": "hflip", "af": "volume=2",
				"preset": "fast", "crf": "23", "pix_fmt": "yuv420p",
				"start": "00:00:05", "seek_input": true, "duration": "10",
				"to": "00:01:00", "map": []any{"0:v:0", "0:a:0"},
				"metadata":   map[string]any{"title": "T", "artist": "A"},
				"threads":    4,
				"extra_args": []any{"-movflags", "+faststart"},
			},
			want: []string{"-y", "-ss", "00:00:05", "-i", "in.mp4",
				"-c:v", "libx264", "-c:a", "aac", "-b:v", "2M", "-b:a", "128k",
				"-f", "matroska", "-r", "30", "-vf", "hflip", "-af", "volume=2",
				"-preset", "fast", "-crf", "23", "-pix_fmt", "yuv420p",
				"-t", "10", "-to", "00:01:00",
				"-map", "0:v:0", "-map", "0:a:0",
				"-metadata", "artist=A", "-metadata", "title=T",
				"-threads", "4", "-movflags", "+faststart", "out.mkv"},
		},
		{
			name: "transcode output seek and no overwrite",
			verb: "transcode",
			opts: map[string]any{"input": "in.mp4", "output": "out.mp4", "start": "5", "overwrite": false},
			want: []string{"-i", "in.mp4", "-ss", "5", "out.mp4"},
		},
		{
			name: "transcode multiple inputs",
			verb: "transcode",
			opts: map[string]any{"inputs": []any{"a.mp4", "b.mp4"}, "output": "out.mp4"},
			want: []string{"-y", "-i", "a.mp4", "-i", "b.mp4", "out.mp4"},
		},
		{
			name: "extract_audio default codec",
			verb: "extract_audio",
			opts: map[string]any{"input": "in.mp4", "output": "out.aac"},
			want: []string{"-y", "-i", "in.mp4", "-vn", "-c:a", "copy", "out.aac"},
		},
		{
			name: "extract_audio explicit codec+bitrate",
			verb: "extract_audio",
			opts: map[string]any{"input": "in.mp4", "output": "out.mp3", "audio_codec": "libmp3lame", "audio_bitrate": "192k"},
			want: []string{"-y", "-i", "in.mp4", "-vn", "-c:a", "libmp3lame", "-b:a", "192k", "out.mp3"},
		},
		{
			name: "thumbnail",
			verb: "thumbnail",
			opts: map[string]any{"input": "in.mp4", "output": "out.png", "time": "00:00:03", "size": "320x240"},
			want: []string{"-y", "-ss", "00:00:03", "-i", "in.mp4", "-frames:v", "1", "-s", "320x240", "out.png"},
		},
		{
			name: "extract_frames fps and size",
			verb: "extract_frames",
			opts: map[string]any{"input": "in.mp4", "output_pattern": "f-%04d.png", "fps": "1", "size": "640x480"},
			want: []string{"-y", "-i", "in.mp4", "-vf", "fps=1,scale=640x480", "f-%04d.png"},
		},
		{
			name: "trim default copy",
			verb: "trim",
			opts: map[string]any{"input": "in.mp4", "output": "out.mp4", "start": "5", "end": "10"},
			want: []string{"-y", "-ss", "5", "-i", "in.mp4", "-to", "10", "-c", "copy", "out.mp4"},
		},
		{
			name: "trim duration, reencode",
			verb: "trim",
			opts: map[string]any{"input": "in.mp4", "output": "out.mp4", "start": "5", "duration": "20", "copy": false},
			want: []string{"-y", "-ss", "5", "-i", "in.mp4", "-t", "20", "out.mp4"},
		},
		{
			name: "scale explicit",
			verb: "scale",
			opts: map[string]any{"input": "in.mp4", "output": "out.mp4", "width": 1280, "height": 720},
			want: []string{"-y", "-i", "in.mp4", "-vf", "scale=1280:720", "out.mp4"},
		},
		{
			name: "scale default aspect",
			verb: "scale",
			opts: map[string]any{"input": "in.mp4", "output": "out.mp4", "width": 640},
			want: []string{"-y", "-i", "in.mp4", "-vf", "scale=640:-1", "out.mp4"},
		},
		{
			name: "to_gif defaults",
			verb: "to_gif",
			opts: map[string]any{"input": "in.mp4", "output": "out.gif"},
			want: []string{"-y", "-i", "in.mp4", "-vf", "fps=12,scale=480:-1:flags=lanczos", "out.gif"},
		},
		{
			name: "to_gif overrides",
			verb: "to_gif",
			opts: map[string]any{"input": "in.mp4", "output": "out.gif", "fps": 24, "width": 320},
			want: []string{"-y", "-i", "in.mp4", "-vf", "fps=24,scale=320:-1:flags=lanczos", "out.gif"},
		},
		{
			name: "remux",
			verb: "remux",
			opts: map[string]any{"input": "in.mkv", "output": "out.mp4"},
			want: []string{"-y", "-i", "in.mkv", "-c", "copy", "out.mp4"},
		},
		{
			name: "overlay",
			verb: "overlay",
			opts: map[string]any{"input": "bg.mp4", "overlay": "logo.png", "output": "out.mp4", "position": "10:20"},
			want: []string{"-y", "-i", "bg.mp4", "-i", "logo.png", "-filter_complex", "overlay=10:20", "out.mp4"},
		},
		{
			name: "probe",
			verb: "probe",
			opts: map[string]any{"input": "in.mp4"},
			want: []string{"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "in.mp4"},
		},
		{
			name: "cli escape hatch",
			verb: "cli",
			opts: map[string]any{"args": []any{"-version"}},
			want: []string{"-version"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verbArgs(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("verbArgs(%s): unexpected error: %v", tc.verb, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("verbArgs(%s)\n got: %#v\nwant: %#v", tc.verb, got, tc.want)
			}
		})
	}
}

// TestConcatArgs proves concat's argv builder given a fixed listfile path,
// without touching the filesystem.
func TestConcatArgs(t *testing.T) {
	got := concatArgs(map[string]any{"output": "out.mp4"}, "/tmp/list.txt")
	want := []string{"-y", "-f", "concat", "-safe", "0", "-i", "/tmp/list.txt", "-c", "copy", "out.mp4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("concatArgs (copy)\n got: %#v\nwant: %#v", got, want)
	}

	got = concatArgs(map[string]any{"output": "out.mp4", "reencode": true, "overwrite": false}, "/tmp/list.txt")
	want = []string{"-f", "concat", "-safe", "0", "-i", "/tmp/list.txt", "out.mp4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("concatArgs (reencode, no overwrite)\n got: %#v\nwant: %#v", got, want)
	}
}

// TestVerbArgsErrors covers required-field validation and unknown verbs.
func TestVerbArgsErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"transcode", map[string]any{"output": "o.mp4"}},                 // no input/inputs
		{"transcode", map[string]any{"input": "i.mp4"}},                  // no output
		{"extract_audio", map[string]any{"output": "o.aac"}},             // no input
		{"extract_audio", map[string]any{"input": "i.mp4"}},              // no output
		{"thumbnail", map[string]any{"input": "i.mp4"}},                  // no output
		{"extract_frames", map[string]any{"input": "i.mp4"}},             // no output_pattern
		{"trim", map[string]any{"output": "o.mp4"}},                      // no input
		{"scale", map[string]any{"output": "o.mp4"}},                     // no input
		{"to_gif", map[string]any{"output": "o.gif"}},                    // no input
		{"remux", map[string]any{"output": "o.mp4"}},                     // no input
		{"overlay", map[string]any{"input": "i.mp4", "output": "o.mp4"}}, // no overlay
		{"probe", map[string]any{}},                                      // no input
		{"cli", map[string]any{}},                                        // no args
		{"concat", map[string]any{}},                                     // handled outside verbArgs
		{"nope", map[string]any{}},                                       // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbArgs(tc.verb, tc.opts); err == nil {
			t.Errorf("verbArgs(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers defaults and overrides.
func TestParseConn(t *testing.T) {
	c, err := parseConn(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if c.binary != "ffmpeg" || c.ffprobeBinary != "ffprobe" || !c.overwriteDefault {
		t.Fatalf("defaults: %#v", c)
	}
	c2, err := parseConn(map[string]any{
		"binary": "/usr/local/bin/ffmpeg", "ffprobe_binary": "/usr/local/bin/ffprobe",
		"dir": "/work", "overwrite_default": false, "env": map[string]any{"X": "1"},
		"timeout": "5m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c2.binary != "/usr/local/bin/ffmpeg" || c2.ffprobeBinary != "/usr/local/bin/ffprobe" {
		t.Fatalf("binary overrides: %#v", c2)
	}
	if c2.overwriteDefault {
		t.Fatalf("overwrite_default: want false, got true")
	}
	if c2.dir != "/work" {
		t.Fatalf("dir: got %q", c2.dir)
	}
	if c2.timeout.String() != "5m0s" {
		t.Fatalf("timeout: got %v", c2.timeout)
	}
	env := c2.procEnv()
	found := false
	for _, e := range env {
		if e == "X=1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("procEnv missing X=1: %#v", env)
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, and every verb.
func TestDescribe(t *testing.T) {
	d := ffmpegPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "ffmpeg" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Commands, "ffmpeg") || !contains(d.Capabilities.Commands, "ffprobe") || !d.Capabilities.Spawns {
		t.Fatalf("capabilities: %#v", d.Capabilities)
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Fatalf("egress: want empty, got %#v", d.Capabilities.Egress)
	}
	want := []string{"transcode", "extract_audio", "thumbnail", "extract_frames", "trim",
		"scale", "to_gif", "concat", "remux", "overlay", "probe", "cli"}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(want) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(want))
	}
}

// TestInvokeShim drives runFFmpeg + enrich end to end against fake ffmpeg AND
// ffprobe binaries, proving exit_code passthrough and probe JSON parsing.
func TestInvokeShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX")
	}
	dir := t.TempDir()

	fakeFFmpeg := filepath.Join(dir, "fakeffmpeg")
	ffmpegScript := "#!/bin/sh\necho stderr-log >&2\n" +
		"case \"$1\" in\n" +
		"-fail) exit 3 ;;\n" +
		"*) exit 0 ;;\nesac\n"
	if err := os.WriteFile(fakeFFmpeg, []byte(ffmpegScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeFFprobe := filepath.Join(dir, "fakeffprobe")
	probeJSON := `{"format":{"filename":"in.mp4"},"streams":[{"codec_type":"video"}]}`
	ffprobeScript := "#!/bin/sh\necho '" + probeJSON + "'\n"
	if err := os.WriteFile(fakeFFprobe, []byte(ffprobeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	p := ffmpegPlugin{}
	conn := map[string]any{"binary": fakeFFmpeg, "ffprobe_binary": fakeFFprobe}

	// remux success → exit_code 0, stderr captured.
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "remux", Connection: conn,
		Options: map[string]any{"input": "in.mkv", "output": "out.mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 0 {
		t.Fatalf("remux exit_code: %#v", res.Outputs)
	}
	if !strings.Contains(res.Outputs["stderr"].(string), "stderr-log") {
		t.Fatalf("remux stderr not captured: %#v", res.Outputs["stderr"])
	}

	// cli with a failing arg → non-zero exit_code passthrough, not an error.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "cli", Connection: conn,
		Options: map[string]any{"args": []any{"-fail"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["exit_code"] != 3 {
		t.Fatalf("cli exit_code: %#v", res.Outputs)
	}

	// probe → parsed result via the ffprobe binary override.
	res, err = p.Invoke(plugin.InvokeRequest{Verb: "probe", Connection: conn,
		Options: map[string]any{"input": "in.mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("probe result: %#v", res.Outputs["result"])
	}
	format, ok := result["format"].(map[string]any)
	if !ok || format["filename"] != "in.mp4" {
		t.Fatalf("probe result.format: %#v", result["format"])
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

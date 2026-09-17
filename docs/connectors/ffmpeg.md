# `ffmpeg` connector

Transcode, extract, trim, scale, and inspect media by shelling out to the
`ffmpeg` (and `ffprobe`) CLI. The common operations are exposed as verbs
(`transcode`, `extract_audio`, `thumbnail`, `extract_frames`, `trim`, `scale`,
`to_gif`, `concat`, `remux`, `overlay`, `probe`), plus a `cli` escape hatch for
any invocation a first-class verb does not cover.

- **Kind:** connector (verbs only — no source events)
- **Source:** [`connectors/ffmpeg/main.go`](../../connectors/ffmpeg/main.go)
- **Provides:** `ffmpeg`
- **Capabilities:** spawns `ffmpeg` / `ffprobe`; no declared egress (ffmpeg can
  still read `http(s)://` inputs when you pass one — see below)

```yaml
connectors:
  f: { use: ffmpeg }
triggers:
  - on: gh.release
    steps:
      - id: clip
        uses: f.trim
        options: { input: raw.mp4, output: clip.mp4, start: "00:00:10", duration: "30" }
```

## Setup

Installs `ffmpeg` and `ffprobe`; there is no auth — purely local media processing.

**Prerequisites:** `ffmpeg` and `ffprobe` on PATH (override with `binary` / `ffprobe_binary`).

1. Install the `ffmpeg` package, which bundles `ffprobe`: `apt install ffmpeg` (Debian/Ubuntu), `brew install ffmpeg` (macOS), or a static build from [ffmpeg.org/download](https://ffmpeg.org/download.html).
2. Confirm both binaries resolve: `ffmpeg -version && ffprobe -version`.
3. If inputs/outputs are relative paths, set `dir` to the working directory they should resolve against.

```yaml
connectors:
  f: { use: ffmpeg, dir: /var/media }
```

## Connection

Every field is optional. Credentials/targets are read per-invocation from the
connector instance.

| key | type | purpose |
|-----|------|---------|
| `binary` | string | override the ffmpeg binary path (default `ffmpeg`) |
| `ffprobe_binary` | string | override the ffprobe binary path (default `ffprobe`) |
| `dir` | string | working directory for the process (relative input/output paths resolve here) |
| `env` | map | default process environment for every invocation |
| `timeout` | duration | default per-verb timeout (default `30m`); a verb's `timeout` option overrides it |
| `overwrite_default` | boolean | default `-y` behavior when a verb does not set its own `overwrite` option (default `true`) |

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code`. ffmpeg logs its progress to stderr, so `stderr`
carries that output.

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr (ffmpeg's progress/log output lands here) |
| `exit_code` | integer | process exit code (0 = success) |

`probe` adds a structured `result` output (below), populated only on
`exit_code == 0`.

## Overwrite behavior

Every verb that writes an output file passes `-y` unless told not to:

- Set `overwrite: false` on the verb call to omit `-y` for that call.
- Set the connection's `overwrite_default: false` to omit `-y` by default for
  every call on that connector instance (a call's own `overwrite` option still
  wins).

`probe` and `cli` do not add `-y` automatically — `cli` is a raw escape hatch,
so include `-y` yourself if you need it.

## Verbs

### `transcode` — general-purpose transcode

The general-purpose verb; reach for a narrower verb (`trim`/`scale`/
`extract_audio`/…) when it fits.

| option | type | flag |
|--------|------|------|
| `input` | string | single input (use `inputs` for multiple `-i`) |
| `inputs` | list | multiple inputs, each its own `-i` |
| `output` * | string | output path (positional) |
| `video_codec` | string | `-c:v` |
| `audio_codec` | string | `-c:a` |
| `video_bitrate` | string | `-b:v` |
| `audio_bitrate` | string | `-b:a` |
| `format` | string | `-f` |
| `fps` | string | `-r` |
| `vf` | string | `-vf` |
| `af` | string | `-af` |
| `preset` | string | `-preset` |
| `crf` | string | `-crf` |
| `pix_fmt` | string | `-pix_fmt` |
| `start` | string | `-ss` — before the first `-i` (fast input seek) when `seek_input: true`, else after (output seek) |
| `seek_input` | boolean | selects input-seek placement for `start` |
| `duration` | string | `-t` |
| `to` | string | `-to` |
| `map` | list | `-map`, repeated |
| `metadata` | map | `-metadata key=value`, one per entry (sorted) |
| `threads` | integer | `-threads` |
| `overwrite` | boolean | override the connection's `overwrite_default` for this call |
| `extra_args` | list | raw flags inserted before the output |

```yaml
uses: f.transcode
options:
  input: in.mov
  output: out.mp4
  video_codec: libx264
  audio_codec: aac
  crf: "23"
  preset: fast
  start: "00:00:05"
  seek_input: true
  duration: "10"
```

### `extract_audio` — pull the audio track

`input` *, `output` *, `audio_codec` (`-c:a`, default `copy`), `audio_bitrate`
(`-b:a`). Always adds `-vn`.

### `thumbnail` — grab a single frame

`input` *, `output` *, `time` (`-ss`, input seek), `size` (`-s WxH`). Always
adds `-frames:v 1`.

### `extract_frames` — sample a sequence of frames

`input` *, `output_pattern` * (e.g. `frame-%04d.png`), `fps` and `size` fold
into a single `-vf fps=N,scale=WxH`.

### `trim` — cut a clip

`input` *, `output` *, `start` (`-ss`), `end` (`-to`, wins over `duration` if
both are set) or `duration` (`-t`), `copy` (`-c copy`, default `true`; set
`false` to re-encode instead of stream-copying).

### `scale` — resize video

`input` *, `output` *, `width`, `height` → `-vf scale=width:height`. Either
dimension defaults to `-1` (preserve aspect ratio).

### `to_gif` — animated GIF

`input` *, `output` *, `fps` (default `12`), `width` (default `480`; height is
always `-1`) → `-vf "fps=N,scale=W:-1:flags=lanczos"`.

### `concat` — join multiple inputs

`inputs` * (list), `output` *, `reencode` (boolean; default `false` uses
`-c copy` for stream-compatible inputs, `true` re-encodes for mixed inputs).

Writes a temporary ffmpeg concat-demuxer list file (`file '<path>'` per input,
single quotes escaped), invokes `ffmpeg -f concat -safe 0 -i <listfile> [-c
copy] output`, then removes the list file.

```yaml
uses: f.concat
options:
  inputs: [part1.mp4, part2.mp4, part3.mp4]
  output: full.mp4
```

### `remux` — change container without re-encoding

`input` *, `output` *. Always `-c copy`.

### `overlay` — composite one input over another

`input` * (background, first `-i`), `overlay` * (second `-i`), `output` *,
`position` (`x:y` for `-filter_complex overlay=x:y`, default `0:0`).

### `probe` — inspect format and streams

`input` *. Runs `ffprobe -v quiet -print_format json -show_format
-show_streams <input>` and parses the JSON into the **`result`** output
(`result.format`, `result.streams`). No `-y`/overwrite handling — ffprobe never
writes files.

```yaml
uses: f.probe
options: { input: clip.mp4 }
# result.format.duration, result.streams[0].codec_name, …
```

### `cli` — any ffmpeg invocation

`args` * (raw argv appended after the binary). The escape hatch for anything
the first-class verbs don't model; runs against the ffmpeg binary (not
ffprobe).

```yaml
uses: f.cli
options: { args: [-i, in.mp4, -filter:v, "eq=brightness=0.1", out.mp4] }
```

## Capabilities & security

Declares `Commands: [ffmpeg, ffprobe]` and `Spawns: true`; `Egress: []`.

- ffmpeg is capable of reading network inputs (`http://`, `https://`, `rtsp://`,
  …) when an `input` option names one — that is a **capability of the ffmpeg
  binary itself**, not a network call this connector declares or initiates on
  your behalf. If you point a verb at a remote input, be aware the invocation
  will reach the network even though `Egress` is empty; scope which inputs a
  trigger can pass accordingly.
- Verb options become CLI argv **directly** (`exec.CommandContext`, no shell),
  so there is no shell-quoting/injection surface in how options are assembled.
- A non-zero exit is data, not an error — always check `exit_code` before
  trusting an output file exists.

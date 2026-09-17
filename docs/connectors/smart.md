# `smart` connector

Disk **S.M.A.R.T.** health by shelling out to `smartctl` (smartmontools). The
tool is exposed as verbs (`scan`, `info`, `health`, `attributes`, `all`,
`capabilities`, `test`, `log`) plus a `cli` escape hatch, and a **poll source**
that watches a set of devices and emits a `health` event when one fails its
self-assessment.

- **Kind:** connector (verbs + source)
- **Source:** [`connectors/smart/main.go`](../../connectors/smart/main.go)
- **Provides:** `smart`
- **Capabilities:** spawns `smartctl` / `sudo`; no declared egress (it reads
  local devices and dials nothing)

```yaml
connectors:
  disks:
    use: smart
    sudo: true
    devices: [/dev/sda, /dev/sdb]
    poll_interval: 15m
triggers:
  - on: disks.health
    steps:
      - id: page
        uses: pd.trigger
        options: { summary: "{{.title}} — {{.reallocated_sectors}} reallocated sectors" }
```

## Two facts about `smartctl` that shape this connector

1. **Its exit status is a bitmask, not a success flag.** Bit 3 (`8`) means "the
   disk is failing" — exactly the case you care about. A non-zero exit is
   therefore **data**: it comes back as `exit_code`, the JSON is still parsed
   into `result`, and nothing is raised as an invocation error.
2. **Reading a device needs raw access.** `sudo: true` prepends `sudo` to the
   command line rather than requiring the daemon itself to run as root.

## Setup

Installs `smartmontools`, which provides `smartctl`; reading most devices needs root.

**Prerequisites:** `smartctl` on PATH (override with `binary`); `sudo` on PATH if using `sudo: true`.

1. Install smartmontools: `apt install smartmontools` (Debian/Ubuntu), `brew install smartmontools` (macOS), or `yum install smartmontools` (RHEL/CentOS).
2. Confirm device access: `sudo smartctl --scan` should list attached drives.
3. If the daemon doesn't run as root, grant it passwordless access to just this binary via a sudoers rule (e.g. `conductor ALL=(root) NOPASSWD: /usr/sbin/smartctl`), then set `sudo: true`.

```yaml
connectors:
  disks: { use: smart, sudo: true, devices: [/dev/sda, /dev/sdb] }
```

## Connection

Every field is optional.

| key | type | purpose |
|-----|------|---------|
| `binary` | string | `smartctl` binary path (default `smartctl`) |
| `sudo` | boolean | prepend `sudo` — reading a device needs raw access |
| `devices` | list | devices the **source** polls, e.g. `[/dev/sda, /dev/nvme0]`; empty = discover with `--scan-open` |
| `poll_interval` | duration | source poll period (default `5m`) |
| `env` | map | default process environment for every invocation |
| `timeout` | duration | default per-verb timeout (default `2m`); a verb's `timeout` option overrides it |

```yaml
connectors:
  disks:   { use: smart, sudo: true }                       # discover everything
  nas:     { use: smart, sudo: true, devices: [/dev/sda, /dev/sdb], poll_interval: 1h }
  nosudo:  { use: smart, binary: /usr/local/sbin/smartctl } # already privileged
```

## Common output shape

Every verb returns the process result; a **non-zero exit is data, not an
error** — inspect `exit_code` (and remember it is a bitmask).

| output | type | notes |
|--------|------|-------|
| `stdout` | string | captured stdout |
| `stderr` | string | captured stderr |
| `exit_code` | integer | smartctl's status **bitmask** (0 = clean) |
| `result` | any | the parsed `--json` document (every verb but `cli`) |

Every verb except `cli` runs with `--json`, and `result` is parsed **regardless
of `exit_code`** — a failing disk exits non-zero and its JSON is the whole point.

## Verbs

| verb | smartctl | options |
|------|----------|---------|
| `scan` | `--scan` / `--scan-open` | `open` (boolean) |
| `info` | `-i` | `device` * |
| `health` | `-H` | `device` * |
| `attributes` | `-A` | `device` * |
| `all` | `-a`, or `-x` with `extended` | `device` *, `extended` (boolean) |
| `capabilities` | `-c` | `device` * |
| `test` | `-t <type> <device>` | `device` *, `type` * (`short`/`long`/`conveyance`/`offline`) |
| `log` | `-l <type> <device>` | `device` *, `type` (default `error`) |
| `cli` | raw argv, **no** `--json` | `args` * |

### `scan` — enumerate devices

`open` runs `--scan-open` (opens each device to identify it) instead of
`--scan`. Extra output **`devices`**: the scanned device names, flattened out of
`result.devices[].name`.

```yaml
uses: disks.scan
options: { open: true }
# → smartctl --json --scan-open
```

### `health` — the overall-health self-assessment

The cheap check. Extra output **`passed`** (boolean), lifted from
`result.smart_status.passed`; `false` is the drive predicting its own failure.

```yaml
uses: disks.health
options: { device: /dev/sda }
# → sudo smartctl --json -H /dev/sda
```

### `info` / `attributes` / `capabilities`

`info` (`-i`) is identity — model, serial, firmware, capacity. `attributes`
(`-A`) is the vendor attribute table. `capabilities` (`-c`) is what the device
supports and how long its self-tests take. All take `device` *.

### `all` — everything smartctl knows

`device` *, `extended`. Default `-a`; `extended: true` uses `-x`, which adds the
device-statistics and SATA/SCSI logs.

### `test` — start a self-test

`device` *, `type` * (`short`, `long`, `conveyance`, `offline`). The verb
**starts** the test and returns immediately — smartctl does not wait. Poll
`log` with `type: selftest` for the result.

```yaml
uses: disks.test
options: { device: /dev/sda, type: short }
# → sudo smartctl --json -t short /dev/sda
```

### `log` — read a device log

`device` *, `type` (default `error`; also `selftest`, `devstat`, `scttemp`, …).

### `cli` — raw smartctl

`args` * (raw argv appended after the binary). The escape hatch for anything the
first-class verbs don't model. **`--json` is not added** — pass it yourself if
you want a parseable `result`.

```yaml
uses: disks.cli
options: { args: [-s, "on", /dev/sda] }
```

## Source — the `health` event

Every `poll_interval`, the source runs `smartctl --json -H -A <device>` for each
device in `devices` — or, when `devices` is empty, for each device
`--scan-open` finds — and emits **one `health` event per device that FAILED its
self-assessment** (`smart_status.passed == false`).

- A device with **no** `smart_status` (SMART disabled, an enclosure smartctl
  cannot interrogate) is *not* an alert — it is a device we know nothing about.
- Events are **deduped on device + status**, so a disk that has been failing for
  a week emits once, not once per cycle.
- A failed scan backs off 30s rather than becoming a hot loop; the loop exits
  cleanly when the daemon cancels it.

| context | type | notes |
|---------|------|-------|
| `device` | string | the device node |
| `model_name` | string | |
| `serial_number` | string | |
| `passed` | boolean | always `false` (the event only fires on failure) |
| `temperature_c` | integer | `temperature.current` |
| `power_on_hours` | integer | `power_on_time.hours` |
| `reallocated_sectors` | integer | ATA attribute 5; `0` on devices with no attribute table (NVMe) |
| `health` | string | `FAILED` |

| filter | type | matches |
|--------|------|---------|
| `devices` | list | the device is one of these |
| `device` | string | the device equals this |
| `models` | list | `model_name` is one of these |
| `model` | string | `model_name` equals this |

```yaml
connectors:
  disks: { use: smart, sudo: true, poll_interval: 30m }
triggers:
  - on: disks.health
    filters: { devices: [/dev/sda, /dev/sdb] }
    steps:
      - id: notify
        uses: ntfy.publish
        options:
          title: "{{.title}}"
          message: "{{.model_name}} ({{.serial_number}}) — {{.reallocated_sectors}} reallocated, {{.power_on_hours}}h powered on"
```

## Capabilities & security

Declares `Commands: [smartctl, sudo]` and `Spawns: true`; no `Egress`.

- Verb options become CLI argv **directly** (`exec.CommandContext`, no shell), so
  there is no shell-quoting/injection surface in how options are assembled.
- `sudo: true` means the daemon can run `smartctl` as root. Scope it with a
  sudoers rule that permits exactly that binary rather than a blanket NOPASSWD —
  `cli` and `test` are real device commands, not read-only introspection.
- `device` options are **scoped** (`scope: device`), so conductor gates which
  device an agent-driven dispatch may name.

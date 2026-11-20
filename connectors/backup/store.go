package main

// The storage model every driver implements. A backup SET is a named series
// of snapshots; a snapshot is one immutable blob named
//
//	<set>-<UTC stamp>.<ext>        e.g. calendar-work-20261120T031500Z.ics
//
// and carries the sha256 and size of its PLAINTEXT, recorded at put time, so
// `verify` can prove a restore returns exactly what was written.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const stampLayout = "20060102T150405Z"

type snapshot struct {
	Name   string    // <set>-<stamp>.<ext>
	At     time.Time // from the stamp
	Size   int64     // plaintext bytes; -1 when unknown
	SHA256 string    // plaintext; "" when unknown (e.g. an encrypted manifest with no identity)
	Enc    string    // "" (plain), "age", "restic", "borg"
	ref    string    // driver handle: restic snapshot id, borg archive, stored object name
}

func (s snapshot) out() map[string]any {
	m := map[string]any{"name": s.Name, "time": s.At.Format(time.RFC3339), "encryption": strOr(s.Enc, "none")}
	if s.Size >= 0 {
		m["size"] = s.Size
	}
	if s.SHA256 != "" {
		m["sha256"] = s.SHA256
	}
	return m
}

// store is one tier's backend.
type store interface {
	put(ctx context.Context, set string, s snapshot, data []byte) error
	list(ctx context.Context, set string) ([]snapshot, error) // newest first
	get(ctx context.Context, set string, s snapshot) ([]byte, error)
	remove(ctx context.Context, set string, ss []snapshot) error
	check(ctx context.Context, set string) error // structural/repository check
}

func snapName(set string, at time.Time, ext string) string {
	return fmt.Sprintf("%s-%s.%s", set, at.UTC().Format(stampLayout), ext)
}

// parseName accepts exactly <set>-<stamp>.<ext>; anything else (another
// set's snapshot that shares a prefix, a stray file) is not ours.
func parseName(set, name string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(name, set+"-")
	if !ok || len(rest) < len(stampLayout)+2 || rest[len(stampLayout)] != '.' {
		return time.Time{}, false
	}
	at, err := time.Parse(stampLayout, rest[:len(stampLayout)])
	if err != nil || !extRe.MatchString(rest[len(stampLayout)+1:]) {
		return time.Time{}, false
	}
	return at, true
}

func sortNewest(ss []snapshot) {
	sort.Slice(ss, func(i, j int) bool {
		if !ss[i].At.Equal(ss[j].At) {
			return ss[i].At.After(ss[j].At)
		}
		return ss[i].Name > ss[j].Name
	})
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// --- retention (identical on every driver) ---

type retention struct{ daily, weekly, monthly int }

// keep marks the newest snapshot overall plus the newest of each of the
// most recent r.daily days, r.weekly ISO weeks and r.monthly months. ss must
// be newest first.
func keep(ss []snapshot, r retention) map[string]bool {
	kept := map[string]bool{}
	if len(ss) > 0 {
		kept[ss[0].Name] = true
	}
	bucket := func(n int, key func(time.Time) string) {
		seen := map[string]bool{}
		for _, s := range ss {
			if len(seen) >= n {
				return
			}
			if k := key(s.At); !seen[k] {
				seen[k] = true
				kept[s.Name] = true
			}
		}
	}
	bucket(r.daily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(r.weekly, func(t time.Time) string { y, w := t.ISOWeek(); return strconv.Itoa(y) + "-W" + strconv.Itoa(w) })
	bucket(r.monthly, func(t time.Time) string { return t.Format("2006-01") })
	return kept
}

// applyRetention removes what keep does not mark (or only reports it).
func applyRetention(ctx context.Context, st store, set string, r retention, dry bool) (pruned, kept []any, err error) {
	ss, err := st.list(ctx, set)
	if err != nil {
		return nil, nil, err
	}
	k := keep(ss, r)
	var drop []snapshot
	pruned, kept = []any{}, []any{}
	for _, s := range ss {
		if k[s.Name] {
			kept = append(kept, s.Name)
		} else {
			drop = append(drop, s)
			pruned = append(pruned, s.Name)
		}
	}
	if !dry && len(drop) > 0 {
		if err := st.remove(ctx, set, drop); err != nil {
			return nil, kept, err
		}
	}
	return pruned, kept, nil
}

// --- running the external tools ---

type toolError struct {
	tool   string
	code   int
	stderr string
	err    error
}

func (e *toolError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if len(msg) > 2000 {
		msg = "…" + msg[len(msg)-2000:]
	}
	if msg == "" {
		msg = e.err.Error()
	}
	return fmt.Sprintf("%s (exit %d): %s", e.tool, e.code, msg)
}

func exitCode(err error) int {
	var te *toolError
	if errors.As(err, &te) {
		return te.code
	}
	return -1
}

// run executes a tool with extra env (KEY=VALUE) appended to the plugin's
// own (already minimal under the jail), feeding stdin when non-nil.
func run(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s is not installed (or not on the plugin's PATH)", name)
		}
		return out.Bytes(), &toolError{tool: name, code: code, stderr: errb.String(), err: err}
	}
	return out.Bytes(), nil
}

// shellQuote quotes one word for tools that split a command line shell-style
// (BORG_RSH, restic's sftp.args).
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r == '=' || r == ':' || r == '@' || r == ',' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

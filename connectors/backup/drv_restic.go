package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// resticStore keeps each snapshot as one restic snapshot of a single
// stdin file named after it, tagged with the set and the plaintext's
// sha256/size. restic encrypts and deduplicates everything natively; the
// repo may be any restic backend (local path, sftp:, s3:, b2:, azure:, gs:,
// rest:, rclone:).
type resticStore struct {
	repo     string
	password string
	env      []string
	ssh      *sshCfg
	autoInit bool
	checkPct string // --read-data-subset for check; "" = metadata only
}

func (r *resticStore) cmd(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	env := append([]string{"RESTIC_REPOSITORY=" + r.repo, "RESTIC_PASSWORD=" + r.password, "TZ=UTC", "RESTIC_PROGRESS_FPS=0"}, r.env...)
	pre := []string{"--no-cache", "-q"}
	if r.ssh != nil && strings.HasPrefix(r.repo, "sftp:") {
		words, _, _, cleanup, err := r.ssh.setup()
		if err != nil {
			return nil, err
		}
		defer cleanup()
		host := sftpHost(r.repo)
		if host == "" {
			return nil, fmt.Errorf("restic: cannot read user@host from repo %q", r.repo)
		}
		quoted := make([]string, 0, len(words)+3)
		for _, w := range words {
			quoted = append(quoted, shellQuote(w))
		}
		quoted = append(quoted, shellQuote(host), "-s", "sftp")
		pre = append(pre, "-o", "sftp.command="+strings.Join(quoted, " "))
	}
	return run(ctx, env, stdin, "restic", append(pre, args...)...)
}

// sftpHost extracts [user@]host from "sftp:user@host:/path" or
// "sftp://user@host[:port]//path".
func sftpHost(repo string) string {
	rest := strings.TrimPrefix(repo, "sftp:")
	if strings.HasPrefix(rest, "//") {
		rest = strings.TrimPrefix(rest, "//")
		h, _, _ := strings.Cut(rest, "/")
		h, _, _ = strings.Cut(h, ":")
		return h
	}
	h, _, _ := strings.Cut(rest, ":")
	return h
}

func (r *resticStore) ensure(ctx context.Context) error {
	_, err := r.cmd(ctx, nil, "cat", "config")
	if err == nil {
		return nil
	}
	if exitCode(err) == 10 && r.autoInit { // 10: repository does not exist
		_, err = r.cmd(ctx, nil, "init")
	}
	return err
}

func setTag(set string) string { return "conductor-set=" + set }

func (r *resticStore) put(ctx context.Context, set string, s snapshot, data []byte) error {
	if err := r.ensure(ctx); err != nil {
		return err
	}
	_, err := r.cmd(ctx, data, "backup", "--stdin", "--stdin-filename", s.Name,
		"--host", "conductor", "--time", s.At.UTC().Format("2006-01-02 15:04:05"),
		"--tag", setTag(set), "--tag", "sha256="+s.SHA256, "--tag", "size="+strconv.FormatInt(s.Size, 10))
	return err
}

func (r *resticStore) list(ctx context.Context, set string) ([]snapshot, error) {
	out, err := r.cmd(ctx, nil, "snapshots", "--json", "--tag", setTag(set))
	if exitCode(err) == 10 {
		return nil, nil // no repo yet: no snapshots
	}
	if err != nil {
		return nil, err
	}
	var items []struct {
		ID    string   `json:"id"`
		Paths []string `json:"paths"`
		Tags  []string `json:"tags"`
	}
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	var ss []snapshot
	for _, it := range items {
		if len(it.Paths) != 1 {
			continue
		}
		name := strings.TrimPrefix(it.Paths[0], "/")
		at, ok := parseName(set, name)
		if !ok {
			continue
		}
		s := snapshot{Name: name, At: at, Size: -1, Enc: "restic", ref: it.ID}
		for _, t := range it.Tags {
			if v, ok := strings.CutPrefix(t, "sha256="); ok {
				s.SHA256 = v
			}
			if v, ok := strings.CutPrefix(t, "size="); ok {
				s.Size, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		ss = append(ss, s)
	}
	sortNewest(ss)
	return ss, nil
}

func (r *resticStore) get(ctx context.Context, _ string, s snapshot) ([]byte, error) {
	return r.cmd(ctx, nil, "dump", s.ref, "/"+s.Name)
}

func (r *resticStore) remove(ctx context.Context, _ string, ss []snapshot) error {
	args := []string{"forget", "--prune"}
	for _, s := range ss {
		args = append(args, s.ref)
	}
	_, err := r.cmd(ctx, nil, args...)
	return err
}

func (r *resticStore) check(ctx context.Context, _ string) error {
	args := []string{"check"}
	if r.checkPct != "" {
		args = append(args, "--read-data-subset="+r.checkPct)
	}
	_, err := r.cmd(ctx, nil, args...)
	return err
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// borgStore keeps each snapshot as one borg archive named after it, holding
// a single stdin file of that name; the archive comment records the
// plaintext's sha256/size. borg encrypts (repokey-blake2 unless
// encryption: none) and deduplicates natively; the repo is a local path or
// ssh://user@host[:port]/path with borg installed on the server.
type borgStore struct {
	repo       string
	passphrase string
	encryption string // borg init --encryption (repokey-blake2 | none)
	env        []string
	ssh        *sshCfg
	autoInit   bool
	checkData  bool
}

func (b *borgStore) cmd(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	base, err := os.MkdirTemp("", "borg-base-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(base)
	env := append([]string{
		"BORG_REPO=" + b.repo, "BORG_PASSPHRASE=" + b.passphrase,
		"BORG_BASE_DIR=" + base, "BORG_EXIT_CODES=modern",
		// A fresh BORG_BASE_DIR every call has no memory of the repo; these
		// keep borg from asking (it would hang, then fail, non-interactive).
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes", "BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
		"BORG_DISPLAY_PASSPHRASE=no", "TZ=UTC",
	}, b.env...)
	if b.ssh != nil && strings.HasPrefix(b.repo, "ssh://") {
		words, _, _, cleanup, err := b.ssh.setup()
		if err != nil {
			return nil, err
		}
		defer cleanup()
		q := make([]string, len(words))
		for i, w := range words {
			q[i] = shellQuote(w)
		}
		env = append(env, "BORG_RSH="+strings.Join(q, " "))
	}
	return run(ctx, env, stdin, "borg", args...)
}

const (
	borgRepoMissing = 13 // Repository.DoesNotExist (modern exit codes)
	borgNotARepo    = 15 // Repository.InvalidRepository
)

func (b *borgStore) ensure(ctx context.Context) error {
	_, err := b.cmd(ctx, nil, "info", "::")
	if err == nil {
		return nil
	}
	if c := exitCode(err); (c == borgRepoMissing || c == borgNotARepo) && b.autoInit {
		if strings.HasPrefix(b.repo, "/") {
			if err := os.MkdirAll(filepath.Dir(b.repo), 0o700); err != nil {
				return err
			}
		}
		_, err = b.cmd(ctx, nil, "init", "--encryption="+b.encryption, "--make-parent-dirs")
	}
	return err
}

func (b *borgStore) put(ctx context.Context, _ string, s snapshot, data []byte) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	// Plain words, not JSON: borg expands {placeholders} in a comment.
	comment := fmt.Sprintf("sha256=%s size=%d", s.SHA256, s.Size)
	_, err := b.cmd(ctx, data, "create", "--stdin-name", s.Name, "--comment", comment,
		"--timestamp", s.At.UTC().Format("2006-01-02T15:04:05"), "::"+s.Name, "-")
	return err
}

func (b *borgStore) list(ctx context.Context, set string) ([]snapshot, error) {
	out, err := b.cmd(ctx, nil, "list", "--glob-archives", set+"-*", "--format", "{archive}{TAB}{comment}{NL}", "::")
	if c := exitCode(err); c == borgRepoMissing || c == borgNotARepo {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ss []snapshot
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name, comment, _ := strings.Cut(sc.Text(), "\t")
		at, ok := parseName(set, name)
		if !ok {
			continue
		}
		s := snapshot{Name: name, At: at, Size: -1, Enc: "borg", ref: name}
		for _, f := range strings.Fields(comment) {
			if v, ok := strings.CutPrefix(f, "sha256="); ok {
				s.SHA256 = v
			}
			if v, ok := strings.CutPrefix(f, "size="); ok {
				s.Size, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		ss = append(ss, s)
	}
	sortNewest(ss)
	return ss, nil
}

func (b *borgStore) get(ctx context.Context, _ string, s snapshot) ([]byte, error) {
	return b.cmd(ctx, nil, "extract", "--stdout", "::"+s.ref, s.Name)
}

func (b *borgStore) remove(ctx context.Context, _ string, ss []snapshot) error {
	args := []string{"delete", "::"}
	for _, s := range ss {
		args = append(args, s.ref)
	}
	if _, err := b.cmd(ctx, nil, args...); err != nil {
		return err
	}
	_, err := b.cmd(ctx, nil, "compact", "::")
	return err
}

func (b *borgStore) check(ctx context.Context, _ string) error {
	args := []string{"check"}
	if b.checkData {
		args = append(args, "--verify-data")
	}
	if _, err := b.cmd(ctx, nil, append(args, "::")...); err != nil {
		return fmt.Errorf("borg check: %w", err)
	}
	return nil
}

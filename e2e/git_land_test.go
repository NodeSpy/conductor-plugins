package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
)

// TestGitLandEndToEnd drives conductor-git-land over the real plugin
// protocol against real git: an agent's workspace commits on a branch, the
// branch moves under it, and keep → replay → push lands the work on the new
// head — or, when the two touch the same lines, replay reports the conflict
// and nothing is pushed; the work stays at the kept ref either way.
func TestGitLandEndToEnd(t *testing.T) {
	bin := rpctest.BuildConnector(t, "git-land")
	c := rpctest.Start(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if decl.Type != "git-land" || decl.Kind != plugin.KindConnector {
		t.Fatalf("decl = %s/%s, want connector/git-land", decl.Kind, decl.Type)
	}

	type world struct{ remote, ws, store, base string }
	setup := func(t *testing.T) world {
		root := t.TempDir()
		w := world{remote: filepath.Join(root, "remote.git"), ws: filepath.Join(root, "ws"), store: filepath.Join(root, "land", "store.git")}
		git(t, root, "init", "-q", "--bare", "-b", "main", w.remote)
		git(t, root, "clone", "-q", w.remote, w.ws)
		write(t, w.ws, "app.txt", "one\ntwo\nthree\n")
		git(t, w.ws, "add", "app.txt")
		git(t, w.ws, "commit", "-qm", "base")
		git(t, w.ws, "push", "-q", "origin", "HEAD:refs/heads/pr-7")
		w.base = git(t, w.ws, "rev-parse", "HEAD")
		return w
	}
	// moveRemote commits on pr-7 elsewhere (another author's push).
	moveRemote := func(t *testing.T, w world, file, body string) string {
		other := filepath.Join(t.TempDir(), "other")
		git(t, filepath.Dir(other), "clone", "-q", "-b", "pr-7", w.remote, other)
		write(t, other, file, body)
		git(t, other, "add", file)
		git(t, other, "commit", "-qm", "author's change")
		git(t, other, "push", "-q", "origin", "pr-7")
		return git(t, other, "rev-parse", "HEAD")
	}
	invoke := func(t *testing.T, w world, verb string, opts map[string]any) map[string]any {
		t.Helper()
		out, err := c.Invoke(ctx, plugin.InvokeRequest{Verb: verb, Connection: map[string]any{"dir": w.store}, Options: opts})
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		return out
	}
	land := func(t *testing.T, w world) (kept, replayed map[string]any) {
		kept = invoke(t, w, "keep", map[string]any{"from": w.ws, "ref": "refs/kept/pr-7/a1"})
		if kept["exit_code"] != float64(0) || kept["sha"] != git(t, w.ws, "rev-parse", "HEAD") {
			t.Fatalf("keep = %v", kept)
		}
		replayed = invoke(t, w, "replay", map[string]any{"head": "refs/kept/pr-7/a1", "base": w.base, "remote": w.remote, "branch": "pr-7"})
		return kept, replayed
	}

	t.Run("a clean move replays and pushes", func(t *testing.T) {
		w := setup(t)
		write(t, w.ws, "app.txt", "one\ntwo\nthree\nfour\n")
		git(t, w.ws, "commit", "-qam", "fixer's change")
		moved := moveRemote(t, w, "README", "hi\n")
		_, r := land(t, w)
		if r["moved"] != true || r["clean"] != true || r["commits"] != float64(1) || r["onto"] != moved {
			t.Fatalf("replay = %v", r)
		}
		p := invoke(t, w, "push", map[string]any{"remote": w.remote, "rev": r["head"], "branch": "pr-7"})
		if p["pushed"] != true || p["exit_code"] != float64(0) {
			t.Fatalf("push = %v", p)
		}
		tip := git(t, w.ws, "ls-remote", w.remote, "refs/heads/pr-7")
		if !strings.HasPrefix(tip, r["head"].(string)) {
			t.Fatalf("remote pr-7 = %q, want %v", tip, r["head"])
		}
		if parent := git(t, w.store, "rev-parse", r["head"].(string)+"^"); parent != moved {
			t.Fatalf("replayed onto %s, want the moved head %s", parent, moved)
		}
		if msg := git(t, w.store, "log", "-1", "--format=%s", r["head"].(string)); msg != "fixer's change" {
			t.Fatalf("replayed message %q", msg)
		}
	})

	t.Run("a conflicting move reports the paths and keeps the work", func(t *testing.T) {
		w := setup(t)
		write(t, w.ws, "app.txt", "one\nfixer\nthree\n")
		git(t, w.ws, "commit", "-qam", "fixer's change")
		mine := git(t, w.ws, "rev-parse", "HEAD")
		moveRemote(t, w, "app.txt", "one\nauthor\nthree\n")
		_, r := land(t, w)
		if r["moved"] != true || r["clean"] != false || r["conflict_commit"] != mine {
			t.Fatalf("replay = %v", r)
		}
		if cs, _ := r["conflicts"].([]any); len(cs) != 1 || cs[0] != "app.txt" {
			t.Fatalf("conflicts = %v", r["conflicts"])
		}
		if got := git(t, w.store, "rev-parse", "refs/kept/pr-7/a1"); got != mine {
			t.Fatalf("kept ref = %s, want %s", got, mine)
		}
	})

	t.Run("an unmoved branch says so", func(t *testing.T) {
		w := setup(t)
		write(t, w.ws, "b.txt", "b\n")
		git(t, w.ws, "add", "b.txt")
		git(t, w.ws, "commit", "-qm", "b")
		if _, r := land(t, w); r["moved"] != false || r["onto"] != w.base || r["clean"] != false || str(r["head"]) != "" {
			t.Fatalf("replay = %v", r)
		}
	})

	t.Run("without a base it replays from the merge base", func(t *testing.T) {
		w := setup(t)
		write(t, w.ws, "app.txt", "one\ntwo\nthree\nfour\n")
		git(t, w.ws, "commit", "-qam", "fixer's change")
		moved := moveRemote(t, w, "README", "hi\n")
		invoke(t, w, "keep", map[string]any{"from": w.ws, "ref": "refs/kept/pr-7/a1"})
		r := invoke(t, w, "replay", map[string]any{"head": "refs/kept/pr-7/a1", "remote": w.remote, "branch": "pr-7"})
		if r["clean"] != true || r["commits"] != float64(1) || r["base"] != w.base || r["onto"] != moved {
			t.Fatalf("replay = %v", r)
		}
	})

	t.Run("a base that is not an ancestor of head is refused", func(t *testing.T) {
		w := setup(t)
		write(t, w.ws, "app.txt", "x\n")
		git(t, w.ws, "commit", "-qam", "x")
		moved := moveRemote(t, w, "README", "hi\n")
		invoke(t, w, "keep", map[string]any{"from": w.ws, "ref": "refs/kept/pr-7/a1"})
		invoke(t, w, "replay", map[string]any{"head": "refs/kept/pr-7/a1", "base": w.base, "remote": w.remote, "branch": "pr-7"}) // fetches moved
		if _, err := c.Invoke(ctx, plugin.InvokeRequest{Verb: "replay", Connection: map[string]any{"dir": w.store},
			Options: map[string]any{"head": "refs/kept/pr-7/a1", "base": moved, "onto": moved}}); err == nil {
			t.Fatal("want a refusal")
		}
	})

	t.Run("a change the new head already holds is dropped", func(t *testing.T) {
		w := setup(t)
		write(t, w.ws, "README", "hi\n")
		git(t, w.ws, "add", "README")
		git(t, w.ws, "commit", "-qm", "same change")
		moved := moveRemote(t, w, "README", "hi\n")
		if _, r := land(t, w); r["clean"] != true || r["commits"] != float64(0) || r["dropped"] != float64(1) || r["head"] != moved {
			t.Fatalf("replay = %v", r)
		}
	})

	t.Run("a merge in the range is refused", func(t *testing.T) {
		w := setup(t)
		git(t, w.ws, "checkout", "-qb", "side")
		write(t, w.ws, "s.txt", "s\n")
		git(t, w.ws, "add", "s.txt")
		git(t, w.ws, "commit", "-qm", "side")
		git(t, w.ws, "checkout", "-q", "-")
		write(t, w.ws, "m.txt", "m\n")
		git(t, w.ws, "add", "m.txt")
		git(t, w.ws, "commit", "-qm", "main")
		git(t, w.ws, "merge", "-q", "--no-edit", "side")
		moveRemote(t, w, "README", "hi\n")
		if _, r := land(t, w); r["clean"] != false || !strings.Contains(str(r["reason"]), "merge") {
			t.Fatalf("replay = %v", r)
		}
	})

	t.Run("push is fast-forward only", func(t *testing.T) {
		w := setup(t)
		write(t, w.ws, "app.txt", "x\n")
		git(t, w.ws, "commit", "-qam", "x")
		moveRemote(t, w, "README", "hi\n")
		invoke(t, w, "keep", map[string]any{"from": w.ws, "ref": "refs/kept/pr-7/a1"})
		// The un-replayed commit no longer descends from the remote head.
		p := invoke(t, w, "push", map[string]any{"remote": w.remote, "rev": "refs/kept/pr-7/a1", "branch": "pr-7"})
		if p["pushed"] != false || p["rejected"] != true || p["exit_code"] == float64(0) {
			t.Fatalf("push = %v", p)
		}
		for _, bad := range []map[string]any{
			{"remote": w.remote, "rev": "+refs/kept/pr-7/a1", "branch": "pr-7"},
			{"remote": "--upload-pack=x", "rev": "refs/kept/pr-7/a1", "branch": "pr-7"},
			{"remote": w.remote, "rev": "refs/kept/pr-7/a1", "branch": "a..b"},
		} {
			if _, err := c.Invoke(ctx, plugin.InvokeRequest{Verb: "push", Connection: map[string]any{"dir": w.store}, Options: bad}); err == nil {
				t.Errorf("push %v: want a refusal", bad)
			}
		}
	})

	t.Run("hooks never run", func(t *testing.T) {
		w := setup(t)
		write(t, w.ws, "app.txt", "one\ntwo\nthree\nfour\n")
		git(t, w.ws, "commit", "-qam", "fixer's change")
		moveRemote(t, w, "README", "hi\n")
		invoke(t, w, "keep", map[string]any{"from": w.ws, "ref": "refs/kept/pr-7/a1"})
		marker := filepath.Join(t.TempDir(), "ran")
		hook := filepath.Join(w.store, "hooks", "pre-push")
		write(t, filepath.Dir(hook), "pre-push", "#!/bin/sh\ntouch "+marker+"\nexit 1\n")
		if err := os.Chmod(hook, 0o755); err != nil {
			t.Fatal(err)
		}
		r := invoke(t, w, "replay", map[string]any{"head": "refs/kept/pr-7/a1", "base": w.base, "remote": w.remote, "branch": "pr-7"})
		if p := invoke(t, w, "push", map[string]any{"remote": w.remote, "rev": r["head"], "branch": "pr-7"}); p["pushed"] != true {
			t.Fatalf("push = %v", p)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a hook in the store ran")
		}
	})

	t.Run("sign signs the replayed commits", func(t *testing.T) {
		if _, err := exec.LookPath("ssh-keygen"); err != nil {
			t.Skip("no ssh-keygen")
		}
		w := setup(t)
		write(t, w.ws, "app.txt", "one\ntwo\nthree\nfour\n")
		git(t, w.ws, "commit", "-qam", "fixer's change")
		moveRemote(t, w, "README", "hi\n")
		key := filepath.Join(t.TempDir(), "signing")
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, out)
		}
		conn := map[string]any{"dir": w.store, "sign": true, "config": map[string]any{"gpg.format": "ssh", "user.signingkey": key}}
		for _, step := range []struct {
			verb string
			opts map[string]any
		}{
			{"keep", map[string]any{"from": w.ws, "ref": "refs/kept/pr-7/a1"}},
			{"replay", map[string]any{"head": "refs/kept/pr-7/a1", "base": w.base, "remote": w.remote, "branch": "pr-7"}},
		} {
			out, err := c.Invoke(ctx, plugin.InvokeRequest{Verb: step.verb, Connection: conn, Options: step.opts})
			if err != nil {
				t.Fatalf("%s: %v", step.verb, err)
			}
			if step.verb == "replay" {
				if raw := git(t, w.store, "cat-file", "commit", str(out["head"])); !strings.Contains(raw, "-----BEGIN SSH SIGNATURE-----") {
					t.Fatalf("replayed commit is not signed:\n%s", raw)
				}
			}
		}
	})

	t.Run("the store dir is required", func(t *testing.T) {
		if _, err := c.Invoke(ctx, plugin.InvokeRequest{Verb: "keep", Connection: map[string]any{}, Options: map[string]any{"from": "/x", "ref": "refs/k"}}); err == nil {
			t.Fatal("keep without a dir: want a refusal")
		}
	})
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func str(v any) string { s, _ := v.(string); return s }

package exposurekit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// findProcessAlive returns nil if pid is still alive (signal 0 delivered
// successfully), or an error otherwise. Unix-only; callers using it skip on
// windows.
func findProcessAlive(pidStr string) error {
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return err
	}
	return syscall.Kill(pid, syscall.Signal(0))
}

func TestLeasesAddReleaseStopInstance(t *testing.T) {
	l := NewLeases()
	var stoppedA, stoppedB int
	idA := l.Add("a", func() { stoppedA++ })
	idB := l.Add("b", func() { stoppedB++ })
	if l.Len() != 2 {
		t.Fatalf("len = %d, want 2", l.Len())
	}
	l.StopInstance("a")
	if stoppedA != 1 || stoppedB != 0 {
		t.Fatalf("stoppedA=%d stoppedB=%d", stoppedA, stoppedB)
	}
	if l.Len() != 1 {
		t.Fatalf("len after StopInstance = %d, want 1", l.Len())
	}
	// Releasing the already-stopped lease is a no-op, not a panic.
	l.Release(idA)
	if stoppedA != 1 {
		t.Fatalf("release of already-stopped lease re-ran stop")
	}
	l.Release(idB)
	if stoppedB != 1 || l.Len() != 0 {
		t.Fatalf("release: stoppedB=%d len=%d", stoppedB, l.Len())
	}
	// Releasing an unknown id must not panic.
	l.Release("no-such-id")
}

// TestLeasesReleaseAll is the regression test for finding #1 (children
// outlive the plugin): a main that calls ReleaseAll after plugin.Serve
// returns must tear down every still-open lease, across every instance, not
// just the one named in a "stop" RPC.
func TestLeasesReleaseAll(t *testing.T) {
	l := NewLeases()
	var stoppedA, stoppedB, stoppedC int
	l.Add("a", func() { stoppedA++ })
	l.Add("b", func() { stoppedB++ })
	l.Add("c", func() { stoppedC++ })
	if l.Len() != 3 {
		t.Fatalf("len = %d, want 3", l.Len())
	}
	l.ReleaseAll()
	if stoppedA != 1 || stoppedB != 1 || stoppedC != 1 {
		t.Fatalf("stoppedA=%d stoppedB=%d stoppedC=%d, want all 1", stoppedA, stoppedB, stoppedC)
	}
	if l.Len() != 0 {
		t.Fatalf("len after ReleaseAll = %d, want 0", l.Len())
	}
	// Idempotent / safe on an empty table.
	l.ReleaseAll()
}

// TestLeasesAddCappedEnforcesLimit is the regression test for finding #8 (no
// cap on concurrent leases per instance).
func TestLeasesAddCappedEnforcesLimit(t *testing.T) {
	l := NewLeases()
	const max = 3
	for i := 0; i < max; i++ {
		if _, err := l.AddCapped("inst", func() {}, max); err != nil {
			t.Fatalf("lease %d: unexpected error: %v", i, err)
		}
	}
	if _, err := l.AddCapped("inst", func() {}, max); err == nil {
		t.Fatal("expected an error at the cap, got none")
	}
	// A different instance is unaffected by another instance's cap.
	if _, err := l.AddCapped("other", func() {}, max); err != nil {
		t.Fatalf("a different instance should not be capped: %v", err)
	}
	// Freeing one slot on "inst" allows one more.
	l.StopInstance("inst")
	if _, err := l.AddCapped("inst", func() {}, max); err != nil {
		t.Fatalf("after StopInstance freed the slots: %v", err)
	}
}

func TestPortOf(t *testing.T) {
	cases := []struct {
		addr    string
		want    string
		wantErr bool
	}{
		{":8099", "8099", false},
		{"127.0.0.1:8099", "8099", false},
		{"0.0.0.0:9100", "9100", false},
		{"not-an-addr", "", true},
		// finding #4: PortOf must require a numeric 1-65535 port and reject a
		// host beginning with '-', since callers splice the result straight
		// into a vendor CLI's argv.
		{"127.0.0.1:--https=1234", "", true},
		{"127.0.0.1:0", "", true},
		{"127.0.0.1:65536", "", true},
		{"127.0.0.1:not-a-port", "", true},
		{"-evilhost:8099", "", true},
	}
	for _, tc := range cases {
		got, err := PortOf(tc.addr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("PortOf(%q): expected error", tc.addr)
			}
			continue
		}
		if err != nil {
			t.Errorf("PortOf(%q): unexpected error %v", tc.addr, err)
		}
		if got != tc.want {
			t.Errorf("PortOf(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func TestExpandArgv(t *testing.T) {
	got := ExpandArgv([]string{"--to", "localhost:{{.port}}", "at", "{{.addr}}"}, "8099", "127.0.0.1:8099")
	want := []string{"--to", "localhost:8099", "at", "127.0.0.1:8099"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRunAndScanFindsURLAndStops(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	url, stop, err := RunAndScan([]string{"sh", "-c", "echo noise; echo https://x.example; sleep 5"}, DefaultURL, 5*time.Second, nil)
	if err != nil || url != "https://x.example" {
		t.Fatalf("RunAndScan: %q %v", url, err)
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return — process not killed")
	}
	// Idempotent.
	stop()
}

func TestRunAndScanMissingBinary(t *testing.T) {
	if _, _, err := RunAndScan([]string{"definitely-not-a-real-binary-xyz"}, DefaultURL, time.Second, nil); err == nil {
		t.Fatal("expected an error for a missing binary")
	}
}

func TestRunAndScanTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	start := time.Now()
	_, _, err := RunAndScan([]string{"sh", "-c", "echo nothing-url-shaped; sleep 5"}, DefaultURL, 200*time.Millisecond, nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestRunAndScanCustomPattern(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	url, stop, err := RunAndScan([]string{"sh", "-c", "echo id=abc123; sleep 5"}, regexp.MustCompile(`id=\S+`), 5*time.Second, nil)
	if err != nil || url != "id=abc123" {
		t.Fatalf("RunAndScan: %q %v", url, err)
	}
	stop()
}

// TestRunAndScanKillsWholeProcessGroup is the regression test for finding #2
// (no process groups): stop() must kill a spawned tunnel's own children too,
// not just the direct child — otherwise a grandchild (here, a detached
// background "sleep") is reparented and keeps running after stop().
func TestRunAndScanKillsWholeProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are a POSIX concept")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := fmt.Sprintf(`echo https://g.example
(sleep 100 & echo $! > %s)
sleep 5`, pidFile)
	url, stop, err := RunAndScan([]string{"sh", "-c", script}, DefaultURL, 5*time.Second, nil)
	if err != nil || url != "https://g.example" {
		t.Fatalf("RunAndScan: %q %v", url, err)
	}

	var pidBytes []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pidBytes, _ = os.ReadFile(pidFile)
		if len(pidBytes) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatal("the grandchild never recorded its own pid")
	}

	stop()

	// The grandchild's pgid is unaffected by its reparenting when the
	// detaching subshell exited, so killing -pgid of the direct child must
	// have reached it too.
	deadline = time.Now().Add(2 * time.Second)
	for {
		if err := findProcessAlive(pid); err != nil {
			return // gone — the whole group was killed
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid %s is still alive after stop()", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunAndScanStopToleratesSelfReapedProcess is part of the regression
// test for finding #6 (zombies): once the background reaper has already
// collected a process that exited on its own, stop() must still return
// promptly rather than hang on a second wait.
func TestRunAndScanStopToleratesSelfReapedProcess(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	url, stop, err := RunAndScan([]string{"sh", "-c", "echo https://x.example; exit 0"}, DefaultURL, 5*time.Second, nil)
	if err != nil || url != "https://x.example" {
		t.Fatalf("RunAndScan: %q %v", url, err)
	}
	// Give the process time to exit on its own, well before stop() runs.
	time.Sleep(300 * time.Millisecond)

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop() hung on an already-exited process")
	}
}

// TestRunAndScanReapsSelfExitedProcess is the regression test for finding #6
// (zombies when a tunnel process exits on its own): the background reaper
// must collect it even though nobody ever calls stop().
func TestRunAndScanReapsSelfExitedProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie-state check via /proc is linux-specific")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := fmt.Sprintf("echo https://z.example; echo $$ > %s; exit 0", pidFile)
	url, stop, err := RunAndScan([]string{"sh", "-c", script}, DefaultURL, 5*time.Second, nil)
	if err != nil || url != "https://z.example" {
		t.Fatalf("RunAndScan: %q %v", url, err)
	}
	defer stop() // must tolerate the process already being reaped below

	var pidBytes []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pidBytes, _ = os.ReadFile(pidFile)
		if len(pidBytes) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatal("sh never recorded its own pid")
	}

	// Nobody calls stop() here. Without a background reaper, a process that
	// exits on its own stays a zombie ('Z' in /proc/<pid>/stat) until stop()
	// eventually runs cmd.Wait() for it — which, for a tunnel that silently
	// died without being closed, may be never.
	deadline = time.Now().Add(2 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			return // reaped and gone from /proc entirely — reaping worked
		}
		if idx := strings.LastIndex(string(stat), ") "); idx >= 0 && idx+2 < len(stat) && stat[idx+2] != 'Z' {
			return // exited and not (or no longer) a zombie
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %s is still a zombie 2s after exiting on its own: %s", pid, stat)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunAndScanPreferredPrefersKnownPatternOverFirstMatch and
// TestRunAndScanPreferredFallsBackToLastMatch are kit-level unit tests for
// RunAndScanPreferred, backing sshtunnel's finding #7 fix.
func TestRunAndScanPreferredPrefersKnownPatternOverFirstMatch(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	known := regexp.MustCompile(`https://\S+\.known\.example`)
	url, stop, err := RunAndScanPreferred(
		[]string{"sh", "-c", "echo https://banner.example/not-it; echo https://x.known.example; sleep 5"},
		[]*regexp.Regexp{known}, DefaultURL, 5*time.Second, nil)
	if err != nil || url != "https://x.known.example" {
		t.Fatalf("RunAndScanPreferred: %q %v, want the known-pattern match, not the first line", url, err)
	}
	stop()
}

func TestRunAndScanPreferredFallsBackToLastMatch(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	known := regexp.MustCompile(`https://\S+\.known\.example`)
	url, stop, err := RunAndScanPreferred(
		[]string{"sh", "-c", "echo https://banner.example/first; echo https://banner.example/last; sleep 5"},
		[]*regexp.Regexp{known}, DefaultURL, 300*time.Millisecond, nil)
	if err != nil || url != "https://banner.example/last" {
		t.Fatalf("RunAndScanPreferred fallback: %q %v, want the LAST generic match", url, err)
	}
	stop()
}

func TestRunOnce(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	out, err := RunOnce(context.Background(), []string{"sh", "-c", "echo hello"}, 5*time.Second)
	if err != nil || out != "hello\n" {
		t.Fatalf("RunOnce: %q %v", out, err)
	}
}

package exposurekit

import (
	"context"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

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

func TestRunOnce(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	out, err := RunOnce(context.Background(), []string{"sh", "-c", "echo hello"}, 5*time.Second)
	if err != nil || out != "hello\n" {
		t.Fatalf("RunOnce: %q %v", out, err)
	}
}

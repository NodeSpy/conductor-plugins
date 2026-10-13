//go:build windows

package exposurekit

import "os/exec"

// SetNewProcessGroup is a no-op on windows: there is no POSIX process-group
// concept here, and the release still must build and run on windows. A
// spawned tunnel binary's own grandchildren are not reaped by KillProcessGroup
// on this platform.
func SetNewProcessGroup(cmd *exec.Cmd) {}

// KillProcessGroup kills just the direct child process on windows.
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

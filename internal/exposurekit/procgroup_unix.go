//go:build !windows

package exposurekit

import (
	"os/exec"
	"syscall"
)

// SetNewProcessGroup starts cmd in its own process group (setpgid, pgid ==
// the child's own pid): a spawned tunnel binary (ssh, cloudflared, ngrok, …)
// may itself fork helpers, and killing only the direct child leaves those
// behind. Pairs with KillProcessGroup.
func SetNewProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillProcessGroup sends SIGKILL to cmd's whole process group. Falls back to
// killing just the process if the group lookup fails (e.g. it already
// exited) — best-effort, never fatal, since this always runs on a release
// path.
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
}

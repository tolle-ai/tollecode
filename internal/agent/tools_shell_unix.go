//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the command in its own process group so the whole group —
// the shell plus every child it spawns — can be signalled at once.
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcGroup SIGKILLs the entire process group rooted at pid. With
// Setpgid=true the pgid equals the leader's pid, so the negative pid targets
// every process in the group — including children that would otherwise keep the
// stdout/stderr pipes open and hang the read loop, making the command
// impossible to stop.
func killProcGroup(pid int) {
	if pid <= 0 {
		return
	}
	syscall.Kill(-pid, syscall.SIGKILL)
}

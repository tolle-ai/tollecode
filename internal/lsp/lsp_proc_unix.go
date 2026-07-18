//go:build !windows

package lsp

import (
	"os/exec"
	"syscall"
)

func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcGroup kills the process group rooted at pid, which on Unix equals
// the pgid when Setpgid=true. The negative pid targets the whole group.
func killProcGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL)
}

//go:build windows

package lsp

import "os/exec"

func setProcGroup(_ *exec.Cmd) {}

func killProcGroup(pid int) {}

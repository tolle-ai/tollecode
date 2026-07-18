//go:build !windows

package main

import "syscall"

func sysProcDetach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

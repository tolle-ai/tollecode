//go:build windows

package agent

import "os/exec"

// Windows has no POSIX process groups; the exec package's own cancellation plus
// cmd.WaitDelay handle teardown, so these are no-ops. Process.Kill still targets
// the shell itself.
func setProcGroup(_ *exec.Cmd) {}

func killProcGroup(_ int) {}

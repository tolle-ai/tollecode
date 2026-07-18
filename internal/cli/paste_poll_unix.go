//go:build !windows

package cli

import (
	"time"

	"golang.org/x/sys/unix"
)

// waitReadable reports whether fd has data available to read within timeout.
// Used to tell a standalone Esc apart from the start of an escape sequence.
func waitReadable(fd uintptr, timeout time.Duration) (bool, error) {
	ms := int(timeout / time.Millisecond)
	if ms <= 0 {
		ms = 1
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, ms)
		if err == unix.EINTR {
			continue // interrupted by a signal — retry the poll
		}
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
}

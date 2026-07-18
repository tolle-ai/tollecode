//go:build !windows

package cli

import (
	"os"
	"syscall"

	"golang.org/x/term"
)

// flushStdin discards any bytes already waiting in the terminal input queue.
// It briefly switches to raw mode so partial input (e.g. a lone Esc with no
// trailing newline) is drained too, then restores the previous state. On a
// non-TTY stdin (piped input, cloud mode) MakeRaw fails and this is a no-op,
// so scripted/piped answers are never discarded.
func flushStdin() {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return
	}
	defer term.Restore(fd, old)

	buf := make([]byte, 256)
	for {
		ready, err := waitReadable(uintptr(fd), 0)
		if err != nil || !ready {
			return
		}
		if _, err := syscall.Read(fd, buf); err != nil {
			return
		}
	}
}

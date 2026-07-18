//go:build darwin

package cli

import "golang.org/x/sys/unix"

// Darwin/BSD termios ioctl request numbers.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

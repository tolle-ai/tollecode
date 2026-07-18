//go:build linux

package cli

import "golang.org/x/sys/unix"

// Linux termios ioctl request numbers.
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

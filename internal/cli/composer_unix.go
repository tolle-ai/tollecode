//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// composerSupported gates the pinned composer to the platforms that also have
// the termios key watcher (escwatch_unix.go), so pinning and during-turn
// typing always ship together.
const composerSupported = true

// cursorRow asks the terminal where the cursor is (DSR, ESC[6n) and parses
// the ESC[<row>;<col>R reply from stdin, so composer setup can keep printing
// content from where the banner ended instead of jumping to the region
// bottom. Runs before readline/escwatch start, so stdin has no competing
// reader. Reads are gated by poll(2) with a hard time budget (os.File
// deadlines silently don't fire on tty fds), so a terminal that never
// answers can't hang startup. Returns 0 when the position can't be
// determined — callers fall back to the region-bottom behavior.
func cursorRow() int {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd) // no ICANON/ECHO: the reply must not echo or line-buffer
	if err != nil {
		return 0
	}
	defer term.Restore(fd, old)
	if _, err := os.Stdout.WriteString("\033[6n"); err != nil {
		return 0
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	var got []byte
	for len(got) < 64 && !bytes.ContainsRune(got, 'R') {
		remain := time.Until(deadline)
		if remain <= 0 {
			return 0
		}
		if ok, err := waitReadable(uintptr(fd), remain); err != nil || !ok {
			return 0
		}
		var chunk [32]byte
		m, err := unix.Read(fd, chunk[:])
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || m <= 0 {
			return 0
		}
		got = append(got, chunk[:m]...)
	}
	// Reply: ESC [ <row> ; <col> R — scan from the last ESC[ so any queued
	// typed-ahead bytes before the reply don't break the parse.
	s := string(got)
	i := strings.LastIndex(s, "\033[")
	if i < 0 {
		return 0
	}
	s = s[i+2:]
	j := strings.IndexByte(s, ';')
	if j < 0 {
		return 0
	}
	row, err := strconv.Atoi(s[:j])
	if err != nil || row < 1 {
		return 0
	}
	return row
}

// watchResize re-anchors the pinned rows on terminal resize (SIGWINCH).
// Stopped by closing winchStop (teardown).
func (c *composer) watchResize() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	stop := c.winchStop
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-stop:
				return
			case <-ch:
				c.resize()
			}
		}
	}()
}

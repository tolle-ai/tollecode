package cli

import (
	"bytes"
	"io"
)

// rlstdout.go bounds readline's screen erasure so it coexists with the pinned
// composer. chzyer/readline's refresh cycle erases from the cursor to the end
// of the SCREEN (ESC[J) before every repaint — from the input row (h-1) that
// wipes the pinned hint row on the last terminal row, so the status line
// vanished whenever the idle prompt was active and only reappeared while a
// turn was running. While the composer is pinned, ESC[J is rewritten to
// ESC[K (erase to end of LINE): readline still clears its own wrapped rows
// explicitly with per-line ESC[2K, and endIdleInput re-clears the input+hint
// rows after a wrapped edit anyway. When the composer is inactive (legacy
// inline prompt) writes pass through untouched.

var (
	escEraseBelow = []byte("\x1b[J")
	escEraseLine  = []byte("\x1b[K")
)

// rlBoundedStdout is installed as readline.Config.Stdout — every terminal
// write readline makes goes through Write.
type rlBoundedStdout struct {
	c    *composer
	out  io.Writer
	tail []byte // trailing partial ESC[J prefix held between writes
}

func newRLBoundedStdout(c *composer, out io.Writer) *rlBoundedStdout {
	return &rlBoundedStdout{c: c, out: out}
}

func (w *rlBoundedStdout) Write(p []byte) (int, error) {
	data := p
	if len(w.tail) > 0 {
		data = append(w.tail, p...)
		w.tail = nil
	}
	if !w.c.isActive() {
		_, err := w.out.Write(data)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
	// Hold back a trailing "\x1b" / "\x1b[" so an ESC[J split across two
	// writes is still caught. Withholding it is invisible: the terminal
	// cannot render a partial escape sequence either.
	if hold := partialEraseSuffix(data); hold > 0 {
		w.tail = append([]byte(nil), data[len(data)-hold:]...)
		data = data[:len(data)-hold]
	}
	if _, err := w.out.Write(bytes.ReplaceAll(data, escEraseBelow, escEraseLine)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// partialEraseSuffix reports how many trailing bytes of p form a proper
// prefix of ESC[J (1 for a bare ESC, 2 for ESC-[), 0 otherwise.
func partialEraseSuffix(p []byte) int {
	if bytes.HasSuffix(p, []byte{0x1b, '['}) {
		return 2
	}
	if bytes.HasSuffix(p, []byte{0x1b}) {
		return 1
	}
	return 0
}

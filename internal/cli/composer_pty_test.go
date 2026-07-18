package cli

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestCursorRowDSR drives cursorRow over a real pseudo-terminal: a fake
// terminal on the pty master answers the DSR query (ESC[6n) with a cursor
// position report, which cursorRow must parse.
func TestCursorRowDSR(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = tty, tty
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	// Fake terminal: wait for the query on the master side, then reply with
	// a report placing the cursor on row 17.
	go func() {
		var seen []byte
		buf := make([]byte, 16)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				return
			}
			seen = append(seen, buf[:n]...)
			if bytes.Contains(seen, []byte("\033[6n")) {
				ptmx.WriteString("\033[17;5R")
				return
			}
		}
	}()

	res := make(chan int, 1)
	go func() { res <- cursorRow() }()
	select {
	case got := <-res:
		if got != 17 {
			t.Fatalf("cursorRow = %d, want 17", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cursorRow did not return")
	}
}

// TestCursorRowNoReply: a terminal that never answers must not hang startup —
// cursorRow falls back to 0 once its read deadline passes.
func TestCursorRowNoReply(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = tty, tty
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	go func() { // swallow the query, never reply
		buf := make([]byte, 16)
		for {
			if _, err := ptmx.Read(buf); err != nil {
				return
			}
		}
	}()

	res := make(chan int, 1)
	go func() { res <- cursorRow() }()
	select {
	case got := <-res:
		if got != 0 {
			t.Fatalf("cursorRow = %d, want 0 on no reply", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cursorRow hung on a silent terminal")
	}
}

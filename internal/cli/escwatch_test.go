//go:build darwin || linux

package cli

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
)

// withPTYStdin points os.Stdin at a pty slave and drains the master, so the
// cbreak key watcher can be driven with real key bytes.
func withPTYStdin(t *testing.T) (write func([]byte), cleanup func()) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	old := os.Stdin
	os.Stdin = tty
	go io.Copy(io.Discard, ptmx)
	return func(b []byte) { ptmx.Write(b) },
		func() { os.Stdin = old; tty.Close(); ptmx.Close() }
}

func TestKeyWatcherEscCancels(t *testing.T) {
	write, cleanup := withPTYStdin(t)
	defer cleanup()

	cancelled := make(chan struct{}, 1)
	w := startKeyWatch(func() {
		select {
		case cancelled <- struct{}{}:
		default:
		}
	}, nil)
	if w == nil {
		t.Fatal("watcher did not start on a pty")
	}
	defer w.stop()

	time.Sleep(60 * time.Millisecond) // let the watcher arm
	write([]byte{0x1b})               // lone Esc

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("Esc did not cancel the turn")
	}
}

func TestKeyWatcherArrowDoesNotCancel(t *testing.T) {
	write, cleanup := withPTYStdin(t)
	defer cleanup()

	cancelled := make(chan struct{}, 1)
	w := startKeyWatch(func() {
		select {
		case cancelled <- struct{}{}:
		default:
		}
	}, nil)
	if w == nil {
		t.Fatal("watcher did not start on a pty")
	}
	defer w.stop()

	time.Sleep(60 * time.Millisecond)
	write([]byte{0x1b, '[', 'B'}) // Down arrow — must NOT cancel
	write([]byte{0x1b, '[', 'A'}) // Up arrow

	select {
	case <-cancelled:
		t.Fatal("arrow key wrongly cancelled the turn")
	case <-time.After(300 * time.Millisecond):
		// good — no cancel
	}
}

func TestKeyWatcherPausedIgnoresEsc(t *testing.T) {
	write, cleanup := withPTYStdin(t)
	defer cleanup()

	cancelled := make(chan struct{}, 1)
	w := startKeyWatch(func() {
		select {
		case cancelled <- struct{}{}:
		default:
		}
	}, nil)
	if w == nil {
		t.Fatal("watcher did not start on a pty")
	}
	defer w.stop()

	w.pause() // a picker owns stdin now
	time.Sleep(60 * time.Millisecond)
	write([]byte{0x1b})

	select {
	case <-cancelled:
		t.Fatal("paused watcher should not cancel")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestKeyWatcherTypesIntoComposer(t *testing.T) {
	write, cleanup := withPTYStdin(t)
	defer cleanup()

	comp := testComposer()
	cancelled := make(chan struct{}, 1)
	w := startKeyWatch(func() {
		select {
		case cancelled <- struct{}{}:
		default:
		}
	}, comp)
	if w == nil {
		t.Fatal("watcher did not start on a pty")
	}
	defer w.stop()

	time.Sleep(60 * time.Millisecond)

	// Typed text lands in the composer; Enter queues it.
	write([]byte("hi there\r"))
	waitFor(t, func() bool {
		q := comp.snapshotQueued()
		return len(q) == 1 && q[0] == "hi there"
	}, "typed message to be queued")

	// Backspace edits the buffer.
	write([]byte("abcd"))
	write([]byte{0x7f})
	waitFor(t, func() bool { return comp.snapshotBuf() == "abc" }, "backspace to apply")

	// Esc with buffered text clears it instead of cancelling…
	write([]byte{0x1b})
	waitFor(t, func() bool { return comp.snapshotBuf() == "" }, "esc to clear the buffer")
	time.Sleep(150 * time.Millisecond)
	select {
	case <-cancelled:
		t.Fatal("esc with buffered text must not cancel the turn")
	default:
	}

	// …and with an empty buffer it cancels, as before.
	write([]byte{0x1b})
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("esc with empty buffer did not cancel")
	}

	// Arrow keys are still swallowed, not inserted.
	write([]byte{0x1b, '[', 'B'})
	time.Sleep(150 * time.Millisecond)
	if got := comp.snapshotBuf(); got != "" {
		t.Fatalf("arrow key leaked into the composer: %q", got)
	}
}

func TestKeyWatcherPasteIntoComposer(t *testing.T) {
	write, cleanup := withPTYStdin(t)
	defer cleanup()

	comp := testComposer()
	w := startKeyWatch(func() {}, comp)
	if w == nil {
		t.Fatal("watcher did not start on a pty")
	}
	defer w.stop()

	time.Sleep(60 * time.Millisecond)

	// Bracketed paste split across writes, including a split end marker.
	// Terminals separate pasted lines with a lone CR (which the pty's ICRNL
	// then maps to NL before the watcher reads it).
	write([]byte("\x1b[200~li"))
	write([]byte("ne1\rline2\x1b[2"))
	write([]byte("01~"))
	waitFor(t, func() bool { return comp.snapshotBuf() == "line1\nline2" }, "paste to land in the buffer")

	// Enter queues the multi-line message whole.
	write([]byte("\r"))
	waitFor(t, func() bool {
		q := comp.snapshotQueued()
		return len(q) == 1 && q[0] == "line1\nline2"
	}, "pasted message to be queued")
}

func TestConsumeEscSequence(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"\x1b", 0},      // lone Esc — undecidable here
		{"\x1b[", 0},     // partial CSI
		{"\x1b[A", 3},    // arrow
		{"\x1b[1;5C", 6}, // modified arrow
		{"\x1bOP", 3},    // SS3
		{"\x1bx", 2},     // Alt+key
	}
	for _, c := range cases {
		if got := consumeEscSequence([]byte(c.in)); got != c.want {
			t.Errorf("consumeEscSequence(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

//go:build darwin || linux

package cli

import (
	"strings"
	"testing"
)

// newTestWatcher builds a watcher over a pinned composer, with a cancel hook
// that records whether the turn would have been interrupted.
func newTestWatcher() (*keyWatcher, *composer, *bool) {
	c := &composer{out: &strings.Builder{}, enabled: true, active: true, h: 24, w: 80}
	cancelled := false
	w := &keyWatcher{comp: c, cancel: func() { cancelled = true }}
	return w, c, &cancelled
}

// marked renders the buffer with the caret position shown as "|".
func marked(c *composer) string {
	r := []rune(string(c.buf))
	return string(r[:c.cur]) + "|" + string(r[c.cur:])
}

// TestComposerEditingKeys drives the real key watcher with the byte sequences a
// terminal actually sends, so the escape decoding and the composer's caret
// arithmetic are covered together.
func TestComposerEditingKeys(t *testing.T) {
	const (
		left   = "\x1b[D"
		right  = "\x1b[C"
		home   = "\x1b[H"
		end    = "\x1b[F"
		del    = "\x1b[3~"
		wleft  = "\x1b[1;5D"
		wright = "\x1b[1;5C"
		ss3lf  = "\x1bOD" // application cursor-key mode
		bs     = "\x7f"
	)
	cases := []struct{ name, input, want string }{
		{"typing leaves the caret at the end", "hello", "hello|"},
		{"left arrow moves back", "hello" + left + left, "hel|lo"},
		{"right arrow moves forward", "hello" + home + right, "h|ello"},
		{"insert happens at the caret", "hello" + left + left + "XY", "helXY|lo"},
		{"backspace deletes before the caret", "hello" + left + left + bs, "he|lo"},
		{"delete removes under the caret", "hello" + left + left + del, "hel|o"},
		{"home jumps to the start", "hello" + home + ">", ">|hello"},
		{"end jumps to the end", "hello" + home + end + "!", "hello!|"},
		{"word-left skips a word", "foo bar baz" + wleft + "Z", "foo bar Z|baz"},
		{"word-right from home", "foo bar" + home + wright + "Z", "fooZ| bar"},
		{"SS3 arrows work too", "hello" + ss3lf + ss3lf, "hel|lo"},
		{"ctrl-a is home", "hello\x01>", ">|hello"},
		{"ctrl-e is end", "hello\x01\x05!", "hello!|"},
		{"ctrl-b/ctrl-f move by one", "hello\x02\x02\x06", "hell|o"},
		{"ctrl-w deletes the previous word", "foo bar baz\x17", "foo bar |"},
		{"caret clamps at the end", "hi" + right + right + "!", "hi!|"},
		{"caret clamps at the start", "hi" + left + left + left + ">", ">|hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, c, _ := newTestWatcher()
			w.pend = append(w.pend, tc.input...)
			w.process()
			if got := marked(c); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSplitEscapeIsNotMistakenForEscapeKey: with VMIN=1 the terminal routinely
// delivers the ESC of an arrow key on its own, so the watcher must not treat a
// held ESC as a standalone Escape on the first quiet poll. Doing so cancelled
// the turn and then typed the sequence's tail ("[D") into the composer as text.
func TestSplitEscapeIsNotMistakenForEscapeKey(t *testing.T) {
	for _, seq := range []string{"\x1b[D", "\x1b[C", "\x1b[A", "\x1b[1;5D", "\x1bOD"} {
		for split := 1; split < len(seq); split++ {
			w, c, cancelled := newTestWatcher()
			w.comp.buf = []rune("draft")
			w.comp.cur = 5

			w.pend = append(w.pend, seq[:split]...)
			w.process()
			w.resolveIdleEscape() // a quiet poll: starts the clock, decides nothing
			w.pend = append(w.pend, seq[split:]...)
			w.process()

			if *cancelled {
				t.Errorf("seq %q split at %d cancelled the turn", seq, split)
			}
			if got := string(c.buf); got != "draft" {
				t.Errorf("seq %q split at %d corrupted the buffer: %q", seq, split, got)
			}
		}
	}
}

// TestOrphanedEscapeTailIsSwallowed: if the settle window really does expire
// (a slow link) the ESC is consumed as a standalone Escape — but when the tail
// then turns up it must be discarded, not typed as literal "[D".
func TestOrphanedEscapeTailIsSwallowed(t *testing.T) {
	for _, seq := range []string{"\x1b[D", "\x1b[1;5D", "\x1bOD"} {
		w, c, _ := newTestWatcher()

		w.pend = append(w.pend, seq[:1]...)
		w.process()
		w.resolveIdleEscape() // starts the settle clock
		w.escHeldAt = w.escHeldAt.Add(-2 * escapeSettle)
		w.resolveIdleEscape() // settle expired — declares a standalone Escape
		if !w.escOrphaned {
			t.Fatalf("seq %q: expected the ESC to be marked orphaned", seq)
		}
		w.pend = append(w.pend, seq[1:]...)
		w.process()

		if got := string(c.buf); got != "" {
			t.Errorf("seq %q leaked its tail into the composer: %q", seq, got)
		}
	}
}

// TestOrphanFlagDoesNotEatRealInput: the orphan guard must only claim a genuine
// CSI/SS3 tail. Ordinary text typed right after an Escape has to survive.
func TestOrphanFlagDoesNotEatRealInput(t *testing.T) {
	w, c, _ := newTestWatcher()
	w.escOrphaned = true
	w.pend = append(w.pend, "hello"...)
	w.process()
	if got := string(c.buf); got != "hello" {
		t.Errorf("orphan guard swallowed real input: %q", got)
	}
	if w.escOrphaned {
		t.Error("orphan flag should clear once ordinary input arrives")
	}
}

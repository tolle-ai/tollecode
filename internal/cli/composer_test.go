package cli

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// testComposer returns an installed (active) composer whose terminal writes go
// to io.Discard, for exercising the buffer/queue state machine without a TTY.
func testComposer() *composer {
	return &composer{out: io.Discard, enabled: true, active: true, h: 24, w: 80}
}

// snapshotBuf / snapshotQueued read composer state under the lock (the key
// watcher goroutine mutates it concurrently in the PTY tests).
func (c *composer) snapshotBuf() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

func (c *composer) snapshotQueued() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.queued...)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestComposerQueueAndPartialBuffer(t *testing.T) {
	c := testComposer()
	c.insertRunes([]rune("hello"))
	c.enterPressed()
	c.insertRunes([]rune("world"))
	c.enterPressed()
	c.insertRunes([]rune("part"))

	if msg, ok := c.dequeue(); !ok || msg != "hello" {
		t.Fatalf("first dequeue = %q, %v", msg, ok)
	}
	if msg, ok := c.dequeue(); !ok || msg != "world" {
		t.Fatalf("second dequeue = %q, %v", msg, ok)
	}
	if msg, ok := c.dequeue(); ok {
		t.Fatalf("queue should be empty, got %q", msg)
	}
	if got := c.takeBuffer(); got != "part" {
		t.Fatalf("takeBuffer = %q, want %q", got, "part")
	}
	if got := c.takeBuffer(); got != "" {
		t.Fatalf("second takeBuffer = %q, want empty", got)
	}
}

func TestComposerEnterIgnoresBlank(t *testing.T) {
	c := testComposer()
	c.insertRunes([]rune("   "))
	c.enterPressed()
	if msg, ok := c.dequeue(); ok {
		t.Fatalf("blank line should not queue, got %q", msg)
	}
}

func TestComposerBackspaceAndClear(t *testing.T) {
	c := testComposer()
	c.insertRunes([]rune("abc"))
	c.backspace()
	if got := c.snapshotBuf(); got != "ab" {
		t.Fatalf("after backspace buf = %q, want %q", got, "ab")
	}
	if !c.clearBuf() {
		t.Fatal("clearBuf with text should report true")
	}
	if c.clearBuf() {
		t.Fatal("clearBuf when empty should report false")
	}
}

func TestComposerNilAndDisabledNoOp(t *testing.T) {
	var c *composer
	c.insertRunes([]rune("x"))
	c.enterPressed()
	if c.isActive() {
		t.Fatal("nil composer reports active")
	}
	if _, ok := c.dequeue(); ok {
		t.Fatal("nil composer queued a message")
	}
	if got := c.takeBuffer(); got != "" {
		t.Fatalf("nil takeBuffer = %q", got)
	}

	d := &composer{out: io.Discard} // constructed but never set up
	d.insertRunes([]rune("x"))
	d.enterPressed()
	if _, ok := d.dequeue(); ok {
		t.Fatal("inactive composer queued a message")
	}
}

func TestComposerInputLineFitsWidth(t *testing.T) {
	c := testComposer()
	c.w = 30
	c.queued = []string{"a"}
	c.buf = []rune(strings.Repeat("x", 100))
	line := c.inputLine()
	if !strings.Contains(line, "⏎ 1 queued") {
		t.Fatalf("input row missing queued note: %q", line)
	}
	if got := visibleWidth(line); got > c.w {
		t.Fatalf("input row overflows: %d > %d", got, c.w)
	}
	// Newlines from pastes must render on the single row.
	c.buf = []rune("line1\nline2")
	if line := c.inputLine(); strings.Contains(line, "\n") {
		t.Fatalf("input row contains a raw newline: %q", line)
	}
}

// TestComposerInputLineCaret: the caret is painted (reverse video) rather than
// moved to, since the real cursor stays in the scroll region during a turn. It
// must be visible at every position, and the row must stay strictly inside the
// terminal width at every caret position — touching the last column makes the
// terminal wrap the pinned row.
func TestComposerInputLineCaret(t *testing.T) {
	c := testComposer()
	c.w = 40
	c.buf = []rune("hello")

	for _, cur := range []int{0, 1, 5} {
		c.cur = cur
		line := c.inputLine()
		if !strings.Contains(line, ansiReverse) {
			t.Errorf("cur=%d: no caret rendered: %q", cur, line)
		}
	}
	// An empty buffer still shows a caret to type at.
	c.buf, c.cur = nil, 0
	if line := c.inputLine(); !strings.Contains(line, ansiReverse) {
		t.Errorf("empty buffer has no caret: %q", line)
	}

	// A buffer far wider than the row scrolls horizontally: the caret stays
	// visible and the row never reaches the last column.
	c.buf = []rune(strings.Repeat("abcde", 40))
	for _, cur := range []int{0, 1, 37, 60, 199, 200} {
		c.cur = cur
		line := c.inputLine()
		if !strings.Contains(line, ansiReverse) {
			t.Errorf("cur=%d: caret scrolled out of view: %q", cur, line)
		}
		if got := visibleWidth(line); got >= c.w {
			t.Errorf("cur=%d: input row reaches the last column: %d >= %d", cur, got, c.w)
		}
	}
}

// TestComposerResizeClearsStalePinnedRows: when the terminal grows taller,
// the pinned rows drawn at the old bottom land inside the new content area —
// resize must erase them or they linger mid-screen as ghost rule/❯/hint lines.
func TestComposerResizeClearsStalePinnedRows(t *testing.T) {
	var out strings.Builder
	c := &composer{out: &out, enabled: true, active: true, h: 25, w: 94}

	c.resizeTo(213, 64)

	// Every pinned row at the old bottom must be moved-to and cleared.
	for row := 25 - composerReservedRows + 1; row <= 25; row++ {
		want := fmt.Sprintf("\033[%d;1H\033[2K", row)
		if !strings.Contains(out.String(), want) {
			t.Fatalf("resize did not clear stale pinned row %d: %q", row, out.String())
		}
	}
	// And the region must be re-issued for the new height.
	wantRegion := fmt.Sprintf("\033[1;%dr", 64-composerReservedRows)
	if !strings.Contains(out.String(), wantRegion) {
		t.Fatalf("resize did not re-issue the region %q: %q", wantRegion, out.String())
	}

	// Shrinking must NOT emit clears for rows beyond the new screen (they
	// would clamp to the last row and wipe the freshly drawn pinned rows).
	out.Reset()
	c.resizeTo(94, 25)
	if strings.Contains(out.String(), "\033[62;1H\033[2K") {
		t.Fatalf("shrink resize cleared off-screen rows: %q", out.String())
	}
}

// TestComposerEmergencyResetWipesPinnedRows: the force-quit path (double
// Ctrl-C → os.Exit, which skips the deferred teardown) must leave the terminal
// as clean as teardown does. Dropping the scroll region is not enough — without
// an erase the pinned rows stay painted on screen after the process is gone.
func TestComposerEmergencyResetWipesPinnedRows(t *testing.T) {
	var out strings.Builder
	c := &composer{out: &out, enabled: true, active: true, h: 25, w: 94}

	c.emergencyReset()
	got := out.String()

	firstPinned := 25 - composerReservedRows + 1
	want := fmt.Sprintf("\033[r\033[%d;1H\033[J", firstPinned)
	if got != want {
		t.Fatalf("emergencyReset = %q, want %q", got, want)
	}
	// The region reset must precede the absolute move: while a region is
	// installed, a move to a row outside it is clamped, so erasing would start
	// from the wrong row and leave the composer behind.
	if strings.Index(got, "\033[r") > strings.Index(got, ";1H") {
		t.Errorf("region reset must come before the cursor move: %q", got)
	}

	// A composer too short to have ever pinned must emit nothing rather than
	// address row 0 or a negative row.
	out.Reset()
	(&composer{out: &out, h: 2}).emergencyReset()
	if out.String() != "" {
		t.Errorf("emergencyReset on an unpinnable composer wrote %q", out.String())
	}
}

// TestRLBoundedStdout: while the composer is pinned, readline's ESC[J
// (erase to end of screen — would wipe the pinned hint row) must be rewritten
// to ESC[K, including when the sequence is split across Write calls; with the
// composer inactive, bytes pass through untouched.
func TestRLBoundedStdout(t *testing.T) {
	var out strings.Builder
	c := &composer{out: io.Discard, enabled: true, active: true, h: 24, w: 80}
	w := newRLBoundedStdout(c, &out)

	if n, err := w.Write([]byte("abc\x1b[Jdef")); err != nil || n != 9 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := out.String(); got != "abc\x1b[Kdef" {
		t.Fatalf("pinned write = %q, want %q", got, "abc\x1b[Kdef")
	}

	// Sequence split across writes: the partial prefix is held, then rewritten.
	out.Reset()
	_, _ = w.Write([]byte("x\x1b["))
	_, _ = w.Write([]byte("Jy"))
	if got := out.String(); got != "x\x1b[Ky" {
		t.Fatalf("split write = %q, want %q", got, "x\x1b[Ky")
	}

	// ESC[2K (line clear) and other sequences must not be touched.
	out.Reset()
	_, _ = w.Write([]byte("\x1b[2K\x1b[A"))
	if got := out.String(); got != "\x1b[2K\x1b[A" {
		t.Fatalf("unrelated sequences rewritten: %q", got)
	}

	// Inactive composer: passthrough, including a previously held tail.
	out.Reset()
	_, _ = w.Write([]byte("q\x1b")) // tail held while active
	c.mu.Lock()
	c.active = false
	c.mu.Unlock()
	_, _ = w.Write([]byte("[Jz"))
	if got := out.String(); got != "q\x1b[Jz" {
		t.Fatalf("inactive write = %q, want %q", got, "q\x1b[Jz")
	}
}

func TestComposerHintLineFitsWidth(t *testing.T) {
	for _, w := range []int{20, 40, 60, 120} {
		line := composerHintLine("claude-opus-4-8", "build", []string{"alpha", "beta"}, "MyAgent", w)
		if got := visibleWidth(line); got > w {
			t.Fatalf("hint line overflows width %d: %d (%q)", w, got, line)
		}
	}
	// The mode segment must always survive.
	if line := composerHintLine("m", "plan", nil, "", 20); !strings.Contains(line, "PLAN") {
		t.Fatalf("hint line lost the mode segment: %q", line)
	}
}

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

// TestComposerResizeClearsStalePinnedRows: when the terminal grows taller,
// the pinned rows drawn at the old bottom land inside the new content area —
// resize must erase them or they linger mid-screen as ghost rule/❯/hint lines.
func TestComposerResizeClearsStalePinnedRows(t *testing.T) {
	var out strings.Builder
	c := &composer{out: &out, enabled: true, active: true, h: 25, w: 94}

	c.resizeTo(213, 64)

	// Old pinned rows (23, 24, 25 for h=25) must be moved-to and cleared.
	for _, row := range []int{23, 24, 25} {
		want := fmt.Sprintf("\033[%d;1H\033[2K", row)
		if !strings.Contains(out.String(), want) {
			t.Fatalf("resize did not clear stale pinned row %d: %q", row, out.String())
		}
	}
	// And the region must be re-issued for the new height (64 - 3 = 61).
	if !strings.Contains(out.String(), "\033[1;61r") {
		t.Fatalf("resize did not re-issue the region: %q", out.String())
	}

	// Shrinking must NOT emit clears for rows beyond the new screen (they
	// would clamp to the last row and wipe the freshly drawn pinned rows).
	out.Reset()
	c.resizeTo(94, 25)
	if strings.Contains(out.String(), "\033[62;1H\033[2K") {
		t.Fatalf("shrink resize cleared off-screen rows: %q", out.String())
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

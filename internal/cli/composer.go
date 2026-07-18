package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/term"
)

// composer.go implements the pinned bottom composer: an ANSI scroll region
// (DECSTBM) keeps the last three terminal rows fixed — a rule, the input row,
// and a hint row — while all conversation output scrolls in the region above,
// Claude Code-style. While a turn is streaming, keystrokes are routed here by
// the key watcher (escwatch_unix.go) so the user can keep typing: Enter queues
// the message for auto-send after the turn; unsubmitted text is prefilled into
// the next readline prompt.
//
// Invariants:
//  1. The real cursor lives inside the scroll region except while readline is
//     editing on the input row — so every existing fmt-based renderer
//     (StreamRenderer, gradientLoader, markdown, pickers) works unchanged.
//  2. Every pinned-row draw is a single Stdout write that saves and restores
//     the cursor (DECSC/DECRC). It can therefore interleave between loader
//     frames and renderer prints without moving the shared cursor or leaking
//     SGR state (DECRC restores attributes too).
//  3. While readline edits (idleInput), the DECSC slot belongs to the
//     long-lived bracket set in beginIdleInput; pinned redraws then use a
//     variant that skips the input row and ends with an absolute move back to
//     it instead of DECRC.

// composerReservedRows is the pinned area height: rule + input + hints.
const composerReservedRows = 3

// composerMinRows/Cols are the smallest terminal the pinned layout makes
// sense in; anything smaller falls back to the legacy inline prompt.
const (
	composerMinRows = 9
	composerMinCols = 20
)

// activeComposer points at the REPL's composer while pinning is active, for
// code that can't easily reach the REPL instance (picker sizing via
// contentRows, the force-quit terminal reset).
var activeComposer atomic.Pointer[composer]

type composer struct {
	mu      sync.Mutex
	out     io.Writer // terminal writer (os.Stdout; injectable for tests)
	enabled bool      // terminal + platform support pinning; false → every method no-ops
	active  bool      // region currently installed
	h, w    int       // cached terminal size (updated by resize)

	// Hint-row state (setHints).
	mode, model, agentLabel string
	activeSkills            []string

	// During-turn input state, fed by the key watcher.
	buf    []rune   // unsubmitted composer text
	queued []string // Enter-queued messages, drained by the REPL after the turn

	idleInput        bool // readline currently editing on the input row
	savedCursorValid bool // beginIdleInput's DECSC bracket survives (no resize since)

	// onIdleResize lets the REPL refresh readline's prompt after a mid-edit
	// resize (the composer can't reach the readline instance itself).
	onIdleResize func()

	winchStop chan struct{}
}

func newComposer() *composer { return &composer{out: os.Stdout} }

// setup probes the terminal and installs the scroll region. When the terminal
// is unsuitable (not a TTY, too small, unsupported platform) the composer
// stays disabled and every method is a no-op — the REPL then falls back to
// the legacy footer-above-prompt flow.
func (c *composer) setup() {
	if c == nil || !composerSupported {
		return
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h < composerMinRows || w < composerMinCols {
		return
	}
	// Where did the content flow (banner, provider lines) end? Asked BEFORE
	// taking the lock — the DSR round-trip reads stdin.
	row := cursorRow()
	c.mu.Lock()
	c.enabled = true
	c.active = true
	c.w, c.h = w, h
	c.winchStop = make(chan struct{})
	if row > 0 && row <= h-composerReservedRows {
		// Content ends above the future pinned area: install the region
		// (DECSTBM homes the cursor) and resume printing right where the
		// banner left off, so the conversation grows from the top of the
		// screen, Claude Code-style — not from the region bottom.
		fmt.Fprintf(c.out, "\033[1;%dr\033[%d;1H", h-composerReservedRows, row)
	} else {
		// Cursor already inside the future pinned area (terminal was full),
		// or its position is unknown: reserve the pinned rows while the whole
		// screen still scrolls (pushing existing content above them), then
		// install the region and resume at the region bottom.
		fmt.Fprintf(c.out, "\n\n\n\033[1;%dr\033[%d;1H", h-composerReservedRows, h-composerReservedRows)
	}
	c.redrawLocked()
	c.mu.Unlock()
	c.watchResize()
	activeComposer.Store(c)
}

// teardown clears the pinned rows and drops the scroll region, leaving the
// cursor where the content flow ended. Idempotent; deferred by the REPL.
func (c *composer) teardown() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.active {
		c.mu.Unlock()
		return
	}
	c.active = false
	if c.winchStop != nil {
		close(c.winchStop)
		c.winchStop = nil
	}
	// Save the content cursor, wipe the pinned rows, reset the region (which
	// homes the cursor), then restore — so whatever prints next (exit message,
	// shell prompt) lands right after the conversation content.
	fmt.Fprintf(c.out, "\0337\033[%d;1H\033[J\033[r\0338", c.h-composerReservedRows+1)
	c.mu.Unlock()
	activeComposer.Store(nil)
}

// emergencyReset is the force-quit path (os.Exit skips deferred teardown):
// one unconditional write that drops the region and parks the cursor on the
// last row. Deliberately lock-free — it must work from a signal handler even
// if the composer mutex is held.
func (c *composer) emergencyReset() {
	if c == nil {
		return
	}
	fmt.Fprintf(c.out, "\033[r\033[%d;1H\n", c.h)
}

// isActive reports whether the pinned layout is currently installed.
func (c *composer) isActive() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// setHints updates the hint-row content (and the input-row prompt color,
// which follows the mode) and repaints.
func (c *composer) setHints(mode, model string, skills []string, agentLabel string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode, c.model, c.agentLabel = mode, model, agentLabel
	c.activeSkills = skills
	c.redrawLocked()
}

// redraw repaints the pinned rows (Invariant 2).
func (c *composer) redraw() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.redrawLocked()
}

func (c *composer) redrawLocked() {
	if !c.active {
		return
	}
	var b strings.Builder
	if !c.idleInput {
		b.WriteString("\0337") // DECSC — matched by the DECRC below (Invariant 2)
	}
	// Rule row.
	fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.h-2)
	b.WriteString(c.ruleLine())
	// Input row — owned by readline while idle-editing (Invariant 3).
	if !c.idleInput {
		fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.h-1)
		b.WriteString(c.inputLine())
	}
	// Hint row.
	fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.h)
	b.WriteString(c.hintLine())
	if c.idleInput {
		// Leave the cursor on the input row for readline; the caller triggers
		// rl.Refresh() when this runs mid-edit (resize).
		fmt.Fprintf(&b, "\033[%d;1H", c.h-1)
	} else {
		b.WriteString("\0338") // DECRC
	}
	io.WriteString(c.out, b.String())
}

func (c *composer) ruleLine() string {
	w := c.w - 4
	if w < 4 {
		w = 4
	}
	return "  " + ansiDim + strings.Repeat("─", w) + ansiReset
}

func (c *composer) hintLine() string {
	return composerHintLine(c.model, c.mode, c.activeSkills, c.agentLabel, c.w)
}

// inputLine renders the composer input row: the mode-colored prompt glyph,
// the (tail of the) during-turn buffer, and a right-aligned queued-count note.
func (c *composer) inputLine() string {
	promptColor := colorGreen
	if c.mode == "plan" {
		promptColor = colorPrimary
	}
	queuedNote := ""
	if n := len(c.queued); n > 0 {
		queuedNote = fmt.Sprintf("⏎ %d queued", n)
	}
	avail := c.w - 5 // "  ❯ " prefix + last-column margin
	if queuedNote != "" {
		avail -= visibleWidth(queuedNote) + 2
	}
	if avail < 4 {
		avail = 4
	}
	// Newlines (multi-line pastes) render compactly on the single input row.
	text := strings.ReplaceAll(string(c.buf), "\n", "⏎")
	if rr := []rune(text); len(rr) > avail {
		text = "…" + string(rr[len(rr)-avail+1:]) // show the tail being typed
	}
	line := "  " + promptColor + ansiBold + "❯" + ansiReset + " " + text
	if queuedNote != "" {
		pad := c.w - 1 - visibleWidth(line) - visibleWidth(queuedNote)
		if pad < 1 {
			pad = 1
		}
		line += strings.Repeat(" ", pad) + ansiDim + queuedNote + ansiReset
	}
	return line
}

// ── Idle (readline) coexistence ───────────────────────────────────────────────

// beginIdleInput hands the input row to readline: saves the content cursor
// (the long-lived DECSC bracket, Invariant 3), moves to the cleared input
// row, and marks the idle-edit phase so redraws stop touching that row.
func (c *composer) beginIdleInput() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	c.idleInput = true
	c.savedCursorValid = true
	fmt.Fprintf(c.out, "\0337\033[%d;1H\033[2K", c.h-1)
}

// endIdleInput takes the input row back after Readline returns and restores
// the cursor into the scroll region so content printing resumes where it
// left off.
func (c *composer) endIdleInput() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active || !c.idleInput {
		return
	}
	c.idleInput = false
	// A long input may have wrapped onto the hint row — clear both rows;
	// redrawLocked repaints them right after.
	fmt.Fprintf(c.out, "\033[%d;1H\033[2K\033[%d;1H\033[2K", c.h-1, c.h)
	if c.savedCursorValid {
		io.WriteString(c.out, "\0338")
	} else {
		// The DECSC bracket was invalidated by a mid-edit resize; fall back to
		// the region bottom.
		fmt.Fprintf(c.out, "\033[%d;1H", c.h-composerReservedRows)
	}
	c.redrawLocked()
}

// ── During-turn buffer & queue (fed by the key watcher) ──────────────────────

// insertRunes appends typed (or pasted) runes to the composer buffer.
func (c *composer) insertRunes(rs []rune) {
	if c == nil || len(rs) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	c.buf = append(c.buf, rs...)
	c.redrawLocked()
}

// backspace removes the last buffered rune.
func (c *composer) backspace() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active || len(c.buf) == 0 {
		return
	}
	c.buf = c.buf[:len(c.buf)-1]
	c.redrawLocked()
}

// clearBuf clears any typed-during-turn text and reports whether there was
// any — the key watcher uses this to make the first Esc clear the text and
// the next one interrupt the turn.
func (c *composer) clearBuf() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active || len(c.buf) == 0 {
		return false
	}
	c.buf = nil
	c.redrawLocked()
	return true
}

// enterPressed queues the current buffer as a message to auto-send once the
// running turn finishes.
func (c *composer) enterPressed() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	text := strings.TrimSpace(string(c.buf))
	c.buf = nil
	if text != "" {
		c.queued = append(c.queued, text)
	}
	c.redrawLocked()
}

// dequeue pops the oldest queued message. ok=false when the queue is empty.
func (c *composer) dequeue() (msg string, ok bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queued) == 0 {
		return "", false
	}
	msg = c.queued[0]
	c.queued = c.queued[1:]
	c.redrawLocked()
	return msg, true
}

// takeBuffer removes and returns any unsubmitted during-turn text so the REPL
// can prefill it into the next readline prompt.
func (c *composer) takeBuffer() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) == 0 {
		return ""
	}
	s := string(c.buf)
	c.buf = nil
	c.redrawLocked()
	return s
}

// ── Sizing & resize ───────────────────────────────────────────────────────────

// contentRows returns the height of the scrollable content area.
func (c *composer) contentRows() int {
	if c == nil {
		_, h := terminalSize()
		return h
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active {
		return c.h - composerReservedRows
	}
	_, h := terminalSize()
	return h
}

// contentRows is the package-level variant for code that can't reach the
// REPL's composer (pickers, prompts): the scrollable rows above the pinned
// area, or the whole terminal height when pinning is off.
func contentRows() int {
	return activeComposer.Load().contentRows()
}

// resize re-anchors the region and pinned rows after a terminal size change.
func (c *composer) resize() {
	if c == nil {
		return
	}
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	c.resizeTo(w, h)
}

// resizeTo is resize's testable core: re-anchors to an explicit new size.
func (c *composer) resizeTo(w, h int) {
	c.mu.Lock()
	if !c.active || (w == c.w && h == c.h) {
		c.mu.Unlock()
		return
	}
	if h < composerMinRows || w < composerMinCols {
		// Too small for pinning now: drop the region and fall back to the
		// legacy flow (the REPL loop re-checks isActive every iteration).
		c.active = false
		c.enabled = false
		fmt.Fprintf(c.out, "\033[r\033[%d;1H", h)
		c.mu.Unlock()
		activeComposer.Store(nil)
		return
	}
	oldH := c.h
	c.w, c.h = w, h
	// Drop the region so absolute moves reach every row, and when the terminal
	// grew taller wipe the pinned rows at the OLD bottom — they now sit inside
	// the content area and would otherwise be stranded mid-screen as ghost
	// rule/input/hint lines. Then re-issue the region for the new height
	// (DECSTBM homes the cursor) and park the content cursor at the new region
	// bottom. Content may visually shift a line on resize — acceptable, it
	// self-heals as output continues.
	var b strings.Builder
	b.WriteString("\033[r")
	if h > oldH {
		for row := oldH - composerReservedRows + 1; row <= oldH; row++ {
			fmt.Fprintf(&b, "\033[%d;1H\033[2K", row)
		}
	}
	fmt.Fprintf(&b, "\033[1;%dr\033[%d;1H", h-composerReservedRows, h-composerReservedRows)
	io.WriteString(c.out, b.String())
	c.savedCursorValid = false
	c.redrawLocked()
	idle := c.idleInput
	hook := c.onIdleResize
	c.mu.Unlock()
	if idle && hook != nil {
		hook() // rl.Refresh() — repaint the prompt on the re-anchored input row
	}
}

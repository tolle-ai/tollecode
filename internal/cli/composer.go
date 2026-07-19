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

// composerReservedRows is the pinned area height. Bottom-up the rows are:
//
//	h-4  status   — the turn loader (pinned here, not printed inline, so it
//	               stays put instead of scrolling away with the transcript)
//	h-3  rule     — top border of the composer box
//	h-2  input    — the prompt row (owned by readline while idle-editing)
//	h-1  rule     — bottom border, separating the box from the hints
//	h    hints    — mode · model · shortcuts
const composerReservedRows = 5

// Row offsets from the terminal's last row, indexing the layout above.
const (
	rowHints      = 0
	rowRuleBottom = 1
	rowInput      = 2
	rowRuleTop    = 3
	rowStatus     = 4
)

// row converts a rowX offset into an absolute terminal row.
func (c *composer) row(offset int) int { return c.h - offset }

// composerMinRows/Cols are the smallest terminal the pinned layout makes
// sense in; anything smaller falls back to the legacy inline prompt. The
// minimum leaves at least four content rows above the pinned area.
const (
	composerMinRows = composerReservedRows + 4
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

	// status is the pinned status row's content — the loader's rendered line
	// while a turn runs, empty when idle (setStatus).
	status string

	// During-turn input state, fed by the key watcher.
	buf    []rune   // unsubmitted composer text
	cur    int      // caret position in buf, 0..len(buf) (len == at the end)
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
		// install the region and resume at the region bottom. The newline run
		// must track composerReservedRows — one per reserved row.
		fmt.Fprintf(c.out, "%s\033[1;%dr\033[%d;1H",
			strings.Repeat("\n", composerReservedRows), h-composerReservedRows, h-composerReservedRows)
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

// emergencyReset is the force-quit path (os.Exit skips deferred teardown): one
// unconditional write that drops the scroll region and wipes the pinned rows,
// leaving the cursor where the conversation content ended so the shell prompt
// lands there. Deliberately lock-free — it must work from a signal handler even
// if the composer mutex is held.
//
// Order matters: the region reset comes FIRST so the absolute move can address
// rows inside the former pinned area (a move is clamped to the region while one
// is installed), then ESC[J erases from the top of that area down. Without the
// erase the composer stays painted on screen after the process exits.
func (c *composer) emergencyReset() {
	if c == nil || c.h <= composerReservedRows {
		return
	}
	fmt.Fprintf(c.out, "\033[r\033[%d;1H\033[J", c.h-composerReservedRows+1)
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
	// Status row — the loader parks here while a turn runs; blank when idle.
	fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.row(rowStatus))
	b.WriteString(c.status)
	// Top rule.
	fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.row(rowRuleTop))
	b.WriteString(c.ruleLine())
	// Input row — owned by readline while idle-editing (Invariant 3).
	if !c.idleInput {
		fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.row(rowInput))
		b.WriteString(c.inputLine())
	}
	// Bottom rule — separates the input box from the hints below it.
	fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.row(rowRuleBottom))
	b.WriteString(c.ruleLine())
	// Hint row.
	fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.row(rowHints))
	b.WriteString(c.hintLine())
	if c.idleInput {
		// Leave the cursor on the input row for readline; the caller triggers
		// rl.Refresh() when this runs mid-edit (resize).
		fmt.Fprintf(&b, "\033[%d;1H", c.row(rowInput))
	} else {
		b.WriteString("\0338") // DECRC
	}
	io.WriteString(c.out, b.String())
}

// setStatus updates the pinned status row (the loader's line) and repaints it
// alone — this runs on the loader's 70ms ticker, so it must not redraw the
// whole pinned block. Invariant 2 applies: one write, DECSC/DECRC bracketed.
func (c *composer) setStatus(s string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active || s == c.status {
		return
	}
	c.status = s
	// While readline edits, the DECSC slot belongs to beginIdleInput's
	// long-lived bracket (Invariant 3) — end with an absolute move back to the
	// input row instead of clobbering it with a DECRC.
	if c.idleInput {
		fmt.Fprintf(c.out, "\033[%d;1H\033[2K%s\033[%d;1H",
			c.row(rowStatus), s, c.row(rowInput))
		return
	}
	fmt.Fprintf(c.out, "\0337\033[%d;1H\033[2K%s\0338", c.row(rowStatus), s)
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
	// The replacement is rune-for-rune, so caret indices carry over unchanged.
	runes := []rune(strings.ReplaceAll(string(c.buf), "\n", "⏎"))
	cur := c.cur
	if cur < 0 {
		cur = 0
	}
	if cur > len(runes) {
		cur = len(runes)
	}

	// Horizontal window: scroll the view so the caret stays visible on a buffer
	// longer than the row. One cell is reserved for a caret sitting past the
	// last character, which is where it is while you are simply typing.
	win := avail
	if win < 2 {
		win = 2
	}
	start := 0
	if cur >= win {
		start = cur - win + 1
	}
	if start > 0 {
		// A leading "…" marks the scrolled-off text and costs a cell, so the
		// window has to give one back — otherwise the row overruns into the
		// terminal's last column and wraps.
		if win > 1 {
			win--
		}
		if start = cur - win + 1; start < 0 {
			start = 0
		}
	}
	end := start + win
	if end > len(runes) {
		end = len(runes)
	}

	var text strings.Builder
	if start > 0 {
		text.WriteString(ansiDim + "…" + ansiReset)
	}
	for i := start; i < end; i++ {
		if i == cur {
			text.WriteString(ansiReverse + string(runes[i]) + ansiReset)
			continue
		}
		text.WriteRune(runes[i])
	}
	if cur >= end {
		text.WriteString(ansiReverse + " " + ansiReset) // caret past the last rune
	}
	line := "  " + promptColor + ansiBold + "❯" + ansiReset + " " + text.String()
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
	fmt.Fprintf(c.out, "\0337\033[%d;1H\033[2K", c.row(rowInput))
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
	// A long input may have wrapped past the input row onto the rows below it
	// — clear the input row and everything under it; redrawLocked repaints
	// them right after.
	var b strings.Builder
	for off := rowInput; off >= rowHints; off-- {
		fmt.Fprintf(&b, "\033[%d;1H\033[2K", c.row(off))
	}
	io.WriteString(c.out, b.String())
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

// insertRunes inserts typed (or pasted) runes at the caret.
func (c *composer) insertRunes(rs []rune) {
	if c == nil || len(rs) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	c.clampCur()
	c.buf = append(c.buf[:c.cur], append(append([]rune{}, rs...), c.buf[c.cur:]...)...)
	c.cur += len(rs)
	c.redrawLocked()
}

// backspace removes the rune before the caret.
func (c *composer) backspace() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clampCur()
	if !c.active || c.cur == 0 {
		return
	}
	c.buf = append(c.buf[:c.cur-1], c.buf[c.cur:]...)
	c.cur--
	c.redrawLocked()
}

// deleteForward removes the rune under the caret (the Delete key).
func (c *composer) deleteForward() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clampCur()
	if !c.active || c.cur >= len(c.buf) {
		return
	}
	c.buf = append(c.buf[:c.cur], c.buf[c.cur+1:]...)
	c.redrawLocked()
}

// moveCursor shifts the caret by delta runes, clamped to the buffer.
func (c *composer) moveCursor(delta int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	before := c.cur
	c.cur += delta
	c.clampCur()
	if c.cur != before {
		c.redrawLocked()
	}
}

// moveCursorTo parks the caret at the start (0) or the end (-1) of the buffer.
func (c *composer) moveCursorTo(pos int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	before := c.cur
	if pos < 0 {
		c.cur = len(c.buf)
	} else {
		c.cur = 0
	}
	if c.cur != before {
		c.redrawLocked()
	}
}

// moveWord shifts the caret one word left (dir<0) or right (dir>0), using the
// readline convention: skip any run of spaces, then the run of non-spaces.
func (c *composer) moveWord(dir int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	c.clampCur()
	before := c.cur
	if dir < 0 {
		for c.cur > 0 && c.buf[c.cur-1] == ' ' {
			c.cur--
		}
		for c.cur > 0 && c.buf[c.cur-1] != ' ' {
			c.cur--
		}
	} else {
		for c.cur < len(c.buf) && c.buf[c.cur] == ' ' {
			c.cur++
		}
		for c.cur < len(c.buf) && c.buf[c.cur] != ' ' {
			c.cur++
		}
	}
	if c.cur != before {
		c.redrawLocked()
	}
}

// deleteWordBack removes the word before the caret (Ctrl-W).
func (c *composer) deleteWordBack() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return
	}
	c.clampCur()
	end := c.cur
	for c.cur > 0 && c.buf[c.cur-1] == ' ' {
		c.cur--
	}
	for c.cur > 0 && c.buf[c.cur-1] != ' ' {
		c.cur--
	}
	if c.cur == end {
		return
	}
	c.buf = append(c.buf[:c.cur], c.buf[end:]...)
	c.redrawLocked()
}

// clampCur keeps the caret inside the buffer. Every mutation routes through it
// because the REPL can swap the buffer out (takeBuffer, enterPressed) between
// key events.
func (c *composer) clampCur() {
	if c.cur < 0 {
		c.cur = 0
	}
	if c.cur > len(c.buf) {
		c.cur = len(c.buf)
	}
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
	c.buf, c.cur = nil, 0
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
	c.buf, c.cur = nil, 0
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
	c.buf, c.cur = nil, 0
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

package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// promptMu serialises every interactive prompt that flips the terminal into raw
// mode (permission prompts, clarifications, free-text). Parallel sub-agents can
// reach these concurrently; without this lock two overlapping term.MakeRaw /
// term.Restore pairs race — the second captures the already-raw state as its
// "cooked" baseline and restores stdin to raw, leaving OPOST/ONLCR off so every
// later `\n` prints without a carriage return (the staircase text scatter).
var promptMu sync.Mutex

// choiceprompt.go implements the Claude Code–style numbered choice prompt used
// for shell-command permissions and clarification questions:
//
//	Do you want to proceed?
//	  1. Yes
//	❯ 2. Yes, and don't ask again this session
//	  3. No
//
// It runs in raw mode so the arrow keys drive a "❯" cursor instead of leaking
// through as literal ^[[A/^[[B. Digit keys jump straight to an option, Enter
// selects the highlighted row, and Esc / Ctrl-C cancel.

// permissionChoices are the options offered for a shell-command permission
// request, mirroring the Claude Code prompt (Yes / Yes-always / No).
var permissionChoices = []string{
	"Yes",
	"Yes, and don't ask again this session",
	"No",
}

// runChoicePrompt shows header (optional) above options and lets the user pick
// with ↑/↓ + Enter (or a digit). It returns the chosen 0-based index, or -1 if
// the user cancelled. initial is the row highlighted on entry. When stdin isn't
// a TTY it falls back to reading a number.
func runChoicePrompt(header string, options []string, initial int) int {
	if len(options) == 0 {
		return -1
	}
	cursor := initial
	if cursor < 0 || cursor >= len(options) {
		cursor = 0
	}

	// Serialise raw-mode entry so a concurrent prompt can't corrupt the terminal
	// state we capture and restore below.
	promptMu.Lock()
	defer promptMu.Unlock()

	// Take stdin from the turn key watcher for the duration of the picker.
	pauseKeyWatch()
	defer resumeKeyWatch()

	flushStdin() // drop stale typeahead so it isn't misread as a keypress

	headerLines := 0
	if header != "" {
		headerLines = 1
	}

	// Cap how many option rows are shown at once so a long list can't overrun
	// the scrollable area (which would desync the in-place cursor math) — the
	// content region above the pinned composer, or the whole screen without it.
	rows := contentRows()
	visible := len(options)
	if maxV := rows - headerLines - 3; maxV >= 3 && visible > maxV {
		visible = maxV
	}
	total := headerLines + visible + 1 // header + visible options + hint
	start := 0
	clampView := func() {
		if cursor < start {
			start = cursor
		}
		if cursor >= start+visible {
			start = cursor - visible + 1
		}
	}

	// Reserve the block's rows while still in cooked mode so a bottom-of-screen
	// scroll happens now, not mid-draw (which would desync the cursor math).
	for i := 0; i < total; i++ {
		fmt.Println()
	}
	fmt.Printf("\033[%dA\r", total)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		// No raw mode (piped/non-TTY): fall back to a numbered line read.
		return choicePromptCooked(header, options)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	clampView()
	drawChoicePrompt(header, options, cursor, start, visible)

	buf := make([]byte, 8)
	for {
		n, rerr := os.Stdin.Read(buf)
		if rerr != nil || n == 0 {
			clearChoicePrompt(total)
			return -1
		}
		b := buf[:n]
		switch {
		case n == 1 && b[0] == 27: // lone Esc → cancel
			clearChoicePrompt(total)
			return -1
		case n == 1 && b[0] == 3: // Ctrl-C → cancel
			clearChoicePrompt(total)
			return -1
		case n == 1 && (b[0] == 13 || b[0] == 10): // Enter → select highlighted
			clearChoicePrompt(total)
			return cursor
		case n >= 3 && b[0] == 27 && b[1] == '[' && b[2] == 'A', // Up (CSI)
			n >= 3 && b[0] == 27 && b[1] == 'O' && b[2] == 'A': // Up (SS3)
			if cursor > 0 {
				cursor--
			}
		case n >= 3 && b[0] == 27 && b[1] == '[' && b[2] == 'B', // Down (CSI)
			n >= 3 && b[0] == 27 && b[1] == 'O' && b[2] == 'B': // Down (SS3)
			if cursor < len(options)-1 {
				cursor++
			}
		case n == 1 && b[0] >= '1' && b[0] <= '9': // digit → jump + select
			if idx := int(b[0] - '1'); idx < len(options) {
				clearChoicePrompt(total)
				return idx
			}
		}
		clampView()
		drawChoicePrompt(header, options, cursor, start, visible)
	}
}

// drawChoicePrompt renders the block (a scrolling window [start, start+visible)
// of options) in place, leaving the cursor at its top row so the next redraw
// overwrites it cleanly.
func drawChoicePrompt(header string, options []string, cursor, start, visible int) {
	w, _ := terminalSize()
	labelW := w - 8
	if labelW < 12 {
		labelW = 12
	}

	headerLines := 0
	if header != "" {
		fmt.Printf("\r\033[2K  %s%s%s\n", ansiBold, header, ansiReset)
		headerLines = 1
	}

	for row := 0; row < visible; row++ {
		i := start + row
		if i >= len(options) {
			fmt.Print("\r\033[2K\n")
			continue
		}
		label := truncateVisible(options[i], labelW)
		if i == cursor {
			// Full-brightness (vs. the dimmed unselected rows below) is enough
			// contrast to read as "selected" without a dedicated color.
			fmt.Printf("\r\033[2K%s%s❯%s %s%s%d.%s %s%s\n",
				colorPrimary, ansiBold, ansiReset,
				colorPrimary, ansiBold, i+1, ansiReset,
				label, ansiReset)
		} else {
			fmt.Printf("\r\033[2K  %s%d.%s %s%s%s\n",
				colorPrimary, i+1, ansiReset,
				ansiDim, label, ansiReset)
		}
	}

	hint := "↑↓ navigate · enter select · esc cancel"
	if visible < len(options) {
		hint = fmt.Sprintf("%s · %d/%d", hint, cursor+1, len(options))
	}
	fmt.Printf("\r\033[2K  %s%s%s", ansiDim, hint, ansiReset)
	// Move back up to the first row of the block (hint has no trailing newline).
	fmt.Printf("\033[%dA\r", headerLines+visible)
}

// clearChoicePrompt erases the block and leaves the cursor at its top row so the
// caller can print a compact result in its place.
func clearChoicePrompt(total int) {
	for i := 0; i < total; i++ {
		fmt.Print("\r\033[2K\n")
	}
	fmt.Printf("\033[%dA\r", total)
}

// readFreeText prompts for and reads a single line of free-text input (cooked
// mode). Returns the trimmed text, or "" if the user just pressed Enter.
func readFreeText(color string) string {
	// Serialise raw/cooked-mode entry (see promptMu) alongside the other prompts.
	promptMu.Lock()
	defer promptMu.Unlock()

	// Suspend the key watcher and re-enable canonical input + echo so the user
	// can see and edit what they type.
	pauseKeyWatch()
	defer resumeKeyWatch()
	restore := enterLineMode()
	defer restore()

	flushStdin()
	fmt.Printf("  %s%s❯%s ", color, ansiBold, ansiReset)
	raw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(raw)
}

// choicePromptCooked is the non-TTY fallback: print the options and read a number.
func choicePromptCooked(header string, options []string) int {
	if header != "" {
		fmt.Printf("  %s%s%s\n", ansiBold, header, ansiReset)
	}
	for i, opt := range options {
		fmt.Printf("  %s%d.%s %s%s%s\n", colorPrimary, i+1, ansiReset, ansiDim, opt, ansiReset)
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("  %s%s❯%s ", colorPrimary, ansiBold, ansiReset)
		raw, err := reader.ReadString('\n')
		s := strings.TrimSpace(raw)
		if s == "" {
			return -1
		}
		var num int
		if _, e := fmt.Sscanf(s, "%d", &num); e == nil && num >= 1 && num <= len(options) {
			return num - 1
		}
		if err != nil {
			return -1
		}
		fmt.Printf("  %sEnter a number 1–%d%s\n", ansiDim, len(options), ansiReset)
	}
}

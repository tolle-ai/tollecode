package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Bracketed-paste handling for the chzyer/readline-based REPL.
//
// readline v1.5.1 has no notion of bracketed paste: when multi-line text is
// pasted, every embedded newline is read as an Enter keypress, so only the
// first line is captured and it submits immediately (activating the agent
// before the user presses Enter). readline's own terminal parser even swallows
// the ESC[200~/ESC[201~ markers, and its FuncFilterInputRune hook only ever
// sees a single rune at a time — so the paste has to be handled one layer
// lower, at the stdin reader, before readline's rune parser runs.
//
// Streaming the raw multi-line content into readline is also unusable: readline
// is a single-line editor that redraws the whole (wrapped) line on every rune,
// so a big paste flickers/"glitches" as it inserts. Instead we buffer the whole
// paste and hand readline a compact placeholder chip ("[Pasted 12 lines]"); the
// real content is stashed and swapped back in when the line is submitted. Only a
// short, single-line chip ever enters readline's buffer, so there is no redraw
// storm and nothing is sent until the user presses Enter.
const (
	pasteBegin = "\x1b[200~"
	pasteEnd   = "\x1b[201~"

	// Toggle the terminal's bracketed-paste mode.
	pasteEnableSeq  = "\x1b[?2004h"
	pasteDisableSeq = "\x1b[?2004l"

	// charInterrupt is Ctrl-C. A standalone Esc is rewritten to it so pressing
	// Esc at the prompt exits the CLI just like Ctrl-C does.
	charInterrupt = 0x03

	// escapeTimeout is how long to wait after a lone ESC before deciding it is a
	// standalone Escape rather than the start of an escape sequence (arrow keys,
	// etc.). Key-generated sequences arrive as one burst, so their bytes are
	// already readable well within this window; a real Escape is followed only
	// by the next (human-scale) keypress. Matches vim's ttimeoutlen default.
	escapeTimeout = 50 * time.Millisecond
)

func enableBracketedPaste()  { _, _ = os.Stdout.WriteString(pasteEnableSeq) }
func disableBracketedPaste() { _, _ = os.Stdout.WriteString(pasteDisableSeq) }

// bracketedPasteReader wraps stdin and translates bracketed-paste sequences.
//
// It is deliberately goroutine-free: readline's terminal goroutine calls Read
// synchronously, so doing the work inline avoids leaking a reader goroutine on
// every readline recreate (the @/% pickers close and rebuild the instance).
type bracketedPasteReader struct {
	base    io.Reader
	fd      uintptr // stdin fd for the escape-timeout poll; valid only if hasFd
	hasFd   bool
	pend    []byte       // bytes read but not yet resolved (partial marker)
	accum   []byte       // content buffered while inside a paste
	out     bytes.Buffer // translated bytes ready to hand to readline
	inPaste bool
	seq     int

	mu     sync.Mutex        // guards pastes (written on Read, read on expand)
	pastes map[string]string // placeholder chip -> real pasted content
}

func newBracketedPasteReader(base io.Reader) *bracketedPasteReader {
	b := &bracketedPasteReader{base: base, pastes: map[string]string{}}
	// Real stdin is an *os.File; its fd lets us poll for a following byte so a
	// standalone Esc can be told apart from an escape sequence. Non-file bases
	// (tests) simply skip that and block-read as before.
	if f, ok := base.(interface{ Fd() uintptr }); ok {
		b.fd = f.Fd()
		b.hasFd = true
	}
	return b
}

// Close is a no-op: the underlying stdin (os.Stdin) is process-owned and must
// outlive the many readline instances the REPL creates, so it is never closed.
func (b *bracketedPasteReader) Close() error { return nil }

// expand swaps any placeholder chips in a submitted line back to the real pasted
// content, then clears the stash. Called from the REPL after Readline() returns.
func (b *bracketedPasteReader) expand(line string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pastes) == 0 {
		return line
	}
	for chip, content := range b.pastes {
		line = strings.ReplaceAll(line, chip, content)
	}
	b.pastes = map[string]string{}
	return line
}

func (b *bracketedPasteReader) Read(p []byte) (int, error) {
	// Resolve anything buffered from a previous call first.
	b.translate(false)
	for b.out.Len() == 0 {
		// When the only thing buffered is a bare ESC (not mid-paste), poll
		// briefly: if no following byte arrives it is a standalone Escape, which
		// we turn into Ctrl-C so the REPL exits.
		if b.hasFd && !b.inPaste && b.holdingEscape() {
			if ready, err := waitReadable(b.fd, escapeTimeout); err == nil && !ready {
				b.resolveIdleEscape()
				break
			}
		}
		buf := make([]byte, 4096)
		n, err := b.base.Read(buf)
		if n > 0 {
			b.pend = append(b.pend, buf[:n]...)
			b.translate(false)
		}
		if err != nil {
			b.translate(true) // flush held bytes / unterminated paste
			if b.out.Len() == 0 {
				return 0, err
			}
			break
		}
	}
	return b.out.Read(p)
}

// holdingEscape reports whether b.pend is an unresolved ESC-led prefix — the
// only thing translate leaves buffered when it needs more bytes to decide.
func (b *bracketedPasteReader) holdingEscape() bool {
	return len(b.pend) > 0 && b.pend[0] == 0x1b
}

// resolveIdleEscape is called when a held ESC prefix produced no follow-up byte.
// A lone ESC is a standalone Escape → emit Ctrl-C to exit; a longer stalled
// prefix is just an incomplete sequence → emit it literally for readline.
func (b *bracketedPasteReader) resolveIdleEscape() {
	if len(b.pend) == 1 {
		b.pend = b.pend[:0]
		b.out.WriteByte(charInterrupt)
		return
	}
	b.translate(true)
}

// translate consumes as much of b.pend as it can resolve. Content typed outside
// a paste is emitted to b.out untouched; content inside a paste is buffered into
// b.accum and turned into a placeholder chip once the paste-end marker arrives.
// When flush is true (EOF) any still-ambiguous bytes are resolved literally.
func (b *bracketedPasteReader) translate(flush bool) {
	i := 0
	for i < len(b.pend) {
		c := b.pend[i]

		if c == 0x1b { // ESC — possibly a paste marker
			matched, isBegin, complete := matchPasteMarker(b.pend[i:])
			if !complete {
				if !flush {
					break // hold the partial marker until more bytes arrive
				}
				b.emit(c) // no more input: it was not a marker
				i++
				continue
			}
			if matched {
				if isBegin {
					if !b.inPaste {
						b.inPaste = true
						b.accum = b.accum[:0]
					}
				} else if b.inPaste {
					b.inPaste = false
					b.finishPaste()
				}
				i += len(pasteBegin) // begin and end markers share a length
				continue
			}
			// A complete escape sequence that is not a paste marker (arrow key,
			// etc.) — pass the ESC through untouched.
			b.emit(c)
			i++
			continue
		}

		b.emit(c)
		i++
	}
	b.pend = b.pend[i:]

	if flush && b.inPaste { // EOF mid-paste: finalize what we captured
		b.inPaste = false
		b.finishPaste()
	}
}

// emit routes a byte to the paste buffer when inside a paste, else straight to
// the output for readline.
func (b *bracketedPasteReader) emit(c byte) {
	if b.inPaste {
		b.accum = append(b.accum, c)
		return
	}
	b.out.WriteByte(c)
}

// finishPaste converts the just-captured paste into either inline text (single
// line — behaves exactly like typing) or a placeholder chip (multi-line).
func (b *bracketedPasteReader) finishPaste() {
	content := normalizePasteNewlines(b.accum)
	b.accum = b.accum[:0]
	if content == "" {
		return
	}
	if !strings.Contains(content, "\n") {
		b.out.WriteString(content) // single-line paste: keep it inline & editable
		return
	}
	lines := strings.Count(strings.TrimRight(content, "\n"), "\n") + 1
	b.seq++
	chip := fmt.Sprintf("[Pasted %d lines #%d]", lines, b.seq)
	b.mu.Lock()
	b.pastes[chip] = content
	b.mu.Unlock()
	b.out.WriteString(chip)
}

// normalizePasteNewlines converts CRLF / CR line endings to plain "\n".
func normalizePasteNewlines(p []byte) string {
	s := string(p)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// matchPasteMarker reports whether s starts with a bracketed-paste marker.
//   - complete=false means s is a viable prefix of a marker but too short to
//     decide yet (caller should read more before acting).
//   - complete=true, matched=true identifies the marker (isBegin distinguishes
//     ESC[200~ from ESC[201~).
//   - complete=true, matched=false means s cannot be a marker.
func matchPasteMarker(s []byte) (matched, isBegin, complete bool) {
	const n = len(pasteBegin) // == len(pasteEnd)
	max := n
	if len(s) < n {
		max = len(s)
	}
	beginPrefix := hasBytePrefix(s, pasteBegin, max)
	endPrefix := hasBytePrefix(s, pasteEnd, max)
	if len(s) < n {
		if beginPrefix || endPrefix {
			return false, false, false // still viable, need more bytes
		}
		return false, false, true
	}
	if beginPrefix {
		return true, true, true
	}
	if endPrefix {
		return true, false, true
	}
	return false, false, true
}

// hasBytePrefix reports whether the first n bytes of s equal marker[:n].
func hasBytePrefix(s []byte, marker string, n int) bool {
	for i := 0; i < n; i++ {
		if s[i] != marker[i] {
			return false
		}
	}
	return true
}

//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// keyWatcher reads stdin during an agent turn. It uses cbreak mode — canonical
// input and echo off (so single keys arrive immediately and aren't printed),
// but output post-processing left ON so the turn's normal "\n" output still
// returns to column 0. Ctrl-C is handled by the SIGINT handler (ISIG stays
// enabled).
//
// A lone Esc cancels the turn. Everything else is routed into the pinned
// composer (when active): printable runes and pastes build up a message on the
// input row, Enter queues it for auto-send after the turn, Backspace/Ctrl-U
// edit it — and with text pending, the first Esc clears it instead of
// cancelling. With no composer (legacy mode) all non-Esc input is discarded,
// matching the old behavior.
type keyWatcher struct {
	cancel context.CancelFunc
	comp   *composer // nil-safe: every composer method no-ops when nil/inactive
	fd     int
	old    unix.Termios
	mu     sync.Mutex
	paused bool
	fired  bool
	closed bool
	stopCh chan struct{}
	doneCh chan struct{}

	// Input-scanning state (touched only by the run goroutine).
	pend     []byte // bytes read but not yet resolved (partial escape/UTF-8)
	inPaste  bool   // inside an ESC[200~ … ESC[201~ bracketed paste
	pasteAcc []byte // paste content accumulated so far

	// escHeldAt is when the currently-held lone ESC was first seen, zeroed the
	// moment any byte arrives. A standalone Escape is declared only after
	// escapeSettle of continuous silence — see resolveIdleEscape.
	escHeldAt time.Time
	// escOrphaned records that a lone ESC was just consumed as a standalone
	// Escape. If its remainder shows up on the very next read after all, that
	// remainder is the tail of an escape sequence and must be swallowed rather
	// than typed into the composer as text.
	escOrphaned bool
}

// escapeSettle is how long a held ESC must go unaccompanied before it counts as
// a standalone Escape keypress rather than the opening byte of an arrow/function
// key. The read loop uses VMIN=1, so the terminal routinely hands over the ESC
// of a multi-byte sequence on its own — deciding on the first quiet poll would
// (and did) cancel turns when the rest of the sequence was merely slow, which is
// normal over SSH, in tmux, or under load. Escape-to-cancel is a destructive
// action, so it is worth several poll rounds of confirmation.
const escapeSettle = 150 * time.Millisecond

func startKeyWatch(cancel context.CancelFunc, comp *composer) *keyWatcher {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil // not a TTY — nothing to watch (piped/one-shot mode)
	}
	cbreak := *old
	cbreak.Lflag &^= unix.ICANON | unix.ECHO
	cbreak.Cc[unix.VMIN] = 1
	cbreak.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &cbreak); err != nil {
		return nil
	}
	w := &keyWatcher{
		cancel: cancel, comp: comp, fd: fd, old: *old,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
	activeKeyWatch.Store(w)
	go w.run()
	return w
}

func (w *keyWatcher) run() {
	defer close(w.doneCh)
	buf := make([]byte, 64)
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}
		w.mu.Lock()
		paused := w.paused
		w.mu.Unlock()
		if paused {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		ready, err := waitReadable(uintptr(w.fd), 60*time.Millisecond)
		if err != nil {
			return
		}
		if !ready {
			// Nothing arrived after a held lone ESC — it may be a real Escape
			// keypress rather than the start of an arrow/function-key sequence.
			// A previously orphaned ESC can no longer be claimed by a tail: the
			// window for its remainder to show up has now passed.
			w.escOrphaned = false
			w.resolveIdleEscape()
			continue
		}
		w.mu.Lock()
		if w.paused {
			w.mu.Unlock()
			continue
		}
		n, _ := syscall.Read(w.fd, buf)
		w.mu.Unlock()
		if n <= 0 {
			continue
		}
		w.pend = append(w.pend, buf[:n]...)
		w.escHeldAt = time.Time{} // bytes arrived — the silence run is broken
		w.process()
	}
}

// resolveIdleEscape fires when a held ESC got no follow-up bytes within a poll
// window. It declares a standalone Escape only once the ESC has sat alone for
// escapeSettle — the first quiet poll merely starts the clock, since the tail
// of a genuine arrow key can trail its ESC by more than one window. With
// typed-during-turn text pending a standalone Escape clears the text;
// otherwise (or on the next press) it cancels the turn.
func (w *keyWatcher) resolveIdleEscape() {
	if w.inPaste || len(w.pend) != 1 || w.pend[0] != 0x1b {
		return
	}
	if w.escHeldAt.IsZero() {
		w.escHeldAt = time.Now()
		return
	}
	if time.Since(w.escHeldAt) < escapeSettle {
		return
	}
	w.pend = w.pend[:0]
	w.escHeldAt = time.Time{}
	// If the tail turns up on the next read after all, it belongs to this ESC.
	w.escOrphaned = true
	if w.comp.clearBuf() {
		return
	}
	w.trigger()
}

// process consumes w.pend: paste-marker brackets, escape sequences (ignored),
// control keys, and printable runes routed into the composer. Partial trailing
// sequences stay in pend until more bytes arrive (or the idle-Esc timeout).
func (w *keyWatcher) process() {
	for len(w.pend) > 0 {
		if w.inPaste {
			if !w.consumePaste() {
				return
			}
			continue
		}
		if w.escOrphaned {
			// A lone ESC was resolved as a standalone Escape and its remainder
			// arrived late. Swallow the tail — without this it lands in the
			// composer as literal "[D" / "OD" / "[1;5D".
			switch n := consumeOrphanTail(w.pend); {
			case n > 0:
				w.pend = w.pend[n:]
				w.escOrphaned = false
				continue
			case n < 0:
				return // viable tail prefix — wait for the rest
			default:
				w.escOrphaned = false // ordinary input; fall through
			}
		}
		c := w.pend[0]
		if c == 0x1b {
			matched, isBegin, complete := matchPasteMarker(w.pend)
			if !complete {
				return // viable marker prefix — need more bytes to decide
			}
			if matched {
				w.pend = w.pend[len(pasteBegin):]
				if isBegin {
					w.inPaste = true
					w.pasteAcc = w.pasteAcc[:0]
				}
				continue
			}
			consumed := consumeEscSequence(w.pend)
			if consumed == 0 {
				return // lone/partial ESC — resolved by the idle timeout
			}
			w.applyEditKey(classifyEditKey(w.pend[:consumed]))
			w.pend = w.pend[consumed:]
			continue
		}
		switch {
		case c == '\r' || c == '\n':
			w.comp.enterPressed()
			w.pend = w.pend[1:]
		case c == 0x7f || c == 0x08: // Backspace
			w.comp.backspace()
			w.pend = w.pend[1:]
		case c == 0x15: // Ctrl-U — clear the line
			w.comp.clearBuf()
			w.pend = w.pend[1:]
		case c == 0x01: // Ctrl-A — start of line
			w.comp.moveCursorTo(0)
			w.pend = w.pend[1:]
		case c == 0x05: // Ctrl-E — end of line
			w.comp.moveCursorTo(-1)
			w.pend = w.pend[1:]
		case c == 0x17: // Ctrl-W — delete the word before the caret
			w.comp.deleteWordBack()
			w.pend = w.pend[1:]
		case c == 0x02: // Ctrl-B — back one char (readline convention)
			w.comp.moveCursor(-1)
			w.pend = w.pend[1:]
		case c == 0x06: // Ctrl-F — forward one char
			w.comp.moveCursor(1)
			w.pend = w.pend[1:]
		case c < 0x20: // other control bytes (incl. Ctrl-C, owned by ISIG) — ignore
			w.pend = w.pend[1:]
		default:
			if !utf8.FullRune(w.pend) && len(w.pend) < utf8.UTFMax {
				return // partial multibyte rune — wait for the rest
			}
			r, size := utf8.DecodeRune(w.pend)
			if r != utf8.RuneError && unicode.IsPrint(r) {
				w.comp.insertRunes([]rune{r})
			}
			w.pend = w.pend[size:]
		}
	}
}

// consumePaste moves bytes into pasteAcc until the paste-end marker arrives,
// handling a marker split across reads. Returns false when more bytes are
// needed to make progress.
func (w *keyWatcher) consumePaste() bool {
	if idx := bytes.Index(w.pend, []byte(pasteEnd)); idx >= 0 {
		w.pasteAcc = append(w.pasteAcc, w.pend[:idx]...)
		w.pend = w.pend[idx+len(pasteEnd):]
		w.inPaste = false
		content := normalizePasteNewlines(w.pasteAcc)
		w.pasteAcc = w.pasteAcc[:0]
		if content != "" {
			w.comp.insertRunes([]rune(content))
		}
		return true
	}
	// No end marker yet: absorb everything except a trailing prefix of the
	// marker, which must wait for the next read to be recognised.
	keep := len(w.pend)
	for k := len(pasteEnd) - 1; k > 0; k-- {
		if k <= len(w.pend) && bytes.HasPrefix([]byte(pasteEnd), w.pend[len(w.pend)-k:]) {
			keep = len(w.pend) - k
			break
		}
	}
	w.pasteAcc = append(w.pasteAcc, w.pend[:keep]...)
	w.pend = w.pend[keep:]
	return false
}

// consumeEscSequence returns the length of a complete escape sequence at the
// start of p (CSI, SS3, or Alt+key), or 0 when p might still be a partial
// sequence (or a lone Esc) that needs more bytes / the idle timeout.
func consumeEscSequence(p []byte) int {
	if len(p) < 2 {
		return 0
	}
	switch p[1] {
	case '[': // CSI: params/intermediates 0x20–0x3F, final byte 0x40–0x7E
		for i := 2; i < len(p); i++ {
			if p[i] >= 0x40 && p[i] <= 0x7e {
				return i + 1
			}
			if i > 32 { // runaway junk — drop what we have
				return i
			}
		}
		return 0
	case 'O': // SS3 (some arrows / F-keys): ESC O <one byte>
		if len(p) >= 3 {
			return 3
		}
		return 0
	default: // Alt+key or a stray two-byte pair
		return 2
	}
}

// editKey is a line-editing action decoded from an escape sequence or control
// byte. Up/Down deliberately have no entry: the composer is a single row, and
// history recall belongs to the idle readline prompt.
type editKey int

const (
	keyNone editKey = iota
	keyLeft
	keyRight
	keyWordLeft
	keyWordRight
	keyHome
	keyEnd
	keyDelete
)

// classifyEditKey decodes a complete escape sequence into an editing action.
// It accepts both CSI (ESC[) and SS3 (ESCO) encodings — terminals switch
// between them depending on application/cursor-key mode — and the modified
// CSI 1;<mod> forms that Ctrl/Alt+arrow produce.
func classifyEditKey(seq []byte) editKey {
	if len(seq) < 3 || seq[0] != 0x1b || (seq[1] != '[' && seq[1] != 'O') {
		return keyNone
	}
	final := seq[len(seq)-1]
	params := string(seq[2 : len(seq)-1])
	// A modifier parameter (ESC[1;5D and friends) promotes arrows to word-wise
	// motion. Any modifier will do — Ctrl and Alt are both used in the wild.
	modified := strings.Contains(params, ";")
	switch final {
	case 'D':
		if modified {
			return keyWordLeft
		}
		return keyLeft
	case 'C':
		if modified {
			return keyWordRight
		}
		return keyRight
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	case '~':
		switch params {
		case "1", "7":
			return keyHome
		case "4", "8":
			return keyEnd
		case "3":
			return keyDelete
		}
	}
	return keyNone
}

// applyEditKey routes a decoded action to the composer.
func (w *keyWatcher) applyEditKey(k editKey) {
	switch k {
	case keyLeft:
		w.comp.moveCursor(-1)
	case keyRight:
		w.comp.moveCursor(1)
	case keyWordLeft:
		w.comp.moveWord(-1)
	case keyWordRight:
		w.comp.moveWord(1)
	case keyHome:
		w.comp.moveCursorTo(0)
	case keyEnd:
		w.comp.moveCursorTo(-1)
	case keyDelete:
		w.comp.deleteForward()
	}
}

// consumeOrphanTail reports how to treat p when the ESC that opened it was
// already consumed as a standalone Escape: a positive length for a complete
// headless CSI/SS3 tail, -1 when p is a viable tail that needs more bytes, and
// 0 when p is ordinary input that has nothing to do with the lost ESC.
func consumeOrphanTail(p []byte) int {
	if len(p) == 0 || (p[0] != '[' && p[0] != 'O') {
		return 0
	}
	// Re-attach a synthetic ESC so the existing parser decides, keeping one
	// definition of what a complete sequence looks like.
	n := consumeEscSequence(append([]byte{0x1b}, p...))
	if n == 0 {
		return -1
	}
	return n - 1
}

func (w *keyWatcher) trigger() {
	w.mu.Lock()
	first := !w.fired
	w.fired = true
	w.mu.Unlock()
	if first {
		fmt.Printf("\r\n  %s⎋ interrupting…%s\r\n", ansiDim, ansiReset)
		w.cancel()
	}
}

func (w *keyWatcher) pause() {
	w.mu.Lock()
	w.paused = true
	w.mu.Unlock()
}

func (w *keyWatcher) resume() {
	w.mu.Lock()
	w.paused = false
	w.mu.Unlock()
}

// restore puts the terminal back to its pre-turn mode. Idempotent.
func (w *keyWatcher) restore() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	unix.IoctlSetTermios(w.fd, ioctlSetTermios, &w.old)
}

// stop ends the watcher goroutine and restores the terminal.
func (w *keyWatcher) stop() {
	if w == nil {
		return
	}
	activeKeyWatch.Store(nil)
	close(w.stopCh)
	<-w.doneCh
	w.restore()
}

// enterLineMode temporarily re-enables canonical input + echo (for a free-text
// prompt) and returns a func that restores the prior mode. No-op off a TTY.
func enterLineMode() func() {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return func() {}
	}
	cooked := *old
	cooked.Lflag |= unix.ICANON | unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &cooked); err != nil {
		return func() {}
	}
	return func() { unix.IoctlSetTermios(fd, ioctlSetTermios, old) }
}

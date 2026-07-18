//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
}

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
			// Nothing arrived after a held lone ESC — it's a real Escape
			// keypress, not the start of an arrow/function-key sequence.
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
		w.process()
	}
}

// resolveIdleEscape fires when a held ESC got no follow-up bytes within the
// poll window: a standalone Escape. With typed-during-turn text pending it
// clears the text; otherwise (or on the next press) it cancels the turn.
func (w *keyWatcher) resolveIdleEscape() {
	if w.inPaste || len(w.pend) != 1 || w.pend[0] != 0x1b {
		return
	}
	w.pend = w.pend[:0]
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
			w.pend = w.pend[consumed:] // arrow keys etc. — ignore
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

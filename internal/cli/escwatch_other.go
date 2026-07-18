//go:build !darwin && !linux

package cli

import "context"

// keyWatcher is a no-op on platforms without the termios cbreak support used by
// escwatch_unix.go (Windows, other unixes). Esc-to-cancel is unavailable there;
// Ctrl-C still cancels the turn via the SIGINT handler.
type keyWatcher struct{}

func startKeyWatch(_ context.CancelFunc, _ *composer) *keyWatcher { return nil }

func (w *keyWatcher) pause()   {}
func (w *keyWatcher) resume()  {}
func (w *keyWatcher) restore() {}
func (w *keyWatcher) stop()    {}

// enterLineMode is a no-op where the terminal is already in line mode during a
// turn (no cbreak watcher is installed).
func enterLineMode() func() { return func() {} }

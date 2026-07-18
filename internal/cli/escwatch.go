package cli

import "sync/atomic"

// escwatch.go coordinates the per-turn key watcher (see escwatch_unix.go) with
// the interactive pickers. While a turn runs, the watcher owns stdin in cbreak
// mode so a lone Esc can cancel it; when a picker or free-text prompt needs
// stdin, it pauses the watcher first so the two never fight over bytes.

// activeKeyWatch points at the watcher for the in-flight turn, if any.
var activeKeyWatch atomic.Pointer[keyWatcher]

// pauseKeyWatch suspends the turn key watcher (if one is active) so a picker can
// read stdin. Safe to call when no watcher is running.
func pauseKeyWatch() {
	if w := activeKeyWatch.Load(); w != nil {
		w.pause()
	}
}

// resumeKeyWatch re-arms the turn key watcher after a picker finishes.
func resumeKeyWatch() {
	if w := activeKeyWatch.Load(); w != nil {
		w.resume()
	}
}

// restoreTerminalOnExit puts the terminal back to its pre-turn mode. Called on
// the force-quit path (os.Exit skips deferred stop()), so a killed turn never
// leaves the terminal in no-echo cbreak mode — or with the composer's scroll
// region still installed.
func restoreTerminalOnExit() {
	if w := activeKeyWatch.Load(); w != nil {
		w.restore()
	}
	activeComposer.Load().emergencyReset()
}

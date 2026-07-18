//go:build !darwin && !linux

package cli

// composerSupported: no pinned composer without the termios key watcher —
// these platforms keep the legacy inline-prompt flow.
const composerSupported = false

func (c *composer) watchResize() {}

// cursorRow is unavailable without the unix termios plumbing; 0 makes
// composer setup fall back to resuming at the region bottom.
func cursorRow() int { return 0 }

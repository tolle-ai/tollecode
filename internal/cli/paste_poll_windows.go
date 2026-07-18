//go:build windows

package cli

import "time"

// waitReadable is a no-op on Windows: standalone-Escape detection is skipped and
// Read falls back to a normal blocking read. Reporting "ready" keeps the caller
// out of the idle-Escape branch.
func waitReadable(fd uintptr, timeout time.Duration) (bool, error) {
	return true, nil
}

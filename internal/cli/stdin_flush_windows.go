//go:build windows

package cli

// flushStdin is a no-op on Windows; the console input model differs and the
// Unix typeahead-drain approach does not apply directly.
func flushStdin() {}

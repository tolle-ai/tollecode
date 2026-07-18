package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// pickerVisible is the max entry rows a fuzzy picker shows at once.
const pickerVisible = 9

// pickerVisibleRows returns how many entry rows the fuzzy pickers (files,
// agents, select) may show — pickerVisible, clamped so the whole block
// (header + divider + entries + footer) still fits inside the scrollable
// content area when the pinned composer is active. The in-place redraw math
// desyncs if the block is taller than the scroll region.
func pickerVisibleRows() int {
	v := contentRows() - 4
	if v > pickerVisible {
		v = pickerVisible
	}
	if v < 2 {
		v = 2
	}
	return v
}

// pickerTotalRows is the full block height for the current terminal:
// header + divider + entry rows + footer.
func pickerTotalRows() int { return pickerVisibleRows() + 3 }

// RunFilePicker shows an interactive fuzzy file/folder picker in raw terminal mode.
// initialQuery pre-filters the list on open. Returns the selected path relative
// to workspace, or "" if the user cancels.
func RunFilePicker(workspace, initialQuery string) string {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return ""
	}

	entries := pickerCollect(root)
	query := initialQuery
	filtered := pickerFilter(entries, query)
	// When the initial query returns nothing but entries exist, clear it so the
	// user sees all files immediately (they can re-type to filter).
	if len(filtered) == 0 && len(entries) > 0 {
		query = ""
		filtered = pickerFilter(entries, "")
	}
	cursor := 0

	// Reserve space below the current cursor position for the picker UI.
	total := pickerTotalRows()
	for i := 0; i < total; i++ {
		fmt.Println()
	}
	fmt.Printf("\033[%dA\r", total)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		pickerClear()
		return ""
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	pickerDraw(query, filtered, cursor)

	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			pickerClear()
			return ""
		}
		b := buf[:n]

		switch {
		case n == 1 && b[0] == 27: // Esc
			pickerClear()
			return ""
		case n == 1 && b[0] == 3: // Ctrl+C
			pickerClear()
			return ""
		case n == 1 && (b[0] == 13 || b[0] == 10): // Enter
			pickerClear()
			if cursor < len(filtered) {
				return filtered[cursor]
			}
			return ""
		case n == 1 && b[0] == 9: // Tab — accept selection
			pickerClear()
			if cursor < len(filtered) {
				return filtered[cursor]
			}
			return ""
		case n == 1 && b[0] == 127: // Backspace
			if len(query) > 0 {
				rr := []rune(query)
				query = string(rr[:len(rr)-1])
				filtered = pickerFilter(entries, query)
				cursor = 0
			}
		case n == 3 && b[0] == 27 && b[1] == '[' && b[2] == 'A': // Up arrow
			if cursor > 0 {
				cursor--
			}
		case n == 3 && b[0] == 27 && b[1] == '[' && b[2] == 'B': // Down arrow
			if cursor < len(filtered)-1 {
				cursor++
			}
		case n == 1 && b[0] >= 32 && b[0] < 127: // Printable ASCII
			query += string(rune(b[0]))
			filtered = pickerFilter(entries, query)
			cursor = 0
		}

		pickerDraw(query, filtered, cursor)
	}
}

// pickerDraw redraws the full picker UI starting at the current cursor position,
// then moves the cursor back to the top of the picker area so the next call
// redraws in-place.
func pickerDraw(query string, filtered []string, cursor int) {
	w, _ := terminalSize()
	visible := pickerVisibleRows()
	lineW := w - 8
	if lineW < 20 {
		lineW = 20
	}
	ruleW := w - 4
	if ruleW < 4 {
		ruleW = 4
	}

	// Header — "  ◈  Files  [query]"
	fmt.Printf("\r\033[2K  %s%s◈  Files%s  %s%s\n",
		colorPrimary, ansiBold, ansiReset,
		query, ansiReset)

	// Divider
	fmt.Printf("\r\033[2K  %s%s%s\n",
		ansiDim, strings.Repeat("─", ruleW), ansiReset)

	// Entries — always `visible` rows
	start := 0
	if cursor >= visible {
		start = cursor - visible + 1
	}
	for i := 0; i < visible; i++ {
		idx := start + i
		if idx < len(filtered) {
			entry := filtered[idx]
			rr := []rune(entry)
			if len(rr) > lineW {
				entry = "…" + string(rr[len(rr)-(lineW-1):])
			}
			if idx == cursor {
				// Full-brightness (vs. the dimmed unselected rows below) is enough
				// contrast to read as "selected" without a dedicated color.
				fmt.Printf("\r\033[2K  %s%s▸ %s%s\n",
					colorPrimary+ansiBold, ansiReset, entry, ansiReset)
			} else {
				fmt.Printf("\r\033[2K    %s%s%s\n", ansiDim, entry, ansiReset)
			}
		} else {
			fmt.Print("\r\033[2K\n")
		}
	}

	// Footer — no trailing newline so the terminal doesn't scroll.
	fmt.Printf("\r\033[2K  %s%d files  ↑↓ navigate  enter/tab select  ⌫ clear  esc cancel%s",
		ansiDim, len(filtered), ansiReset)

	// Return cursor to the header line.
	// Lines with \n: header(1) + divider(1) + entries(visible) = visible+2
	// Footer has no \n; cursor is at the end of line visible+2.
	// Move up visible+2 to return to line 0 (header).
	fmt.Printf("\033[%dA\r", visible+2)
}

// pickerClear erases all picker lines and positions the cursor at the top
// of the now-blank area, ready for readline to resume.
func pickerClear() {
	total := pickerTotalRows()
	for i := 0; i < total; i++ {
		fmt.Print("\r\033[2K\n")
	}
	fmt.Printf("\033[%dA\r", total)
}

// pickerCollect walks the workspace and returns all file/folder paths (relative,
// folders suffixed with "/"), skipping ignored directories.
func pickerCollect(root string) []string {
	var entries []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if cliIgnore[d.Name()] {
				return filepath.SkipDir
			}
			if path == root {
				return nil
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			rel += "/"
		}
		entries = append(entries, rel)
		if len(entries) >= 8000 {
			return filepath.SkipAll
		}
		return nil
	})
	return entries
}

// pickerFilter returns entries matching q (case-insensitive).
// Ranking: exact base-name match → prefix match → path contains q.
func pickerFilter(entries []string, q string) []string {
	if q == "" {
		if len(entries) > 200 {
			return entries[:200]
		}
		return entries
	}
	ql := strings.ToLower(q)
	var exact, prefix, contains []string
	for _, e := range entries {
		base := strings.ToLower(filepath.Base(strings.TrimSuffix(e, "/")))
		low := strings.ToLower(e)
		if base == ql {
			exact = append(exact, e)
		} else if strings.HasPrefix(base, ql) || strings.HasPrefix(low, ql) {
			prefix = append(prefix, e)
		} else if strings.Contains(low, ql) {
			contains = append(contains, e)
		}
	}
	result := append(exact, append(prefix, contains...)...)
	if len(result) > 200 {
		return result[:200]
	}
	return result
}

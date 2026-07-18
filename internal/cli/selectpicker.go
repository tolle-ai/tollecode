package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// pickItem is one row in an interactive selection picker.
type pickItem struct {
	id       string
	label    string
	sublabel string
}

// RunMultiSelect shows an interactive checklist with the same look and key model
// as the file/agent pickers: type to filter, ↑↓ to move, Space to toggle, Enter to
// confirm, Esc/Ctrl-C to cancel. preselected ids start checked. It returns the
// chosen ids (in original item order) and ok=false when the user cancels.
// anySelected reports whether at least one entry in the toggle map is on.
func anySelected(selected map[string]bool) bool {
	for _, v := range selected {
		if v {
			return true
		}
	}
	return false
}

func RunMultiSelect(title string, items []pickItem, preselected []string) ([]string, bool) {
	selected := make(map[string]bool, len(preselected))
	for _, id := range preselected {
		selected[id] = true
	}
	// autoPick: on a FRESH pick (nothing preselected), a bare Enter selects the
	// highlighted row. This is what a user expects when they open a picker to add
	// an item and press Enter on it without first hitting Space — otherwise their
	// choice is silently dropped. Disabled when editing an existing selection so
	// "toggle everything off, press Enter" still means "clear all".
	autoPick := len(preselected) == 0
	chosen, ok := runSelectLoop(title, items, selected, true, autoPick)
	if !ok {
		return nil, false
	}
	// Preserve original item order in the returned slice.
	var out []string
	for _, it := range items {
		if chosen[it.id] {
			out = append(out, it.id)
		}
	}
	return out, true
}

// RunSingleSelect shows an interactive single-choice picker. Enter (or Space)
// selects the highlighted row. Returns the chosen id, or "" when the user cancels
// (ok=false). When allowNone is true, an explicit "(none)" row is offered first.
func RunSingleSelect(title string, items []pickItem, allowNone bool) (string, bool) {
	if allowNone {
		items = append([]pickItem{{id: "", label: "(none)", sublabel: ""}}, items...)
	}
	selected := map[string]bool{}
	chosen, ok := runSelectLoop(title, items, selected, false, false)
	if !ok {
		return "", false
	}
	for _, it := range items {
		if chosen[it.id] {
			return it.id, true
		}
	}
	return "", true
}

// runSelectLoop is the shared raw-mode render/input loop for both the multi- and
// single-select pickers. multi controls whether Space toggles (checklist) or Enter
// commits the single highlighted row.
func runSelectLoop(title string, items []pickItem, selected map[string]bool, multi, autoPick bool) (map[string]bool, bool) {
	query := ""
	filtered := filterPickItems(items, query)
	cursor := 0

	total := pickerTotalRows()
	for i := 0; i < total; i++ {
		fmt.Println()
	}
	fmt.Printf("\033[%dA\r", total)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		agentPickerClear()
		return nil, false
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	drawSelect(title, query, filtered, cursor, selected, multi)

	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			agentPickerClear()
			return nil, false
		}
		b := buf[:n]

		switch {
		case n == 1 && b[0] == 27: // Esc
			agentPickerClear()
			return nil, false
		case n == 1 && b[0] == 3: // Ctrl+C
			agentPickerClear()
			return nil, false
		case n == 1 && (b[0] == 13 || b[0] == 10): // Enter
			if !multi {
				agentPickerClear()
				if cursor < len(filtered) {
					return map[string]bool{filtered[cursor].id: true}, true
				}
				return selected, true
			}
			// Multi-select: Enter confirms the toggled set. On a fresh pick where
			// the user hit Enter on a row without toggling it first, select that
			// row so their choice isn't silently lost (Esc still gives an empty set).
			if autoPick && !anySelected(selected) && cursor < len(filtered) {
				selected[filtered[cursor].id] = true
			}
			agentPickerClear()
			return selected, true
		case n == 1 && b[0] == 32: // Space
			if cursor < len(filtered) {
				if multi {
					id := filtered[cursor].id
					selected[id] = !selected[id]
				} else {
					agentPickerClear()
					return map[string]bool{filtered[cursor].id: true}, true
				}
			}
		case n == 1 && b[0] == 127: // Backspace
			if len(query) > 0 {
				rr := []rune(query)
				query = string(rr[:len(rr)-1])
				filtered = filterPickItems(items, query)
				cursor = 0
			}
		case n == 3 && b[0] == 27 && b[1] == '[' && b[2] == 'A': // Up
			if cursor > 0 {
				cursor--
			}
		case n == 3 && b[0] == 27 && b[1] == '[' && b[2] == 'B': // Down
			if cursor < len(filtered)-1 {
				cursor++
			}
		case n == 1 && b[0] >= 33 && b[0] < 127: // Printable (space reserved for toggle)
			query += string(rune(b[0]))
			filtered = filterPickItems(items, query)
			cursor = 0
		}

		drawSelect(title, query, filtered, cursor, selected, multi)
	}
}

func drawSelect(title, query string, filtered []pickItem, cursor int, selected map[string]bool, multi bool) {
	w, _ := terminalSize()
	visible := pickerVisibleRows()
	lineW := w - 14
	if lineW < 20 {
		lineW = 20
	}
	ruleW := w - 4
	if ruleW < 4 {
		ruleW = 4
	}

	fmt.Printf("\r\033[2K  %s%s◈  %s%s  %s%s\n",
		colorPrimary, ansiBold, title, ansiReset,
		query, ansiReset)
	fmt.Printf("\r\033[2K  %s%s%s\n",
		ansiDim, strings.Repeat("─", ruleW), ansiReset)

	start := 0
	if cursor >= visible {
		start = cursor - visible + 1
	}
	for i := 0; i < visible; i++ {
		idx := start + i
		if idx < len(filtered) {
			e := filtered[idx]

			box := ""
			if multi {
				if selected[e.id] {
					box = colorGreen + "✓ " + ansiReset
				} else {
					box = ansiDim + "· " + ansiReset
				}
			}

			label := e.label
			rr := []rune(label)
			if len(rr) > lineW-6 {
				label = string(rr[:lineW-7]) + "…"
			}

			if idx == cursor {
				// Full-brightness (vs. the dimmed unselected rows below) is enough
				// contrast to read as "selected" without a dedicated color.
				fmt.Printf("\r\033[2K  %s▸ %s%s%s%s  %s%s%s\n",
					colorPrimary+ansiBold, ansiReset, box, label, ansiReset,
					ansiDim, e.sublabel, ansiReset)
			} else {
				fmt.Printf("\r\033[2K    %s%s%s%s  %s%s%s\n",
					box, ansiDim, label, ansiReset,
					ansiDim, e.sublabel, ansiReset)
			}
		} else {
			fmt.Print("\r\033[2K\n")
		}
	}

	hint := "↑↓ navigate  enter select  esc cancel"
	if multi {
		n := 0
		for _, v := range selected {
			if v {
				n++
			}
		}
		hint = fmt.Sprintf("%d selected  ·  space toggle  enter select/confirm  esc skip", n)
	}
	fmt.Printf("\r\033[2K  %s%s%s", ansiDim, hint, ansiReset)
	fmt.Printf("\033[%dA\r", visible+2)
}

// filterPickItems returns items matching q (case-insensitive) across label + sublabel.
func filterPickItems(items []pickItem, q string) []pickItem {
	if q == "" {
		return items
	}
	ql := strings.ToLower(q)
	var exact, prefix, contains []pickItem
	for _, e := range items {
		low := strings.ToLower(e.label)
		if low == ql {
			exact = append(exact, e)
		} else if strings.HasPrefix(low, ql) {
			prefix = append(prefix, e)
		} else if strings.Contains(low, ql) || strings.Contains(strings.ToLower(e.sublabel), ql) {
			contains = append(contains, e)
		}
	}
	return append(exact, append(prefix, contains...)...)
}

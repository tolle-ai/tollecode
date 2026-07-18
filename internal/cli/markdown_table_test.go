package cli

import (
	"strings"
	"testing"
)

// TestTableBordersAlign renders a table whose cells contain snake_case names,
// colored inline code, and links — the exact content that used to desync the
// borders — and asserts every rendered row has identical visible width and that
// the vertical bars line up in the same columns. It also checks underscores in
// identifiers survive (no stray italic parsing).
func TestTableBordersAlign(t *testing.T) {
	src := "" +
		"| Item | Detail |\n" +
		"|------|--------|\n" +
		"| File | `/etc/nginx/sites-available/sms_payments.web` (enabled via symlink) |\n" +
		"| Root | /var/www/sms_payments_web (Angular SPA) |\n" +
		"| server_name | [transact.tollesoft.com](https://transact.tollesoft.com) |\n" +
		"| Status | **Working** correctly |\n"

	out := RenderAssistantMarkdown(src, 100)
	plain := reANSI.ReplaceAllString(out, "")
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")

	// Collect the table lines (those containing box-drawing glyphs).
	var tbl []string
	for _, ln := range lines {
		if strings.ContainsAny(ln, "┌┬┐├┼┤└┴┘│") {
			tbl = append(tbl, ln)
		}
	}
	if len(tbl) < 4 {
		t.Fatalf("expected a full table, got %d lines:\n%s", len(tbl), plain)
	}

	// Every table row must have the same visible width…
	width := visibleWidth(tbl[0])
	for i, ln := range tbl {
		if w := visibleWidth(ln); w != width {
			t.Errorf("row %d width %d != %d (misaligned border):\n%q", i, w, width, ln)
		}
	}

	// …and the vertical bars must sit in identical columns on every content row.
	barCols := func(s string) []int {
		var cols []int
		for i, r := range []rune(s) {
			if r == '│' {
				cols = append(cols, i)
			}
		}
		return cols
	}
	want := barCols(tbl[1]) // header row
	for i, ln := range tbl {
		if !strings.Contains(ln, "│") {
			continue // border rows use ┼/┬ etc.
		}
		got := barCols(ln)
		if len(got) != len(want) {
			t.Errorf("row %d has %d bars, want %d:\n%q", i, len(got), len(want), ln)
			continue
		}
		for j := range got {
			if got[j] != want[j] {
				t.Errorf("row %d bar %d at col %d, want %d:\n%q", i, j, got[j], want[j], ln)
			}
		}
	}

	// snake_case identifiers keep their underscores (no italic mangling).
	if !strings.Contains(plain, "sms_payments_web") {
		t.Errorf("underscores were mangled; got:\n%s", plain)
	}
}

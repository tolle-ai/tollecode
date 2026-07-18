package cli

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// markdown.go renders an assistant message (markdown source) into ANSI-styled
// terminal lines that match the Claude Code look: a single "●" bullet leads the
// block and every wrapped line sits two columns beneath it. Tables become
// box-drawing grids, lists get "–" markers, and inline **bold** / *italic* /
// `code` are styled. It is a deliberately small renderer — enough to cover the
// markdown that shows up in assistant prose, not a spec-complete parser.

const ansiUnderline = "\033[4m"

// bullet marking the start of an assistant message block. Plain, not the accent
// color — it opens every single assistant message, so coloring it would make
// the accent the most frequent color in the app instead of a reserved one.
var mdBullet = ansiBold + "●" + ansiReset

// RenderAssistantMarkdown formats a full assistant message for the terminal.
// The returned string ends with a trailing newline (empty input → "").
func RenderAssistantMarkdown(src string, width int) string {
	src = stripEmoji(strings.TrimRight(src, "\n"))
	if strings.TrimSpace(src) == "" {
		return ""
	}
	if width < 20 {
		width = 20
	}
	// Content sits under "● " / "  ", i.e. a two-column indent.
	lines := renderMarkdownBlocks(src, width-2)

	var sb strings.Builder
	for i, ln := range lines {
		switch {
		case i == 0:
			sb.WriteString(mdBullet + " " + ln + "\n")
		case ln == "":
			sb.WriteString("\n") // blank separator — no trailing indent
		default:
			sb.WriteString("  " + ln + "\n")
		}
	}
	return sb.String()
}

// ── Block parsing ─────────────────────────────────────────────────────────────

var (
	reHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reULItem    = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reOLItem    = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
	reTableSep  = regexp.MustCompile(`^\s*\|?[\s:|-]*-[\s:|-]*\|?\s*$`)
	reFenceOpen = regexp.MustCompile("^\\s{0,3}(```|~~~)(.*)$")
)

// isHRule reports whether a line is a thematic break (3+ of the same -, * or _,
// spaces allowed). Go's RE2 has no backreferences, so this is done by hand.
func isHRule(line string) bool {
	s := strings.TrimSpace(line)
	if len(s) < 3 {
		return false
	}
	var ch byte
	count := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			continue
		}
		if c != '-' && c != '*' && c != '_' {
			return false
		}
		if ch == 0 {
			ch = c
		} else if c != ch {
			return false
		}
		count++
	}
	return count >= 3
}

// renderMarkdownBlocks turns markdown source into styled, wrapped lines with no
// left indent (the caller adds the bullet/indent). Blocks are separated by a
// single blank line.
func renderMarkdownBlocks(src string, width int) []string {
	raw := strings.Split(strings.ReplaceAll(src, "\t", "    "), "\n")
	var out []string

	emitBlank := func() {
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
	}

	i := 0
	for i < len(raw) {
		line := raw[i]

		// Blank line → block separator.
		if strings.TrimSpace(line) == "" {
			emitBlank()
			i++
			continue
		}

		// Fenced code block.
		if m := reFenceOpen.FindStringSubmatch(line); m != nil {
			fence := m[1]
			i++
			var code []string
			for i < len(raw) && !strings.HasPrefix(strings.TrimSpace(raw[i]), fence) {
				code = append(code, raw[i])
				i++
			}
			if i < len(raw) {
				i++ // consume closing fence
			}
			emitBlank()
			out = append(out, renderCodeBlock(code, width)...)
			emitBlank()
			continue
		}

		// Table: a header row containing '|' immediately followed by a separator.
		if strings.Contains(line, "|") && i+1 < len(raw) && reTableSep.MatchString(raw[i+1]) {
			var rows []string
			for i < len(raw) && strings.Contains(raw[i], "|") && strings.TrimSpace(raw[i]) != "" {
				rows = append(rows, raw[i])
				i++
			}
			emitBlank()
			out = append(out, renderTable(rows, width)...)
			emitBlank()
			continue
		}

		// Heading.
		if m := reHeading.FindStringSubmatch(line); m != nil {
			emitBlank()
			text := layoutRuns(parseInline(m[2]), width)
			for _, t := range text {
				out = append(out, ansiBold+stripReset(t)+ansiReset)
			}
			emitBlank()
			i++
			continue
		}

		// Horizontal rule.
		if isHRule(line) {
			emitBlank()
			rw := width
			if rw > 60 {
				rw = 60
			}
			out = append(out, ansiDim+strings.Repeat("─", rw)+ansiReset)
			emitBlank()
			i++
			continue
		}

		// Blockquote.
		if strings.HasPrefix(strings.TrimLeft(line, " "), ">") {
			var quote []string
			for i < len(raw) && strings.HasPrefix(strings.TrimLeft(raw[i], " "), ">") {
				q := strings.TrimLeft(raw[i], " ")
				q = strings.TrimPrefix(q, ">")
				q = strings.TrimPrefix(q, " ")
				quote = append(quote, q)
				i++
			}
			emitBlank()
			for _, ln := range layoutRuns(parseInline(strings.Join(quote, " ")), width-2) {
				out = append(out, ansiDim+"▏ "+ansiReset+ln)
			}
			emitBlank()
			continue
		}

		// List (unordered or ordered), possibly multi-item / nested.
		if reULItem.MatchString(line) || reOLItem.MatchString(line) {
			var items []string
			for i < len(raw) {
				if reULItem.MatchString(raw[i]) || reOLItem.MatchString(raw[i]) {
					items = append(items, raw[i])
					i++
				} else if strings.TrimSpace(raw[i]) == "" {
					break
				} else if strings.HasPrefix(raw[i], "  ") {
					// Continuation of the previous item.
					items[len(items)-1] += " " + strings.TrimSpace(raw[i])
					i++
				} else {
					break
				}
			}
			out = append(out, renderList(items, width)...)
			continue
		}

		// Paragraph: consecutive non-blank, non-special lines.
		var para []string
		for i < len(raw) && strings.TrimSpace(raw[i]) != "" &&
			!reHeading.MatchString(raw[i]) && !isHRule(raw[i]) &&
			!reULItem.MatchString(raw[i]) && !reOLItem.MatchString(raw[i]) &&
			reFenceOpen.FindStringSubmatch(raw[i]) == nil &&
			!strings.HasPrefix(strings.TrimLeft(raw[i], " "), ">") {
			// Stop if this line begins a table.
			if strings.Contains(raw[i], "|") && i+1 < len(raw) && reTableSep.MatchString(raw[i+1]) {
				break
			}
			para = append(para, strings.TrimSpace(raw[i]))
			i++
		}
		out = append(out, layoutRuns(parseInline(strings.Join(para, " ")), width)...)
	}

	// Trim trailing blank lines.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// ── Lists ─────────────────────────────────────────────────────────────────────

func renderList(items []string, width int) []string {
	var out []string
	for _, it := range items {
		var indent int
		var marker, body string
		if m := reOLItem.FindStringSubmatch(it); m != nil {
			indent = len(m[1])
			marker = m[2] + "."
			body = m[3]
		} else if m := reULItem.FindStringSubmatch(it); m != nil {
			indent = len(m[1])
			marker = "–"
			body = m[2]
		} else {
			continue
		}
		nest := strings.Repeat("  ", indent/2)
		markCol := utf8.RuneCountInString(marker) + 1 // marker + space
		avail := width - len(nest) - markCol
		if avail < 8 {
			avail = 8
		}
		wrapped := layoutRuns(parseInline(body), avail)
		for j, ln := range wrapped {
			if j == 0 {
				out = append(out, nest+ansiBold+marker+ansiReset+" "+ln)
			} else {
				out = append(out, nest+strings.Repeat(" ", markCol)+ln)
			}
		}
	}
	return out
}

// ── Code blocks ───────────────────────────────────────────────────────────────

func renderCodeBlock(code []string, width int) []string {
	out := make([]string, 0, len(code))
	for _, ln := range code {
		ln = strings.ReplaceAll(ln, "\t", "    ")
		if visibleWidth(ln) > width-2 && width > 4 {
			ln = truncateVisible(ln, width-2)
		}
		out = append(out, ansiDim+"│ "+ansiReset+ansiDim+ln+ansiReset)
	}
	return out
}

// ── Tables ────────────────────────────────────────────────────────────────────

func splitTableRow(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func renderTable(rows []string, width int) []string {
	if len(rows) < 1 {
		return nil
	}
	header := splitTableRow(rows[0])
	var body [][]string
	for _, r := range rows[1:] {
		if reTableSep.MatchString(r) {
			continue
		}
		body = append(body, splitTableRow(r))
	}
	cols := len(header)
	for _, b := range body {
		if len(b) > cols {
			cols = len(b)
		}
	}
	if cols == 0 {
		return nil
	}

	// Natural column widths (from visible cell content).
	colw := make([]int, cols)
	measure := func(cells []string) {
		for c := 0; c < cols; c++ {
			cell := ""
			if c < len(cells) {
				cell = cells[c]
			}
			if w := visibleWidth(inlineText(cell)); w > colw[c] {
				colw[c] = w
			}
		}
	}
	measure(header)
	for _, b := range body {
		measure(b)
	}

	// Cap total width to the terminal; shrink the widest column(s) if needed.
	// Table overhead: borders "│ " ... " │" plus " │ " between columns.
	overhead := 3*cols + 1
	total := overhead
	for _, w := range colw {
		total += w
	}
	for total > width {
		widest := 0
		for c := 1; c < cols; c++ {
			if colw[c] > colw[widest] {
				widest = c
			}
		}
		if colw[widest] <= 6 {
			break
		}
		colw[widest]--
		total--
	}

	// Dim borders so the grid structure recedes behind the cell content.
	bcol := ansiDim
	border := func(l, m, r string) string {
		var sb strings.Builder
		sb.WriteString(bcol + l)
		for c := 0; c < cols; c++ {
			sb.WriteString(strings.Repeat("─", colw[c]+2))
			if c < cols-1 {
				sb.WriteString(m)
			}
		}
		sb.WriteString(r + ansiReset)
		return sb.String()
	}
	renderRow := func(cells []string, bold bool) string {
		var sb strings.Builder
		sb.WriteString(bcol + "│" + ansiReset)
		for c := 0; c < cols; c++ {
			cell := ""
			if c < len(cells) {
				cell = cells[c]
			}
			cell = fitCell(cell, colw[c], bold)
			sb.WriteString(" " + cell + " " + bcol + "│" + ansiReset)
		}
		return sb.String()
	}

	out := []string{border("┌", "┬", "┐"), renderRow(header, true), border("├", "┼", "┤")}
	for _, b := range body {
		out = append(out, renderRow(b, false))
	}
	out = append(out, border("└", "┴", "┘"))
	return out
}

// fitCell renders a table cell's inline markdown, padded (or truncated) to
// exactly w visible columns. Width and styling are derived from the same
// parseInline result, so the padding always matches the rendered content and
// the right-hand border stays aligned across every row.
func fitCell(cell string, w int, bold bool) string {
	runs := parseInline(cell)
	var plainB strings.Builder
	for _, r := range runs {
		plainB.WriteString(r.text)
	}
	plain := plainB.String()
	vis := visibleWidth(plain)

	var styled string
	if vis > w {
		styled = truncateVisible(plain, w) // drop styling on a truncated cell
		vis = visibleWidth(styled)
	} else {
		styled = strings.Join(flattenRuns(runs), "")
	}
	if bold {
		styled = ansiBold + styled + ansiReset
	}
	if w > vis {
		styled += strings.Repeat(" ", w-vis)
	}
	return styled
}

// ── Inline styling ────────────────────────────────────────────────────────────

type styledRun struct {
	style string // ANSI prefix ("" for plain)
	text  string // plain text, no ANSI
}

var (
	reInlineCode  = regexp.MustCompile("`([^`]+)`")
	reBold        = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	reItalicStar  = regexp.MustCompile(`\*([^*]+)\*`)
	reItalicUnder = regexp.MustCompile(`_([^_]+)_`)
	reLink        = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reStrike      = regexp.MustCompile(`~~([^~]+)~~`)
)

// isWordByte reports whether b is part of an identifier (letters, digits, _).
// Used to keep underscore emphasis from firing inside snake_case names.
func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// parseInline splits a line of markdown into styled runs. It handles code spans,
// bold, italic, strikethrough and links; anything else is plain text.
func parseInline(s string) []styledRun {
	// Links first: reduce [text](url) to its text so downstream regexes see prose.
	s = reLink.ReplaceAllString(s, ansiUnderline+colorPrimary+"$1"+ansiReset+"\x00")
	var runs []styledRun
	parseInlineInto(&runs, s, "")
	return runs
}

// parseInlineInto recursively tokenizes s, appending styled runs. baseStyle is
// carried from any enclosing emphasis.
func parseInlineInto(runs *[]styledRun, s, baseStyle string) {
	for len(s) > 0 {
		locs := map[string][]int{}
		record := func(name string, re *regexp.Regexp) {
			if m := re.FindStringIndex(s); m != nil {
				locs[name] = m
			}
		}
		record("code", reInlineCode)
		record("bold", reBold)
		record("istar", reItalicStar)
		record("iunder", reItalicUnder)
		record("strike", reStrike)

		name, best := "", -1
		for n, loc := range locs {
			if best == -1 || loc[0] < best {
				best, name = loc[0], n
			}
		}
		if name == "" {
			emitPlain(runs, s, baseStyle)
			return
		}
		loc := locs[name]

		// Underscore emphasis only fires at word boundaries — otherwise the
		// underscores in identifiers like sms_payments_web would be eaten (and,
		// worse, would desync table-cell widths). When invalid, emit up to and
		// including the opening "_" literally and advance past it.
		if name == "iunder" {
			beforeOK := loc[0] == 0 || !isWordByte(s[loc[0]-1])
			afterOK := loc[1] == len(s) || !isWordByte(s[loc[1]])
			if !beforeOK || !afterOK {
				emitPlain(runs, s[:loc[0]+1], baseStyle)
				s = s[loc[0]+1:]
				continue
			}
		}

		if loc[0] > 0 {
			emitPlain(runs, s[:loc[0]], baseStyle)
		}
		seg := s[loc[0]:loc[1]]
		switch name {
		case "code":
			inner := reInlineCode.FindStringSubmatch(seg)[1]
			*runs = append(*runs, styledRun{style: ansiDim, text: inner})
		case "bold":
			m := reBold.FindStringSubmatch(seg)
			inner := m[1]
			if inner == "" {
				inner = m[2]
			}
			parseInlineInto(runs, inner, baseStyle+ansiBold)
		case "istar":
			parseInlineInto(runs, reItalicStar.FindStringSubmatch(seg)[1], baseStyle+ansiItalic)
		case "iunder":
			parseInlineInto(runs, reItalicUnder.FindStringSubmatch(seg)[1], baseStyle+ansiItalic)
		case "strike":
			inner := reStrike.FindStringSubmatch(seg)[1]
			*runs = append(*runs, styledRun{style: baseStyle + "\033[9m", text: inner})
		}
		s = s[loc[1]:]
	}
}

// inlineText returns the plain, visible text of a cell after inline parsing —
// the single source of truth for a cell's display width, so padding always
// matches the styled render and table borders never drift.
func inlineText(cell string) string {
	var b strings.Builder
	for _, r := range parseInline(cell) {
		b.WriteString(r.text)
	}
	return b.String()
}

func emitPlain(runs *[]styledRun, s, style string) {
	s = strings.ReplaceAll(s, "\x00", "") // link sentinel
	if s == "" {
		return
	}
	*runs = append(*runs, styledRun{style: style, text: s})
}

// flattenRuns joins styled runs into a single ANSI string (no wrapping).
func flattenRuns(runs []styledRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		if r.style != "" {
			out = append(out, r.style+r.text+ansiReset)
		} else {
			out = append(out, r.text)
		}
	}
	return out
}

// layoutRuns word-wraps styled runs to width. A "word" is a maximal run of
// non-space characters and may span several styled runs — so `**plan**:` stays
// glued as "plan:" rather than gaining a stray space. Each styled piece is
// emitted self-contained (style+text+reset) so styles never bleed across a wrap.
func layoutRuns(runs []styledRun, width int) []string {
	if width < 1 {
		width = 1
	}
	type piece struct{ style, text string }

	var lines []string
	var line strings.Builder
	col := 0
	firstWord := true

	var word []piece // styled pieces making up the in-progress word
	wordW := 0

	flushWord := func() {
		if len(word) == 0 {
			return
		}
		need := wordW
		if !firstWord {
			need++ // separating space
		}
		if !firstWord && col+need > width {
			lines = append(lines, line.String())
			line.Reset()
			col = 0
			firstWord = true
		}
		if !firstWord {
			line.WriteByte(' ')
			col++
		}
		for _, p := range word {
			if p.style != "" {
				line.WriteString(p.style + p.text + ansiReset)
			} else {
				line.WriteString(p.text)
			}
		}
		col += wordW
		firstWord = false
		word = word[:0]
		wordW = 0
	}

	for _, run := range runs {
		// Spaces (including at run boundaries) delimit words; non-space pieces
		// with no space between them belong to the same word.
		for pi, part := range strings.Split(run.text, " ") {
			if pi > 0 {
				flushWord() // a space separated this part from the previous
			}
			if part == "" {
				continue
			}
			word = append(word, piece{run.style, part})
			wordW += visibleWidth(part)
		}
	}
	flushWord()

	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	if len(lines) == 0 {
		lines = append(lines, "")
	}
	return lines
}

// ── Width helpers ─────────────────────────────────────────────────────────────

var reANSI = regexp.MustCompile(`\033\[[0-9;]*m`)

// visibleWidth counts printable columns, ignoring ANSI SGR sequences. Runes are
// counted as width 1 — adequate for Latin/box-drawing prose, approximate for
// wide CJK/emoji.
func visibleWidth(s string) int {
	return utf8.RuneCountInString(reANSI.ReplaceAllString(s, ""))
}

// truncateVisible cuts a plain string to at most w columns, adding an ellipsis.
func truncateVisible(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:w-1]) + "…"
}

// stripReset removes trailing resets so a heading style can wrap the whole line.
func stripReset(s string) string {
	return strings.TrimSuffix(s, ansiReset)
}

// isEmojiRune reports whether r is a pictographic emoji (or emoji modifier). It
// deliberately excludes the Arrows (→ ↳) and Box-drawing blocks the CLI relies
// on, so only true emoji like ✅ ⚠️ 🎉 are removed.
func isEmojiRune(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // emoji, emoticons, transport, pictographs
		return true
	case r >= 0x2600 && r <= 0x27BF: // misc symbols + dingbats (✅ ⚠ ❌ ✔ ✂ …)
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors (emoji presentation)
		return true
	case r == 0x200D: // zero-width joiner (compound emoji)
		return true
	default:
		return false
	}
}

// stripEmoji removes emoji from assistant text. It also drops a single space
// left dangling by a removed emoji so "✅ Yes" becomes "Yes", not " Yes".
func stripEmoji(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	skipped := false
	for _, r := range s {
		if isEmojiRune(r) {
			skipped = true
			continue
		}
		if skipped && r == ' ' {
			skipped = false
			continue // collapse the space that trailed the removed emoji
		}
		skipped = false
		b.WriteRune(r)
	}
	return b.String()
}

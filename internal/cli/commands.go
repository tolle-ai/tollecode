package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
)

// ── /todo ─────────────────────────────────────────────────────────────────────

func printTodos(workspace, sessionID string) {
	todos, err := session.GetTodos(workspace, sessionID)
	if err != nil || len(todos) == 0 {
		fmt.Printf("  %sNo todos for this session yet.%s\n", ansiDim, ansiReset)
		return
	}

	// "pending" and unrecognized statuses/priorities fall through to the zero
	// value ("") for these maps — i.e. plain text, no color.
	iconMap := map[string]string{
		"pending":     "○",
		"in_progress": "◐",
		"completed":   "●",
	}
	styleMap := map[string]string{
		"in_progress": ansiBold + colorPrimary,
		"completed":   ansiDim + colorGreen,
	}
	prioMap := map[string]string{
		"high": colorRed,
		"low":  ansiDim,
	}
	order := map[string]int{"in_progress": 0, "pending": 1, "completed": 2}

	sort.Slice(todos, func(i, j int) bool {
		a := order[todos[i].Status]
		b := order[todos[j].Status]
		return a < b
	})

	fmt.Println()
	fmt.Printf("  %s%sTodos%s\n", colorPrimary, ansiBold, ansiReset)
	fmt.Println(drawRule())
	for _, t := range todos {
		icon := iconMap[t.Status]
		if icon == "" {
			icon = "○"
		}
		style := styleMap[t.Status]
		prio := prioMap[t.Priority]
		text := t.Text
		if text == "" {
			text = t.Content
		}
		fmt.Printf("    %s%s%s %s[%s]%s %s%s%s\n",
			style, icon, ansiReset,
			prio, strings.ToUpper(t.Priority[:1]), ansiReset,
			style, text, ansiReset)
	}

	incomplete := 0
	inProg := 0
	for _, t := range todos {
		if t.Status != "completed" {
			incomplete++
			if t.Status == "in_progress" {
				inProg++
			}
		}
	}
	if incomplete > 0 {
		parts := []string{}
		if inProg > 0 {
			parts = append(parts, fmt.Sprintf("%d in progress", inProg))
		}
		if pending := incomplete - inProg; pending > 0 {
			parts = append(parts, fmt.Sprintf("%d pending", pending))
		}
		fmt.Printf("\n    %s⚠  %s%s\n", ansiDim, strings.Join(parts, ", "), ansiReset)
	}
	fmt.Println()
}

// ── /usage ────────────────────────────────────────────────────────────────────

func printUsage(workspace string) {
	sessions, _ := session.List(workspace)
	if len(sessions) == 0 {
		fmt.Printf("\n  %sNo sessions yet. Start chatting to generate data.%s\n\n", ansiDim, ansiReset)
		return
	}

	byMode := map[string]int{}
	byProvider := map[string]int{}
	byModel := map[string]int{}
	perDay := map[string]int{}
	totalMsgs := 0

	for _, s := range sessions {
		if s.Mode != "" {
			byMode[s.Mode]++
		}
		if s.Provider != "" {
			byProvider[s.Provider]++
		}
		if s.Model != "" {
			byModel[s.Model]++
		}
		day := s.CreatedAt
		if len(day) >= 10 {
			day = day[:10]
		}
		perDay[day]++
		if s.MessageCount != nil {
			totalMsgs += *s.MessageCount
		}
	}

	fmt.Println()
	fmt.Printf("  %s%sSession Analytics%s\n", colorPrimary, ansiBold, ansiReset)
	fmt.Println(drawRule())
	fmt.Println()
	fmt.Printf("  %sTotal sessions%s  %d\n", ansiDim, ansiReset, len(sessions))
	fmt.Printf("  %sTotal messages%s  %d\n", ansiDim, ansiReset, totalMsgs)
	if len(sessions) > 0 {
		fmt.Printf("  %sAvg messages%s   %.1f per session\n",
			ansiDim, ansiReset, float64(totalMsgs)/float64(len(sessions)))
	}
	fmt.Println()

	if len(byProvider) > 0 {
		fmt.Printf("  %s%sBy Provider%s\n", colorPrimary, ansiBold, ansiReset)
		printCountTable(byProvider, "provider")
	}
	if len(byModel) > 0 {
		fmt.Printf("  %s%sBy Model%s\n", colorPrimary, ansiBold, ansiReset)
		printCountTable(byModel, "model")
	}
	if len(byMode) > 0 {
		fmt.Printf("  %s%sBy Mode%s\n", colorPrimary, ansiBold, ansiReset)
		printCountTable(byMode, "mode")
	}

	// Last 7 days activity
	days := make([]string, 0, len(perDay))
	for d := range perDay {
		days = append(days, d)
	}
	sort.Strings(days)
	if len(days) > 0 {
		window := days
		if len(window) > 14 {
			window = window[len(window)-14:]
		}
		maxCount := 0
		for _, d := range window {
			if perDay[d] > maxCount {
				maxCount = perDay[d]
			}
		}
		fmt.Printf("  %s%sDaily Activity%s\n", colorPrimary, ansiBold, ansiReset)
		fmt.Println()
		for _, d := range window {
			n := perDay[d]
			barW := 0
			if maxCount > 0 {
				barW = n * 28 / maxCount
				if barW == 0 && n > 0 {
					barW = 1
				}
			}
			bar := strings.Repeat("█", barW) + strings.Repeat("░", 28-barW)
			fmt.Printf("  %s%s%s  %s%s%s  %d\n",
				ansiDim, d, ansiReset,
				colorPrimary, bar, ansiReset,
				n)
		}
		fmt.Println()
	}
}

func printCountTable(m map[string]int, _ string) {
	type kv struct{ k string; v int }
	rows := make([]kv, 0, len(m))
	total := 0
	for k, v := range m {
		rows = append(rows, kv{k, v})
		total += v
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].v > rows[j].v })
	maxW := 28
	for _, row := range rows {
		barW := 0
		if total > 0 {
			barW = row.v * maxW / total
			if barW == 0 {
				barW = 1
			}
		}
		bar := strings.Repeat("█", barW) + strings.Repeat("░", maxW-barW)
		fmt.Printf("    %-30s %s%s%s %s%d%s\n",
			row.k,
			colorPrimary, bar, ansiReset,
			ansiDim, row.v, ansiReset)
	}
	fmt.Println()
}

// ── /memory ───────────────────────────────────────────────────────────────────

type memIndexRecord struct {
	File      string   `json:"file"`
	Summary   string   `json:"summary"`
	Keywords  []string `json:"keywords"`
	Timestamp string   `json:"timestamp"`
}

func memDir(ws string) string        { return filepath.Join(ws, ".agent", "memory") }
func memConfigPath(ws string) string { return filepath.Join(memDir(ws), "config.json") }
func memIndexPath(ws string) string  { return filepath.Join(memDir(ws), "index.jsonl") }

func isMemoryEnabled(ws string) bool {
	data, err := os.ReadFile(memConfigPath(ws))
	if err != nil {
		return false
	}
	var cfg map[string]bool
	_ = json.Unmarshal(data, &cfg)
	return cfg["enabled"]
}

func setMemoryEnabled(ws string, enabled bool) {
	_ = os.MkdirAll(memDir(ws), 0o755)
	data, _ := json.Marshal(map[string]any{"enabled": enabled})
	_ = os.WriteFile(memConfigPath(ws), data, 0o644)
}

func loadMemIndex(ws string) []memIndexRecord {
	data, err := os.ReadFile(memIndexPath(ws))
	if err != nil {
		return nil
	}
	var out []memIndexRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r memIndexRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func readMemDetail(ws, filename string) string {
	path := filepath.Join(memDir(ws), filename)
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	past := false
	var lines []string
	for sc.Scan() {
		line := sc.Text()
		if !past {
			if line == "---" {
				past = true
			}
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func deleteMemEntry(ws string, idx int) {
	records := loadMemIndex(ws)
	if idx < 0 || idx >= len(records) {
		return
	}
	_ = os.Remove(filepath.Join(memDir(ws), records[idx].File))
	records = append(records[:idx], records[idx+1:]...)
	saveMemIndex(ws, records)
}

func saveMemIndex(ws string, records []memIndexRecord) {
	_ = os.MkdirAll(memDir(ws), 0o755)
	f, err := os.Create(memIndexPath(ws))
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, r := range records {
		_ = enc.Encode(r)
	}
}

func handleMemoryCmd(workspace, arg string) {
	parts := strings.SplitN(strings.TrimSpace(arg), " ", 2)
	sub := strings.ToLower(parts[0])
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch sub {
	case "":
		enabled := isMemoryEnabled(workspace)
		records := loadMemIndex(workspace)
		status := "disabled"
		statusColor := colorRed
		if enabled {
			status = "enabled"
			statusColor = colorGreen
		}
		fmt.Printf("\n  %s%s◉  Workspace Memory%s\n", colorPrimary, ansiBold, ansiReset)
		fmt.Println(drawRule())
		fmt.Printf("  %sStatus:%s   %s%s%s\n", ansiDim, ansiReset, statusColor, status, ansiReset)
		fmt.Printf("  %sEntries:%s  %d\n", ansiDim, ansiReset, len(records))
		fmt.Printf("\n  %sCommands: /memory [on|off|list|view N|delete N|search <q>|stats]%s\n", ansiDim, ansiReset)
		fmt.Printf("  %sOr ask: /memory what did we do yesterday and today%s\n\n", ansiDim, ansiReset)

	case "on":
		setMemoryEnabled(workspace, true)
		fmt.Printf("  %s%s✓  Workspace memory enabled.%s\n", ansiBold, colorGreen, ansiReset)

	case "off":
		setMemoryEnabled(workspace, false)
		fmt.Printf("  %s✗  Workspace memory disabled.%s\n", ansiBold, ansiReset)

	case "list":
		records := loadMemIndex(workspace)
		if len(records) == 0 {
			fmt.Printf("  %sNo memory entries yet.%s\n", ansiDim, ansiReset)
			return
		}
		fmt.Println()
		fmt.Printf("  %s%s◉  Workspace Memory — All Entries%s\n", colorPrimary, ansiBold, ansiReset)
		fmt.Println(drawRule())
		fmt.Printf("\n  %s%4s  %-12s  %-40s  %s%s\n",
			ansiBold+colorPrimary, "#", "Date", "Summary", "Keywords", ansiReset)
		for i, r := range records {
			ts := r.Timestamp
			if len(ts) > 10 {
				ts = ts[:10]
			}
			summary := r.Summary
			if len(summary) > 39 {
				summary = summary[:39]
			}
			kws := strings.Join(r.Keywords, ", ")
			if len(kws) > 30 {
				kws = kws[:30] + "…"
			}
			fmt.Printf("  %s%4d%s  %s%-12s%s  %-40s  %s%s%s\n",
				colorPrimary, i+1, ansiReset,
				ansiDim, ts, ansiReset,
				summary,
				ansiDim, kws, ansiReset)
		}
		fmt.Println()

	case "view":
		n, ok := parseIdx(rest)
		if !ok {
			fmt.Printf("  %sUsage: /memory view <n>%s\n", colorRed, ansiReset)
			return
		}
		idx := n - 1
		records := loadMemIndex(workspace)
		if idx < 0 || idx >= len(records) {
			fmt.Printf("  %sEntry #%s not found.%s\n", colorRed, rest, ansiReset)
			return
		}
		r := records[idx]
		detail := readMemDetail(workspace, r.File)
		fmt.Println()
		fmt.Printf("  %s%s◉  Memory #%d%s\n", colorPrimary, ansiBold, idx+1, ansiReset)
		fmt.Println(drawRule())
		fmt.Printf("  %sDate:%s     %s\n", ansiDim, ansiReset, r.Timestamp[:min(19, len(r.Timestamp))])
		fmt.Printf("  %sSummary:%s  %s\n", ansiDim, ansiReset, r.Summary)
		if len(r.Keywords) > 0 {
			fmt.Printf("  %sKeywords:%s %s\n", ansiDim, ansiReset, strings.Join(r.Keywords, ", "))
		}
		if detail != "" {
			fmt.Println()
			for _, line := range strings.Split(detail, "\n") {
				fmt.Printf("  %s\n", line)
			}
		}
		fmt.Println()

	case "delete":
		n, ok := parseIdx(rest)
		if !ok {
			fmt.Printf("  %sUsage: /memory delete <n>%s\n", colorRed, ansiReset)
			return
		}
		idx := n - 1
		records := loadMemIndex(workspace)
		if idx < 0 || idx >= len(records) {
			fmt.Printf("  %sEntry #%s not found.%s\n", colorRed, rest, ansiReset)
			return
		}
		summary := records[idx].Summary
		fmt.Printf("\n  %sDelete memory #%d:%s %s\n", ansiBold, idx+1, ansiReset, summary)
		fmt.Printf("  Confirm delete? (y/N): ")
		reader := bufio.NewReader(os.Stdin)
		ans, _ := reader.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(ans)) == "y" {
			deleteMemEntry(workspace, idx)
			fmt.Printf("  %s✓  Deleted.%s\n", ansiDim+colorGreen, ansiReset)
		} else {
			fmt.Printf("  %sCancelled.%s\n", ansiDim, ansiReset)
		}

	case "search":
		if rest == "" {
			fmt.Printf("  %sUsage: /memory search <query>%s\n", colorRed, ansiReset)
			return
		}
		runMemorySearch(workspace, rest)

	case "stats":
		records := loadMemIndex(workspace)
		enabled := isMemoryEnabled(workspace)
		fmt.Println()
		fmt.Printf("  %s%s◉  Workspace Memory — Stats%s\n", colorPrimary, ansiBold, ansiReset)
		fmt.Println(drawRule())
		fmt.Printf("  %sEnabled:%s  %v\n", ansiDim, ansiReset, enabled)
		fmt.Printf("  %sEntries:%s  %d\n", ansiDim, ansiReset, len(records))
		if len(records) > 0 {
			fmt.Printf("  %sOldest:%s   %s\n", ansiDim, ansiReset, records[0].Timestamp[:min(10, len(records[0].Timestamp))])
			last := records[len(records)-1]
			fmt.Printf("  %sNewest:%s   %s\n", ansiDim, ansiReset, last.Timestamp[:min(10, len(last.Timestamp))])
		}
		fmt.Println()

	default:
		// Not a recognized subcommand — treat the whole argument as a search
		// query, so free-text questions like "/memory what did we do yesterday"
		// return matching entries instead of a hard error.
		fmt.Printf("  %sSearching memory (commands: on|off|list|view N|delete N|search <q>|stats)%s\n",
			ansiDim, ansiReset)
		runMemorySearch(workspace, strings.TrimSpace(arg))
	}
}

// searchStopwords are filler words ignored by runMemorySearch's per-word
// matching (the full-phrase match still sees them).
var searchStopwords = map[string]bool{
	"and": true, "are": true, "did": true, "for": true, "how": true,
	"the": true, "that": true, "this": true, "was": true, "were": true,
	"what": true, "when": true, "where": true, "which": true, "who": true,
	"with": true, "you": true, "your": true, "our": true, "does": true,
	"have": true, "has": true, "had": true, "can": true, "will": true,
}

// runMemorySearch prints the entries matching query. The full query is tried
// as a phrase; individual words (3+ chars) also match, so natural-language
// queries still hit. Results are ranked by how many words they match.
func runMemorySearch(workspace, query string) {
	records := loadMemIndex(workspace)
	q := strings.ToLower(query)

	// The whole phrase counts more than any single word. Short words and
	// filler words are skipped so "/memory what did we do with the grid"
	// effectively searches for "grid".
	tokens := []string{}
	for _, t := range strings.Fields(q) {
		if len(t) >= 3 && !searchStopwords[t] {
			tokens = append(tokens, t)
		}
	}

	matches := func(hay string) int {
		hay = strings.ToLower(hay)
		score := 0
		if strings.Contains(hay, q) {
			score += len(tokens) + 1
		}
		for _, t := range tokens {
			if strings.Contains(hay, t) {
				score++
			}
		}
		return score
	}

	type scored struct {
		rec   memIndexRecord
		score int
	}
	var found []scored
	for _, r := range records {
		s := matches(r.Summary) + matches(strings.Join(r.Keywords, " "))
		if s > 0 {
			found = append(found, scored{r, s})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].score > found[j].score })

	if len(found) == 0 {
		fmt.Printf("  %sNo results for '%s'.%s\n", ansiDim, query, ansiReset)
		return
	}
	fmt.Println()
	fmt.Printf("  %s%s◉  Search results for '%s' (%d found)%s\n",
		colorPrimary, ansiBold, query, len(found), ansiReset)
	fmt.Println(drawRule())
	for i, f := range found {
		r := f.rec
		ts := r.Timestamp
		if len(ts) > 19 {
			ts = ts[:19]
		}
		fmt.Printf("  %s%d.%s %s[%s]%s %s%s\n",
			colorPrimary, i+1, ansiReset,
			ansiDim, ts, ansiReset,
			r.Summary, ansiReset)
		if len(r.Keywords) > 0 {
			// Hang "keywords:" under the "[timestamp]" tag above — that
			// starts right after "  N. ", so the indent tracks N's width
			// instead of assuming it's always a single digit.
			indent := strings.Repeat(" ", 4+len(strconv.Itoa(i+1)))
			fmt.Printf("%s%skeywords: %s%s\n", indent, ansiDim, strings.Join(r.Keywords, ", "), ansiReset)
		}
	}
	fmt.Println()
}

// parseIdx parses a 1-based entry index; ok is false for non-numeric input so
// callers can print a usage hint instead of a misleading "not found".
func parseIdx(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

// ── /memory <natural-language question> ────────────────────────────────────────
//
// A free-text argument like "/memory what did we do yesterday and today" is not a
// keyword search — it's a request for a spoken-language status update. We resolve
// the date range from the phrasing, gather the work-log entries in that window,
// and ask the model to report what was accomplished, the way an engineer briefs a
// lead. Offline (no provider) we fall back to listing the matching entries.

// memorySubcommands are the reserved first words of /memory; anything else is
// treated as a natural-language question.
var memorySubcommands = map[string]bool{
	"": true, "on": true, "off": true, "list": true,
	"view": true, "delete": true, "search": true, "stats": true,
}

// isMemoryQuery reports whether arg is a natural-language question (non-empty and
// not one of the reserved subcommands) rather than a structured command.
func isMemoryQuery(arg string) bool {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return false
	}
	first := strings.ToLower(strings.Fields(arg)[0])
	return !memorySubcommands[first]
}

// memEntry is one work-log entry gathered for a summary, with its detail body.
type memEntry struct {
	When     time.Time
	Summary  string
	Keywords []string
	Detail   string
}

var lastNDaysRe = regexp.MustCompile(`(?:last|past)\s+(\d+)\s+days?`)
var isoDateRe = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)

// memoryDateRange derives an inclusive-start, exclusive-end window from the date
// phrasing in q (evaluated in now's location), plus a human label for it. When q
// names several ranges ("yesterday and today") they are unioned. ok is false when
// q contains no recognizable date phrase — the caller then defaults to recent
// entries.
func memoryDateRange(q string, now time.Time) (start, end time.Time, label string, ok bool) {
	lq := strings.ToLower(q)
	loc := now.Location()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	type span struct {
		start, end time.Time
		label      string
	}
	var spans []span
	add := func(s, e time.Time, l string) { spans = append(spans, span{s, e, l}) }

	if strings.Contains(lq, "today") {
		add(todayStart, todayStart.AddDate(0, 0, 1), "today")
	}
	if strings.Contains(lq, "yesterday") {
		add(todayStart.AddDate(0, 0, -1), todayStart, "yesterday")
	}

	// Weeks start Monday.
	daysSinceMon := (int(now.Weekday()) + 6) % 7
	weekStart := todayStart.AddDate(0, 0, -daysSinceMon)
	if strings.Contains(lq, "this week") {
		add(weekStart, todayStart.AddDate(0, 0, 1), "this week")
	}
	if strings.Contains(lq, "last week") {
		add(weekStart.AddDate(0, 0, -7), weekStart, "last week")
	} else if strings.Contains(lq, "past week") {
		add(todayStart.AddDate(0, 0, -6), todayStart.AddDate(0, 0, 1), "the past week")
	}

	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	if strings.Contains(lq, "this month") {
		add(monthStart, todayStart.AddDate(0, 0, 1), "this month")
	}
	if strings.Contains(lq, "last month") {
		add(monthStart.AddDate(0, -1, 0), monthStart, "last month")
	}

	if m := lastNDaysRe.FindStringSubmatch(lq); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			add(todayStart.AddDate(0, 0, -(n-1)), todayStart.AddDate(0, 0, 1), fmt.Sprintf("the last %d days", n))
		}
	}

	if m := isoDateRe.FindString(lq); m != "" {
		if d, err := time.ParseInLocation("2006-01-02", m, loc); err == nil {
			add(d, d.AddDate(0, 0, 1), d.Format("Jan 2, 2006"))
		}
	}

	if len(spans) == 0 {
		return time.Time{}, time.Time{}, "recently", false
	}

	// Order by start so the joined label reads chronologically ("yesterday and
	// today"), and union into one covering window.
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
	start, end = spans[0].start, spans[0].end
	labels := make([]string, 0, len(spans))
	seen := map[string]bool{}
	for _, s := range spans {
		if s.start.Before(start) {
			start = s.start
		}
		if s.end.After(end) {
			end = s.end
		}
		if !seen[s.label] {
			seen[s.label] = true
			labels = append(labels, s.label)
		}
	}
	return start, end, joinLabels(labels), true
}

// joinLabels renders date labels as "a", "a and b", or "a, b and c".
func joinLabels(labels []string) string {
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	case 2:
		return labels[0] + " and " + labels[1]
	default:
		return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
	}
}

const maxSummaryEntries = 50

// selectMemories returns the work-log entries to summarize. When matched, only
// entries whose timestamp falls in [start,end) are kept; otherwise the most
// recent maxSummaryEntries are used. Entries come back oldest-first so the prompt
// reads as a timeline.
func selectMemories(workspace string, records []memIndexRecord, start, end time.Time, matched bool, loc *time.Location) []memEntry {
	var out []memEntry
	for _, r := range records {
		ts, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			// Undated records are only usable in the "no explicit range" case.
			if matched {
				continue
			}
		} else {
			ts = ts.In(loc)
			if matched && (ts.Before(start) || !ts.Before(end)) {
				continue
			}
		}
		out = append(out, memEntry{
			When:     ts,
			Summary:  r.Summary,
			Keywords: r.Keywords,
			Detail:   readMemDetail(workspace, r.File),
		})
	}
	// Cap to the most recent N (list is oldest-first, so trim from the front).
	if len(out) > maxSummaryEntries {
		out = out[len(out)-maxSummaryEntries:]
	}
	return out
}

// buildMemorySummaryPrompt returns the system and user prompts for the status
// summary. Detail bodies are truncated and the whole block is capped so the
// request stays bounded regardless of how much history matched.
func buildMemorySummaryPrompt(query, label string, entries []memEntry) (system, user string) {
	system = "You are a software engineer giving your team lead a brief, honest status update. " +
		"You are given dated work-log entries from a coding assistant's memory. " +
		"Summarize what was actually accomplished in plain spoken language, grouped by theme, most important first. " +
		"Refer to when things happened using the dates. Use past tense and short paragraphs or bullets. " +
		"Do not invent work that is not in the entries. If the entries are thin, say so plainly."

	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s\n\n", strings.TrimSpace(query))
	fmt.Fprintf(&b, "Work-log entries for %s (%d entr%s), oldest first:\n\n", label, len(entries), plural(len(entries), "y", "ies"))

	const maxDetail = 600
	const maxTotal = 20000
	for _, e := range entries {
		date := "undated"
		if !e.When.IsZero() {
			date = e.When.Format("Mon Jan 2, 2006 15:04")
		}
		fmt.Fprintf(&b, "- [%s] %s\n", date, strings.TrimSpace(e.Summary))
		if d := strings.TrimSpace(e.Detail); d != "" {
			fmt.Fprintf(&b, "%s\n", indentBlock(truncate(d, maxDetail), "    "))
		}
		if b.Len() > maxTotal {
			b.WriteString("\n(older entries omitted to stay within length)\n")
			break
		}
	}
	b.WriteString("\nGive the status update now.")
	return system, b.String()
}

// truncate shortens s to at most max bytes, appending an ellipsis when cut. It
// trims on a rune boundary so multi-byte characters are never split.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// streamMemorySummary runs a one-shot completion and returns the collected text.
func streamMemorySummary(ctx context.Context, provider ai.Provider, model, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	ch, err := provider.Stream(ctx, ai.StreamRequest{
		Model:     model,
		System:    system,
		Messages:  []ai.ChatMessage{{Role: "user", Content: user}},
		MaxTokens: 1024,
	})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for ev := range ch {
		switch ev.Type {
		case "token":
			sb.WriteString(ev.Text)
		case "error":
			if ev.Err != nil {
				return strings.TrimSpace(sb.String()), ev.Err
			}
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// summarizeMemory answers a natural-language /memory question with a spoken
// status update over the memories in the requested date range.
func (r *TolleREPL) summarizeMemory(ctx context.Context, query string) {
	records := loadMemIndex(r.workspace)
	if len(records) == 0 {
		fmt.Printf("  %sNo memory entries yet.%s\n", ansiDim, ansiReset)
		return
	}

	now := time.Now()
	start, end, label, matched := memoryDateRange(query, now)
	entries := selectMemories(r.workspace, records, start, end, matched, now.Location())

	if len(entries) == 0 {
		fmt.Printf("  %sNo memory entries recorded %s.%s\n", ansiDim, label, ansiReset)
		return
	}

	provider := ai.Global.Get(r.providerID)
	if provider == nil {
		// Offline: fall back to listing the matching entries deterministically.
		fmt.Printf("  %sNo model reachable — showing the %d entr%s from %s instead.%s\n",
			ansiDim, len(entries), plural(len(entries), "y", "ies"), label, ansiReset)
		printMemoryEntries(entries)
		return
	}

	fmt.Printf("\n  %s%s◉  What we did · %s%s\n", colorPrimary, ansiBold, label, ansiReset)
	fmt.Println(drawRule())

	system, user := buildMemorySummaryPrompt(query, label, entries)
	if r.renderer != nil {
		r.renderer.StartLoader("")
	}
	text, err := streamMemorySummary(ctx, provider, r.model, system, user)
	if r.renderer != nil {
		r.renderer.StopLoader()
	}
	if err != nil || text == "" {
		fmt.Printf("  %sCouldn't generate a summary — showing the entries instead.%s\n", ansiDim, ansiReset)
		printMemoryEntries(entries)
		return
	}
	fmt.Println(RenderAssistantMarkdown(text, termWidth()))
	fmt.Println()
}

// printMemoryEntries lists entries as a simple dated timeline — the offline /
// fallback rendering for a summary request.
func printMemoryEntries(entries []memEntry) {
	fmt.Println()
	for _, e := range entries {
		date := "undated"
		if !e.When.IsZero() {
			date = e.When.Format("Jan 2 15:04")
		}
		fmt.Printf("  %s%s%s  %s\n", ansiDim, date, ansiReset, strings.TrimSpace(e.Summary))
	}
	fmt.Println()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── /skill ────────────────────────────────────────────────────────────────────

type skillDef struct {
	Name        string
	Description string
	Source      string // "global" | "workspace"
	Body        string
}

func loadSkills(workspace string) []skillDef {
	var out []skillDef
	globalDir := filepath.Join(config.Home(), "skills")
	out = append(out, scanSkillDir(globalDir, "global")...)
	if workspace != "" {
		wsDir := filepath.Join(workspace, ".agent", "skills")
		out = append(out, scanSkillDir(wsDir, "workspace")...)
	}
	return out
}

func scanSkillDir(dir, source string) []skillDef {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []skillDef
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		name, desc, body := parseSkillFile(path)
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".md")
		}
		out = append(out, skillDef{Name: name, Description: desc, Source: source, Body: body})
	}
	return out
}

func parseSkillFile(path string) (name, desc, body string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inFM, pastFM := false, false
	var bodyLines []string
	for sc.Scan() {
		line := sc.Text()
		if !inFM && !pastFM && line == "---" {
			inFM = true
			continue
		}
		if inFM {
			if line == "---" {
				inFM, pastFM = false, true
				continue
			}
			kv := strings.SplitN(line, ":", 2)
			if len(kv) == 2 {
				switch strings.TrimSpace(kv[0]) {
				case "name":
					name = strings.TrimSpace(kv[1])
				case "description":
					desc = strings.TrimSpace(kv[1])
				}
			}
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	body = strings.Join(bodyLines, "\n")
	return
}

func handleSkillCmd(workspace, sessionID, arg string) {
	parts := strings.SplitN(strings.TrimSpace(arg), " ", 2)
	sub := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}
	subLower := strings.ToLower(sub)

	skills := loadSkills(workspace)

	// Load active skills from session
	active := []string{}
	if sessionID != "" {
		s, err := session.Load(workspace, sessionID)
		if err == nil && s != nil {
			active = s.ActiveSkills
		}
	}

	isActive := func(name string) bool {
		for _, a := range active {
			if a == name {
				return true
			}
		}
		return false
	}

	switch subLower {
	case "":
		// List all skills
		if len(skills) == 0 {
			fmt.Printf("  %sNo skills found. Add .md files to ~/.tollecode/skills/ or .agent/skills/%s\n",
				ansiDim, ansiReset)
			return
		}
		fmt.Println()
		fmt.Printf("  %s%s◈  Skills%s\n", colorPrimary, ansiBold, ansiReset)
		fmt.Println(drawRule())
		fmt.Printf("\n  %s%s%-20s  %-10s  %-35s  %s%s\n",
			ansiBold, colorPrimary, "Name", "Source", "Description", "Active", ansiReset)
		for _, sk := range skills {
			// The dedicated Active column already carries the on/off signal —
			// the name itself doesn't need its own copy of that emphasis.
			active_ := ansiDim + "○ off" + ansiReset
			if isActive(sk.Name) {
				active_ = ansiBold + colorGreen + "● on" + ansiReset
			}
			desc := sk.Description
			if len(desc) > 34 {
				desc = desc[:34]
			}
			fmt.Printf("  %-20s  %s%-10s%s  %s%-35s%s  %s\n",
				sk.Name,
				ansiDim, sk.Source, ansiReset,
				ansiDim, desc, ansiReset,
				active_)
		}
		fmt.Println()
		if len(active) > 0 {
			fmt.Printf("  %sActive: %s%s\n", ansiDim, strings.Join(active, ", "), ansiReset)
		} else {
			fmt.Printf("  %sNo active skills. Use /skill <name> to activate.%s\n", ansiDim, ansiReset)
		}
		fmt.Println()

	case "clear":
		if sessionID != "" {
			_, _ = session.UpdateFields(workspace, sessionID, map[string]any{"activeSkills": []string{}})
		}
		fmt.Printf("  %sAll skills deactivated.%s\n", ansiDim, ansiReset)

	case "show":
		if rest == "" {
			fmt.Printf("  %sUsage: /skill show <name>%s\n", colorRed, ansiReset)
			return
		}
		for _, sk := range skills {
			if strings.EqualFold(sk.Name, rest) {
				fmt.Println()
				fmt.Printf("  %s%s%s  %s%s%s\n",
					ansiBold, sk.Name, ansiReset,
					ansiDim, sk.Description, ansiReset)
				if sk.Body != "" {
					fmt.Printf("  %s(%d chars)%s\n", ansiDim, len(sk.Body), ansiReset)
					for _, line := range strings.Split(sk.Body, "\n")[:min(30, len(strings.Split(sk.Body, "\n")))] {
						fmt.Printf("    %s\n", line)
					}
					if lines := strings.Split(sk.Body, "\n"); len(lines) > 30 {
						fmt.Printf("    %s… (truncated)%s\n", ansiDim, ansiReset)
					}
				}
				fmt.Println()
				return
			}
		}
		fmt.Printf("  %sSkill '%s' not found.%s\n", colorRed, rest, ansiReset)

	default:
		// Toggle skill by name
		skillName := sub
		found := false
		for _, sk := range skills {
			if strings.EqualFold(sk.Name, skillName) {
				found = true
				skillName = sk.Name // use canonical name
				break
			}
		}
		if !found {
			fmt.Printf("  %sSkill '%s' not found.%s\n", colorRed, skillName, ansiReset)
			if len(skills) > 0 {
				names := make([]string, len(skills))
				for i, sk := range skills {
					names[i] = sk.Name
				}
				fmt.Printf("  %sAvailable: %s%s\n", ansiDim, strings.Join(names, ", "), ansiReset)
			}
			return
		}
		newActive := []string{}
		removed := false
		for _, a := range active {
			if a == skillName {
				removed = true
			} else {
				newActive = append(newActive, a)
			}
		}
		if !removed {
			newActive = append(newActive, skillName)
			fmt.Printf("  %s%s✓  Skill '%s' activated.%s\n", ansiBold, colorGreen, skillName, ansiReset)
		} else {
			fmt.Printf("  %sSkill '%s' deactivated.%s\n", ansiDim, skillName, ansiReset)
		}
		if sessionID != "" {
			_, _ = session.UpdateFields(workspace, sessionID, map[string]any{"activeSkills": newActive})
		}
	}
}

// ── /configure ────────────────────────────────────────────────────────────────

var providerTypes = []struct{ id, label string }{
	{"anthropic", "Anthropic Claude      api.anthropic.com"},
	{"openai", "OpenAI                api.openai.com (or compatible)"},
	{"ollama", "Ollama local          localhost:11434"},
	{"ollama-cloud", "Ollama cloud          custom endpoint + API key"},
	{"custom", "Custom                any OpenAI-compatible endpoint"},
}

func handleConfigure(workspace string) {
	reader := bufio.NewReader(os.Stdin)
	for {
		ai.Global.Reload()
		cfgs := loadProviderConfigs()

		fmt.Println()
		fmt.Printf("  %s%s◉  Configure providers%s\n", colorPrimary, ansiBold, ansiReset)
		fmt.Println(drawRule())
		fmt.Println()

		if len(cfgs) == 0 {
			fmt.Printf("  %sNo providers configured.%s\n", ansiDim, ansiReset)
		} else {
			for i, p := range cfgs {
				enabled := p["enabled"]
				statusColor := colorGreen
				statusLabel := "enabled"
				if e, ok := enabled.(bool); ok && !e {
					statusColor = ansiDim
					statusLabel = "disabled"
				}
				name, _ := p["name"].(string)
				if name == "" {
					name, _ = p["id"].(string)
				}
				ptype, _ := p["type"].(string)
				fmt.Printf("  %s%s%d%s  %s%-24s%s %s%-14s%s  %s%s%s\n",
					ansiBold, colorPrimary, i+1, ansiReset,
					ansiBold, name, ansiReset,
					ansiDim, ptype, ansiReset,
					statusColor, statusLabel, ansiReset)
			}
		}

		fmt.Println()
		fmt.Printf("  %s[A]dd  [E]dit  [R]emove  [D]one%s\n\n", ansiDim, ansiReset)
		fmt.Printf("  %s%s❯%s ", colorPrimary, ansiBold, ansiReset)
		raw, _ := reader.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(raw))

		switch choice {
		case "a", "add":
			addProvider(reader, cfgs)
		case "e", "edit":
			editProvider(reader, cfgs)
		case "r", "remove":
			removeProvider(reader, cfgs)
		case "d", "done", "q", "":
			fmt.Printf("  %sDone.%s\n\n", ansiDim, ansiReset)
			return
		}
	}
}

func addProvider(reader *bufio.Reader, cfgs []map[string]any) {
	fmt.Println()
	fmt.Printf("  %s%s◈  Add provider — type%s\n", colorPrimary, ansiBold, ansiReset)
	fmt.Println(drawRule())
	fmt.Println()
	for i, pt := range providerTypes {
		fmt.Printf("  %s%s%d%s  %s%s\n",
			ansiBold, colorPrimary, i+1, ansiReset,
			pt.label, ansiReset)
	}
	fmt.Println()
	fmt.Printf("  Type [1]: ")
	raw, _ := reader.ReadString('\n')
	raw = strings.TrimSpace(raw)
	idx := 1
	if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= len(providerTypes) {
		idx = n
	}
	ptype := providerTypes[idx-1].id

	fmt.Printf("  Name: ")
	name, _ := reader.ReadString('\n')
	name = strings.TrimSpace(name)
	if name == "" {
		name = providerTypes[idx-1].id
	}

	p := map[string]any{
		"id":      sanitizeID(name) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10),
		"type":    ptype,
		"name":    name,
		"enabled": true,
		"models":  []any{},
	}

	needsKey := map[string]bool{"anthropic": true, "openai": true, "ollama-cloud": true, "custom": true}
	needsEndpoint := map[string]bool{"ollama": true, "ollama-cloud": true, "custom": true}

	if needsEndpoint[ptype] {
		defaultEP := ""
		if ptype == "ollama" {
			defaultEP = "http://localhost:11434"
		}
		prompt := "  Endpoint"
		if defaultEP != "" {
			prompt += " [" + defaultEP + "]"
		}
		fmt.Printf("%s: ", prompt)
		ep, _ := reader.ReadString('\n')
		ep = strings.TrimSpace(ep)
		if ep == "" {
			ep = defaultEP
		}
		p["endpoint"] = ep
	}

	if needsKey[ptype] {
		optional := ptype == "custom"
		prompt := "  API key"
		if optional {
			prompt += " (optional)"
		}
		fmt.Printf("%s: ", prompt)
		key, _ := reader.ReadString('\n')
		key = strings.TrimSpace(key)
		p["apiKey"] = key
	}

	fmt.Printf("  Default model (optional): ")
	mdl, _ := reader.ReadString('\n')
	mdl = strings.TrimSpace(mdl)
	if mdl != "" {
		p["models"] = []any{map[string]any{"id": mdl, "name": mdl, "isDefault": true}}
		p["defaultModel"] = mdl
	}

	cfgs = append(cfgs, p)
	if err := saveProviderConfigs(cfgs); err != nil {
		fmt.Printf("  %s%s✗  Save failed: %v%s\n", ansiBold, colorRed, err, ansiReset)
	} else {
		fmt.Printf("  %s%s✓  Provider '%s' added.%s\n", ansiBold, colorGreen, name, ansiReset)
		ai.Global.Reload()
	}
}

func editProvider(reader *bufio.Reader, cfgs []map[string]any) {
	if len(cfgs) == 0 {
		fmt.Printf("  %sNo providers to edit.%s\n", ansiDim, ansiReset)
		return
	}
	fmt.Printf("  Provider number to edit: ")
	raw, _ := reader.ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > len(cfgs) {
		fmt.Printf("  %sInvalid selection.%s\n", colorRed, ansiReset)
		return
	}
	p := cfgs[n-1]
	name, _ := p["name"].(string)

	fmt.Printf("  Editing '%s'\n", name)
	fmt.Printf("  New name [%s]: ", name)
	newName, _ := reader.ReadString('\n')
	newName = strings.TrimSpace(newName)
	if newName != "" {
		p["name"] = newName
	}

	if ep, _ := p["endpoint"].(string); ep != "" {
		fmt.Printf("  Endpoint [%s]: ", ep)
		newEP, _ := reader.ReadString('\n')
		newEP = strings.TrimSpace(newEP)
		if newEP != "" {
			p["endpoint"] = newEP
		}
	}

	fmt.Printf("  API key (leave blank to keep): ")
	newKey, _ := reader.ReadString('\n')
	newKey = strings.TrimSpace(newKey)
	if newKey != "" {
		p["apiKey"] = newKey
	}

	cfgs[n-1] = p
	if err := saveProviderConfigs(cfgs); err != nil {
		fmt.Printf("  %s%s✗  Save failed: %v%s\n", ansiBold, colorRed, err, ansiReset)
	} else {
		fmt.Printf("  %s%s✓  Updated.%s\n", ansiBold, colorGreen, ansiReset)
		ai.Global.Reload()
	}
}

func removeProvider(reader *bufio.Reader, cfgs []map[string]any) {
	if len(cfgs) == 0 {
		fmt.Printf("  %sNo providers to remove.%s\n", ansiDim, ansiReset)
		return
	}
	fmt.Printf("  Provider number to remove: ")
	raw, _ := reader.ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > len(cfgs) {
		fmt.Printf("  %sInvalid selection.%s\n", colorRed, ansiReset)
		return
	}
	p := cfgs[n-1]
	name, _ := p["name"].(string)
	if name == "" {
		name, _ = p["id"].(string)
	}
	fmt.Printf("  Remove '%s'? (y/N): ", name)
	ans, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(ans)) != "y" {
		fmt.Printf("  %sCancelled.%s\n", ansiDim, ansiReset)
		return
	}
	cfgs = append(cfgs[:n-1], cfgs[n:]...)
	if err := saveProviderConfigs(cfgs); err != nil {
		fmt.Printf("  %s%s✗  Save failed: %v%s\n", ansiBold, colorRed, err, ansiReset)
	} else {
		fmt.Printf("  %s%s✓  Removed '%s'.%s\n", ansiBold, colorGreen, name, ansiReset)
		ai.Global.Reload()
	}
}

func loadProviderConfigs() []map[string]any {
	path := filepath.Join(config.Home(), "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfgs []map[string]any
	_ = json.Unmarshal(data, &cfgs)
	return cfgs
}

func saveProviderConfigs(cfgs []map[string]any) error {
	_ = os.MkdirAll(config.Home(), 0o755)
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(config.Home(), "config.json"), append(data, '\n'), 0o644)
}

func sanitizeID(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		} else if c == ' ' || c == '_' {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// ── configure settings ────────────────────────────────────────────────────────

func handleConfigureSettings() {
	reader := bufio.NewReader(os.Stdin)
	s := config.LoadSidecarSettings()

	fmt.Println()
	fmt.Printf("  %s%s◉  Agent Settings%s\n", colorPrimary, ansiBold, ansiReset)
	fmt.Println(drawRule())
	fmt.Println()
	fmt.Printf("  %s1.%s Max tool iterations    %d\n", colorPrimary, ansiReset, s.MaxToolIterations)
	confirmLabel := "off"
	confirmColor := ansiDim
	if s.ConfirmContinue {
		confirmLabel = "on"
		confirmColor = colorGreen
	}
	fmt.Printf("  %s2.%s Confirm continue       %s%s%s\n", colorPrimary, ansiReset, confirmColor, confirmLabel, ansiReset)
	threshLabel := "auto (80%% of max)"
	if s.ConfirmContinueThreshold > 0 {
		threshLabel = fmt.Sprintf("%d", s.ConfirmContinueThreshold)
	}
	fmt.Printf("  %s3.%s Confirm threshold      %s%s%s\n", colorPrimary, ansiReset, ansiDim, threshLabel, ansiReset)
	fmt.Printf("  %s4.%s Egress guardrail       %s\n", colorPrimary, ansiReset, s.EffectiveEgressMode())
	fmt.Println()
	fmt.Printf("  %s[1] max-iterations  [2] confirm-continue  [3] threshold  [4] egress  [D]one%s\n\n", ansiDim, ansiReset)

	for {
		fmt.Printf("  %s%s❯%s ", colorPrimary, ansiBold, ansiReset)
		raw, _ := reader.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(raw))
		switch choice {
		case "1", "max-iterations":
			fmt.Printf("  Max tool iterations [%d]: ", s.MaxToolIterations)
			v, _ := reader.ReadString('\n')
			v = strings.TrimSpace(v)
			if v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					s.MaxToolIterations = n
				} else {
					fmt.Printf("  %sInvalid number.%s\n", colorRed, ansiReset)
					continue
				}
			}
		case "2", "confirm-continue":
			fmt.Printf("  Confirm continue (on/off) [%s]: ", confirmLabel)
			v, _ := reader.ReadString('\n')
			v = strings.ToLower(strings.TrimSpace(v))
			if v == "on" || v == "true" || v == "yes" || v == "1" {
				s.ConfirmContinue = true
			} else if v == "off" || v == "false" || v == "no" || v == "0" {
				s.ConfirmContinue = false
			} else if v != "" {
				fmt.Printf("  %sEnter on or off.%s\n", colorRed, ansiReset)
				continue
			}
		case "3", "threshold":
			fmt.Printf("  Confirm threshold (0 = auto 80%%): ")
			v, _ := reader.ReadString('\n')
			v = strings.TrimSpace(v)
			if v != "" {
				if n, err := strconv.Atoi(v); err == nil && n >= 0 {
					s.ConfirmContinueThreshold = n
				} else {
					fmt.Printf("  %sInvalid number.%s\n", colorRed, ansiReset)
					continue
				}
			}
		case "4", "egress":
			fmt.Printf("  Egress guardrail — off (no scan) / log (warn only) / redact (strip secrets) [%s]: ", s.EffectiveEgressMode())
			v, _ := reader.ReadString('\n')
			v = strings.ToLower(strings.TrimSpace(v))
			if v != "" {
				switch v {
				case "off", "log", "redact":
					s.EgressMode = v
				default:
					fmt.Printf("  %sEnter off, log, or redact.%s\n", colorRed, ansiReset)
					continue
				}
			}
		case "d", "done", "q", "":
			if err := config.SaveSidecarSettings(s); err != nil {
				fmt.Printf("  %s%s✗  Save failed: %v%s\n", ansiBold, colorRed, err, ansiReset)
			} else {
				ai.SyncEgressFromSettings() // apply the saved guardrail mode to this process
				fmt.Printf("  %s%s✓  Settings saved.%s\n\n", ansiBold, colorGreen, ansiReset)
			}
			return
		default:
			fmt.Printf("  %sUnknown option.%s\n", colorRed, ansiReset)
		}
		// Show updated values
		confirmLabel = "off"
		confirmColor = ansiDim
		if s.ConfirmContinue {
			confirmLabel = "on"
			confirmColor = colorGreen
		}
		threshLabel = "auto (80%% of max)"
		if s.ConfirmContinueThreshold > 0 {
			threshLabel = fmt.Sprintf("%d", s.ConfirmContinueThreshold)
		}
		fmt.Printf("\n  %s1.%s Max iterations  %d   %s2.%s Confirm  %s%s%s   %s3.%s Threshold  %s%s%s   %s4.%s Egress  %s%s\n\n",
			colorPrimary, ansiReset, s.MaxToolIterations,
			colorPrimary, ansiReset, confirmColor, confirmLabel, ansiReset,
			colorPrimary, ansiReset, ansiDim, threshLabel, ansiReset,
			colorPrimary, ansiReset, s.EffectiveEgressMode(), ansiReset,
		)
	}
}

// ── Exported entry points for cmd/tollecode ───────────────────────────────────

func RunConfigure(workspace string)        { handleConfigure(workspace) }
func RunConfigureAdd()                     { addProvider(bufio.NewReader(os.Stdin), loadProviderConfigs()) }
func RunConfigureRemoveInteractive()       { removeProvider(bufio.NewReader(os.Stdin), loadProviderConfigs()) }
func RunConfigureSettings()                { handleConfigureSettings() }

func RunConfigureRemoveByID(id string) {
	cfgs := loadProviderConfigs()
	idx := -1
	for i, p := range cfgs {
		if pid, _ := p["id"].(string); pid == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		fmt.Printf("  %sProvider '%s' not found.%s\n", colorRed, id, ansiReset)
		return
	}
	name, _ := cfgs[idx]["name"].(string)
	if name == "" {
		name = id
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("  Remove '%s'? (y/N): ", name)
	ans, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(ans)) != "y" {
		fmt.Printf("  %sCancelled.%s\n", ansiDim, ansiReset)
		return
	}
	cfgs = append(cfgs[:idx], cfgs[idx+1:]...)
	if err := saveProviderConfigs(cfgs); err != nil {
		fmt.Printf("  %s%s✗  Save failed: %v%s\n", ansiBold, colorRed, err, ansiReset)
	} else {
		fmt.Printf("  %s%s✓  Removed '%s'.%s\n", ansiBold, colorGreen, name, ansiReset)
		ai.Global.Reload()
	}
}

func RunConfigureSetKey(providerID, key string) {
	cfgs := loadProviderConfigs()
	for i, p := range cfgs {
		if pid, _ := p["id"].(string); pid == providerID {
			cfgs[i]["apiKey"] = key
			if err := saveProviderConfigs(cfgs); err != nil {
				fmt.Printf("  %s%s✗  Save failed: %v%s\n", ansiBold, colorRed, err, ansiReset)
			} else {
				name, _ := p["name"].(string)
				if name == "" {
					name = providerID
				}
				fmt.Printf("  %s%s✓  Key updated for %s.%s\n", ansiBold, colorGreen, name, ansiReset)
				ai.Global.Reload()
			}
			return
		}
	}
	fmt.Printf("  %sProvider '%s' not found.%s\n", colorRed, providerID, ansiReset)
}

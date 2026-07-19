package cli

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tolle-ai/tollecode/internal/session"
	"golang.org/x/term"
)

// Version is the CLI version. It defaults to the value below and is
// overridden at build time via:
//
//	-ldflags "-X github.com/tolle-ai/tollecode/internal/cli.Version=<v>"
//
// so every published binary reports a distinct, verifiable version.
var Version = "3.0.1"

// ── ANSI helpers ──────────────────────────────────────────────────────────────

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiItalic = "\033[3m"
	// ansiReverse draws the composer caret. The real terminal cursor lives in
	// the scroll region during a turn (Invariant 1), so the caret on the pinned
	// input row has to be painted rather than moved to.
	ansiReverse = "\033[7m"
)

func ansiRGB(r, g, b int) string {
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

var (
	// colorPrimary is the single brand accent (#7B5CF5, this monorepo's own
	// design-system token). There's no terminal-background detection anywhere in
	// this package, so one fixed truecolor value has to hold up against both a
	// light and a dark terminal — this hue is the balance point.
	colorPrimary = ansiRGB(123, 92, 245)  // #7B5CF5
	colorGreen   = ansiRGB(74, 222, 128)  // #4ADE80 — diff add / success
	colorRed     = ansiRGB(248, 113, 113) // #F87171 — diff remove / error
)

var logoRows = []string{
	"▐████████▌",
	"   ████   ",
	"   ████   ",
	"   ▀▀▀▀   ",
}

// gradientColor renders one stop of loaderGradient as a truecolor escape.
func gradientColor(i int) string {
	c := loaderGradient[i]
	return ansiRGB(c[0], c[1], c[2])
}

// logoColors shades the logo light-to-dark, top to bottom, using stops from
// the same gradient ramp the loader animates through — one hue, one source.
var logoColors = []string{gradientColor(4), gradientColor(3), gradientColor(2), gradientColor(1)}

func terminalSize() (width, height int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80, 24
	}
	return w, h
}

func termWidth() int {
	w, _ := terminalSize()
	return w
}

// wrapPlain word-wraps plain (unstyled) text to width, returning one string per
// line. Long single words are left intact rather than hard-split.
func wrapPlain(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	var lines []string
	var cur strings.Builder
	curW := 0
	for _, word := range strings.Fields(s) {
		ww := len([]rune(word))
		if curW > 0 && curW+1+ww > width {
			lines = append(lines, cur.String())
			cur.Reset()
			curW = 0
		}
		if curW > 0 {
			cur.WriteByte(' ')
			curW++
		}
		cur.WriteString(word)
		curW += ww
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	if len(lines) == 0 {
		lines = append(lines, "")
	}
	return lines
}

func drawRule() string {
	w := termWidth() - 4
	if w < 4 {
		w = 4
	}
	return "  " + ansiDim + strings.Repeat("─", w) + ansiReset
}

// ── Intro / ready banners ─────────────────────────────────────────────────────

// PrintIntro renders the startup banner as a bordered box, sized to the
// terminal but capped so it doesn't stretch absurdly wide on a large one.
func PrintIntro(workspace string, sessionCount int) {
	fmt.Println()
	home := os.Getenv("HOME")
	wsDisplay := strings.Replace(workspace, home, "~", 1)

	sessionHint := ""
	if sessionCount > 0 {
		plural := "s"
		if sessionCount == 1 {
			plural = ""
		}
		sessionHint = fmt.Sprintf("  ·  %d session%s", sessionCount, plural)
	}

	boxWidth := termWidth() - 4
	if boxWidth > 68 {
		boxWidth = 68
	}
	if boxWidth < 44 {
		boxWidth = 44
	}
	contentW := boxWidth - 4 // minus the "│ " / " │" border + padding on each side
	border := ansiDim

	row := func(content string) string {
		pad := contentW - visibleWidth(content)
		if pad < 0 {
			pad = 0
		}
		return "  " + border + "│ " + ansiReset + content + strings.Repeat(" ", pad) + border + " │" + ansiReset
	}

	const gap = "    "
	titleLine := logoColors[0] + ansiBold + logoRows[0] + ansiReset + gap +
		ansiBold + colorPrimary + "TolleCode" + ansiReset + ansiDim + "  v" + Version + ansiReset
	subtitleLine := logoColors[1] + ansiBold + logoRows[1] + ansiReset + gap +
		ansiDim + ansiItalic + "AI coding assistant" + ansiReset

	pathBudget := contentW - visibleWidth(logoRows[2]) - len(gap) - visibleWidth(sessionHint)
	if pathBudget < 3 {
		pathBudget = 3
	}
	pathLine := logoColors[2] + ansiBold + logoRows[2] + ansiReset + gap +
		ansiBold + truncateVisible(wsDisplay, pathBudget) + ansiReset + ansiDim + sessionHint + ansiReset
	tailLine := logoColors[3] + ansiBold + logoRows[3] + ansiReset

	fmt.Println("  " + border + "╭" + strings.Repeat("─", boxWidth-2) + "╮" + ansiReset)
	fmt.Println(row(""))
	fmt.Println(row(titleLine))
	fmt.Println(row(subtitleLine))
	fmt.Println(row(pathLine))
	fmt.Println(row(tailLine))
	fmt.Println(row(""))
	fmt.Println("  " + border + "╰" + strings.Repeat("─", boxWidth-2) + "╯" + ansiReset)
	fmt.Println()
}

// PrintReady shows the resolved provider/model/mode once at startup. The
// per-turn composer footer (PrintComposerFooter) repeats the model/mode on
// every prompt, so this doesn't need its own hint list or trailing rule.
func PrintReady(providerID, model, mode string) {
	modeColor := colorGreen
	if mode == "plan" {
		modeColor = colorPrimary
	}
	fmt.Printf("  %sprovider%s  %s  ·  %s\n", ansiDim, ansiReset, providerID, model)
	fmt.Printf("  %smode%s      %s%s%s\n", ansiDim, ansiReset, ansiBold+modeColor, strings.ToUpper(mode), ansiReset)
	fmt.Println()
}

func PrintHelp() {
	fmt.Println()
	w := termWidth() - 12
	if w < 4 {
		w = 4
	}
	fmt.Printf("  %s%sCommands%s  %s%s%s\n",
		colorPrimary, ansiBold, ansiReset,
		ansiDim, strings.Repeat("─", w)+ansiReset, "")
	fmt.Println()

	sections := []struct {
		name string
		rows [][2]string
	}{
		{"Files", [][2]string{
			{"@", "Open interactive file picker (↑↓ navigate, type to filter)"},
			{"@query", "Open picker pre-filtered by name (e.g. @Button)"},
			{"@path in message", "Auto-attach file content to the message on send"},
		}},
		{"Session", [][2]string{
			{"/help", "Show this help"},
			{"/clear", "Clear the screen"},
			{"/exit  or  ctrl-c", "Quit"},
			{"/new", "Start a fresh session"},
			{"/sessions", "List recent sessions"},
			{"/session <id>", "Resume a session by ID prefix"},
		}},
		{"Model & Provider", [][2]string{
			{"/configure", "Add or manage AI providers"},
			{"/settings", "Agent settings (iterations, egress guardrail)"},
			{"/provider", "Switch provider interactively"},
			{"/model [name]", "Switch model (fetches live list if no name)"},
			{"/mode [plan|build]", "Switch mode or show current"},
			{"/thinking [0|1k|4k|10k|32k]", "Set extended thinking budget"},
		}},
		{"Memory", [][2]string{
			{"/memory", "Show memory status"},
			{"/memory <question>", "Summarize what was done (e.g. \"what did we do yesterday\")"},
			{"/memory on|off", "Enable or disable workspace memory"},
			{"/memory list", "List all memory entries"},
			{"/memory view <n>", "View full content of entry #n"},
			{"/memory delete <n>", "Delete entry #n"},
			{"/memory search <query>", "Keyword search across memory"},
			{"/memory stats", "Show memory statistics"},
		}},
		{"Desktop Control", [][2]string{
			{"/screen <task>", "Control the physical screen (screenshot → LLM → click/type). Shell auto-allowed."},
		}},
		{"Agents & Teams", [][2]string{
			{"%", "Open interactive agent/team picker"},
			{"%query", "Open picker pre-filtered by name (e.g. %review)"},
			{"/agent", "Pick an agent or team for this session (↑↓, type to filter)"},
			{"/agent <name>", "Select an agent/team by name"},
			{"/agent clear", "Return to the general assistant"},
			{"/agents", "Manage agents — create, edit, assign skills, delete"},
			{"/teams", "Manage teams — create, edit lead & members, delete"},
		}},
		{"Skills & Tasks", [][2]string{
			{"/skills", "Manage skills — create, edit, delete (interactive)"},
			{"/skill", "List available skills"},
			{"/skill <name>", "Toggle a skill on/off for this session"},
			{"/skill show <name>", "Show skill content"},
			{"/skill clear", "Deactivate all skills"},
			{"/todo", "View agent's todo list"},
		}},
		{"Analytics", [][2]string{
			{"/usage", "Show session usage analytics"},
		}},
	}

	for _, sec := range sections {
		fmt.Printf("  %s%s%s%s\n", colorPrimary, ansiBold, sec.name, ansiReset)
		for _, r := range sec.rows {
			fmt.Printf("    %s%-38s%s%s%s\n",
				colorPrimary, r[0], ansiReset,
				ansiDim, r[1]+ansiReset)
		}
		fmt.Println()
	}
}

// ── Composer footer ───────────────────────────────────────────────────────────

// composerHintLine builds the one-line status/hint string shown under the
// pinned composer input (or above the prompt on the legacy fallback path):
// mode, model, keyboard hints, active agent, and active skills. Optional tail
// segments are dropped rather than letting the line exceed width — a wrapped
// pinned row would corrupt the terminal's bottom line.
func composerHintLine(model, mode string, activeSkills []string, agentLabel string, width int) string {
	modeColor := colorGreen
	if mode == "plan" {
		modeColor = colorPrimary
	}
	modelStr := model
	if len(modelStr) > 45 {
		modelStr = modelStr[:45]
	}

	type seg struct{ plain, styled string }
	segs := []seg{
		{strings.ToUpper(mode), ansiBold + modeColor + strings.ToUpper(mode) + ansiReset},
		{modelStr, modelStr},
		{"? for shortcuts", ansiDim + "?" + ansiReset + " for shortcuts"},
		{"@ files", ansiDim + "@" + ansiReset + " files"},
		{"% agents", ansiDim + "%" + ansiReset + " agents"},
	}
	if agentLabel != "" {
		segs = append(segs, seg{"◎ " + agentLabel, ansiDim + "◎ " + agentLabel + ansiReset})
	}
	if len(activeSkills) > 0 {
		s := "skills: " + strings.Join(activeSkills, ", ")
		segs = append(segs, seg{s, ansiDim + s + ansiReset})
	}

	const sep = "  ·  "
	line := "  " + segs[0].styled
	used := 2 + visibleWidth(segs[0].plain)
	for _, s := range segs[1:] {
		add := len(sep) + visibleWidth(s.plain)
		if used+add > width {
			break
		}
		line += sep + s.styled
		used += add
	}
	return line
}

// PrintComposerFooter prints the rule + status/hint line that immediately
// precedes the input prompt on every turn — the legacy (non-pinned) fallback
// used when the pinned composer is unavailable (non-TTY, tiny terminal).
func PrintComposerFooter(model, mode string, activeSkills []string, agentLabel string) {
	fmt.Println(drawRule())
	fmt.Println(composerHintLine(model, mode, activeSkills, agentLabel, termWidth()))
}

// echoUserLine prints a submitted prompt into the content flow. With the
// pinned composer, the input row is cleared after submit, so this is what
// keeps the user's message visible in the scrollback.
func echoUserLine(text string) {
	fmt.Printf("\n  %s%s❯%s %s\n", colorPrimary, ansiBold, ansiReset, text)
}

// ── Sub-agent history cards ───────────────────────────────────────────────────

// PrintSubagentCards displays sub-agent execution history — shown when
// switching sessions or after session load (mirrors Python's print_subagent_cards).
func PrintSubagentCards(subagents []map[string]any) {
	if len(subagents) == 0 {
		return
	}
	fmt.Println()
	fmt.Printf("  %s%sSub-agents%s  %s%s%s\n",
		colorPrimary, ansiBold, ansiReset,
		ansiDim, strings.Repeat("─", termWidth()-16)+ansiReset, "")
	fmt.Println()

	for i := len(subagents) - 1; i >= 0; i-- {
		sa := subagents[i]
		color, _ := sa["color"].(string)
		c := safeColor(color)
		role, _ := sa["role"].(string)
		if role == "" {
			role = "agent"
		}
		title, _ := sa["title"].(string)
		if title == "" {
			title = "—"
		}
		if len(title) > 70 {
			title = title[:70]
		}
		status, _ := sa["status"].(string)
		result, _ := sa["result"].(string)
		result = strings.ReplaceAll(strings.TrimSpace(result), "\n", " ")

		statusIcon := "⟳"
		statusStyle := colorPrimary
		if status == "done" {
			statusIcon = "✓"
			statusStyle = colorGreen
		} else if status == "error" {
			statusIcon = "✗"
			statusStyle = colorRed
		}

		fmt.Printf("  %s%s⟳  %s%s  %s%s\n",
			ansiBold, c, role, ansiReset,
			title, ansiReset)

		line2 := fmt.Sprintf("     %s%s %s%s", ansiDim+statusStyle, statusIcon, status, ansiReset)
		if result != "" {
			if len(result) > 120 {
				result = result[:120]
			}
			line2 += fmt.Sprintf("  %s·  %s%s", ansiDim, result, ansiReset)
		}
		fmt.Println(line2)
		fmt.Println()
	}
	fmt.Println(drawRule())
	fmt.Println()
}

// ── Tool icons / display names ────────────────────────────────────────────────

var toolIcons = map[string]string{
	"read_file":          "○",
	"write_file":         "●",
	"edit_file":          "◇",
	"list_directory":     "◈",
	"run_shell":          "▶",
	"create_plan":        "◆",
	"spawn_sub_agent":    "⟳",
	"wait_for_subagents": "◷",
	"finish":             "✓",
	"finish_task":        "✓",
	"TodoWrite":          "☰",
	"TodoRead":           "☰",
}

// clean display names (no trailing spaces) — used for subagent tool labels
var toolNames = map[string]string{
	"read_file":          "Read",
	"write_file":         "Write",
	"edit_file":          "Edit",
	"list_directory":     "List",
	"run_shell":          "Run",
	"create_plan":        "Plan",
	"spawn_sub_agent":    "Spawn",
	"wait_for_subagents": "Wait",
	"finish":             "Done",
	"finish_task":        "Done",
	"TodoWrite":          "Todos",
	"TodoRead":           "Todos",
}

// file extension → language label (matches Python _FILE_EXT_LABEL exactly)
var fileExtLabel = map[string]string{
	".py": "python", ".ts": "typescript", ".js": "javascript",
	".tsx": "tsx", ".jsx": "jsx", ".md": "markdown",
	".json": "json", ".yaml": "yaml", ".yml": "yaml",
	".toml": "toml", ".sh": "shell", ".env": "env",
	".html": "html", ".css": "css", ".rs": "rust",
	".go": "go", ".txt": "text", ".sql": "sql",
	".c": "c", ".cpp": "c++", ".h": "header",
	".java": "java", ".kt": "kotlin", ".swift": "swift",
	".rb": "ruby", ".php": "php", ".cs": "c#",
	".lock": "lockfile", ".gitignore": "git", ".dockerfile": "docker",
}

func fileTypeLabel(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	return fileExtLabel[ext]
}

func toolIcon(tool string) string {
	if ic, ok := toolIcons[tool]; ok {
		return ic
	}
	return "◦"
}

func toolDisplayName(tool string) string {
	if n, ok := toolNames[tool]; ok {
		return n
	}
	words := strings.Fields(strings.ReplaceAll(tool, "_", " "))
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// toolDetail matches Python _tool_detail exactly — including TodoWrite summary.
func toolDetail(tool string, inp map[string]any) string {
	switch tool {
	case "run_shell":
		cmd, _ := inp["command"].(string)
		if len(cmd) > 100 {
			return cmd[:100]
		}
		return cmd
	case "read_file", "write_file", "edit_file":
		path, _ := inp["path"].(string)
		return path
	case "list_directory":
		path, _ := inp["path"].(string)
		if path == "" {
			return "."
		}
		return path
	case "spawn_sub_agent":
		role, _ := inp["role"].(string)
		if role == "" {
			role = "agent"
		}
		title, _ := inp["taskTitle"].(string)
		if title == "" {
			title, _ = inp["task"].(string)
		}
		if len(title) > 60 {
			title = title[:60]
		}
		return fmt.Sprintf("[%s] %s", role, title)
	case "finish", "finish_task":
		summary, _ := inp["summary"].(string)
		if len(summary) > 80 {
			return summary[:80]
		}
		return summary
	case "TodoWrite":
		todos, _ := inp["todos"].([]any)
		if len(todos) == 0 {
			return ""
		}
		n := len(todos)
		completed, inProg := 0, 0
		for _, t := range todos {
			if tm, ok := t.(map[string]any); ok {
				switch tm["status"] {
				case "completed":
					completed++
				case "in_progress":
					inProg++
				}
			}
		}
		if completed == n {
			return fmt.Sprintf("%d todos — all done", n)
		}
		var parts []string
		if inProg > 0 {
			parts = append(parts, fmt.Sprintf("%d in progress", inProg))
		}
		if pending := n - completed - inProg; pending > 0 {
			parts = append(parts, fmt.Sprintf("%d pending", pending))
		}
		if completed > 0 {
			parts = append(parts, fmt.Sprintf("%d done", completed))
		}
		return fmt.Sprintf("%d todos — %s", n, strings.Join(parts, ", "))
	case "TodoRead":
		return ""
	}
	// generic: first 2 key=value pairs
	var pairs []string
	for k, v := range inp {
		pairs = append(pairs, fmt.Sprintf("%s=%v", k, v))
		if len(pairs) >= 2 {
			break
		}
	}
	return strings.Join(pairs, "  ")
}

func safeColor(raw string) string {
	if len(raw) >= 4 && raw[0] == '#' {
		var r, g, b int
		if len(raw) == 7 {
			fmt.Sscanf(raw[1:], "%02x%02x%02x", &r, &g, &b)
		} else if len(raw) == 4 {
			fmt.Sscanf(raw[1:], "%1x%1x%1x", &r, &g, &b)
			r, g, b = r*17, g*17, b*17
		}
		return ansiRGB(r, g, b)
	}
	return colorPrimary
}

// ── Directory tree renderer ───────────────────────────────────────────────────

var reDirCounts = regexp.MustCompile(`(\d+) files?, (\d+) dirs?`)

type dirEntry struct {
	depth int
	name  string
	meta  string
	isDir bool
}

// renderDirTree converts list_directory raw text output into tree-style lines,
// matching Python's _render_dir_tree exactly.
func renderDirTree(output string, maxLines int) []string {
	if maxLines <= 0 {
		maxLines = 28
	}
	rawLines := strings.Split(output, "\n")
	var entries []dirEntry

	for _, line := range rawLines {
		stripped := strings.TrimSpace(line)
		if stripped == "" {
			continue
		}
		if strings.HasPrefix(stripped, "Path:") ||
			strings.HasPrefix(stripped, "(") ||
			strings.HasPrefix(stripped, "Excluded:") {
			continue
		}
		leading := len(line) - len(strings.TrimLeft(line, " \t"))
		depth := leading / 2

		// Split name from metadata on 2+ spaces
		parts := strings.SplitN(stripped, "  ", 2)
		name := parts[0]
		meta := ""
		if len(parts) > 1 {
			meta = strings.TrimSpace(parts[1])
		}
		entries = append(entries, dirEntry{
			depth: depth,
			name:  name,
			meta:  meta,
			isDir: strings.HasSuffix(name, "/"),
		})
	}

	if len(entries) == 0 {
		return nil
	}

	n := len(entries)
	var result []string

	limit := n
	if limit > maxLines {
		limit = maxLines
	}

	for i, e := range entries[:limit] {
		// Build │/space prefix for each ancestor depth
		var prefixParts []string
		for d := 0; d < e.depth; d++ {
			hasContinuation := false
			for j := i + 1; j < n; j++ {
				if entries[j].depth < d {
					break
				}
				if entries[j].depth == d {
					hasContinuation = true
					break
				}
			}
			if hasContinuation {
				prefixParts = append(prefixParts, "│   ")
			} else {
				prefixParts = append(prefixParts, "    ")
			}
		}

		// Connector
		isLast := true
		for j := i + 1; j < n; j++ {
			if entries[j].depth < e.depth {
				break
			}
			if entries[j].depth == e.depth {
				isLast = false
				break
			}
		}
		connector := "└── "
		if !isLast {
			connector = "├── "
		}
		prefix := strings.Join(prefixParts, "") + connector

		var sb strings.Builder
		sb.WriteString("    ") // nested one level under the tool-call header at column 2
		sb.WriteString(ansiDim + prefix + ansiReset)

		if e.isDir {
			sb.WriteString(ansiBold + e.name + ansiReset)
			if m := reDirCounts.FindStringSubmatch(e.meta); m != nil {
				nf, _ := strconv.Atoi(m[1])
				nd, _ := strconv.Atoi(m[2])
				var hint []string
				if nf > 0 {
					hint = append(hint, fmt.Sprintf("%df", nf))
				}
				if nd > 0 {
					hint = append(hint, fmt.Sprintf("%dd", nd))
				}
				if len(hint) > 0 {
					sb.WriteString(ansiDim + "  " + strings.Join(hint, "/") + ansiReset)
				}
			}
		} else {
			sb.WriteString(e.name)
			var info []string
			if label := fileTypeLabel(e.name); label != "" {
				info = append(info, label)
			}
			if e.meta != "" {
				info = append(info, e.meta)
			}
			if len(info) > 0 {
				sb.WriteString(ansiDim + "  " + strings.Join(info, " · ") + ansiReset)
			}
		}
		result = append(result, sb.String())
	}

	overflow := n - maxLines
	if overflow > 0 {
		noun := "entries"
		if overflow == 1 {
			noun = "entry"
		}
		result = append(result, fmt.Sprintf("    %s  … %d more %s%s",
			ansiDim, overflow, noun, ansiReset))
	}

	return result
}

// ── StreamRenderer ────────────────────────────────────────────────────────────

type StreamRenderer struct {
	mu            sync.Mutex
	textOpen      bool
	textBuf       strings.Builder // accumulates assistant tokens for markdown rendering
	thinkingShown bool
	diffShown     bool
	lastTool      string
	lastToolInp   map[string]any
	subAgents     map[string]map[string]any
	loader        *gradientLoader
	workspace     string
	sessionID     string
}

func NewStreamRenderer() *StreamRenderer {
	return &StreamRenderer{
		subAgents: make(map[string]map[string]any),
		loader:    newGradientLoader(),
	}
}

func (r *StreamRenderer) SetSession(workspace, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workspace = workspace
	r.sessionID = sessionID
}

func (r *StreamRenderer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.textOpen = false
	r.textBuf.Reset()
	r.thinkingShown = false
	r.diffShown = false
	r.lastTool = ""
	r.lastToolInp = nil
	r.subAgents = make(map[string]map[string]any)
}

// StartLoader begins the turn's status line. effortLabel (e.g. "thinking
// 32k") is shown in the hint parenthetical when non-empty.
func (r *StreamRenderer) StartLoader(effortLabel string) { r.loader.start(effortLabel) }
func (r *StreamRenderer) StopLoader()                    { r.loader.stop() }

func (r *StreamRenderer) HandleEvent(m map[string]any) {
	eventType, _ := m["type"].(string)

	r.mu.Lock()
	defer r.mu.Unlock()

	switch eventType {
	case "token":
		chunk, _ := m["content"].(string)
		if chunk == "" {
			return
		}
		// Buffer the message text; it is rendered as a formatted markdown block
		// at the next segment boundary. The loader keeps spinning meanwhile to
		// signal that the assistant is still writing.
		r.textBuf.WriteString(chunk)
		r.textOpen = true

	case "thinking":
		if !r.thinkingShown {
			r.loader.pause()
			fmt.Printf("  %s%s◌ thinking…%s\n", ansiDim, ansiItalic, ansiReset)
			r.thinkingShown = true
		}

	case "tool_call":
		r.loader.pause()
		r.flushText()
		tool, _ := m["tool"].(string)
		inp, _ := m["toolInput"].(map[string]any)
		if inp == nil {
			inp = map[string]any{}
		}
		// Sub-agent tool calls: indented, dimmed, label padded to 8 chars
		if subID, ok := m["subSessionId"].(string); ok && subID != "" {
			sa := r.subAgents[subID]
			c := colorPrimary
			if sa != nil {
				if v, ok := sa["color"].(string); ok && v != "" {
					c = v
				}
			}
			label := fmt.Sprintf("%-8s", toolDisplayName(tool))
			detail := toolDetail(tool, inp)
			if len(detail) > 80 {
				detail = detail[:80]
			}
			if detail != "" {
				fmt.Printf("    %s%s%s%s%s%s%s\n",
					ansiDim, c, label, ansiReset,
					ansiDim, detail, ansiReset)
			} else {
				fmt.Printf("    %s%s%s%s\n",
					ansiDim, c, label, ansiReset)
			}
			return
		}
		// The header itself is NOT printed here. This event fires the instant
		// the model finishes generating a tool-call block — for a turn with
		// several parallel tool calls (e.g. many Write calls), all of them
		// fire back-to-back before any of them actually run, which would
		// print every header first and only stream diffs in afterward once
		// dispatch gets around to executing them. Printing is deferred to
		// "tool_use_start" instead, which fires one at a time in true
		// execution order, right before each Dispatch call — so the header
		// lands immediately next to its own file_diff/tool_result.

	case "tool_use_start":
		r.loader.pause()
		tool, _ := m["tool"].(string)
		inp, _ := m["toolInput"].(map[string]any)
		if inp == nil {
			inp = map[string]any{}
		}
		r.lastTool = tool
		r.lastToolInp = inp
		r.diffShown = false
		r.printToolCall(tool, inp)

	case "file_diff":
		r.loader.pause()
		r.flushText()
		r.printFileDiff(m)
		r.diffShown = true
		r.loader.resume()

	case "tool_result":
		r.loader.pause()
		// The file_diff event already rendered what changed for this write/edit —
		// skip the terse "saved" / "Edited …" line so it isn't shown twice.
		if r.diffShown && (r.lastTool == "write_file" || r.lastTool == "edit_file") {
			r.loader.resume()
			return
		}
		// The finish "Done" line already shows the full summary — don't repeat it
		// (truncated) as a "↳ …" result line.
		if r.lastTool == "finish" || r.lastTool == "finish_task" {
			r.loader.resume()
			return
		}
		output, _ := m["toolOutput"].(string)
		success, _ := m["success"].(bool)
		r.printToolResult(output, success)
		r.loader.resume()

	case "status":
		// status events use "status" field not "content"
		msg, _ := m["status"].(string)
		if msg == "" {
			msg, _ = m["content"].(string)
		}
		if msg != "" && msg != "thinking" {
			r.loader.pause()
			r.flushText()
			fmt.Printf("  %s◉  %s%s\n", ansiDim, msg, ansiReset)
			r.loader.resume()
		}

	case "done":
		// No trailing rule here — the composer footer draws one immediately
		// before the next prompt, so this would just double it up.
		r.flushText()
		r.loader.stop()
		r.printTodoChecklist()

	case "cancelled":
		r.flushText()
		r.loader.stop()
		fmt.Printf("\n  %s[cancelled]%s\n", ansiDim, ansiReset)

	case "error", "agent_error":
		r.flushText()
		r.loader.stop()
		msg, _ := m["message"].(string)
		if msg == "" {
			msg, _ = m["content"].(string)
		}
		if msg == "" {
			msg = "unknown error"
		}
		fmt.Printf("\n  %s%s✗  %s%s\n", ansiBold, colorRed, msg, ansiReset)

	case "sub_agent_spawned":
		r.loader.pause()
		r.flushText()
		role, _ := m["role"].(string)
		title, _ := m["taskTitle"].(string)
		color, _ := m["color"].(string)
		subID, _ := m["subSessionId"].(string)
		c := safeColor(color)
		if subID != "" {
			r.subAgents[subID] = map[string]any{"role": role, "color": c, "title": title}
		}
		if len(title) > 70 {
			title = title[:70]
		}
		fmt.Printf("\n  %s%s⟳  %s%s  %s\"%s\"%s\n",
			ansiBold, c, role, ansiReset,
			ansiDim, title, ansiReset)

	case "sub_agent_done":
		r.loader.pause()
		subID, _ := m["subSessionId"].(string)
		sa := r.subAgents[subID]
		role := "agent"
		c := colorPrimary
		if sa != nil {
			if v, ok := sa["role"].(string); ok && v != "" {
				role = v
			}
			if v, ok := sa["color"].(string); ok && v != "" {
				c = v
			}
		}
		status, _ := m["status"].(string)
		icon, style := "✓", ansiDim+colorGreen
		if status != "done" {
			icon, style = "✗", ansiDim+colorRed
		}
		fmt.Printf("  %s%s  %s%s%s  done%s\n", style, icon, c, ansiDim, role, ansiReset)
	}
}

// flushText renders any buffered assistant text as a formatted markdown block
// (a "●" bullet with bold/italic/lists/tables) and clears the buffer. The loader
// is paused around the write so the bottom bar never interleaves with output.
// A blank line brackets the block so it breathes against neighbouring tool calls.
func (r *StreamRenderer) flushText() {
	if r.textBuf.Len() == 0 {
		r.textOpen = false
		return
	}
	text := r.textBuf.String()
	r.textBuf.Reset()
	r.textOpen = false

	block := RenderAssistantMarkdown(text, termWidth())
	if block == "" {
		return
	}
	r.loader.pause()
	fmt.Println()
	fmt.Print(block)
	fmt.Println()
}

// printToolCall matches Python CLIStreamRenderer._print_tool_call exactly.
func (r *StreamRenderer) printToolCall(tool string, inp map[string]any) {
	icon := toolIcon(tool)

	switch tool {
	case "run_shell":
		cmd, _ := inp["command"].(string)
		if len(cmd) > 120 {
			cmd = cmd[:120]
		}
		// "  ▶  Run    $ command"
		fmt.Printf("  %s%s%s  %sRun    %s$ %s%s\n",
			ansiBold, colorPrimary, icon,
			colorPrimary, ansiReset,
			ansiDim, cmd+ansiReset)

	case "read_file", "write_file", "edit_file":
		path, _ := inp["path"].(string)
		label := "Read   "
		switch tool {
		case "write_file":
			label = "Write  "
		case "edit_file":
			label = "Edit   "
		}
		ftype := fileTypeLabel(path)
		// "  ○  Read   path/to/file  [go]"
		if ftype != "" {
			fmt.Printf("  %s%s%s  %s%s%s%s%s  %s[%s]%s\n",
				ansiBold, colorPrimary, icon,
				colorPrimary, label, ansiReset,
				ansiDim, path,
				ansiDim, ftype, ansiReset)
		} else {
			fmt.Printf("  %s%s%s  %s%s%s%s%s%s\n",
				ansiBold, colorPrimary, icon,
				colorPrimary, label, ansiReset,
				ansiDim, path, ansiReset)
		}

	case "list_directory":
		path, _ := inp["path"].(string)
		if path == "" {
			path = "."
		}
		fmt.Printf("  %s%s%s  %sList   %s%s%s\n",
			ansiBold, colorPrimary, icon,
			colorPrimary, ansiReset,
			ansiDim, path+ansiReset)

	case "finish", "finish_task":
		summary, _ := inp["summary"].(string)
		head := fmt.Sprintf("  %s%s%s  %sDone%s",
			ansiBold, ansiDim+colorGreen, icon,
			ansiDim+colorGreen, ansiReset)
		if strings.TrimSpace(summary) == "" {
			fmt.Println(head)
			break
		}
		// Wrap the full summary (no truncation) with a hanging indent so it
		// aligns under the first line's text.
		const indent = "         " // 9 cols: "  ✓  Done" width + a gap
		for i, ln := range wrapPlain(summary, termWidth()-len(indent)-1) {
			if i == 0 {
				fmt.Printf("%s   %s%s%s\n", head, ansiDim, ln, ansiReset)
			} else {
				fmt.Printf("%s%s%s%s\n", indent, ansiDim, ln, ansiReset)
			}
		}

	case "spawn_sub_agent":
		role, _ := inp["role"].(string)
		if role == "" {
			role = "agent"
		}
		title, _ := inp["taskTitle"].(string)
		if title == "" {
			title, _ = inp["task"].(string)
		}
		if len(title) > 60 {
			title = title[:60]
		}
		line := fmt.Sprintf("  %s%s%s  %sSpawn  %s[%s]%s",
			ansiBold, colorPrimary, icon,
			colorPrimary, ansiReset,
			ansiDim, role+ansiReset)
		if title != "" {
			line += "  " + ansiDim + title + ansiReset
		}
		fmt.Println(line)

	case "TodoWrite", "TodoRead":
		detail := toolDetail(tool, inp)
		line := fmt.Sprintf("  %s%s%s  %sTodos  %s",
			ansiBold, colorPrimary, icon,
			colorPrimary, ansiReset)
		if detail != "" {
			line += ansiDim + detail + ansiReset
		}
		fmt.Println(line)

	default:
		label := toolDisplayName(tool)
		detail := toolDetail(tool, inp)
		if len(detail) > 80 {
			detail = detail[:80]
		}
		line := fmt.Sprintf("  %s%s%s  %s%s%s",
			ansiBold, colorPrimary, icon,
			colorPrimary, label, ansiReset)
		if detail != "" {
			line += "  " + ansiDim + detail + ansiReset
		}
		fmt.Println(line)
	}
}

func (r *StreamRenderer) printToolResult(output string, success bool) {
	if output == "" {
		return
	}
	okStyle := ansiDim + colorGreen
	errStyle := ansiDim + colorRed
	icon := "✔"
	style := okStyle
	if !success {
		icon = "✗"
		style = errStyle
	}

	switch r.lastTool {
	case "list_directory":
		for _, line := range renderDirTree(output, 28) {
			fmt.Println(line)
		}

	case "run_shell":
		fmt.Printf("  %s  %s%s\n", style, icon, ansiReset)
		lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
		shown := lines
		if len(shown) > 18 {
			shown = lines[:18]
		}
		for _, line := range shown {
			fmt.Printf("     %s%s%s\n", ansiDim, line, ansiReset)
		}
		if len(lines) > 18 {
			fmt.Printf("     %s… %d more lines%s\n", ansiDim, len(lines)-18, ansiReset)
		}

	case "write_file":
		fmt.Printf("  %s  ↳  %s  saved%s\n", okStyle, icon, ansiReset)

	case "read_file":
		lineCount := len(strings.Split(output, "\n"))
		s := "s"
		if lineCount == 1 {
			s = ""
		}
		fmt.Printf("  %s  ↳  %d line%s%s\n", ansiDim, lineCount, s, ansiReset)

	default:
		preview := strings.ReplaceAll(output, "\n", " ")
		if r := []rune(preview); len(r) > 140 {
			preview = string(r[:140]) + "…"
		}
		fmt.Printf("  %s  ↳  %s%s\n", style, preview, ansiReset)
	}
}

// maxDiffLines caps how many diff lines are printed for a single file change so
// a large write/overwrite doesn't flood the terminal.
const maxDiffLines = 80

// printFileDiff renders a Claude-Code-style colored diff of a write/edit, using
// the structured hunks carried on the "file_diff" event. It is printed directly
// under the tool-call header line for the change.
func (r *StreamRenderer) printFileDiff(m map[string]any) {
	hunks := coerceMaps(m["hunks"])
	if len(hunks) == 0 {
		return
	}
	additions := diffInt(m["additions"])
	removals := diffInt(m["removals"])

	// Gutter width = widest line number across the whole diff.
	maxNo := 0
	for _, h := range hunks {
		for _, ln := range coerceMaps(h["lines"]) {
			if n := diffInt(ln["oldNo"]); n > maxNo {
				maxNo = n
			}
			if n := diffInt(ln["newNo"]); n > maxNo {
				maxNo = n
			}
		}
	}
	gw := len(strconv.Itoa(maxNo))
	if gw < 2 {
		gw = 2
	}

	const indent = "    " // nested one level under the tool-call header at column 2
	shown := 0
	truncated := false
	for hi, h := range hunks {
		if hi > 0 {
			fmt.Printf("%s%s⋯%s\n", indent, ansiDim, ansiReset)
		}
		for _, ln := range coerceMaps(h["lines"]) {
			if shown >= maxDiffLines {
				truncated = true
				break
			}
			kind, _ := ln["kind"].(string)
			text, _ := ln["text"].(string)
			text = strings.ReplaceAll(text, "\t", "  ")
			switch kind {
			case "add":
				num := fmt.Sprintf("%*d", gw, diffInt(ln["newNo"]))
				fmt.Printf("%s%s%s%s %s%s+%s %s%s%s\n",
					indent, ansiDim, num, ansiReset,
					ansiBold, colorGreen, ansiReset,
					colorGreen, text, ansiReset)
			case "del":
				num := fmt.Sprintf("%*d", gw, diffInt(ln["oldNo"]))
				fmt.Printf("%s%s%s%s %s%s-%s %s%s%s\n",
					indent, ansiDim, num, ansiReset,
					ansiBold, colorRed, ansiReset,
					colorRed, text, ansiReset)
			default: // context
				num := fmt.Sprintf("%*d", gw, diffInt(ln["newNo"]))
				fmt.Printf("%s%s%s   %s%s\n",
					indent, ansiDim, num, text, ansiReset)
			}
			shown++
		}
		if truncated {
			break
		}
	}

	// Compact summary: "+N  -M" (dimmed), with an ellipsis when truncated.
	var parts []string
	if additions > 0 {
		parts = append(parts, fmt.Sprintf("%s+%d%s", colorGreen, additions, ansiDim))
	}
	if removals > 0 {
		parts = append(parts, fmt.Sprintf("%s-%d%s", colorRed, removals, ansiDim))
	}
	if truncated {
		parts = append(parts, "… more")
	}
	if len(parts) > 0 {
		fmt.Printf("%s%s%s%s\n", indent, ansiDim, strings.Join(parts, "  "), ansiReset)
	}
}

// diffInt coerces a numeric event field to int, tolerating both the native int
// (in-process events) and float64 (JSON-replayed events) representations.
func diffInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// coerceMaps normalizes an event list field to []map[string]any, tolerating both
// the native []map[string]any and the []any a JSON round-trip produces.
func coerceMaps(v any) []map[string]any {
	switch list := v.(type) {
	case []map[string]any:
		return list
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, e := range list {
			if mm, ok := e.(map[string]any); ok {
				out = append(out, mm)
			}
		}
		return out
	}
	return nil
}

// printTodoChecklist prints the session's todo list at the end of a turn,
// mirroring Python CLIStreamRenderer._print_todo_checklist().
func (r *StreamRenderer) printTodoChecklist() {
	if r.workspace == "" || r.sessionID == "" {
		return
	}
	todos, err := session.GetTodos(r.workspace, r.sessionID)
	if err != nil || len(todos) == 0 {
		return
	}

	// "pending" and unrecognized statuses/priorities fall through to the zero
	// value ("") for these maps — i.e. plain text, no color.
	iconMap := map[string]string{"pending": "○", "in_progress": "◐", "completed": "●"}
	styleMap := map[string]string{
		"in_progress": ansiBold + colorPrimary,
		"completed":   ansiDim + colorGreen,
	}
	prioMap := map[string]string{"high": colorRed, "low": ansiDim}
	order := map[string]int{"in_progress": 0, "pending": 1, "completed": 2}

	sorted := make([]session.Todo, len(todos))
	copy(sorted, todos)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if order[sorted[i].Status] > order[sorted[j].Status] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	fmt.Println()
	fmt.Printf("  %s%sTodos%s\n", colorPrimary, ansiBold, ansiReset)
	for _, t := range sorted {
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
		pLabel := "M"
		if len(t.Priority) > 0 {
			pLabel = strings.ToUpper(t.Priority[:1])
		}
		fmt.Printf("    %s%s%s %s[%s]%s %s%s%s\n",
			style, icon, ansiReset,
			prio, pLabel, ansiReset,
			style, text, ansiReset)
	}

	var inProg, pending int
	for _, t := range sorted {
		if t.Status == "in_progress" {
			inProg++
		} else if t.Status == "pending" {
			pending++
		}
	}
	if inProg+pending > 0 {
		parts := []string{}
		if inProg > 0 {
			parts = append(parts, fmt.Sprintf("%d in progress", inProg))
		}
		if pending > 0 {
			parts = append(parts, fmt.Sprintf("%d pending", pending))
		}
		fmt.Printf("    %s⚠  %s%s\n", ansiDim, strings.Join(parts, ", "), ansiReset)
	}
}

// ── Gradient loader ───────────────────────────────────────────────────────────

var loaderGradient = [][3]int{
	{31, 23, 61},
	{68, 51, 135},
	{123, 92, 245},
	{147, 121, 247},
	{169, 149, 249},
	{147, 121, 247},
	{123, 92, 245},
	{68, 51, 135},
}

// loaderWords are whimsical present-participle status words, cycled while a
// turn is running — no literal tool-action words (so it never implies a
// specific real operation) and no emoji, matching stripEmoji's convention.
var loaderWords = []string{
	"Percolating", "Marinating", "Noodling", "Ruminating", "Cogitating",
	"Simmering", "Brewing", "Pondering", "Mulling", "Conjuring",
	"Divining", "Puzzling", "Wrangling", "Untangling", "Synthesizing",
	"Assembling", "Deliberating", "Contemplating", "Excavating", "Spelunking",
	"Calibrating", "Orchestrating", "Weaving", "Sculpting", "Distilling",
	"Sketching", "Threading", "Charting", "Piecing", "Composing",
}

func randomLoaderWord() string {
	return loaderWords[rand.Intn(len(loaderWords))]
}

// wordRefreshInterval re-rolls the status word for turns that run long enough
// that a single word would otherwise sit there the whole time.
const wordRefreshInterval = 12 * time.Second

// formatLoaderElapsed renders a turn's running time for the status line:
// "12s", "3m 41s", or "1h 05m" once it runs that long.
func formatLoaderElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// activeLoader points at the running loader, for code that can't reach the
// StreamRenderer but must not print over the status line (printAboveLoader).
// Mirrors activeComposer's role for the pinned rows.
var activeLoader atomic.Pointer[gradientLoader]

// printAboveLoader writes s to stdout without colliding with the status line.
//
// The loader parks the cursor mid-line with no trailing newline, so any write
// that skips this path lands ON the status line and its newline commits that
// composite line to scrollback — which is why an unsynchronized writer makes
// the loader appear to repeat once per message. Callers outside the
// HandleEvent branches (which already pause/resume themselves) must route
// through here. Safe when no loader is running: it degrades to a plain print.
func printAboveLoader(s string) {
	l := activeLoader.Load()
	if l == nil {
		fmt.Print(s)
		return
	}
	// pause() is idempotent and clears the line; resume() only lifts the draw
	// block, so a loader the caller found already-paused stays visually clean.
	wasPaused := l.isPaused()
	l.pause()
	fmt.Print(s)
	if !wasPaused {
		l.resume()
	}
}

type gradientLoader struct {
	mu            sync.Mutex
	paused        bool
	lineVisible   bool
	stopCh        chan struct{}
	doneCh        chan struct{}
	running       bool
	startedAt     time.Time // set fresh in start() — anchors the turn's elapsed-time readout
	word          string    // current status word, re-rolled every wordRefreshInterval
	wordChangedAt time.Time
	effortLabel   string // e.g. "thinking 32k" — empty when thinking is off
}

func newGradientLoader() *gradientLoader {
	return &gradientLoader{}
}

func (l *gradientLoader) start(effortLabel string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return
	}
	l.stopCh = make(chan struct{})
	l.doneCh = make(chan struct{})
	l.paused = false
	l.lineVisible = false
	l.running = true
	l.startedAt = time.Now()
	l.word = randomLoaderWord()
	l.wordChangedAt = l.startedAt
	l.effortLabel = effortLabel
	activeLoader.Store(l)
	go l.run()
}

// elapsed returns how long the current (or just-finished) turn has been
// running. startedAt is only reset by the next start(), so reading it right
// after stop() still yields the completed turn's duration.
func (l *gradientLoader) elapsed() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startedAt.IsZero() {
		return 0
	}
	return time.Since(l.startedAt)
}

func (l *gradientLoader) stop() {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return
	}
	l.running = false
	stopCh := l.stopCh
	doneCh := l.doneCh
	l.mu.Unlock()
	close(stopCh)
	<-doneCh // wait for goroutine to clear the line and exit
	// CompareAndSwap, not an unconditional clear: a start() for the next turn
	// may already have claimed the slot.
	activeLoader.CompareAndSwap(l, nil)
}

// pause immediately clears the status line and prevents further draws. Every
// HandleEvent branch calls this before printing, so the line and any other
// output are never visible at the same instant — a plain clear-current-line
// is enough; nothing needs to hunt for the terminal's last row.
//
// Under the pinned composer this is a no-op: the status row sits outside the
// scroll region, so content prints can't collide with it and the loader should
// stay visible and ticking across them rather than blinking out on every tool
// result.
func (l *gradientLoader) pause() {
	if activeComposer.Load().isActive() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.paused {
		return
	}
	l.paused = true
	if l.lineVisible {
		fmt.Print("\r\033[2K")
		l.lineVisible = false
	}
}

func (l *gradientLoader) resume() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = false
}

// isPaused reports whether draws are currently blocked, so printAboveLoader can
// leave an already-paused loader paused instead of resuming it behind the back
// of the HandleEvent branch that paused it.
func (l *gradientLoader) isPaused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.paused
}

func (l *gradientLoader) run() {
	defer close(l.doneCh)
	frame := 0
	ticker := time.NewTicker(70 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopCh:
			l.mu.Lock()
			if l.lineVisible {
				fmt.Print("\r\033[2K")
				l.lineVisible = false
			}
			l.mu.Unlock()
			// Blank the pinned status row so it doesn't strand the last frame
			// between turns. Outside the lock — setStatus takes the composer's.
			activeComposer.Load().setStatus("")
			return
		case <-ticker.C:
			l.mu.Lock()
			if !l.paused {
				now := time.Now()
				if now.Sub(l.wordChangedAt) >= wordRefreshInterval {
					l.word = randomLoaderWord()
					l.wordChangedAt = now
				}

				parts := []string{formatLoaderElapsed(now.Sub(l.startedAt))}
				if l.effortLabel != "" {
					parts = append(parts, l.effortLabel)
				}
				parts = append(parts, "ctrl-c interrupt")
				hint := "(" + strings.Join(parts, " · ") + ")"

				glyphColor := gradientColor((frame / 2) % len(loaderGradient))
				line := fmt.Sprintf("  %s%s✻%s %s%s…%s  %s%s%s",
					ansiBold, glyphColor, ansiReset,
					ansiBold, l.word, ansiReset,
					ansiDim, hint, ansiReset)
				if c := activeComposer.Load(); c.isActive() {
					// Pinned: the status row lives outside the scroll region, so
					// the loader never shares a line with content and needs no
					// carriage-return erase.
					c.setStatus(line)
				} else {
					fmt.Print("\r\033[2K" + line)
					l.lineVisible = true
				}
				frame++
			}
			l.mu.Unlock()
		}
	}
}

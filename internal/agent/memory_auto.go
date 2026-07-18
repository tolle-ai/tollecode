package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/ai"
)

// autoSaveSessionMemory is called in a goroutine after a session turn completes cleanly.
// It reflects on the turn to decide whether it produced a reusable lesson and, if so,
// saves that lesson to the workspace memory store tagged with the turn outcome.
func autoSaveSessionMemory(workspace, userMsg, finalText string, items []map[string]any, provider ai.Provider, model, outcome string) {
	if !isMemoryEnabled(workspace) {
		return
	}

	// Extract changed files from tool items (write_file / edit_file)
	var changedFiles []string
	seen := map[string]bool{}
	for _, item := range items {
		if item["kind"] != "tool" {
			continue
		}
		tu, _ := item["toolUse"].(map[string]any)
		if tu == nil {
			continue
		}
		tool, _ := tu["tool"].(string)
		if tool != "write_file" && tool != "edit_file" {
			continue
		}
		inp, _ := tu["input"].(map[string]any)
		path, _ := inp["path"].(string)
		if path != "" && !seen[path] {
			seen[path] = true
			changedFiles = append(changedFiles, path)
		}
	}

	// Skip turns with no meaningful output (e.g. a clarification-only exchange)
	if finalText == "" && len(changedFiles) == 0 {
		return
	}

	// Reflection gate: ask the model whether this turn produced a durable,
	// reusable lesson. When it does, the lesson becomes the memory summary;
	// when it doesn't, we skip the save entirely so the store stays signal,
	// not a log of every turn.
	lesson, save := reflectOnTurn(userMsg, finalText, changedFiles, outcome, provider, model)
	if !save {
		return
	}

	summary := lesson
	if summary == "" {
		summary = generateMemorySummary(userMsg, finalText, changedFiles, provider, model)
	}

	detail := buildMemoryDetail(userMsg, finalText, changedFiles)

	saveMemoryEntry(workspace, summary, detail, changedFiles, outcome)
}

// reflectOnTurn asks the model whether this turn produced a reusable lesson worth
// remembering for future sessions. It returns the lesson sentence and whether to
// save. On any failure — or when no provider is available — it falls back to the
// prior always-save behaviour (save=true, lesson="") so memory capture degrades
// gracefully rather than going silent.
func reflectOnTurn(userMsg, finalText string, changedFiles []string, outcome string, provider ai.Provider, model string) (lesson string, save bool) {
	if provider == nil {
		return "", true
	}

	var b strings.Builder
	b.WriteString("Task: ")
	b.WriteString(userMsg)
	if finalText != "" {
		b.WriteString("\n\nAgent response:\n")
		b.WriteString(truncate(finalText, 2000))
	}
	if len(changedFiles) > 0 {
		b.WriteString("\n\nFiles modified: ")
		b.WriteString(strings.Join(changedFiles, ", "))
	}
	if outcome != "" {
		b.WriteString("\n\nTurn outcome: ")
		b.WriteString(outcome)
	}

	req := ai.StreamRequest{
		Model: model,
		System: "You decide whether a software development turn produced a durable, reusable lesson worth remembering for future sessions — " +
			"an architectural decision, a standing user preference, a non-obvious gotcha, or a pattern that worked. " +
			"Routine, one-off, or trivial actions are NOT worth remembering.",
		Messages: []ai.ChatMessage{{
			Role: "user",
			Content: "If there is a reusable lesson, reply with a single sentence stating it (max 25 words, start with a verb). " +
				"If there is nothing worth remembering, reply with exactly: SKIP\n\n" + b.String(),
		}},
		MaxTokens: 100,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := provider.Stream(ctx, req)
	if err != nil {
		return "", true
	}

	var sb strings.Builder
	for ev := range ch {
		if ev.Type == "token" {
			sb.WriteString(ev.Text)
		}
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", true
	}
	if strings.HasPrefix(strings.ToUpper(out), "SKIP") {
		return "", false
	}
	return out, true
}

// generateMemorySummary calls the LLM for a one-sentence summary of what was accomplished.
// Falls back to a truncated version of the user message if the LLM call fails.
func generateMemorySummary(userMsg, finalText string, changedFiles []string, provider ai.Provider, model string) string {
	if provider == nil {
		return truncate(userMsg, 120)
	}

	var promptBuf strings.Builder
	promptBuf.WriteString("Task: ")
	promptBuf.WriteString(userMsg)
	if finalText != "" {
		promptBuf.WriteString("\n\nAgent response:\n")
		promptBuf.WriteString(truncate(finalText, 2000))
	}
	if len(changedFiles) > 0 {
		promptBuf.WriteString("\n\nFiles modified: ")
		promptBuf.WriteString(strings.Join(changedFiles, ", "))
	}

	req := ai.StreamRequest{
		Model:  model,
		System: "You are a concise technical writer summarising a software development session for a memory log.",
		Messages: []ai.ChatMessage{{
			Role: "user",
			Content: "Write a single sentence (max 20 words) describing what was accomplished. " +
				"Start with a past-tense verb. No preamble, no punctuation at the end.\n\n" +
				promptBuf.String(),
		}},
		MaxTokens: 80,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := provider.Stream(ctx, req)
	if err != nil {
		return truncate(userMsg, 120)
	}

	var sb strings.Builder
	for ev := range ch {
		if ev.Type == "token" {
			sb.WriteString(ev.Text)
		}
	}
	if s := strings.TrimSpace(sb.String()); s != "" {
		return s
	}
	return truncate(userMsg, 120)
}

// buildMemoryDetail formats the rich markdown detail section stored in the .md file.
func buildMemoryDetail(userMsg, finalText string, changedFiles []string) string {
	var b strings.Builder
	b.WriteString("**Task:** ")
	b.WriteString(userMsg)
	b.WriteString("\n")

	if len(changedFiles) > 0 {
		b.WriteString("\n**Files changed:**\n")
		for _, f := range changedFiles {
			b.WriteString("- `")
			b.WriteString(f)
			b.WriteString("`\n")
		}
	}

	if finalText != "" {
		b.WriteString("\n**Agent response:**\n")
		b.WriteString(truncate(finalText, 600))
	}

	return strings.TrimSpace(b.String())
}

// saveMemoryEntry writes one memory entry (.md file + index.jsonl record).
// keywords is used to populate the index record; for session auto-saves we use changed file paths.
// outcome tags the turn result (e.g. "completed") so recall can weight lessons that worked.
func saveMemoryEntry(workspace, summary, detail string, keywords []string, outcome string) {
	dir := filepath.Join(workspace, ".agent", "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	now := time.Now().UTC()
	ts := now.Format(time.RFC3339Nano)
	filename := now.Format("2006-01-02T15-04-05_000Z") + ".md"

	kws := keywords
	if kws == nil {
		kws = []string{}
	}

	kwLine := strings.Join(kws, ", ")
	content := "## " + ts + " | summary: " + summary + "\nkeywords: " + kwLine + "\n---\n" + detail + "\n"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		return
	}

	type rec struct {
		File      string   `json:"file"`
		Summary   string   `json:"summary"`
		Keywords  []string `json:"keywords"`
		Timestamp string   `json:"timestamp"`
		Outcome   string   `json:"outcome,omitempty"`
	}
	r := rec{File: filename, Summary: summary, Keywords: kws, Timestamp: ts, Outcome: outcome}

	f, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(r)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

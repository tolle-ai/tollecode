package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeMemory creates one memory .md file + index.jsonl line in the workspace's
// memory dir and returns the generated filename.
func writeMemory(t *testing.T, dir, summary, keywords, detail, outcome string, ts time.Time) string {
	t.Helper()
	stamp := ts.UTC().Format(time.RFC3339Nano)
	file := ts.UTC().Format("2006-01-02T15-04-05_000000000Z") + ".md"
	body := "## " + stamp + " | summary: " + summary + "\nkeywords: " + keywords + "\n---\n" + detail + "\n"
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	kwLine := ""
	if keywords != "" {
		parts := strings.Split(keywords, ", ")
		for i, p := range parts {
			if i > 0 {
				kwLine += ", "
			}
			kwLine += `"` + p + `"`
		}
	}
	rec := `{"file":"` + file + `","summary":"` + summary + `","keywords":[` + kwLine + `],"timestamp":"` + stamp + `","outcome":"` + outcome + `"}` + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(rec); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return file
}

// newMemoryWorkspace returns a temp workspace with memory enabled and the memory
// dir created.
func newMemoryWorkspace(t *testing.T) (ws, memDir string) {
	t.Helper()
	ws = t.TempDir()
	memDir = filepath.Join(ws, ".agent", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "config.json"), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, memDir
}

func TestRecallMemory_DisabledOrEmpty(t *testing.T) {
	// Memory not enabled → empty.
	ws := t.TempDir()
	if got := RecallMemory(ws, "anything about parsing", 5); got != "" {
		t.Fatalf("expected empty for disabled memory, got %q", got)
	}

	// Enabled but no records → empty.
	ws2, _ := newMemoryWorkspace(t)
	if got := RecallMemory(ws2, "anything about parsing", 5); got != "" {
		t.Fatalf("expected empty for empty store, got %q", got)
	}
}

func TestRecallMemory_RanksRelevantAndExcludesIrrelevant(t *testing.T) {
	ws, dir := newMemoryWorkspace(t)
	now := time.Now().UTC()

	writeMemory(t, dir, "Refactored the invoice parser to stream tokens",
		"parser, invoice", "Changed parser.go to use a streaming tokenizer.", "completed", now)
	writeMemory(t, dir, "Updated the marketing homepage copy",
		"homepage, copy", "Edited index.html hero text.", "completed", now)

	out := RecallMemory(ws, "fix a bug in the invoice parser", 5)
	if out == "" {
		t.Fatal("expected a recall block, got empty")
	}
	if !strings.Contains(out, "invoice parser") {
		t.Fatalf("expected relevant memory in recall, got:\n%s", out)
	}
	if strings.Contains(out, "marketing homepage") {
		t.Fatalf("irrelevant memory should not be recalled, got:\n%s", out)
	}
	if !strings.Contains(out, "## Learned context") {
		t.Fatalf("expected the Learned context header, got:\n%s", out)
	}
}

func TestRecallMemory_HonorsDisabledSet(t *testing.T) {
	ws, dir := newMemoryWorkspace(t)
	now := time.Now().UTC()
	file := writeMemory(t, dir, "Discovered the auth token must be lowercased",
		"auth, token", "Tokens are compared case-sensitively on the server.", "completed", now)

	// Sanity: recalled before disabling.
	if out := RecallMemory(ws, "auth token handling", 5); !strings.Contains(out, "auth token") {
		t.Fatalf("expected memory recalled before disabling, got:\n%s", out)
	}

	// Disable it → excluded.
	if err := os.WriteFile(filepath.Join(dir, "disabled.json"), []byte(`["`+file+`"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := RecallMemory(ws, "auth token handling", 5); out != "" {
		t.Fatalf("disabled memory should be excluded, got:\n%s", out)
	}
}

func TestRecallMemory_RecencyBreaksTies(t *testing.T) {
	ws, dir := newMemoryWorkspace(t)
	now := time.Now().UTC()

	// Two equally-relevant memories; the newer one should rank first.
	writeMemory(t, dir, "Chose Postgres for the cache layer",
		"cache, postgres", "old decision", "completed", now.Add(-60*24*time.Hour))
	writeMemory(t, dir, "Chose Redis for the cache layer",
		"cache, redis", "new decision", "completed", now)

	out := RecallMemory(ws, "what did we pick for the cache layer", 5)
	iRedis := strings.Index(out, "Redis")
	iPostgres := strings.Index(out, "Postgres")
	if iRedis < 0 || iPostgres < 0 {
		t.Fatalf("expected both cache memories recalled, got:\n%s", out)
	}
	if iRedis > iPostgres {
		t.Fatalf("newer memory should rank first, got:\n%s", out)
	}
}

func TestTokenize_DropsStopwordsAndShort(t *testing.T) {
	got := tokenize("How do we fix the invoice parser?")
	for _, want := range []string{"fix", "invoice", "parser"} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected token %q, missing from %v", want, got)
		}
	}
	for _, drop := range []string{"how", "do", "we", "the"} {
		if _, ok := got[drop]; ok {
			t.Errorf("stopword/short token %q should be dropped, got %v", drop, got)
		}
	}
}

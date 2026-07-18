package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tolle-ai/tollecode/internal/ai"
)

// captureStdout is shared with markdown_stream_test.go.

// ── /help ─────────────────────────────────────────────────────────────────────

// Every command the dispatcher handles must be discoverable from /help.
func TestPrintHelpListsAllDispatchedCommands(t *testing.T) {
	out := captureStdout(t, PrintHelp)

	wantCommands := []string{
		"/help", "/clear", "/exit", "/new", "/sessions", "/session",
		"/configure", "/settings", "/provider", "/model", "/mode", "/thinking",
		"/memory", "/screen", "/agent", "/agents", "/teams",
		"/skills", "/skill", "/todo", "/usage",
	}
	for _, cmd := range wantCommands {
		if !strings.Contains(out, cmd) {
			t.Errorf("help output missing %q", cmd)
		}
	}
	// Memory subcommands documented in help.
	for _, sub := range []string{"/memory on|off", "/memory list", "/memory view <n>",
		"/memory delete <n>", "/memory search <query>", "/memory stats"} {
		if !strings.Contains(out, sub) {
			t.Errorf("help output missing memory row %q", sub)
		}
	}
	// The @ file picker and % agent picker triggers.
	for _, trigger := range []string{"@query", "%query"} {
		if !strings.Contains(out, trigger) {
			t.Errorf("help output missing picker trigger %q", trigger)
		}
	}
}

// ── dispatcher routing ────────────────────────────────────────────────────────

func TestHandleCommandRouting(t *testing.T) {
	ctx := context.Background()
	r := &TolleREPL{workspace: t.TempDir(), mode: "build", running: true}

	out := captureStdout(t, func() { r.handleCommand(ctx, "/mode plan") })
	if r.mode != "plan" || !strings.Contains(out, "PLAN") {
		t.Errorf("/mode plan: mode=%q out=%q", r.mode, out)
	}

	out = captureStdout(t, func() { r.handleCommand(ctx, "/mode bogus") })
	if !strings.Contains(out, "Unknown mode") {
		t.Errorf("/mode bogus should reject: %q", out)
	}

	out = captureStdout(t, func() { r.handleCommand(ctx, "/thinking 4k") })
	if r.thinkingBudget != 4096 {
		t.Errorf("/thinking 4k: budget=%d out=%q", r.thinkingBudget, out)
	}

	out = captureStdout(t, func() { r.handleCommand(ctx, "/nonsense") })
	if !strings.Contains(out, "Unknown command /nonsense") {
		t.Errorf("unknown command not reported: %q", out)
	}

	out = captureStdout(t, func() { r.handleCommand(ctx, "/help") })
	if !strings.Contains(out, "Commands") {
		t.Errorf("/help produced no help output: %q", out)
	}

	out = captureStdout(t, func() { r.handleCommand(ctx, "/todo") })
	if !strings.Contains(out, "No active session") {
		t.Errorf("/todo without session: %q", out)
	}

	out = captureStdout(t, func() { r.handleCommand(ctx, "/screen") })
	if !strings.Contains(out, "Usage: /screen") {
		t.Errorf("/screen without arg should print usage: %q", out)
	}

	out = captureStdout(t, func() { r.handleCommand(ctx, "/usage") })
	if !strings.Contains(out, "No sessions yet") {
		t.Errorf("/usage on empty workspace: %q", out)
	}

	out = captureStdout(t, func() { r.handleCommand(ctx, "/exit") })
	if r.running || !strings.Contains(out, "Bye") {
		t.Errorf("/exit: running=%v out=%q", r.running, out)
	}
}

func TestPickerTriggers(t *testing.T) {
	r := &TolleREPL{}
	cases := []struct {
		text    string
		at, pct bool
	}{
		{"@", true, false},
		{"@Button", true, false},
		{"@a b", false, false}, // spaces → regular message
		{"%", false, true},
		{"%review", false, true},
		{"% team a", false, false},
		{"hello", false, false},
	}
	for _, c := range cases {
		if got := r.isAtPickerTrigger(c.text); got != c.at {
			t.Errorf("isAtPickerTrigger(%q) = %v, want %v", c.text, got, c.at)
		}
		if got := r.isAgentPickerTrigger(c.text); got != c.pct {
			t.Errorf("isAgentPickerTrigger(%q) = %v, want %v", c.text, got, c.pct)
		}
	}
}

// ── @ expansion ───────────────────────────────────────────────────────────────

func TestExpandAtRefs(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sub", "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "my file.txt"), []byte("spaced"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("file ref", func(t *testing.T) {
		msg, resolved := expandAtRefs(ws, "explain @a.txt please")
		if len(resolved) != 1 || resolved[0] != "a.txt" {
			t.Fatalf("resolved = %v", resolved)
		}
		if !strings.Contains(msg, `<file path="a.txt">`) || !strings.Contains(msg, "alpha") {
			t.Errorf("file content not injected: %q", msg)
		}
	})

	t.Run("dir ref", func(t *testing.T) {
		msg, resolved := expandAtRefs(ws, "list @sub")
		if len(resolved) != 1 || resolved[0] != "sub" {
			t.Fatalf("resolved = %v", resolved)
		}
		if !strings.Contains(msg, `<directory path="sub">`) || !strings.Contains(msg, "b.txt") {
			t.Errorf("dir listing not injected: %q", msg)
		}
	})

	t.Run("nested path ref", func(t *testing.T) {
		_, resolved := expandAtRefs(ws, "see @sub/b.txt")
		if len(resolved) != 1 || resolved[0] != "sub/b.txt" {
			t.Fatalf("resolved = %v", resolved)
		}
	})

	t.Run("quoted path with spaces", func(t *testing.T) {
		msg, resolved := expandAtRefs(ws, `read @"my file.txt" now`)
		if len(resolved) != 1 || resolved[0] != "my file.txt" {
			t.Fatalf("resolved = %v", resolved)
		}
		if !strings.Contains(msg, "spaced") {
			t.Errorf("quoted file content not injected: %q", msg)
		}
	})

	t.Run("unresolved ref left alone", func(t *testing.T) {
		msg, resolved := expandAtRefs(ws, "check @missing.txt")
		if resolved != nil {
			t.Fatalf("resolved = %v, want none", resolved)
		}
		if msg != "check @missing.txt" {
			t.Errorf("message changed: %q", msg)
		}
	})

	t.Run("duplicate refs injected once", func(t *testing.T) {
		msg, resolved := expandAtRefs(ws, "@a.txt and @a.txt")
		if len(resolved) != 1 {
			t.Fatalf("resolved = %v", resolved)
		}
		if strings.Count(msg, `<file path="a.txt">`) != 1 {
			t.Errorf("file injected more than once: %q", msg)
		}
	})
}

// ── /memory ───────────────────────────────────────────────────────────────────

func seedMemory(t *testing.T, ws string) {
	t.Helper()
	saveMemIndex(ws, []memIndexRecord{
		{File: "m1.md", Summary: "Implemented jungle grid layout", Keywords: []string{"grid", "layout"}, Timestamp: "2026-07-15T10:00:00Z"},
		{File: "m2.md", Summary: "Fixed provider config bug", Keywords: []string{"provider", "config"}, Timestamp: "2026-07-16T09:00:00Z"},
	})
	if err := os.WriteFile(filepath.Join(memDir(ws), "m1.md"),
		[]byte("## 2026-07-15 | summary: Implemented jungle grid layout\nkeywords: grid, layout\n---\ndetail body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleMemoryCmd(t *testing.T) {
	ws := t.TempDir()
	seedMemory(t, ws)

	t.Run("status shows all subcommands", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "") })
		for _, want := range []string{"Workspace Memory", "on|off|list|view N|delete N|search <q>|stats"} {
			if !strings.Contains(out, want) {
				t.Errorf("status missing %q: %q", want, out)
			}
		}
	})

	t.Run("on and off", func(t *testing.T) {
		captureStdout(t, func() { handleMemoryCmd(ws, "on") })
		if !isMemoryEnabled(ws) {
			t.Error("memory not enabled after /memory on")
		}
		captureStdout(t, func() { handleMemoryCmd(ws, "off") })
		if isMemoryEnabled(ws) {
			t.Error("memory still enabled after /memory off")
		}
	})

	t.Run("list", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "list") })
		if !strings.Contains(out, "Implemented jungle grid layout") || !strings.Contains(out, "Fixed provider config bug") {
			t.Errorf("list missing entries: %q", out)
		}
	})

	t.Run("view by index", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "view 1") })
		if !strings.Contains(out, "Implemented jungle grid layout") || !strings.Contains(out, "detail body") {
			t.Errorf("view 1 missing content: %q", out)
		}
	})

	t.Run("view non-numeric shows usage", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "view abc") })
		if !strings.Contains(out, "Usage: /memory view <n>") {
			t.Errorf("expected usage hint: %q", out)
		}
	})

	t.Run("delete non-numeric shows usage", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "delete abc") })
		if !strings.Contains(out, "Usage: /memory delete <n>") {
			t.Errorf("expected usage hint: %q", out)
		}
	})

	t.Run("view out of range", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "view 99") })
		if !strings.Contains(out, "not found") {
			t.Errorf("expected not-found: %q", out)
		}
	})

	t.Run("search keyword", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "search provider") })
		if !strings.Contains(out, "Fixed provider config bug") {
			t.Errorf("search missed entry: %q", out)
		}
		if strings.Contains(out, "jungle grid") {
			t.Errorf("search matched unrelated entry: %q", out)
		}
	})

	t.Run("search empty shows usage", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "search") })
		if !strings.Contains(out, "Usage: /memory search <query>") {
			t.Errorf("expected usage hint: %q", out)
		}
	})

	t.Run("free text falls back to search", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "what did we do with the jungle grid") })
		if !strings.Contains(out, "Implemented jungle grid layout") {
			t.Errorf("free-text query found nothing: %q", out)
		}
		if !strings.Contains(out, "Searching memory") {
			t.Errorf("fallback hint missing: %q", out)
		}
	})

	t.Run("free text with no match", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "zzz qqq") })
		if !strings.Contains(out, "No results") {
			t.Errorf("expected no results: %q", out)
		}
	})

	t.Run("stats", func(t *testing.T) {
		out := captureStdout(t, func() { handleMemoryCmd(ws, "stats") })
		for _, want := range []string{"Stats", "Entries", "2"} {
			if !strings.Contains(out, want) {
				t.Errorf("stats missing %q: %q", want, out)
			}
		}
	})
}

// Search results are ranked by how many query words they match.
func TestMemorySearchRanking(t *testing.T) {
	ws := t.TempDir()
	saveMemIndex(ws, []memIndexRecord{
		{File: "a.md", Summary: "grid tweaks", Keywords: []string{"grid"}, Timestamp: "2026-07-14T08:00:00Z"},
		{File: "b.md", Summary: "jungle grid layout work", Keywords: []string{"jungle", "grid", "layout"}, Timestamp: "2026-07-15T08:00:00Z"},
	})
	out := captureStdout(t, func() { runMemorySearch(ws, "jungle grid layout") })
	first := strings.Index(out, "jungle grid layout work")
	second := strings.Index(out, "grid tweaks")
	if first == -1 || second == -1 {
		t.Fatalf("expected both entries in results: %q", out)
	}
	if first > second {
		t.Errorf("stronger match not ranked first: %q", out)
	}
}

// ── /memory natural-language summary ───────────────────────────────────────────

func TestIsMemoryQuery(t *testing.T) {
	cases := map[string]bool{
		"":                              false,
		"list":                          false,
		"on":                            false,
		"view 3":                        false,
		"search grid":                   false,
		"STATS":                         false,
		"what did we do yesterday":      true,
		"summarize this week":           true,
		"grid":                          true,
	}
	for in, want := range cases {
		if got := isMemoryQuery(in); got != want {
			t.Errorf("isMemoryQuery(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMemoryDateRange(t *testing.T) {
	// Thursday, 2026-07-16 12:00 UTC.
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	day := func(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

	t.Run("today", func(t *testing.T) {
		s, e, label, ok := memoryDateRange("what did we do today", now)
		if !ok || !s.Equal(day(2026, 7, 16)) || !e.Equal(day(2026, 7, 17)) || label != "today" {
			t.Errorf("got s=%v e=%v label=%q ok=%v", s, e, label, ok)
		}
	})

	t.Run("yesterday and today unioned and ordered", func(t *testing.T) {
		s, e, label, ok := memoryDateRange("what did we do yesterday and today", now)
		if !ok || !s.Equal(day(2026, 7, 15)) || !e.Equal(day(2026, 7, 17)) {
			t.Errorf("got s=%v e=%v ok=%v", s, e, ok)
		}
		if label != "yesterday and today" {
			t.Errorf("label = %q, want %q", label, "yesterday and today")
		}
	})

	t.Run("last N days", func(t *testing.T) {
		s, e, label, ok := memoryDateRange("summary of the last 3 days", now)
		if !ok || !s.Equal(day(2026, 7, 14)) || !e.Equal(day(2026, 7, 17)) || label != "the last 3 days" {
			t.Errorf("got s=%v e=%v label=%q ok=%v", s, e, label, ok)
		}
	})

	t.Run("iso date", func(t *testing.T) {
		s, e, label, ok := memoryDateRange("what happened on 2026-07-10", now)
		if !ok || !s.Equal(day(2026, 7, 10)) || !e.Equal(day(2026, 7, 11)) || label != "Jul 10, 2026" {
			t.Errorf("got s=%v e=%v label=%q ok=%v", s, e, label, ok)
		}
	})

	t.Run("this week is bounded and ok", func(t *testing.T) {
		s, e, _, ok := memoryDateRange("this week's progress", now)
		if !ok || s.After(now) || e.Before(now) {
			t.Errorf("got s=%v e=%v ok=%v", s, e, ok)
		}
	})

	t.Run("no date phrase", func(t *testing.T) {
		_, _, label, ok := memoryDateRange("what have we been building", now)
		if ok || label != "recently" {
			t.Errorf("expected no match, got label=%q ok=%v", label, ok)
		}
	})
}

func TestSelectMemories(t *testing.T) {
	ws := t.TempDir()
	recs := []memIndexRecord{
		{File: "a.md", Summary: "old work", Timestamp: "2026-07-10T09:00:00Z"},
		{File: "b.md", Summary: "yesterday work", Timestamp: "2026-07-15T09:00:00Z"},
		{File: "c.md", Summary: "today work", Timestamp: "2026-07-16T09:00:00Z"},
		{File: "d.md", Summary: "bad timestamp", Timestamp: "not-a-date"},
	}
	start := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)

	t.Run("matched range filters to window", func(t *testing.T) {
		got := selectMemories(ws, recs, start, end, true, time.UTC)
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2: %+v", len(got), got)
		}
		if got[0].Summary != "yesterday work" || got[1].Summary != "today work" {
			t.Errorf("wrong entries: %+v", got)
		}
	})

	t.Run("unmatched includes all incl. undated", func(t *testing.T) {
		got := selectMemories(ws, recs, time.Time{}, time.Time{}, false, time.UTC)
		if len(got) != 4 {
			t.Errorf("got %d entries, want 4", len(got))
		}
	})
}

func TestBuildMemorySummaryPrompt(t *testing.T) {
	entries := []memEntry{
		{When: time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC), Summary: "Wired the jungle grid", Detail: "used a canvas layout"},
		{When: time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC), Summary: "Fixed provider config", Detail: ""},
	}
	system, user := buildMemorySummaryPrompt("what did we do", "yesterday and today", entries)
	if !strings.Contains(strings.ToLower(system), "status update") {
		t.Errorf("system prompt missing persona: %q", system)
	}
	for _, want := range []string{"what did we do", "yesterday and today", "Wired the jungle grid", "Fixed provider config", "used a canvas layout"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q:\n%s", want, user)
		}
	}
}

// stubProvider returns a canned response one token at a time.
type stubProvider struct {
	reply string
	err   error
	got   ai.StreamRequest
}

func (s *stubProvider) DiscoverModels(context.Context) ([]ai.ModelInfo, error) { return nil, nil }
func (s *stubProvider) Stream(_ context.Context, req ai.StreamRequest) (<-chan ai.StreamEvent, error) {
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan ai.StreamEvent)
	go func() {
		defer close(ch)
		for _, w := range strings.Fields(s.reply) {
			ch <- ai.StreamEvent{Type: "token", Text: w + " "}
		}
		ch <- ai.StreamEvent{Type: "done"}
	}()
	return ch, nil
}

func TestStreamMemorySummary(t *testing.T) {
	p := &stubProvider{reply: "We shipped the jungle grid and fixed provider config."}
	out, err := streamMemorySummary(context.Background(), p, "test-model", "sys", "user prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "jungle grid") {
		t.Errorf("summary text lost: %q", out)
	}
	if p.got.Model != "test-model" || p.got.System != "sys" {
		t.Errorf("request not passed through: %+v", p.got)
	}
}

// With no reachable provider, a summary request degrades to a dated listing.
func TestSummarizeMemoryOfflineFallback(t *testing.T) {
	ws := t.TempDir()
	seedMemory(t, ws)
	r := &TolleREPL{workspace: ws, providerID: "nonexistent-provider-xyz"}
	out := captureStdout(t, func() {
		r.summarizeMemory(context.Background(), "what did we do recently")
	})
	if !strings.Contains(out, "No model reachable") {
		t.Errorf("expected offline notice: %q", out)
	}
	if !strings.Contains(out, "jungle grid") || !strings.Contains(out, "provider config") {
		t.Errorf("expected entry listing: %q", out)
	}
}

func TestSummarizeMemoryEmptyRange(t *testing.T) {
	ws := t.TempDir()
	seedMemory(t, ws) // entries are 2026-07-15 and 2026-07-16
	r := &TolleREPL{workspace: ws, providerID: "nonexistent-provider-xyz"}
	out := captureStdout(t, func() {
		// A date with no entries in it.
		r.summarizeMemory(context.Background(), "what did we do on 2020-01-01")
	})
	if !strings.Contains(out, "No memory entries recorded") {
		t.Errorf("expected empty-range notice: %q", out)
	}
}

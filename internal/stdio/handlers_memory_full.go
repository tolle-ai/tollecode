package stdio

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Memory layout (Python sidecar format):
//   <ws>/.agent/memory/
//     config.json            — {"enabled": true/false}
//     index.jsonl            — one index record per line (fast list)
//     <TIMESTAMP>.md         — full detail for each memory entry
//
// index.jsonl record: {"file":"TIMESTAMP.md","summary":"...","keywords":[...],"timestamp":"..."}
//
// .md file format:
//   ## TIMESTAMP | summary: SUMMARY
//   keywords: kw1, kw2, kw3
//   ---
//   DETAIL_TEXT

type indexRecord struct {
	File      string   `json:"file"`
	Summary   string   `json:"summary"`
	Keywords  []string `json:"keywords"`
	Timestamp string   `json:"timestamp"`
}

func memDir(ws string) string         { return filepath.Join(ws, ".agent", "memory") }
func memConfigPath(ws string) string  { return filepath.Join(memDir(ws), "config.json") }
func memIndexPath(ws string) string   { return filepath.Join(memDir(ws), "index.jsonl") }

// ── config ────────────────────────────────────────────────────────────────────

func isMemoryEnabled(ws string) bool {
	data, err := os.ReadFile(memConfigPath(ws))
	if err != nil {
		return false
	}
	var cfg map[string]any
	_ = json.Unmarshal(data, &cfg)
	v, _ := cfg["enabled"].(bool)
	return v
}

func setMemoryEnabled(ws string, enabled bool) {
	_ = os.MkdirAll(memDir(ws), 0o755)
	data, _ := json.Marshal(map[string]any{"enabled": enabled})
	_ = os.WriteFile(memConfigPath(ws), data, 0o644)
}

// ── index ─────────────────────────────────────────────────────────────────────

func loadIndex(ws string) []indexRecord {
	data, err := os.ReadFile(memIndexPath(ws))
	if err != nil {
		return nil
	}
	var records []indexRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r indexRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			records = append(records, r)
		}
	}
	return records
}

func saveIndex(ws string, records []indexRecord) {
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

// readDetail opens a .md file and returns the detail section (after "---").
func readDetail(ws, filename string) string {
	path := filepath.Join(memDir(ws), filename)
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	pastHeader := false
	var lines []string
	for sc.Scan() {
		line := sc.Text()
		if !pastHeader {
			if line == "---" {
				pastHeader = true
			}
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func indexToMap(r indexRecord, detail string) map[string]any {
	kw := r.Keywords
	if kw == nil {
		kw = []string{}
	}
	return map[string]any{
		"timestamp": r.Timestamp,
		"summary":   r.Summary,
		"keywords":  kw,
		"detail":    detail,
		"file":      r.File,
	}
}

// ── handlers ──────────────────────────────────────────────────────────────────

func handleGetMemoryFull(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	enabled := isMemoryEnabled(ws)
	records := loadIndex(ws)
	out := make([]map[string]any, 0, len(records))
	for _, r := range records {
		detail := readDetail(ws, r.File)
		out = append(out, indexToMap(r, detail))
	}
	Emit(map[string]any{
		"type":    "memory",
		"enabled": enabled,
		"entries": out,
		"stats":   map[string]any{"count": len(records)},
	})
}

func handleToggleMemoryFull(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	enabled, _ := cmd["enabled"].(bool)
	setMemoryEnabled(ws, enabled)
	Emit(map[string]any{"type": "memory", "enabled": enabled,
		"entries": []any{}, "stats": map[string]any{"count": 0}})
}

func handleAddMemoryFull(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	text, _ := cmd["text"].(string)
	if text == "" {
		return
	}
	_ = os.MkdirAll(memDir(ws), 0o755)

	now := time.Now().UTC()
	ts := now.Format(time.RFC3339Nano)
	// filename matches Python format: 2026-05-25T15-59-30_386Z.md
	filename := now.Format("2006-01-02T15-04-05_000Z") + ".md"

	content := "## " + ts + " | summary: " + text + "\nkeywords: \n---\n"
	_ = os.WriteFile(filepath.Join(memDir(ws), filename), []byte(content), 0o644)

	r := indexRecord{File: filename, Summary: text, Keywords: []string{}, Timestamp: ts}
	f, err := os.OpenFile(memIndexPath(ws), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		enc := json.NewEncoder(f)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(r)
		f.Close()
	}

	handleGetMemoryFull(state, cmd)
}

func handleDeleteMemoryFull(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	idxF, ok := cmd["index"].(float64)
	if !ok {
		handleGetMemoryFull(state, cmd)
		return
	}
	idx := int(idxF)
	records := loadIndex(ws)
	if idx >= 0 && idx < len(records) {
		// Remove the .md file
		_ = os.Remove(filepath.Join(memDir(ws), records[idx].File))
		records = append(records[:idx], records[idx+1:]...)
		saveIndex(ws, records)
	}
	handleGetMemoryFull(state, cmd)
}

func handleMemoryStatus(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	enabled := isMemoryEnabled(ws)
	records := loadIndex(ws)
	Emit(map[string]any{
		"type":    "memory_status",
		"enabled": enabled,
		"stats":   map[string]any{"count": len(records)},
	})
}

// handleMemoryRebuild scans all .md files and rebuilds index.jsonl from them.
func handleMemoryRebuild(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	dir := memDir(ws)
	entries, err := os.ReadDir(dir)
	if err != nil {
		Emit(map[string]any{"type": "memory_rebuilt", "ok": false, "count": 0})
		return
	}
	var records []indexRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		r := parseMDFile(ws, e.Name())
		if r != nil {
			records = append(records, *r)
		}
	}
	saveIndex(ws, records)
	Emit(map[string]any{"type": "memory_rebuilt", "ok": true, "count": len(records)})
}

// parseMDFile parses the header line of a memory .md file into an index record.
func parseMDFile(ws, filename string) *indexRecord {
	path := filepath.Join(memDir(ws), filename)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	r := indexRecord{File: filename, Keywords: []string{}}

	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "## ") {
			// "## TIMESTAMP | summary: SUMMARY_TEXT"
			rest := strings.TrimPrefix(line, "## ")
			if i := strings.Index(rest, " | summary: "); i >= 0 {
				r.Timestamp = strings.TrimSpace(rest[:i])
				r.Summary = strings.TrimSpace(rest[i+len(" | summary: "):])
			} else {
				r.Timestamp = strings.TrimSpace(rest)
			}
			continue
		}
		if strings.HasPrefix(line, "keywords:") {
			raw := strings.TrimPrefix(line, "keywords:")
			for _, kw := range strings.Split(raw, ",") {
				kw = strings.TrimSpace(kw)
				if kw != "" {
					r.Keywords = append(r.Keywords, kw)
				}
			}
			break // keywords is always before ---
		}
	}
	if r.Timestamp == "" {
		return nil
	}
	return &r
}

func handleMemorySearch(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	q, _ := cmd["q"].(string)
	qLower := strings.ToLower(strings.TrimSpace(q))

	records := loadIndex(ws)
	var matched []map[string]any
	for _, r := range records {
		if qLower == "" ||
			strings.Contains(strings.ToLower(r.Summary), qLower) ||
			kwsContain(r.Keywords, qLower) {
			detail := readDetail(ws, r.File)
			matched = append(matched, indexToMap(r, detail))
		}
	}
	if matched == nil {
		matched = []map[string]any{}
	}
	Emit(map[string]any{"type": "memory_search_result", "entries": matched})
}

func kwsContain(kws []string, q string) bool {
	for _, kw := range kws {
		if strings.Contains(strings.ToLower(kw), q) {
			return true
		}
	}
	return false
}

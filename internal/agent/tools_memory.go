package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

func isMemoryEnabled(workspace string) bool {
	data, err := os.ReadFile(filepath.Join(workspace, ".agent", "memory", "config.json"))
	if err != nil {
		return false
	}
	var cfg map[string]any
	_ = json.Unmarshal(data, &cfg)
	v, _ := cfg["enabled"].(bool)
	return v
}

func toolSaveMemory(workspace string, inp map[string]any) string {
	text, _ := inp["text"].(string)
	if text == "" {
		return "Error: 'text' is required."
	}
	if !isMemoryEnabled(workspace) {
		return "Error: memory is not enabled for this workspace."
	}

	dir := filepath.Join(workspace, ".agent", "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "Error creating memory directory: " + err.Error()
	}

	now := time.Now().UTC()
	ts := now.Format(time.RFC3339Nano)
	filename := now.Format("2006-01-02T15-04-05_000Z") + ".md"

	content := "## " + ts + " | summary: " + text + "\nkeywords: \n---\n"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		return "Error writing memory: " + err.Error()
	}

	type rec struct {
		File      string   `json:"file"`
		Summary   string   `json:"summary"`
		Keywords  []string `json:"keywords"`
		Timestamp string   `json:"timestamp"`
	}
	r := rec{File: filename, Summary: text, Keywords: []string{}, Timestamp: ts}
	f, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		enc := json.NewEncoder(f)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(r)
		f.Close()
	}

	return "Memory saved."
}

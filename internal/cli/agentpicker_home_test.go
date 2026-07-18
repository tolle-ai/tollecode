package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAgentPickerCollect_HonorsTolleHome proves the picker reads agents from the
// same data dir as the rest of the system (config.Home() → TOLLECODE_HOME), not a
// hardcoded ~/.tollecode. This is the regression behind "% for agent does nothing"
// in dev mode, where agents live in ~/.tollecode-dev.
func TestAgentPickerCollect_HonorsTolleHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOLLECODE_HOME", dir)

	agents := `[{"id":"a1","name":"Refactorer","role":"cleanup","status":"active"}]`
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(agents), 0o600); err != nil {
		t.Fatal(err)
	}

	entries := agentPickerCollect()
	if len(entries) != 1 {
		t.Fatalf("expected 1 agent from TOLLECODE_HOME, got %d", len(entries))
	}
	if entries[0].name != "Refactorer" || entries[0].kind != "agent" {
		t.Fatalf("unexpected entry: %+v", entries[0])
	}
}

// TestAgentPickerCollect_EmptyWhenNoFiles confirms an empty (but valid) data dir
// yields no entries — the case the REPL now reports explicitly instead of no-oping.
func TestAgentPickerCollect_EmptyWhenNoFiles(t *testing.T) {
	t.Setenv("TOLLECODE_HOME", t.TempDir())
	if entries := agentPickerCollect(); len(entries) != 0 {
		t.Fatalf("expected 0 entries in empty home, got %d", len(entries))
	}
}

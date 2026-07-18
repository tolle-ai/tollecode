package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuditChain_AppendVerifyAndTamperDetection(t *testing.T) {
	ws := t.TempDir()
	sid := "sess-audit-1"
	actor := Actor{UserID: "u1", TenantID: "org1", Label: "tester"}

	// Reset in-memory head so the test is independent of other tests' state.
	path := auditPath(ws, sid)
	auditMu.Lock()
	delete(auditHeads, path)
	auditMu.Unlock()

	if _, err := AppendAudit(ws, sid, actor, "turn", map[string]any{"model": "opus"}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if _, err := AppendAudit(ws, sid, actor, "tool_exec", map[string]any{"tool": "run_shell", "success": true}); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if _, err := AppendAudit(ws, sid, actor, "approval", map[string]any{"allowed": false, "auto": false}); err != nil {
		t.Fatalf("append 3: %v", err)
	}

	// Chain verifies clean.
	n, err := VerifyAudit(ws, sid)
	if err != nil {
		t.Fatalf("verify: unexpected error: %v", err)
	}
	if n != 3 {
		t.Fatalf("verify: got %d records, want 3", n)
	}

	// Tamper: flip a byte in the middle record's payload on disk.
	data, _ := os.ReadFile(path)
	tampered := []byte(replaceFirst(string(data), "run_shell", "rm_rf_all"))
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAudit(ws, sid); err == nil {
		t.Fatal("verify: expected tamper to be detected, got nil error")
	}
}

func TestAuditChain_RecoversHeadAcrossRestart(t *testing.T) {
	ws := t.TempDir()
	sid := "sess-audit-2"
	path := auditPath(ws, sid)

	auditMu.Lock()
	delete(auditHeads, path)
	auditMu.Unlock()

	if _, err := AppendAudit(ws, sid, Actor{}, "turn", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}

	// Simulate a process restart: drop the in-memory head so the next append must
	// recover seq + prevHash from disk to keep the chain intact.
	auditMu.Lock()
	delete(auditHeads, path)
	auditMu.Unlock()

	if _, err := AppendAudit(ws, sid, Actor{}, "turn", map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}

	n, err := VerifyAudit(ws, sid)
	if err != nil {
		t.Fatalf("verify after restart: %v", err)
	}
	if n != 2 {
		t.Fatalf("got %d records, want 2 (chain must survive restart)", n)
	}
}

func TestAuditFile_IsOwnerOnly(t *testing.T) {
	ws := t.TempDir()
	sid := "sess-audit-3"
	auditMu.Lock()
	delete(auditHeads, auditPath(ws, sid))
	auditMu.Unlock()

	if _, err := AppendAudit(ws, sid, Actor{}, "turn", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(ws, auditDir, sid+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("audit file perm = %o, want 600", perm)
	}
}

func replaceFirst(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

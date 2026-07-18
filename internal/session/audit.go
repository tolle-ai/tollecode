package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The audit log is a per-session, append-only, hash-chained record of governance-
// relevant actions (tool executions, approval decisions, per-turn summaries). It is
// kept separate from the session JSONL — which the frontend and legacy Python
// sidecar read — so its schema can evolve independently and stay tamper-evident.
//
// Each record's Hash covers the record's content plus the previous record's Hash,
// so any insertion, deletion, reordering, or edit breaks VerifyAudit from that
// point on. Files are written 0600 (owner-only), unlike the 0644 session files.
const auditDir = ".agent/audit"

// Actor identifies who an audited action is attributed to. Any field may be empty
// on surfaces that do not carry that identity (e.g. the local desktop app).
type Actor struct {
	UserID   string `json:"userId,omitempty"`
	TenantID string `json:"tenantId,omitempty"`
	Label    string `json:"label,omitempty"` // human-readable: agent name, channel, etc.
}

// AuditRecord is one tamper-evident line in a session's audit log.
type AuditRecord struct {
	Seq       int            `json:"seq"`
	Timestamp string         `json:"ts"`
	SessionID string         `json:"sessionId"`
	Actor     Actor          `json:"actor"`
	Type      string         `json:"type"` // tool_exec | approval | turn
	Payload   map[string]any `json:"payload,omitempty"`
	PrevHash  string         `json:"prevHash"`
	Hash      string         `json:"hash"`
}

type auditHead struct {
	seq  int
	hash string
}

var (
	auditMu    sync.Mutex
	auditHeads = map[string]auditHead{} // keyed by audit file path
)

func auditPath(wsPath, sessionID string) string {
	return filepath.Join(wsPath, auditDir, sessionID+".jsonl")
}

// AppendAudit writes one tamper-evident record to the session's append-only audit
// log and returns its hash. It never touches the main session file; callers should
// log a returned error rather than treat it as fatal to the turn.
func AppendAudit(wsPath, sessionID string, actor Actor, eventType string, payload map[string]any) (string, error) {
	path := auditPath(wsPath, sessionID)

	auditMu.Lock()
	defer auditMu.Unlock()

	head, ok := auditHeads[path]
	if !ok {
		head = recoverAuditHead(path) // survive process restarts
	}

	rec := AuditRecord{
		Seq:       head.seq + 1,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		SessionID: sessionID,
		Actor:     actor,
		Type:      eventType,
		Payload:   payload,
		PrevHash:  head.hash,
	}
	rec.Hash = hashAuditRecord(rec)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return "", err
	}

	auditHeads[path] = auditHead{seq: rec.Seq, hash: rec.Hash}
	return rec.Hash, nil
}

// hashAuditRecord computes the chain hash over the record with its Hash field
// cleared. json.Marshal sorts map keys, so the serialization is deterministic.
func hashAuditRecord(rec AuditRecord) string {
	rec.Hash = ""
	b, _ := json.Marshal(rec)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func recoverAuditHead(path string) auditHead {
	data, err := os.ReadFile(path)
	if err != nil {
		return auditHead{}
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var rec AuditRecord
		if json.Unmarshal(line, &rec) == nil {
			return auditHead{seq: rec.Seq, hash: rec.Hash}
		}
	}
	return auditHead{}
}

// VerifyAudit replays a session's audit log and reports whether the hash chain is
// intact. It returns the number of records verified and an error at the first
// broken link (tampering, truncation, or reordering).
func VerifyAudit(wsPath, sessionID string) (int, error) {
	data, err := os.ReadFile(auditPath(wsPath, sessionID))
	if err != nil {
		return 0, err
	}
	prev := ""
	count := 0
	for _, raw := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var rec AuditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return count, fmt.Errorf("record %d: malformed: %w", count+1, err)
		}
		if rec.PrevHash != prev {
			return count, fmt.Errorf("record %d (seq %d): prevHash breaks the chain", count+1, rec.Seq)
		}
		if rec.Hash != hashAuditRecord(rec) {
			return count, fmt.Errorf("record %d (seq %d): hash mismatch — record was tampered", count+1, rec.Seq)
		}
		prev = rec.Hash
		count++
	}
	return count, nil
}

// AuditSummary renders a bounded, single-line summary of a tool input for the
// audit payload. It records enough to trace what ran without copying unbounded
// content (full redaction is handled by the egress guardrail, separately).
func AuditSummary(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

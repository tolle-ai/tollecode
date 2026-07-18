package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tolle-ai/tollecode/internal/config"
)

// ActiveEntry records a session that received a send_message (running or recently finished).
type ActiveEntry struct {
	SessionID     string `json:"sessionId"`
	WorkspacePath string `json:"workspacePath"`
	Initiator     string `json:"initiator"` // "desktop" | "vscode" | "cli"
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	UpdatedAt     string `json:"updatedAt"`
	Status        string `json:"status"` // "running" | "idle" | "cancelled" | "failed"
}

var regMu sync.Mutex

func registryPath() string {
	return filepath.Join(config.Home(), "active_sessions.json")
}

// RegisterSession marks a session as running. Call when send_message starts.
func RegisterSession(sessionID, workspacePath, initiator string) {
	t := time.Now().UTC().Format(time.RFC3339)
	regMu.Lock()
	defer regMu.Unlock()
	entries := readRegistry()
	// Replace any existing entry for this session.
	out := entries[:0]
	for _, e := range entries {
		if e.SessionID != sessionID {
			out = append(out, e)
		}
	}
	out = append(out, ActiveEntry{
		SessionID:     sessionID,
		WorkspacePath: workspacePath,
		Initiator:     initiator,
		PID:           os.Getpid(),
		StartedAt:     t,
		UpdatedAt:     t,
		Status:        "running",
	})
	writeRegistry(out)
}

// UnregisterSession removes a session from the active registry. Call when a turn finishes.
func UnregisterSession(sessionID string) {
	regMu.Lock()
	defer regMu.Unlock()
	entries := readRegistry()
	out := entries[:0]
	for _, e := range entries {
		if e.SessionID != sessionID {
			out = append(out, e)
		}
	}
	writeRegistry(out)
}

// PurgeDead removes entries whose sidecar process is no longer alive and returns
// the purged entries. Call once on startup so stale "running" entries are cleared.
func PurgeDead() []ActiveEntry {
	regMu.Lock()
	defer regMu.Unlock()
	entries := readRegistry()
	var alive, dead []ActiveEntry
	for _, e := range entries {
		if pidAlive(e.PID) {
			alive = append(alive, e)
		} else {
			dead = append(dead, e)
		}
	}
	if len(dead) > 0 {
		writeRegistry(alive)
	}
	return dead
}

// IsRunning reports whether sessionID currently has a live turn — an entry in
// the active registry owned by a still-alive process. The session WS uses it to
// tell a just-connected client (e.g. a second browser opening the session while
// an agent is mid-turn) that events are already in flight, so it streams them
// instead of waiting for a session_reset boundary that fired before it connected.
func IsRunning(sessionID string) bool {
	regMu.Lock()
	defer regMu.Unlock()
	for _, e := range readRegistry() {
		if e.SessionID == sessionID && e.Status == "running" && pidAlive(e.PID) {
			return true
		}
	}
	return false
}

// ListActive returns a copy of all entries currently in the registry.
func ListActive() []ActiveEntry {
	regMu.Lock()
	defer regMu.Unlock()
	src := readRegistry()
	out := make([]ActiveEntry, len(src))
	copy(out, src)
	return out
}


func readRegistry() []ActiveEntry {
	data, err := os.ReadFile(registryPath())
	if err != nil {
		return nil
	}
	var entries []ActiveEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	return entries
}

func writeRegistry(entries []ActiveEntry) {
	if entries == nil {
		entries = []ActiveEntry{}
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(registryPath()), 0o755)
	_ = os.WriteFile(registryPath(), append(data, '\n'), 0o644)
}

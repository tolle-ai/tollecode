// Package alerts provides a global pub/sub bus for agent-generated alerts
// and persists them to disk for reconnection replay.
package alerts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/config"
)

// Alert is emitted when an agent calls the send_alert tool.
type Alert struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	Workspace string `json:"workspace"`
	Message   string `json:"message"`
	SessionID string `json:"sessionId"`
	Offset    int64  `json:"_off,omitempty"`
	Timestamp string `json:"timestamp"`
}

const subBufSize = 64

var Global = newBus()

type bus struct {
	mu          sync.Mutex
	subscribers map[string]chan Alert
}

func newBus() *bus {
	return &bus{subscribers: make(map[string]chan Alert)}
}

// Publish sends an alert to all subscribers and appends it to the persistent log.
func (b *bus) Publish(a Alert) {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.Timestamp == "" {
		a.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	off := appendAlert(a)
	a.Offset = off

	b.mu.Lock()
	for _, ch := range b.subscribers {
		select {
		case ch <- a:
		default: // slow subscriber — drop
		}
	}
	b.mu.Unlock()
}

// Subscribe returns a channel that receives future alerts and an unsubscribe func.
func (b *bus) Subscribe() (chan Alert, func()) {
	id := uuid.NewString()
	ch := make(chan Alert, subBufSize)
	b.mu.Lock()
	b.subscribers[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		close(ch)
		b.mu.Unlock()
	}
}

// ── persistence ───────────────────────────────────────────────────────────────

func alertsPath() string {
	return filepath.Join(config.Home(), "alerts.jsonl")
}

func appendAlert(a Alert) int64 {
	path := alertsPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0
	}
	defer f.Close()
	off, _ := f.Seek(0, 2)
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(a)
	return off
}

// Tail reads alerts from the file starting at fromOffset.
// Returns (alerts, endOffset).
func Tail(fromOffset int64) ([]Alert, int64) {
	path := alertsPath()
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()

	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, 0); err != nil {
			return nil, fromOffset
		}
	}

	var alerts []Alert
	dec := json.NewDecoder(f)
	var endOff int64
	for dec.More() {
		pos, _ := f.Seek(0, 1)
		_ = pos
		var a Alert
		if err := dec.Decode(&a); err != nil {
			break
		}
		alerts = append(alerts, a)
	}
	endOff, _ = f.Seek(0, 1)
	return alerts, endOff
}

// Publish is a package-level convenience wrapper around Global.Publish.
func Publish(agentID, workspace, message, sessionID string) {
	Global.Publish(Alert{
		AgentID:   agentID,
		Workspace: workspace,
		Message:   message,
		SessionID: sessionID,
	})
}

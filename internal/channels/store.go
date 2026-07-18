// Package channels manages the mapping from external chat platforms to agent
// sessions.  Each unique (platform, chatID) pair is bound to a persistent
// session in a specific workspace so conversation history survives restarts.
package channels

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/tolle-ai/tollecode/internal/config"
)

// Binding maps one external chat conversation to a tollecode session.
type Binding struct {
	Platform      string `json:"platform"`       // e.g. "telegram"
	ChatID        string `json:"chatId"`         // platform-specific conversation ID
	SessionID     string `json:"sessionId"`      // tollecode session UUID
	WorkspacePath string `json:"workspacePath"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
}

var (
	mu       sync.RWMutex
	bindings map[string]*Binding // key = platform+":"+chatID
)

func init() {
	bindings = make(map[string]*Binding)
	load()
}

func key(platform, chatID string) string { return platform + ":" + chatID }

// Find returns the binding for the given platform+chatID, or nil if not found.
func Find(platform, chatID string) *Binding {
	mu.RLock()
	defer mu.RUnlock()
	b := bindings[key(platform, chatID)]
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

// Save persists a binding (creates or updates).
func Save(b *Binding) {
	mu.Lock()
	cp := *b
	bindings[key(b.Platform, b.ChatID)] = &cp
	mu.Unlock()
	persist()
}

// ── persistence ───────────────────────────────────────────────────────────────

func storePath() string {
	return filepath.Join(config.Home(), "channel_bindings.json")
}

func load() {
	data, err := os.ReadFile(storePath())
	if err != nil {
		return
	}
	var list []*Binding
	if json.Unmarshal(data, &list) != nil {
		return
	}
	for _, b := range list {
		bindings[key(b.Platform, b.ChatID)] = b
	}
}

func persist() {
	mu.RLock()
	list := make([]*Binding, 0, len(bindings))
	for _, b := range bindings {
		cp := *b
		list = append(list, &cp)
	}
	mu.RUnlock()
	data, _ := json.MarshalIndent(list, "", "  ")
	_ = os.WriteFile(storePath(), data, 0o644)
}

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SidecarSettings holds user-configurable sidecar-level preferences that are
// separate from provider config. Persisted to ~/.tollecode/sidecar_settings.json.
type SidecarSettings struct {
	// MaxToolIterations caps the agentic loop. 0 means use the built-in default.
	MaxToolIterations int `json:"maxToolIterations,omitempty"`
	// ConfirmContinue, when true, pauses the agent near the iteration limit and
	// asks the user whether to continue instead of failing hard.
	ConfirmContinue bool `json:"confirmContinue,omitempty"`
	// ConfirmContinueThreshold is the iteration number at which the pause fires.
	// 0 means 80% of MaxToolIterations (or the built-in default).
	ConfirmContinueThreshold int `json:"confirmContinueThreshold,omitempty"`
	// TaskTimeoutMinutes is the wall-clock deadline for a todo task (single or team).
	// 0 means use the built-in default (120 minutes).
	TaskTimeoutMinutes int `json:"taskTimeoutMinutes,omitempty"`
	// UserMessageDelayMs is the delay (in milliseconds) before a user message sent
	// during a live stream starts a new agent turn. 0 means use the built-in default
	// of 1500 ms. This keeps brief follow-ups from interrupting an active stream.
	UserMessageDelayMs int `json:"userMessageDelayMs,omitempty"`
	// OllamaNumCtx is the context-window size (num_ctx) requested from Ollama. It is
	// the effective runtime context window for Ollama models — the value the UI shows
	// as the limit and the threshold against which auto-compaction fires. 0 means use
	// the built-in default of 32768.
	OllamaNumCtx int `json:"ollamaNumCtx,omitempty"`
	// MaxOutputTokens is the per-response output-token budget requested from every
	// provider (Anthropic max_tokens, OpenAI max_tokens, Ollama num_predict). Each
	// provider still clamps it down to its model's real output ceiling, so a large
	// value is safe. 0 means use the built-in default of 32000.
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	// EgressMode controls the outbound secrets/PII guardrail applied to every LLM
	// request: "off" (no scan), "log" (flag the finding but send unchanged — the
	// default), or "redact" (replace detected secrets/PII with typed placeholders
	// before the request leaves the machine). "" means the default ("log").
	EgressMode string `json:"egressMode,omitempty"`
	// ChannelShellAutoAllow lets chat-channel agents (Slack/Telegram/Discord) run
	// shell commands with no human approval step. It is OFF by default: chat
	// surfaces are unattended and an incoming message can carry injected content,
	// so autonomous shell execution there is high-risk (the injection -> run_shell
	// chain). Operators must opt in explicitly to restore auto-run behavior.
	ChannelShellAutoAllow bool `json:"channelShellAutoAllow,omitempty"`
}

const defaultMaxToolIterations = 100
const defaultUserMessageDelayMs = 1500
const defaultOllamaNumCtx = 32768
const minOllamaNumCtx = 2048
const defaultMaxOutputTokens = 32000
const minMaxOutputTokens = 256

var (
	settingsMu sync.RWMutex
	cachedSettings *SidecarSettings
)

func sidecarSettingsPath() string {
	return filepath.Join(Home(), "sidecar_settings.json")
}

// LoadSidecarSettings reads settings from disk (or returns defaults).
func LoadSidecarSettings() SidecarSettings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := loadSidecarSettingsLocked()
	cachedSettings = &s
	return s
}

func loadSidecarSettingsLocked() SidecarSettings {
	defaults := SidecarSettings{MaxToolIterations: defaultMaxToolIterations}
	data, err := os.ReadFile(sidecarSettingsPath())
	if err != nil {
		return defaults
	}
	var s SidecarSettings
	if json.Unmarshal(data, &s) != nil {
		return defaults
	}
	if s.MaxToolIterations <= 0 {
		s.MaxToolIterations = defaultMaxToolIterations
	}
	return s
}

// GetSidecarSettings returns the cached settings (loaded at startup).
// Falls back to a fresh disk read if not yet initialised.
func GetSidecarSettings() SidecarSettings {
	settingsMu.RLock()
	if cachedSettings != nil {
		s := *cachedSettings
		settingsMu.RUnlock()
		return s
	}
	settingsMu.RUnlock()
	return LoadSidecarSettings()
}

// SaveSidecarSettings persists s and updates the cache.
func SaveSidecarSettings(s SidecarSettings) error {
	if s.MaxToolIterations <= 0 {
		s.MaxToolIterations = defaultMaxToolIterations
	}
	_ = os.MkdirAll(Home(), 0o755)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(sidecarSettingsPath(), append(data, '\n'), 0o644); err != nil {
		return err
	}
	settingsMu.Lock()
	cachedSettings = &s
	settingsMu.Unlock()
	return nil
}

// EffectiveMaxIterations returns the resolved iteration limit (never < 10).
func (s SidecarSettings) EffectiveMaxIterations() int {
	if s.MaxToolIterations > 0 {
		return s.MaxToolIterations
	}
	return defaultMaxToolIterations
}

// EffectiveConfirmThreshold returns the iteration at which the confirm prompt fires.
func (s SidecarSettings) EffectiveConfirmThreshold() int {
	max := s.EffectiveMaxIterations()
	if s.ConfirmContinueThreshold > 0 && s.ConfirmContinueThreshold < max {
		return s.ConfirmContinueThreshold
	}
	// Default: 80% of max, min 5
	t := max * 80 / 100
	if t < 5 {
		t = 5
	}
	return t
}

// EffectiveUserMessageDelay returns the delay before a queued user message during
// a live stream is processed. Defaults to 1500 ms when not configured.
func (s SidecarSettings) EffectiveUserMessageDelay() time.Duration {
	if s.UserMessageDelayMs > 0 {
		return time.Duration(s.UserMessageDelayMs) * time.Millisecond
	}
	return defaultUserMessageDelayMs * time.Millisecond
}

// EffectiveTaskTimeout returns the wall-clock limit for a todo task.
// Defaults to 2 hours when not configured.
func (s SidecarSettings) EffectiveTaskTimeout() time.Duration {
	if s.TaskTimeoutMinutes > 0 {
		return time.Duration(s.TaskTimeoutMinutes) * time.Minute
	}
	return 120 * time.Minute
}

// EffectiveOllamaNumCtx returns the Ollama context-window size to request at
// runtime. Defaults to 32768 and is clamped to a sane floor so a misconfigured
// tiny value can't truncate every conversation.
func (s SidecarSettings) EffectiveOllamaNumCtx() int {
	if s.OllamaNumCtx >= minOllamaNumCtx {
		return s.OllamaNumCtx
	}
	return defaultOllamaNumCtx
}

// EffectiveMaxOutputTokens returns the per-response output-token budget to request
// from providers. Defaults to 32000 and is clamped to a sane floor so a
// misconfigured tiny value can't truncate every turn. Providers cap it to the
// model's real output ceiling, so there is no upper clamp here.
func (s SidecarSettings) EffectiveMaxOutputTokens() int {
	if s.MaxOutputTokens >= minMaxOutputTokens {
		return s.MaxOutputTokens
	}
	return defaultMaxOutputTokens
}

// EffectiveEgressMode returns the outbound-guardrail mode, defaulting to "log"
// (observe-only) and rejecting unknown values.
func (s SidecarSettings) EffectiveEgressMode() string {
	switch s.EgressMode {
	case "off", "log", "redact":
		return s.EgressMode
	default:
		return "log"
	}
}

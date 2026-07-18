package ai

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/tolle-ai/tollecode/internal/config"
)

// ProviderConfig is one entry in ~/.tollecode/config.json.
type ProviderConfig struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Name         string      `json:"name"`
	Enabled      bool        `json:"enabled"`
	APIKey       string      `json:"apiKey"`
	Endpoint     string      `json:"endpoint"`
	Models       []ModelEntry `json:"models"`
	DefaultModel string      `json:"defaultModel"`
}

// ModelEntry is either a string or an object in the models array.
type ModelEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	raw       string // set when the JSON value was a plain string
}

func (m *ModelEntry) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		m.ID = s
		m.Name = s
		m.raw = s
		return nil
	}
	type alias ModelEntry
	return json.Unmarshal(b, (*alias)(m))
}

// Manager loads and caches provider adapters from config.json.
type Manager struct {
	mu       sync.RWMutex
	adapters map[string]Provider
	configs  map[string]ProviderConfig
	// injectedConfigs holds provider configs injected by server-mode YAML
	// (via InjectConfigs). Reload() re-applies these after rebuilding from
	// config.json so YAML providers are never lost.
	injectedConfigs []ProviderConfig
}

var Global = &Manager{}

func init() {
	Global.Reload()
}

// Reload re-reads config.json and rebuilds adapters.
// After rebuilding from disk, it re-applies any previously injected YAML
// configs so that server-mode providers (from tollecode.yaml) survive a Reload.
func (m *Manager) Reload() {
	cfgs := loadConfigs()
	newAdapters := make(map[string]Provider, len(cfgs))
	newConfigs := make(map[string]ProviderConfig, len(cfgs))

	for _, cfg := range cfgs {
		if cfg.ID == "" || !cfg.Enabled {
			continue
		}
		p := buildAdapter(cfg)
		if p == nil {
			continue
		}
		newAdapters[cfg.ID] = p
		newConfigs[cfg.ID] = cfg
	}

	// Re-apply injected (YAML) configs that aren't already in config.json.
	// desktop-configured providers take precedence.
	m.mu.Lock()
	injected := m.injectedConfigs
	m.mu.Unlock()

	for _, cfg := range injected {
		if cfg.ID == "" || !cfg.Enabled {
			continue
		}
		if _, exists := newAdapters[cfg.ID]; exists {
			continue // desktop-configured provider takes precedence
		}
		p := buildAdapter(cfg)
		if p == nil {
			continue
		}
		newAdapters[cfg.ID] = p
		newConfigs[cfg.ID] = cfg
	}

	m.mu.Lock()
	m.adapters = newAdapters
	m.configs = newConfigs
	m.mu.Unlock()
}

// Get returns the adapter for a provider ID, or nil.
func (m *Manager) Get(id string) Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.adapters[id]
}

// IDs returns all enabled provider IDs.
func (m *Manager) IDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.adapters))
	for id := range m.adapters {
		ids = append(ids, id)
	}
	return ids
}

// Config returns the raw config for a provider.
func (m *Manager) Config(id string) (ProviderConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.configs[id]
	return c, ok
}

// DefaultModel returns the default model ID for a provider.
// Priority: first model with IsDefault=true > cfg.DefaultModel > first model
// in the list.  This intentionally prefers an explicit DefaultModel over the
// first entry in the Models array so that YAML `default_model` takes effect
// even when a `models:` list is also present.
func (m *Manager) DefaultModel(id string) string {
	m.mu.RLock()
	cfg, ok := m.configs[id]
	m.mu.RUnlock()
	if !ok {
		return ""
	}
	for _, model := range cfg.Models {
		if model.IsDefault {
			return model.Name
		}
	}
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel
	}
	if len(cfg.Models) > 0 {
		return cfg.Models[0].Name
	}
	return ""
}

// ResolveProviderID resolves a provider string that could be either a literal
// provider ID (e.g. "prov-1780434925223") or a type alias (e.g. "anthropic",
// "ollama-cloud"). If it's a type alias, it returns the best provider ID of
// that type (preferring those with a default model). Returns ("", false) if
// no matching provider is found.
func (m *Manager) ResolveProviderID(providerOrType string) (string, bool) {
	if providerOrType == "" {
		return "", false
	}
	// Try exact ID match first.
	if m.Get(providerOrType) != nil {
		return providerOrType, true
	}
	// Treat as type alias — find the best provider of that type.
	ids := m.IDs()
	sort.Strings(ids)
	best := ""
	bestHasModel := false
	for _, id := range ids {
		cfg, ok := m.Config(id)
		if !ok || cfg.Type != providerOrType {
			continue
		}
		hasModel := m.DefaultModel(id) != ""
		if best == "" || (hasModel && !bestHasModel) {
			best = id
			bestHasModel = hasModel
		}
	}
	if best != "" {
		return best, true
	}
	return "", false
}

// BestProvider returns the best available provider ID and its default model.
// Priority: preferProvider (if set and available) > anthropic > openai >
// ollama-cloud > ollama > custom > other, alphabetical within a tier.
// Within the same tier, providers with a non-empty default model are
// preferred over those without one, so we never pick an Ollama instance
// that has no model configured.
func (m *Manager) BestProvider(preferProvider, preferModel string) (provider, model string) {
	// If a preferred provider is specified, resolve it (accepts both literal
	// provider IDs and type aliases like "anthropic" or "ollama-cloud").
	if preferProvider != "" {
		if resolved, ok := m.ResolveProviderID(preferProvider); ok {
			dm := preferModel
			if dm == "" {
				dm = m.DefaultModel(resolved)
			}
			return resolved, dm
		}
	}

	ids := m.IDs()
	if len(ids) == 0 {
		return "", ""
	}

	// Sort IDs for deterministic iteration order.
	sort.Strings(ids)

	tierOf := func(id string) int {
		cfg, ok := m.Config(id)
		if !ok {
			return 99
		}
		switch cfg.Type {
		case "anthropic":
			return 0
		case "openai":
			return 1
		case "ollama-cloud":
			return 2
		case "ollama":
			return 3
		case "custom":
			return 4
		}
		return 5
	}

	best := ""
	bestTier := 100
	bestHasModel := false
	for _, id := range ids {
		t := tierOf(id)
		dm := m.DefaultModel(id)
		hasModel := dm != ""
		if t < bestTier || (t == bestTier && hasModel && !bestHasModel) {
			bestTier = t
			best = id
			bestHasModel = hasModel
		}
	}
	if best == "" {
		best = ids[0]
	}
	return best, m.DefaultModel(best)
}

// InjectConfigs merges server-mode provider configs into the in-memory maps
// without writing to disk. Used by StartAPI to load providers from tollecode.yaml
// so they are available alongside the user's ~/.tollecode/config.json providers.
// Existing providers with the same ID are NOT overwritten.
// Injected configs are saved so that Reload() can re-apply them after rebuilding
// from config.json — YAML providers survive Reload.
func (m *Manager) InjectConfigs(cfgs []ProviderConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cfg := range cfgs {
		if cfg.ID == "" || !cfg.Enabled {
			continue
		}
		if _, exists := m.adapters[cfg.ID]; exists {
			continue // desktop-configured provider takes precedence
		}
		p := buildAdapter(cfg)
		if p == nil {
			continue
		}
		m.adapters[cfg.ID] = p
		m.configs[cfg.ID] = cfg
		// Track as injected so Reload() re-applies it.
		m.injectedConfigs = append(m.injectedConfigs, cfg)
	}
}

// SyncFromLiteKV reconciles providers between the Lite app's shared KV store
// (~/.tollecode/lite_kv.json → "lite_providers", which the desktop/web UI reads)
// and config.json (which the CLI and agent runtime read), so a provider
// configured on ANY surface — CLI, Lite desktop, Lite web — is visible on all of
// them.
//
// Reconciliation is deliberately ADD-ONLY in both directions: a provider present
// in one store but missing from the other is copied over; an entry already
// present in a store is never overwritten. This is what keeps it safe —
//   - It never clobbers a provider (or its API key) a surface already has.
//   - It never fights the Lite app's own `save_providers` push (which remains the
//     live path for edits Lite makes to config.json), nor a `tollecode configure`
//     edit to config.json.
// Matching is by provider ID. Writes happen only when something is actually
// added, so a normal already-in-sync start touches nothing.
func (m *Manager) SyncFromLiteKV() {
	raw := config.LiteKVGetAll()["lite_providers"]

	// KV entries kept as raw objects too, so frontend-only fields survive a
	// round-trip when we write the KV back.
	var kvRaw []map[string]any
	var kvProviders []ProviderConfig
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &kvRaw)
		_ = json.Unmarshal([]byte(raw), &kvProviders)
	}

	existing := loadConfigs()
	inConfig := make(map[string]bool, len(existing))
	for _, c := range existing {
		if c.ID != "" {
			inConfig[c.ID] = true
		}
	}
	inKV := make(map[string]bool, len(kvProviders))
	for _, c := range kvProviders {
		if c.ID != "" {
			inKV[c.ID] = true
		}
	}

	// Direction 1: KV → config.json. Add providers the KV has but config.json
	// lacks (e.g. a fresh CLI-only machine, or config that only exists in the
	// seeded KV) so the CLI/agent runtime see everything Lite configured.
	merged := existing
	cfgChanged := false
	for _, c := range kvProviders {
		if c.ID != "" && !inConfig[c.ID] {
			merged = append(merged, c)
			inConfig[c.ID] = true
			cfgChanged = true
		}
	}
	if cfgChanged {
		_ = m.SaveConfigs(merged)
	}

	// Direction 2: config.json → KV. Add providers config.json has but the KV
	// lacks (e.g. one added via `tollecode configure`) so they show up in the
	// Lite desktop/web UI, which reads the KV.
	kvChanged := false
	for _, c := range existing {
		if c.ID == "" || inKV[c.ID] {
			continue
		}
		b, err := json.Marshal(c)
		if err != nil {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(b, &obj) != nil {
			continue
		}
		kvRaw = append(kvRaw, obj)
		inKV[c.ID] = true
		kvChanged = true
	}
	if kvChanged {
		if b, err := json.Marshal(kvRaw); err == nil {
			_ = config.LiteKVSet("lite_providers", string(b))
		}
	}
}

// SaveConfigs writes provider configs to disk and reloads.
func (m *Manager) SaveConfigs(cfgs []ProviderConfig) error {
	path := configPath()
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	m.Reload()
	return nil
}

// OllamaAPIKey returns the first Ollama API key found across all providers.
// Prefers ollama-cloud type, falls back to ollama type. Returns "" if none.
func (m *Manager) OllamaAPIKey() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fallback := ""
	for _, cfg := range m.configs {
		if !cfg.Enabled || cfg.APIKey == "" {
			continue
		}
		if cfg.Type == "ollama-cloud" {
			return cfg.APIKey
		}
		if cfg.Type == "ollama" && fallback == "" {
			fallback = cfg.APIKey
		}
	}
	return fallback
}

// ListForUI returns the provider list in the shape the frontend expects.
func (m *Manager) ListForUI() []map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]map[string]any, 0, len(m.configs))
	for _, cfg := range m.configs {
		models := make([]string, 0, len(cfg.Models))
		defModel := ""
		for _, me := range cfg.Models {
			n := me.Name
			if n == "" {
				n = me.ID
			}
			models = append(models, n)
			if me.IsDefault && defModel == "" {
				defModel = n
			}
		}
		// Fall back to the top-level DefaultModel field if no model has IsDefault.
		if defModel == "" && cfg.DefaultModel != "" {
			defModel = cfg.DefaultModel
		}
		if defModel == "" && len(models) > 0 {
			defModel = models[0]
		}
		out = append(out, map[string]any{
			"id":           cfg.ID,
			"name":         cfg.Name,
			"type":         cfg.Type,
			"enabled":      cfg.Enabled,
			"models":       models,
			"defaultModel": defModel,
			"hasKey":       cfg.APIKey != "",
			"endpoint":     cfg.Endpoint,
		})
	}
	return out
}

// ── internal ──────────────────────────────────────────────────────────────────

func configPath() string {
	return config.Home() + "/config.json"
}

// LoadAllConfigs reads all provider configs from disk (including disabled ones).
func LoadAllConfigs() []ProviderConfig { return loadConfigs() }

func loadConfigs() []ProviderConfig {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil
	}
	var cfgs []ProviderConfig
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return nil
	}
	return cfgs
}

// BuildAdapter creates a provider adapter from a config without registering it.
func BuildAdapter(cfg ProviderConfig) Provider { return buildAdapter(cfg) }

// buildAdapter constructs the provider adapter and wraps it in the egress
// guardrail so every outbound request is scanned for secrets/PII regardless of
// which provider is selected. The wrapper is a no-op when EgressPolicy is off.
func buildAdapter(cfg ProviderConfig) Provider {
	inner := buildRawAdapter(cfg)
	if inner == nil {
		return nil
	}
	return &scanningProvider{inner: inner}
}

// ollamaCloudHost is the default base URL for the hosted Ollama API (the native
// /api/* endpoints live under it), used when an ollama-cloud provider has no
// explicit endpoint configured.
const ollamaCloudHost = "https://ollama.com"

// ollamaCloudEndpoint returns the endpoint to use for an ollama-cloud provider,
// falling back to the hosted API so a configured cloud API key is never pointed at
// localhost (which would fail every request).
func ollamaCloudEndpoint(ep string) string {
	if strings.TrimSpace(ep) == "" {
		return ollamaCloudHost
	}
	return ep
}

func buildRawAdapter(cfg ProviderConfig) Provider {
	switch cfg.Type {
	case "anthropic":
		return NewAnthropicProvider(cfg.APIKey)
	case "openai":
		return NewOpenAIProvider(cfg.APIKey, cfg.Endpoint)
	case "ollama":
		return NewOllamaProvider(cfg.Endpoint, "")
	case "ollama-cloud":
		// Use the native Ollama /api/chat endpoint so think: true/level,
		// message.thinking, and <think> tag extraction all work correctly. Default
		// to the hosted API when no endpoint is configured — otherwise the shared
		// NewOllamaProvider fallback would point the cloud API key at localhost and
		// every request would fail with a connection error despite a valid key.
		return NewOllamaProvider(ollamaCloudEndpoint(cfg.Endpoint), cfg.APIKey)
	case "custom":
		key := cfg.APIKey
		if key == "" {
			key = "custom"
		}
		return NewOpenAIProvider(key, customBaseURL(cfg.Endpoint))
	}
	_ = fmt.Sprintf // avoid unused import
	return nil
}

// customBaseURL normalizes a custom (OpenAI-compatible) provider endpoint into an
// OpenAI base URL. A bare host with no path gets the conventional "/v1" suffix; an
// endpoint that already carries a path — e.g. Google Gemini's ".../v1beta/openai"
// or GitHub Models' ".../inference" — is used verbatim, so free/alternative
// gateways that don't live at "/v1" work too. An already-"/v1" endpoint is left
// alone rather than doubled to "/v1/v1".
func customBaseURL(endpoint string) string {
	ep := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if ep == "" {
		return ep
	}
	if u, err := url.Parse(ep); err == nil && (u.Path == "" || u.Path == "/") {
		return ep + "/v1"
	}
	return ep
}

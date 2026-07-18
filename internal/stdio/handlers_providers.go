package stdio

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/config"
)

func handleGetProviders(state *ServerState, cmd map[string]any) {
	ai.Global.Reload()
	Emit(map[string]any{"type": "providers", "data": ai.Global.ListForUI()})
}

func handleSaveProviders(state *ServerState, cmd map[string]any) {
	raw, _ := json.Marshal(cmd["providers"])
	var cfgs []ai.ProviderConfig
	if err := json.Unmarshal(raw, &cfgs); err != nil {
		Emit(map[string]any{"type": "error", "message": "invalid providers payload: " + err.Error()})
		return
	}

	// Preserve existing API keys where the client sent an empty string.
	existing := loadRawProviderConfigs()
	existingKeys := map[string]string{}
	for _, p := range existing {
		if p.ID != "" && p.APIKey != "" {
			existingKeys[p.ID] = p.APIKey
		}
	}
	for i := range cfgs {
		if cfgs[i].APIKey == "" {
			if k, ok := existingKeys[cfgs[i].ID]; ok {
				cfgs[i].APIKey = k
			}
		}
	}

	if err := ai.Global.SaveConfigs(cfgs); err != nil {
		Emit(map[string]any{"type": "error", "message": "save failed: " + err.Error()})
		return
	}
	Emit(map[string]any{"type": "providers_saved", "ok": true})
}

func handleDiscoverProviders(state *ServerState, cmd map[string]any) {
	requestID, _ := cmd["requestId"].(string)
	providerType, _ := cmd["providerType"].(string)
	apiKey, _ := cmd["apiKey"].(string)
	endpoint, _ := cmd["endpoint"].(string)

	adapter := ai.BuildAdapter(ai.ProviderConfig{
		Type:     providerType,
		APIKey:   apiKey,
		Endpoint: endpoint,
		Enabled:  true,
	})
	if adapter == nil {
		Emit(map[string]any{
			"type":      "providers_discovered",
			"requestId": requestID,
			"models":    []any{},
			"error":     "unsupported provider type: " + providerType,
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	models, err := adapter.DiscoverModels(ctx)
	if err != nil {
		Emit(map[string]any{
			"type":      "providers_discovered",
			"requestId": requestID,
			"models":    []any{},
			"error":     err.Error(),
		})
		return
	}

	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":                      m.ID,
			"name":                    m.Name,
			"contextWindow":           m.ContextWindow,
			"maxOutputTokens":         m.MaxOutputTokens,
			"supportsStreaming":        m.SupportsStreaming,
			"supportsFunctionCalling": m.SupportsFunctionCall,
		})
	}
	Emit(map[string]any{
		"type":      "providers_discovered",
		"requestId": requestID,
		"models":    out,
	})
}

func handleConfigureProvider(state *ServerState, cmd map[string]any) {
	ai.Global.Reload()
	Emit(map[string]any{"type": "providers", "data": ai.Global.ListForUI()})
}

func loadRawProviderConfigs() []ai.ProviderConfig {
	path := filepath.Join(config.Home(), "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfgs []ai.ProviderConfig
	_ = json.Unmarshal(data, &cfgs)
	return cfgs
}

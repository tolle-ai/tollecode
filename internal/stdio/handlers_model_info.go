package stdio

import (
	"context"

	"github.com/tolle-ai/tollecode/internal/ai"
)

// handleGetModelInfo returns context window, vision, tool, and thinking capabilities
// for the given provider + model combination.
func handleGetModelInfo(state *ServerState, cmd map[string]any) {
	providerID, _ := cmd["provider_id"].(string)
	model, _ := cmd["model"].(string)
	if providerID == "" || model == "" {
		Emit(map[string]any{"type": "model_info_result", "error": "provider_id and model are required"})
		return
	}

	provider := ai.Global.Get(providerID)
	if provider == nil {
		Emit(map[string]any{"type": "model_info_result", "error": "provider not found: " + providerID})
		return
	}

	info := resolveModelInfo(provider, model)
	Emit(map[string]any{
		"type":          "model_info_result",
		"model":         model,
		"providerId":    providerID,
		"contextWindow": info.ContextWindow,
		"vision":        info.SupportsVision,
		"tools":         info.SupportsFunctionCall,
		"thinking":      info.SupportsThinking,
	})
}

// resolveModelInfo dispatches to the right provider-specific lookup.
func resolveModelInfo(provider ai.Provider, model string) ai.ModelInfo {
	ctx := context.Background()
	switch p := provider.(type) {
	case *ai.OllamaProvider:
		return p.GetModelInfo(ctx, model)
	case *ai.AnthropicProvider:
		return ai.AnthropicModelInfo(model)
	case *ai.OpenAIProvider:
		return ai.OpenAIModelInfo(model)
	default:
		// OpenAI-compatible custom endpoint — try the OpenAI table as a best-effort.
		return ai.OpenAIModelInfo(model)
	}
}

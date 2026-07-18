package etl

import (
	"errors"
	"strings"

	chromem "github.com/philippgille/chromem-go"
	"github.com/tolle-ai/tollecode/internal/ai"
)

// EmbeddingFuncFor returns a chromem-go EmbeddingFunc for the given provider.
// If providerID is empty or not found, it tries the first available provider.
func EmbeddingFuncFor(providerID string) (chromem.EmbeddingFunc, error) {
	cfg, ok := ai.Global.Config(providerID)
	if !ok {
		ids := ai.Global.IDs()
		if len(ids) == 0 {
			return nil, errors.New("no providers configured")
		}
		cfg, ok = ai.Global.Config(ids[0])
		if !ok {
			return nil, errors.New("no providers configured")
		}
	}

	switch cfg.Type {
	case "openai":
		return chromem.NewEmbeddingFuncOpenAI(cfg.APIKey, chromem.EmbeddingModelOpenAI3Small), nil

	case "ollama":
		endpoint := cfg.Endpoint
		if endpoint == "" {
			endpoint = "http://localhost:11434"
		}
		return chromem.NewEmbeddingFuncOllama(ollamaEmbedModel(cfg), endpoint), nil

	case "ollama-cloud":
		// Ollama Cloud uses OpenAI-compatible /v1/embeddings, not native /api/embeddings.
		ep := strings.TrimRight(cfg.Endpoint, "/") + "/v1"
		return chromem.NewEmbeddingFuncOpenAICompat(ep, cfg.APIKey, ollamaEmbedModel(cfg), nil), nil

	case "custom":
		ep := cfg.Endpoint
		if ep != "" {
			ep = strings.TrimRight(ep, "/") + "/v1"
		}
		key := cfg.APIKey
		if key == "" {
			key = "custom"
		}
		return chromem.NewEmbeddingFuncOpenAICompat(ep, key, "text-embedding-3-small", nil), nil

	default:
		// Anthropic doesn't have an embedding API — fall through to error.
		return nil, errors.New("provider type '" + cfg.Type + "' does not support embeddings; configure an OpenAI or Ollama provider")
	}
}

// ollamaEmbedModel picks the best embedding model from an Ollama provider config.
// Prefers any model whose name contains "embed"; falls back to nomic-embed-text.
func ollamaEmbedModel(cfg ai.ProviderConfig) string {
	for _, m := range cfg.Models {
		id := m.ID
		if id == "" {
			id = m.Name
		}
		if strings.Contains(strings.ToLower(id), "embed") {
			return id
		}
	}
	return "nomic-embed-text"
}

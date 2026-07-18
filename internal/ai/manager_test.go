package ai

import "testing"

// customBaseURL appends the conventional /v1 only for a bare host (the convenience
// case for a hand-typed custom endpoint). Anything that already carries a path —
// the curated free-provider presets, which supply their full base URL, plus
// Gemini's /v1beta/openai and GitHub Models' /inference — is used verbatim, and an
// existing /v1 is never doubled.
func TestCustomBaseURL(t *testing.T) {
	cases := map[string]string{
		"": "",
		// Bare host → append /v1 (backward-compatible convenience).
		"https://api.example.com":  "https://api.example.com/v1",
		"https://api.example.com/": "https://api.example.com/v1",
		"https://api.cerebras.ai":  "https://api.cerebras.ai/v1",
		// Presets supply the full base URL — used verbatim.
		"https://api.groq.com/openai/v1": "https://api.groq.com/openai/v1",
		"https://openrouter.ai/api/v1":   "https://openrouter.ai/api/v1",
		"https://generativelanguage.googleapis.com/v1beta/openai": "https://generativelanguage.googleapis.com/v1beta/openai",
		"https://models.github.ai/inference":                      "https://models.github.ai/inference",
		// Never double an existing /v1.
		"https://api.example.com/v1": "https://api.example.com/v1",
	}
	for in, want := range cases {
		if got := customBaseURL(in); got != want {
			t.Errorf("customBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// An ollama-cloud provider with no endpoint must default to the hosted API, not
// the localhost fallback baked into NewOllamaProvider — otherwise a valid cloud
// key is sent to localhost and every request fails.
func TestOllamaCloudEndpoint(t *testing.T) {
	if got := ollamaCloudEndpoint(""); got != "https://ollama.com" {
		t.Errorf("empty endpoint = %q, want https://ollama.com", got)
	}
	if got := ollamaCloudEndpoint("   "); got != "https://ollama.com" {
		t.Errorf("blank endpoint = %q, want https://ollama.com", got)
	}
	if got := ollamaCloudEndpoint("https://my.proxy/ollama"); got != "https://my.proxy/ollama" {
		t.Errorf("explicit endpoint = %q, want it preserved", got)
	}
}

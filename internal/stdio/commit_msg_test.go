package stdio

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAPIError verifies that real provider failures are surfaced instead of
// being masked as an empty completion — the bug behind "model returned empty
// response" when an Ollama cloud model is rate-limited.
func TestAPIError(t *testing.T) {
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		return m
	}

	// Real Ollama cloud usage-limit body: {"error":"…weekly usage limit…"}.
	limit := decode(`{"error":"you have reached your weekly usage limit, upgrade for higher limits"}`)
	if got := apiError(200, limit); !strings.Contains(got, "weekly usage limit") {
		t.Errorf("ollama error not surfaced: %q", got)
	}

	// OpenAI shape: {"error":{"message":"…","type":"…"}}.
	openai := decode(`{"error":{"message":"model not found","type":"invalid_request_error"}}`)
	if got := apiError(404, openai); got != "model not found" {
		t.Errorf("openai error not surfaced: %q", got)
	}

	// Healthy completion (no error, 200) → no error reported.
	ok := decode(`{"response":"Add login button"}`)
	if got := apiError(200, ok); got != "" {
		t.Errorf("expected no error for healthy response, got %q", got)
	}

	// Non-2xx with no error body → fall back to the status code.
	if got := apiError(500, map[string]any{}); !strings.Contains(got, "500") {
		t.Errorf("expected HTTP status fallback, got %q", got)
	}
}

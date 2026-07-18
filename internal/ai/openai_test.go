package ai

import (
	"errors"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// Only transient failures (429 rate limit, 5xx server) should be retried; a 4xx
// like 400 is a client error a retry can't fix, and a non-API error isn't ours.
func TestIsRetryableOpenAI(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429 rate limit", &openai.APIError{HTTPStatusCode: 429}, true},
		{"503 server", &openai.APIError{HTTPStatusCode: 503}, true},
		{"500 request error", &openai.RequestError{HTTPStatusCode: 500}, true},
		{"400 bad request", &openai.APIError{HTTPStatusCode: 400}, false},
		{"401 auth", &openai.APIError{HTTPStatusCode: 401}, false},
		{"generic error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isRetryableOpenAI(c.err); got != c.want {
			t.Errorf("%s: isRetryableOpenAI = %v, want %v", c.name, got, c.want)
		}
	}
}

// An interrupted assistant reply persists with empty content. OpenAI-compatible
// servers vary in strictness about empty messages; drop them like Ollama tolerates.
func TestBuildOpenAIMessages_DropsEmptyPlainTurns(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hey"},
		{Role: "assistant", Content: ""}, // interrupted
		{Role: "user", Content: "go"},
	}
	out := buildOpenAIMessages("", msgs)
	if len(out) != 2 {
		t.Fatalf("expected the empty assistant turn dropped, got %d messages", len(out))
	}
	for i, m := range out {
		if m.Content == "" && len(m.MultiContent) == 0 && len(m.ToolCalls) == 0 {
			t.Fatalf("message %d is empty and should have been dropped", i)
		}
	}
}

// A tool round-trip must survive intact: system, user, assistant+tool_calls, tool result.
func TestBuildOpenAIMessages_ToolRoundTrip(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "read a.txt"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Name: "read", Input: map[string]any{"path": "a.txt"}}}},
		{Role: "user", ToolResults: []ToolResult{{ToolUseID: "t1", Content: "contents"}}},
	}
	out := buildOpenAIMessages("You are helpful.", msgs)
	// system + user + assistant(tool_calls) + tool
	if len(out) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(out))
	}
	if out[0].Role != openai.ChatMessageRoleSystem {
		t.Fatalf("expected system message first, got %s", out[0].Role)
	}
	if len(out[2].ToolCalls) != 1 {
		t.Fatalf("expected assistant tool_calls preserved")
	}
	if out[3].Role != openai.ChatMessageRoleTool || out[3].ToolCallID != "t1" {
		t.Fatalf("expected tool result with matching tool_call_id")
	}
}

// A large configured budget must be capped to an older model's real output ceiling
// (else OpenAI 400s), while unknown/custom models pass through uncapped.
func TestOpenAIModelInfo_MaxOutput(t *testing.T) {
	cases := map[string]int{
		"gpt-4o":                    16_384,
		"gpt-4":                     8_192,
		"gpt-3.5-turbo":             4_096,
		"o1-preview":                100_000,
		"some-custom-local-model":   0, // unknown → no cap
	}
	for model, want := range cases {
		if got := openAIModelInfo(model).MaxOutputTokens; got != want {
			t.Errorf("MaxOutputTokens(%q) = %d, want %d", model, got, want)
		}
	}
}

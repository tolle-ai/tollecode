package ai

import (
	"encoding/json"
	"strings"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
)

type anthropicMsg = anthropic.MessageParam

const roleUser = anthropic.MessageParamRoleUser

// assertAnthropicValid checks the invariants the Anthropic Messages API enforces
// and that used to fail with "messages: text content blocks must be non-empty":
//   - no text content block is empty,
//   - no tool_result carries an empty text block,
//   - no two consecutive messages share a role (alternation).
func assertAnthropicValid(t *testing.T, msgs []anthropicMsg) {
	t.Helper()
	for i, m := range msgs {
		if len(m.Content) == 0 {
			t.Fatalf("message %d (role=%s) has an empty content array", i, m.Role)
		}
		if i > 0 && msgs[i-1].Role == m.Role {
			t.Fatalf("messages %d and %d are both role=%s (roles must alternate)", i-1, i, m.Role)
		}
		for j, b := range m.Content {
			if b.OfText != nil && b.OfText.Text == "" {
				t.Fatalf("message %d block %d is an empty text block", i, j)
			}
			if b.OfToolUse != nil {
				// input must be a JSON object, never null (the API rejects
				// "tool_use.input: Input should be an object").
				if data, _ := json.Marshal(b.OfToolUse.Input); string(data) == "null" {
					t.Fatalf("message %d block %d is a tool_use with null input", i, j)
				}
			}
			if b.OfToolResult != nil {
				if len(b.OfToolResult.Content) == 0 {
					t.Fatalf("message %d block %d is a tool_result with empty content", i, j)
				}
				for k, c := range b.OfToolResult.Content {
					if c.OfText != nil && c.OfText.Text == "" {
						t.Fatalf("message %d tool_result block %d part %d is an empty text block", i, j, k)
					}
				}
			}
		}
	}
}

// An interrupted / thinking-only assistant turn persists with empty content and
// no tool calls. It must be dropped, and its two neighbouring user turns coalesced,
// rather than sent as an empty text block (the reported 400 Bad Request).
func TestBuildAnthropicMessages_DropsEmptyAssistantTurn(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hey"},
		{Role: "assistant", Content: ""}, // interrupted "Stopped by user" turn
		{Role: "user", Content: "analyze this workspace"},
	}
	out := buildAnthropicMessages(msgs)
	assertAnthropicValid(t, out)

	if len(out) != 1 {
		t.Fatalf("expected the empty assistant turn dropped and the two user turns coalesced into 1 message, got %d", len(out))
	}
	if out[0].Role != roleUser {
		t.Fatalf("expected coalesced turn to be a user turn, got %s", out[0].Role)
	}
	if len(out[0].Content) != 2 {
		t.Fatalf("expected both user texts preserved as 2 blocks, got %d", len(out[0].Content))
	}
}

// A tool that produced no output must still answer its tool_use with a non-empty
// block — Anthropic rejects an empty tool_result just as it rejects empty text.
func TestBuildAnthropicMessages_EmptyToolOutput(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run it"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Name: "run", Input: map[string]any{}}}},
		{Role: "user", ToolResults: []ToolResult{{ToolUseID: "t1", Name: "run", Content: ""}}},
	}
	out := buildAnthropicMessages(msgs)
	assertAnthropicValid(t, out)

	if len(out) != 3 {
		t.Fatalf("expected 3 messages (user, assistant tool_use, user tool_result), got %d", len(out))
	}
}

// A no-arg tool (e.g. mcp__blender__get_scene_info) is captured with nil input,
// and older sessions persist it that way. It must serialize as {} — never null —
// or the follow-up request 400s ("tool_use.input: Input should be an object").
func TestBuildAnthropicMessages_NilToolInputBecomesObject(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "what's in the scene?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Name: "get_scene_info", Input: nil}}},
		{Role: "user", ToolResults: []ToolResult{{ToolUseID: "t1", Name: "get_scene_info", Content: "{\"objects\":[]}"}}},
	}
	out := buildAnthropicMessages(msgs)
	assertAnthropicValid(t, out) // now also rejects null tool_use input

	var seen bool
	for _, m := range out {
		for _, b := range m.Content {
			if b.OfToolUse == nil {
				continue
			}
			seen = true
			if data, _ := json.Marshal(b.OfToolUse.Input); string(data) != "{}" {
				t.Fatalf("nil tool input must serialize to {}, got %s", data)
			}
		}
	}
	if !seen {
		t.Fatal("expected a tool_use block in the built messages")
	}
}

// A degenerate history that is nothing but empty turns must not produce empty
// blocks — it should collapse to no messages rather than a rejected request.
func TestBuildAnthropicMessages_AllEmpty(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "assistant", Content: ""},
		{Role: "user", Content: ""},
		{Role: "assistant", Content: ""},
	}
	out := buildAnthropicMessages(msgs)
	assertAnthropicValid(t, out)
	if len(out) != 0 {
		t.Fatalf("expected all-empty history to collapse to 0 messages, got %d", len(out))
	}
}

// A normal tool round-trip with interleaved text must be preserved intact and valid.
func TestBuildAnthropicMessages_NormalToolRoundTrip(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "read the file"},
		{Role: "assistant", Content: "Reading it now.", ToolCalls: []ToolCall{{ID: "t1", Name: "read", Input: map[string]any{"path": "a.txt"}}}},
		{Role: "user", ToolResults: []ToolResult{{ToolUseID: "t1", Name: "read", Content: "file contents"}}},
		{Role: "assistant", Content: "Done."},
	}
	out := buildAnthropicMessages(msgs)
	assertAnthropicValid(t, out)
	if len(out) != 4 {
		t.Fatalf("expected 4 messages preserved, got %d", len(out))
	}
}

// ── Thinking parameter selection ────────────────────────────────────────────────

func TestAnthropicThinkingMode(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-8":            thinkingAdaptive,
		"claude-opus-4-7":            thinkingAdaptive,
		"claude-opus-4-6":            thinkingAdaptive,
		"claude-sonnet-5":            thinkingAdaptive,
		"claude-sonnet-4-6":          thinkingAdaptive,
		"claude-fable-5":             thinkingAdaptive,
		"claude-opus-4-5":            thinkingBudget,
		"claude-sonnet-4-5":          thinkingBudget,
		"claude-haiku-4-5":           thinkingBudget,
		"claude-3-7-sonnet-20250219": thinkingBudget,
		"claude-3-5-sonnet-20241022": thinkingNone,
		"claude-3-opus-20240229":     thinkingNone,
	}
	for model, want := range cases {
		if got := anthropicThinkingMode(model); got != want {
			t.Errorf("anthropicThinkingMode(%q) = %q, want %q", model, got, want)
		}
	}
}

// The reported bug: current models 400 on budget_tokens. When thinking is requested
// on a current model we must send adaptive thinking, never enabled+budget_tokens.
func TestBuildAnthropicParams_AdaptiveThinkingOnCurrentModel(t *testing.T) {
	req := StreamRequest{
		Model:          "claude-opus-4-8",
		ThinkingBudget: 8000,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	p := buildAnthropicParams(req)
	if p.Thinking.OfAdaptive == nil {
		t.Fatal("expected adaptive thinking on claude-opus-4-8")
	}
	if p.Thinking.OfEnabled != nil {
		t.Fatal("must not send budget_tokens on a current model — it returns a 400")
	}
}

// Legacy models still take enabled+budget_tokens, clamped strictly below max_tokens.
func TestBuildAnthropicParams_BudgetThinkingOnLegacyModel(t *testing.T) {
	req := StreamRequest{
		Model:          "claude-3-7-sonnet-20250219", // max output 8192
		ThinkingBudget: 10000,                        // deliberately above max_tokens
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	p := buildAnthropicParams(req)
	if p.Thinking.OfEnabled == nil {
		t.Fatal("expected enabled+budget_tokens on a legacy model")
	}
	if p.Thinking.OfEnabled.BudgetTokens >= p.MaxTokens {
		t.Fatalf("budget_tokens (%d) must be < max_tokens (%d)", p.Thinking.OfEnabled.BudgetTokens, p.MaxTokens)
	}
}

func TestBuildAnthropicParams_ThinkingOffByDefault(t *testing.T) {
	req := StreamRequest{Model: "claude-opus-4-8", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	p := buildAnthropicParams(req)
	if p.Thinking.OfAdaptive != nil || p.Thinking.OfEnabled != nil {
		t.Fatal("thinking must be off when no budget is requested")
	}
}

func TestBuildAnthropicParams_NoThinkingOnUnsupportedModel(t *testing.T) {
	req := StreamRequest{Model: "claude-3-5-sonnet-20241022", ThinkingBudget: 8000, Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	p := buildAnthropicParams(req)
	if p.Thinking.OfAdaptive != nil || p.Thinking.OfEnabled != nil {
		t.Fatal("must not send a thinking param to a model without thinking support")
	}
}

// ── max_tokens bounds ───────────────────────────────────────────────────────────

func TestBuildAnthropicParams_MaxTokens(t *testing.T) {
	// A configured budget is respected verbatim when below the model ceiling.
	if p := buildAnthropicParams(StreamRequest{Model: "claude-opus-4-8", MaxTokens: 16000}); p.MaxTokens != 16000 {
		t.Fatalf("expected configured max_tokens (16000) respected, got %d", p.MaxTokens)
	}
	// Unset falls back to the built-in default.
	if p := buildAnthropicParams(StreamRequest{Model: "claude-opus-4-8"}); p.MaxTokens != defaultAnthropicMaxTokens {
		t.Fatalf("expected fallback max_tokens %d, got %d", defaultAnthropicMaxTokens, p.MaxTokens)
	}
	// Never exceed the model's real output ceiling (Haiku 4.5 caps at 64k).
	if p := buildAnthropicParams(StreamRequest{Model: "claude-haiku-4-5", MaxTokens: 200000}); p.MaxTokens != 64000 {
		t.Fatalf("expected max_tokens capped at 64000, got %d", p.MaxTokens)
	}
}

// ── Prompt caching ──────────────────────────────────────────────────────────────

func TestBuildAnthropicParams_CacheBreakpoints(t *testing.T) {
	req := StreamRequest{
		Model:    "claude-opus-4-8",
		System:   "You are a helpful assistant.",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}
	p := buildAnthropicParams(req)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// One breakpoint on the system prompt, one on the last message block.
	if n := strings.Count(string(b), "cache_control"); n < 2 {
		t.Fatalf("expected >=2 cache_control breakpoints (system + last message), got %d: %s", n, b)
	}
}

// ── Thinking-block replay (extended thinking + tool use) ────────────────────────

func TestBuildAnthropicMessages_ReplaysThinkingBeforeToolUse(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "weather in Paris?"},
		{
			Role:           "assistant",
			ThinkingBlocks: []ThinkingBlock{{Thinking: "I should call the weather tool", Signature: "sig-abc"}},
			ToolCalls:      []ToolCall{{ID: "t1", Name: "get_weather", Input: map[string]any{"city": "Paris"}}},
		},
		{Role: "user", ToolResults: []ToolResult{{ToolUseID: "t1", Content: "18C"}}},
	}
	out := buildAnthropicMessages(msgs)
	assertAnthropicValid(t, out)

	asst := out[1]
	if asst.Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("expected assistant turn at index 1, got %s", asst.Role)
	}
	if len(asst.Content) == 0 || asst.Content[0].OfThinking == nil {
		t.Fatalf("thinking block must lead the assistant turn preceding a tool_use")
	}
	if asst.Content[0].OfThinking.Signature != "sig-abc" {
		t.Fatalf("thinking signature must be replayed unmodified, got %q", asst.Content[0].OfThinking.Signature)
	}
	if last := asst.Content[len(asst.Content)-1]; last.OfToolUse == nil {
		t.Fatalf("tool_use must follow the thinking block")
	}
}

// A thinking block without a signature can't be replayed (the API rejects
// unsigned/modified thinking) — it must be dropped, not sent.
func TestBuildAnthropicMessages_SkipsUnsignedThinking(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ThinkingBlocks: []ThinkingBlock{{Thinking: "no signature captured"}}, Content: "hello"},
	}
	out := buildAnthropicMessages(msgs)
	assertAnthropicValid(t, out)
	asst := out[1]
	for _, blk := range asst.Content {
		if blk.OfThinking != nil {
			t.Fatalf("an unsigned thinking block must not be replayed")
		}
	}
	if len(asst.Content) == 0 || asst.Content[0].OfText == nil {
		t.Fatalf("expected the text block to remain")
	}
}

func TestBuildAnthropicMessages_ReplaysRedactedThinking(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hi"},
		{
			Role:           "assistant",
			ThinkingBlocks: []ThinkingBlock{{Redacted: "ENCRYPTED_DATA"}},
			ToolCalls:      []ToolCall{{ID: "t1", Name: "noop", Input: map[string]any{}}},
		},
		{Role: "user", ToolResults: []ToolResult{{ToolUseID: "t1", Content: "ok"}}},
	}
	out := buildAnthropicMessages(msgs)
	assertAnthropicValid(t, out)
	if b := out[1].Content[0]; b.OfRedactedThinking == nil || b.OfRedactedThinking.Data != "ENCRYPTED_DATA" {
		t.Fatalf("expected redacted_thinking block replayed with its data")
	}
}

// ── Model metadata ──────────────────────────────────────────────────────────────

func TestAnthropicModelInfo_MaxOutput(t *testing.T) {
	cases := map[string]int{
		"claude-opus-4-8":            128_000,
		"claude-sonnet-5":            128_000,
		"claude-haiku-4-5":           64_000,
		"claude-3-5-sonnet-20241022": 8_192,
	}
	for model, want := range cases {
		if got := anthropicModelInfo(model, model).MaxOutputTokens; got != want {
			t.Errorf("MaxOutputTokens(%q) = %d, want %d", model, got, want)
		}
	}
}

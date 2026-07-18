package ai

import (
	"context"
	"strings"
)

// DetectBase64MediaType returns the MIME type for a raw base64-encoded image
// by inspecting the magic bytes encoded at the start of the string.
func DetectBase64MediaType(data string) string {
	switch {
	case strings.HasPrefix(data, "iVBOR"):
		return "image/png"
	case strings.HasPrefix(data, "R0lGOD"):
		return "image/gif"
	case strings.HasPrefix(data, "UklGR"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// StreamEvent is one event from a streaming LLM response.
type StreamEvent struct {
	Type string // "token" | "thinking" | "thinking_block" | "tool_call" | "done" | "error"

	// token / thinking
	Text string

	// thinking_block (Anthropic extended thinking): the completed block, emitted at
	// content_block_stop so it can be replayed verbatim on the next request in the
	// same tool-use turn. Signature authenticates a normal block; Redacted carries
	// the opaque data of a redacted_thinking block instead.
	Signature string
	Redacted  string

	// tool_call (complete, ready to execute)
	ToolID    string
	ToolName  string
	ToolInput map[string]any

	// done
	InputTokens  int
	OutputTokens int
	FinishReason string // "end_turn" | "tool_use" | "max_tokens" | "stop"

	// error
	Err error
}

// ThinkingBlock is one extended-thinking block from an assistant turn. Anthropic
// requires the thinking that preceded a tool_use to be passed back unmodified on
// the next request within the same turn (with its signature), or it rejects the
// request. Blocks are only replayed for the current tool-use turn — the API
// auto-filters older turns, so they are not persisted to disk.
type ThinkingBlock struct {
	Thinking  string // the thinking text (may be empty when display=omitted)
	Signature string // opaque signature authenticating a normal thinking block
	Redacted  string // opaque data of a redacted_thinking block (set instead of the above)
}

// ToolCall is one tool invocation by the assistant in a turn.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// ChatMessage is one turn in a conversation history.
type ChatMessage struct {
	Role    string // "user" | "assistant"
	Content string

	// For assistant turns that called tools:
	ToolCalls []ToolCall

	// Assistant turns with Anthropic extended thinking: the thinking blocks that
	// led to this turn, replayed on the next request in the same tool-use turn.
	ThinkingBlocks []ThinkingBlock

	// For tool-result turns (role=="user" with tool content):
	ToolResults []ToolResult

	// User-attached images (base64-encoded, vision models only).
	// Not persisted to disk — set only on the current turn's in-memory message.
	Images []string
}

// ToolResult is a resolved tool output sent back to the LLM.
type ToolResult struct {
	ToolUseID     string
	Name          string // tool name — required by native Ollama "tool" role messages
	Content       string
	IsError       bool
	ImageData     string // base64-encoded image; non-empty means include an image content block
	ImageMediaType string // e.g. "image/jpeg"; defaults to "image/jpeg" when empty
}

// ToolDef is the schema definition for one tool passed to the LLM.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ModelInfo describes one model from a provider's catalog.
type ModelInfo struct {
	ID                   string
	Name                 string
	ContextWindow        int
	MaxOutputTokens      int
	SupportsStreaming     bool
	SupportsFunctionCall bool
	SupportsVision       bool
	SupportsThinking     bool
}

// Provider is the interface all LLM adapters must satisfy.
type Provider interface {
	// Stream sends messages and streams events. Caller must drain the channel.
	Stream(ctx context.Context, req StreamRequest) (<-chan StreamEvent, error)
	// DiscoverModels lists models available from this provider.
	DiscoverModels(ctx context.Context) ([]ModelInfo, error)
}

// StreamRequest bundles all parameters for a single streaming call.
type StreamRequest struct {
	Model          string
	System         string
	Messages       []ChatMessage
	Tools          []ToolDef
	MaxTokens      int
	ThinkingBudget int    // Anthropic only: extended thinking budget tokens
	ThinkLevel     string // Ollama: "", "true", "false", "low", "medium", "high"
}

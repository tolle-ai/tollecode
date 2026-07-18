package ai

import (
	"context"
	"encoding/json"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicProvider implements Provider using the official Anthropic Go SDK.
type AnthropicProvider struct {
	client *anthropic.Client
}

func NewAnthropicProvider(apiKey string) *AnthropicProvider {
	// The SDK retries 408/409/429/5xx (incl. 529 overloaded) with exponential
	// backoff. Its default is 2; we lift it to 5 so transient rate-limit and
	// overload blips ride through the way Ollama's own 429 backoff loop does,
	// rather than surfacing a hard error to the user mid-turn.
	c := anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(5),
	)
	return &AnthropicProvider{client: &c}
}

func (p *AnthropicProvider) Stream(ctx context.Context, req StreamRequest) (<-chan StreamEvent, error) {
	stream := p.client.Messages.NewStreaming(ctx, buildAnthropicParams(req))
	ch := make(chan StreamEvent, 64)

	go func() {
		defer close(ch)
		defer stream.Close()

		// Track in-progress content blocks by their index.
		type toolAcc struct {
			id      string
			name    string
			jsonBuf strings.Builder
		}
		type textAcc struct {
			buf strings.Builder
		}
		// Thinking blocks are accumulated so the completed block (text + signature)
		// can be replayed verbatim on the next request in the same tool-use turn.
		type thinkAcc struct {
			buf      strings.Builder
			sig      strings.Builder
			redacted string
		}
		toolBlocks := map[int64]*toolAcc{}
		textBlocks := map[int64]*textAcc{}
		thinkBlocks := map[int64]*thinkAcc{}
		var currentToolIdx int64 = -1
		var inputTokens, outputTokens int
		var stopReason string

		for stream.Next() {
			ev := stream.Current()

			switch ev.Type {
			case "message_start":
				inputTokens = int(ev.Message.Usage.InputTokens)

			case "content_block_start":
				switch ev.ContentBlock.Type {
				case "tool_use":
					toolBlocks[ev.Index] = &toolAcc{
						id:   ev.ContentBlock.ID,
						name: ev.ContentBlock.Name,
					}
					currentToolIdx = ev.Index
				case "text":
					textBlocks[ev.Index] = &textAcc{}
				case "thinking":
					thinkBlocks[ev.Index] = &thinkAcc{}
				case "redacted_thinking":
					thinkBlocks[ev.Index] = &thinkAcc{redacted: ev.ContentBlock.Data}
				}

			case "content_block_delta":
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" {
						if acc, ok := textBlocks[ev.Index]; ok {
							acc.buf.WriteString(ev.Delta.Text)
						} else {
							ch <- StreamEvent{Type: "token", Text: ev.Delta.Text}
						}
					}
				case "thinking_delta":
					if ev.Delta.Thinking != "" {
						ch <- StreamEvent{Type: "thinking", Text: ev.Delta.Thinking}
						if acc, ok := thinkBlocks[ev.Index]; ok {
							acc.buf.WriteString(ev.Delta.Thinking)
						}
					}
				case "signature_delta":
					if acc, ok := thinkBlocks[ev.Index]; ok {
						acc.sig.WriteString(ev.Delta.Signature)
					}
				case "input_json_delta":
					if acc, ok := toolBlocks[ev.Index]; ok {
						acc.jsonBuf.WriteString(ev.Delta.PartialJSON)
					}
				}

			case "content_block_stop":
				if acc, ok := textBlocks[ev.Index]; ok {
					if acc.buf.Len() > 0 {
						ch <- StreamEvent{Type: "token", Text: acc.buf.String()}
					}
					delete(textBlocks, ev.Index)
				}
				if acc, ok := thinkBlocks[ev.Index]; ok {
					// Emit the completed thinking block so the executor can replay it,
					// unmodified, on the next request in this tool-use turn.
					ch <- StreamEvent{
						Type:      "thinking_block",
						Text:      acc.buf.String(),
						Signature: acc.sig.String(),
						Redacted:  acc.redacted,
					}
					delete(thinkBlocks, ev.Index)
				}
				if acc, ok := toolBlocks[ev.Index]; ok {
					var input map[string]any
					_ = json.Unmarshal([]byte(acc.jsonBuf.String()), &input)
					// No-arg tools stream an empty buffer, leaving input nil; the API
					// requires tool_use.input to be an object, so normalize to {}.
					if input == nil {
						input = map[string]any{}
					}
					ch <- StreamEvent{
						Type:      "tool_call",
						ToolID:    acc.id,
						ToolName:  acc.name,
						ToolInput: input,
					}
					delete(toolBlocks, ev.Index)
				}
				if ev.Index == currentToolIdx {
					currentToolIdx = -1
				}

			case "message_delta":
				outputTokens = int(ev.Usage.OutputTokens)
				if ev.Delta.StopReason != "" {
					stopReason = string(ev.Delta.StopReason)
				}

			case "message_stop":
				// nothing to do — done event emitted after loop
			}
		}

		if err := stream.Err(); err != nil {
			if ctx.Err() == nil {
				ch <- StreamEvent{Type: "error", Err: err}
			}
			return
		}

		if stopReason == "" {
			if len(toolBlocks) > 0 {
				stopReason = "tool_use"
			} else {
				stopReason = "end_turn"
			}
		}
		ch <- StreamEvent{
			Type:         "done",
			FinishReason: stopReason,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
		}
	}()

	return ch, nil
}

// AnthropicModelInfo returns known capability metadata for a Claude model ID.
func AnthropicModelInfo(id string) ModelInfo { return anthropicModelInfo(id, id) }

func anthropicModelInfo(id, name string) ModelInfo {
	// Max output: current Claude models stream up to 128k; Haiku 4.5 caps at 64k;
	// Claude 3.x at 8k. (Reporting 8k for every model — as before — throttled long
	// turns and forced needless continuation loops.) Context window is left at a
	// conservative 200k: it drives the auto-compaction threshold, and the current
	// models' true 1M window is a deliberate cost/behaviour choice to opt into.
	maxOutput := 128_000
	m := strings.ToLower(id)
	switch {
	case strings.Contains(m, "haiku-4-5"):
		maxOutput = 64_000
	case strings.Contains(m, "claude-3"), strings.Contains(m, "claude-2"):
		maxOutput = 8_192
	}
	return ModelInfo{
		ID:                   id,
		Name:                 name,
		ContextWindow:        200_000,
		MaxOutputTokens:      maxOutput,
		SupportsStreaming:    true,
		SupportsFunctionCall: true,
		SupportsVision:       true,
		SupportsThinking:     anthropicThinkingMode(id) != thinkingNone,
	}
}

// ── Thinking / effort / caching / max-tokens policy ─────────────────────────────

// defaultAnthropicMaxTokens is the fallback output budget when the caller passes
// none. The configured budget (sidecar_settings.maxOutputTokens, default 32000)
// normally flows in via StreamRequest.MaxTokens.
const defaultAnthropicMaxTokens = 32_000

// buildAnthropicParams assembles the full Messages request from the neutral
// StreamRequest: max_tokens (bounded by the model's real ceiling), the cached
// system prompt, tools, the message history with a trailing cache breakpoint, and
// the model-appropriate thinking parameter. Kept separate from Stream so the exact
// wire payload can be unit-tested without a network call.
func buildAnthropicParams(req StreamRequest) anthropic.MessageNewParams {
	info := anthropicModelInfo(req.Model, req.Model)

	// Honor the caller's configured output budget (sidecar_settings.maxOutputTokens),
	// falling back to a sane default only when unset. We always stream, so a large
	// value carries no HTTP-timeout risk — but never exceed the model's real output
	// ceiling, which would 400.
	maxTok := int64(req.MaxTokens)
	if maxTok <= 0 {
		maxTok = defaultAnthropicMaxTokens
	}
	if info.MaxOutputTokens > 0 && maxTok > int64(info.MaxOutputTokens) {
		maxTok = int64(info.MaxOutputTokens)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: maxTok,
		Messages:  buildAnthropicMessages(req.Messages),
	}
	if req.System != "" {
		// Cache the system prompt (tools render before it, so a breakpoint here
		// caches tools+system together). In an agent loop the same prefix is resent
		// on every tool iteration; caching cuts its input cost ~10x and latency.
		params.System = []anthropic.TextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}
	if len(req.Tools) > 0 {
		params.Tools = buildAnthropicTools(req.Tools)
	}
	// Cache the conversation prefix too: mark the last content block so the next
	// turn reads the whole history up to here from cache instead of reprocessing it.
	applyMessagesCacheBreakpoint(params.Messages)

	applyThinking(&params, req.Model, req.ThinkingBudget)
	return params
}

const (
	thinkingNone     = "none"     // model has no extended-thinking support
	thinkingBudget   = "budget"   // legacy models: thinking: {enabled, budget_tokens}
	thinkingAdaptive = "adaptive" // current models: thinking: {adaptive} (budget_tokens 400s)
)

// anthropicThinkingMode reports how (if at all) a model accepts an extended-thinking
// request. Adaptive thinking arrived with the 4.6 generation; 4.5-and-earlier still
// take an explicit budget; 3.5/3-opus/2.x have no thinking. Unknown (future) IDs
// default to adaptive, the modern surface.
func anthropicThinkingMode(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude-3-5"), strings.Contains(m, "claude-3-opus"),
		strings.Contains(m, "claude-3-haiku"), strings.Contains(m, "claude-2"):
		return thinkingNone
	case strings.Contains(m, "-4-5"), strings.Contains(m, "-4-1"),
		strings.Contains(m, "-4-0"), strings.Contains(m, "claude-3-7"):
		return thinkingBudget
	default:
		return thinkingAdaptive
	}
}

// applyThinking sets the thinking parameter only when a budget was requested,
// choosing the mode the target model actually accepts. This is the fix for the
// 400 the old code hit on every current model: it always sent budget_tokens, which
// Opus 4.6+/Sonnet 4.6+/Sonnet 5/Fable 5 reject — adaptive is their only on-mode.
func applyThinking(params *anthropic.MessageNewParams, model string, budget int) {
	if budget <= 0 {
		return // thinking not requested — leave it off
	}
	switch anthropicThinkingMode(model) {
	case thinkingAdaptive:
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
				// Summarized so the reasoning still streams to the UI (the default is
				// omitted on current models, which would show an empty thinking pane).
				Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
			},
		}
	case thinkingBudget:
		b := int64(budget)
		// budget_tokens must be strictly less than max_tokens (min 1024).
		if params.MaxTokens > 0 && b >= params.MaxTokens {
			b = params.MaxTokens - 1024
		}
		if b < 1024 {
			b = 1024
		}
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: b},
		}
	}
	// thinkingNone: send nothing — the model would reject a thinking param.
}

// applyMessagesCacheBreakpoint marks the final content block of the conversation
// as a cache breakpoint, so each turn reads the whole prior history from cache
// instead of reprocessing it. Combined with the system-prompt breakpoint, this is
// the standard multi-turn caching layout (≤4 breakpoints; we use 2).
func applyMessagesCacheBreakpoint(msgs []anthropic.MessageParam) {
	if len(msgs) == 0 {
		return
	}
	content := msgs[len(msgs)-1].Content
	if len(content) == 0 {
		return
	}
	cc := anthropic.NewCacheControlEphemeralParam()
	switch blk := &content[len(content)-1]; {
	case blk.OfText != nil:
		blk.OfText.CacheControl = cc
	case blk.OfToolResult != nil:
		blk.OfToolResult.CacheControl = cc
	case blk.OfToolUse != nil:
		blk.OfToolUse.CacheControl = cc
	case blk.OfImage != nil:
		blk.OfImage.CacheControl = cc
	}
}

func (p *AnthropicProvider) DiscoverModels(ctx context.Context) ([]ModelInfo, error) {
	pager := p.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var out []ModelInfo
	for pager.Next() {
		m := pager.Current()
		out = append(out, anthropicModelInfo(m.ID, m.DisplayName))
	}
	if err := pager.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// buildAnthropicMessages converts the neutral history into Anthropic message
// params. Unlike Ollama — which silently accepts empty content — the Anthropic
// API rejects a request outright ("messages: text content blocks must be
// non-empty") if any turn carries an empty content array or an empty text block.
// Those degenerate turns are routine: an interrupted or thinking-only assistant
// turn persists with empty text, mid-loop continuations append an empty turn,
// and a tool that produced no output yields an empty tool_result. To reach
// Ollama-grade robustness we (1) never emit an empty text block, (2) drop turns
// that would carry no blocks at all, and (3) coalesce consecutive same-role
// turns so dropping one can never leave two same-role turns adjacent — keeping
// the alternating structure the API expects.
func buildAnthropicMessages(msgs []ChatMessage) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))

	emit := func(role anthropic.MessageParamRole, blocks []anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return // a turn with nothing to say — omit it rather than send an empty block
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...) // merge into the prior same-role turn
			return
		}
		out = append(out, anthropic.MessageParam{Role: role, Content: blocks})
	}

	textBlock := func(s string) anthropic.ContentBlockParamUnion {
		return anthropic.ContentBlockParamUnion{OfText: &anthropic.TextBlockParam{Text: s}}
	}

	// thinkingBlocksFor returns the assistant turn's extended-thinking blocks, which
	// must lead the turn's content and be replayed unmodified (with their signature)
	// so a tool_use that followed them is accepted on the next request.
	thinkingBlocksFor := func(m ChatMessage) []anthropic.ContentBlockParamUnion {
		if m.Role != "assistant" {
			return nil
		}
		var out []anthropic.ContentBlockParamUnion
		for _, tb := range m.ThinkingBlocks {
			switch {
			case tb.Redacted != "":
				out = append(out, anthropic.ContentBlockParamUnion{
					OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: tb.Redacted},
				})
			case tb.Signature != "":
				// Without a signature the block can't be replayed (the API rejects
				// modified/unsigned thinking) — drop it rather than trigger a 400.
				out = append(out, anthropic.ContentBlockParamUnion{
					OfThinking: &anthropic.ThinkingBlockParam{Thinking: tb.Thinking, Signature: tb.Signature},
				})
			}
		}
		return out
	}

	for _, m := range msgs {
		// User turn with tool results
		if m.Role == "user" && len(m.ToolResults) > 0 {
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, textBlock(m.Content))
			}
			for _, tr := range m.ToolResults {
				var content []anthropic.ToolResultBlockParamContentUnion
				if tr.Content != "" {
					content = append(content, anthropic.ToolResultBlockParamContentUnion{
						OfText: &anthropic.TextBlockParam{Text: tr.Content},
					})
				}
				if tr.ImageData != "" {
					mediaType := tr.ImageMediaType
					if mediaType == "" {
						mediaType = "image/jpeg"
					}
					content = append(content, anthropic.ToolResultBlockParamContentUnion{
						OfImage: &anthropic.ImageBlockParam{
							Source: anthropic.ImageBlockParamSourceUnion{
								OfBase64: &anthropic.Base64ImageSourceParam{
									MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
									Data:      tr.ImageData,
								},
							},
						},
					})
				}
				if len(content) == 0 {
					// A tool that returned no output — still send a non-empty block so
					// the tool_use is answered (an empty tool_result is rejected too).
					content = append(content, anthropic.ToolResultBlockParamContentUnion{
						OfText: &anthropic.TextBlockParam{Text: "(no output)"},
					})
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: tr.ToolUseID,
						IsError:   anthropic.Bool(tr.IsError),
						Content:   content,
					},
				})
			}
			emit(anthropic.MessageParamRoleUser, blocks)
			continue
		}

		// Assistant turn with tool calls
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			blocks := thinkingBlocksFor(m) // must lead the turn when thinking is on
			if m.Content != "" {
				blocks = append(blocks, textBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				// Guard sessions persisted before input was normalized: a nil map
				// serializes to JSON null, which the API rejects ("Input should be
				// an object").
				input := tc.Input
				if input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Name,
						Input: input,
					},
				})
			}
			emit(anthropic.MessageParamRoleAssistant, blocks)
			continue
		}

		// Plain text turn (optionally with user-attached images).
		role := anthropic.MessageParamRoleUser
		if m.Role == "assistant" {
			role = anthropic.MessageParamRoleAssistant
		}
		blocks := thinkingBlocksFor(m) // nil for user turns; leads an assistant turn
		if m.Content != "" {
			blocks = append(blocks, textBlock(m.Content))
		}
		for _, img := range m.Images {
			mediaType := DetectBase64MediaType(img)
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfImage: &anthropic.ImageBlockParam{
					Source: anthropic.ImageBlockParamSourceUnion{
						OfBase64: &anthropic.Base64ImageSourceParam{
							MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
							Data:      img,
						},
					},
				},
			})
		}
		emit(role, blocks)
	}
	return out
}

func buildAnthropicTools(tools []ToolDef) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, len(tools))
	for i, t := range tools {
		props := t.InputSchema["properties"]
		required, _ := t.InputSchema["required"].([]any)
		reqStrings := make([]string, 0, len(required))
		for _, r := range required {
			if s, ok := r.(string); ok {
				reqStrings = append(reqStrings, s)
			}
		}
		out[i] = anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: props,
				},
			},
		}
	}
	return out
}

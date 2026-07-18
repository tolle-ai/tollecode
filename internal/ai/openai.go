package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// OpenAIProvider implements Provider for OpenAI-compatible APIs
// (OpenAI, Ollama-cloud, custom endpoints).
type OpenAIProvider struct {
	client *openai.Client
}

func NewOpenAIProvider(apiKey, endpoint string) *OpenAIProvider {
	cfg := openai.DefaultConfig(apiKey)
	if endpoint != "" {
		cfg.BaseURL = endpoint
	}
	return &OpenAIProvider{client: openai.NewClientWithConfig(cfg)}
}

func (p *OpenAIProvider) Stream(ctx context.Context, req StreamRequest) (<-chan StreamEvent, error) {
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 8192
	}
	// Clamp to the model's real output ceiling when known: the configured budget is
	// provider-agnostic and can exceed what an older OpenAI model allows, which 400s.
	// Unknown/custom models report 0 and pass through (the endpoint owner's call).
	if mo := openAIModelInfo(req.Model).MaxOutputTokens; mo > 0 && maxTok > mo {
		maxTok = mo
	}

	msgs := buildOpenAIMessages(req.System, req.Messages)
	oreq := openai.ChatCompletionRequest{
		Model:         req.Model,
		Messages:      msgs,
		MaxTokens:     maxTok,
		Stream:        true,
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
	}
	if len(req.Tools) > 0 {
		oreq.Tools = buildOpenAITools(req.Tools)
	}

	// The go-openai client does no retrying of its own (unlike the Anthropic SDK
	// and our native Ollama loop), so a single 429/5xx blip on the initial request
	// surfaced as a hard error. Retry the stream open with backoff for parity.
	stream, err := p.createStreamWithRetry(ctx, oreq)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamEvent, 64)
	go func() {
		defer close(ch)
		defer stream.Close()

		// Accumulate tool call arguments across deltas.
		type tcAcc struct {
			id     string
			name   string
			argBuf strings.Builder
		}
		tcMap := map[int]*tcAcc{}

		// Accumulate text at sentence boundaries so each token event is a complete
		// sentence rather than individual words. Force-flush before tool calls and at end.
		var textBuf strings.Builder

		emitUpToSentence := func(force bool) {
			s := textBuf.String()
			if len(s) == 0 {
				return
			}
			if force {
				ch <- StreamEvent{Type: "token", Text: s}
				textBuf.Reset()
				return
			}
			lastCut := -1
			for i := 0; i < len(s)-1; i++ {
				c, next := s[i], s[i+1]
				if (c == '.' || c == '!' || c == '?') && (next == ' ' || next == '\n') {
					lastCut = i + 1
				} else if c == '\n' && next == '\n' {
					lastCut = i + 2
				}
			}
			if lastCut > 0 {
				ch <- StreamEvent{Type: "token", Text: s[:lastCut]}
				textBuf.Reset()
				textBuf.WriteString(s[lastCut:])
			}
		}

		emitToolCalls := func() {
			emitUpToSentence(true)
			for _, acc := range tcMap {
				var input map[string]any
				_ = json.Unmarshal([]byte(acc.argBuf.String()), &input)
				// No-arg tools stream empty arguments, leaving input nil; normalize
				// to {} so it never round-trips to the API as null.
				if input == nil {
					input = map[string]any{}
				}
				ch <- StreamEvent{
					Type:      "tool_call",
					ToolID:    acc.id,
					ToolName:  acc.name,
					ToolInput: input,
				}
			}
		}

		// Inline <think> tag extraction — reuses the same stateful parser as the
		// native Ollama provider so cloud models that embed reasoning in <think>
		// blocks (e.g. GLM, QwQ served via OpenAI-compat /v1) are handled correctly.
		var thinkBuf strings.Builder
		inThink := false

		var (
			finishReason string
			usageIn      int
			usageOut     int
		)

		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					ch <- StreamEvent{Type: "error", Err: err}
				}
				return
			}

			// Capture token usage whenever the provider includes it.
			// With IncludeUsage=true, OpenAI-compatible APIs send a final
			// chunk with empty Choices and Usage set.
			if resp.Usage != nil {
				usageIn = resp.Usage.PromptTokens
				usageOut = resp.Usage.CompletionTokens
			}

			if len(resp.Choices) == 0 {
				continue
			}
			delta := resp.Choices[0].Delta

			// Accumulate text, extracting any inline <think>…</think> blocks.
			if delta.Content != "" {
				visible, thinking := extractThinkTags(&thinkBuf, &inThink, delta.Content)
				if thinking != "" {
					ch <- StreamEvent{Type: "thinking", Text: thinking}
				}
				if visible != "" {
					textBuf.WriteString(visible)
					emitUpToSentence(false)
				}
			}

			// Tool call deltas
			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				if idx == nil {
					i := 0
					idx = &i
				}
				acc, ok := tcMap[*idx]
				if !ok {
					acc = &tcAcc{}
					tcMap[*idx] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.argBuf.WriteString(tc.Function.Arguments)
			}

			// Record finish reason and continue the loop so we can read the
			// trailing usage chunk that IncludeUsage sends after this one.
			if r := resp.Choices[0].FinishReason; r == "tool_calls" || r == "stop" {
				finishReason = string(r)
			}
		}

		// Flush any thinking that never received a closing </think> tag.
		if inThink && thinkBuf.Len() > 0 {
			ch <- StreamEvent{Type: "thinking", Text: thinkBuf.String()}
			thinkBuf.Reset()
			inThink = false
		}

		// Emit tool calls and done after the loop so the usage chunk is
		// always captured before we signal completion.
		if len(tcMap) > 0 {
			emitToolCalls()
			if finishReason == "" {
				finishReason = "tool_use"
			}
		} else {
			emitUpToSentence(true)
			if finishReason == "" {
				finishReason = "end_turn"
			}
		}
		ch <- StreamEvent{
			Type:         "done",
			FinishReason: finishReason,
			InputTokens:  usageIn,
			OutputTokens: usageOut,
		}
	}()

	return ch, nil
}

// createStreamWithRetry opens the chat-completion stream, retrying the initial
// request on 429 and 5xx with exponential backoff (up to 5 attempts). Retrying is
// safe here because no events have been emitted yet — a failure to open the stream
// is equivalent to never having started. Non-retryable errors (4xx other than 429)
// return immediately, and ctx cancellation aborts the wait.
func (p *OpenAIProvider) createStreamWithRetry(ctx context.Context, oreq openai.ChatCompletionRequest) (*openai.ChatCompletionStream, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		stream, err := p.client.CreateChatCompletionStream(ctx, oreq)
		if err == nil {
			return stream, nil
		}
		lastErr = err
		if !isRetryableOpenAI(err) {
			return nil, err
		}
		wait := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s, 8s, 16s
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// isRetryableOpenAI reports whether an error from the OpenAI client is a transient
// 429 (rate limit) or 5xx (server) that a retry might clear.
func isRetryableOpenAI(err error) bool {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode == http.StatusTooManyRequests || apiErr.HTTPStatusCode >= 500
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return reqErr.HTTPStatusCode == http.StatusTooManyRequests || reqErr.HTTPStatusCode >= 500
	}
	return false
}

// OpenAIModelInfo maps a model ID to known capability metadata (exported for use in handlers).
func OpenAIModelInfo(id string) ModelInfo { return openAIModelInfo(id) }

// openAIModelInfo maps a model ID to known capability metadata.
func openAIModelInfo(id string) ModelInfo {
	// Prefix-based lookup — newer models inherit from their family.
	// Fields: contextWindow, maxOutput, vision, thinking.
	type entry struct {
		prefix           string
		ctx, maxOut      int
		vision, thinking bool
	}
	table := []entry{
		{"o4", 200_000, 100_000, true, true},
		{"o3", 200_000, 100_000, false, true},
		{"o1", 200_000, 100_000, false, true},
		{"gpt-4o", 128_000, 16_384, true, false},
		{"gpt-4-turbo", 128_000, 4_096, true, false},
		{"gpt-4-vision", 128_000, 4_096, true, false},
		{"gpt-4", 8_192, 8_192, false, false},
		{"gpt-3.5-turbo-16k", 16_385, 4_096, false, false},
		{"gpt-3.5", 16_385, 4_096, false, false},
	}
	for _, e := range table {
		if strings.HasPrefix(id, e.prefix) {
			return ModelInfo{
				ID:                   id,
				Name:                 id,
				ContextWindow:        e.ctx,
				MaxOutputTokens:      e.maxOut,
				SupportsStreaming:    true,
				SupportsFunctionCall: true,
				SupportsVision:       e.vision,
				SupportsThinking:     e.thinking,
			}
		}
	}
	// Unknown / custom model — safe defaults, MaxOutputTokens 0 (no output cap; the
	// custom endpoint owner controls the configured budget).
	return ModelInfo{ID: id, Name: id, ContextWindow: 128_000, SupportsStreaming: true, SupportsFunctionCall: true}
}

func (p *OpenAIProvider) DiscoverModels(ctx context.Context) ([]ModelInfo, error) {
	list, err := p.client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(list.Models))
	for _, m := range list.Models {
		out = append(out, openAIModelInfo(m.ID))
	}
	return out, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func buildOpenAIMessages(system string, msgs []ChatMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: system,
		})
	}
	for _, m := range msgs {
		// User turn with tool results
		if m.Role == "user" && len(m.ToolResults) > 0 {
			for _, tr := range m.ToolResults {
				out = append(out, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					Content:    tr.Content,
					ToolCallID: tr.ToolUseID,
				})
				if tr.ImageData != "" {
					mediaType := tr.ImageMediaType
					if mediaType == "" {
						mediaType = "image/jpeg"
					}
					dataURL := "data:" + mediaType + ";base64," + tr.ImageData
					out = append(out, openai.ChatCompletionMessage{
						Role: openai.ChatMessageRoleUser,
						MultiContent: []openai.ChatMessagePart{
							{
								Type: openai.ChatMessagePartTypeImageURL,
								ImageURL: &openai.ChatMessageImageURL{
									URL: dataURL,
								},
							},
						},
					})
				}
			}
			continue
		}
		// Assistant turn with tool calls
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			var oaiCalls []openai.ToolCall
			for _, tc := range m.ToolCalls {
				input := tc.Input
				if input == nil {
					input = map[string]any{} // avoid marshaling nil to "null"
				}
				args, _ := json.Marshal(input)
				oaiCalls = append(oaiCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: string(args),
					},
				})
			}
			out = append(out, openai.ChatCompletionMessage{
				Role:      openai.ChatMessageRoleAssistant,
				Content:   m.Content,
				ToolCalls: oaiCalls,
			})
			continue
		}
		role := openai.ChatMessageRoleUser
		if m.Role == "assistant" {
			role = openai.ChatMessageRoleAssistant
		}
		if m.Role == "user" && len(m.Images) > 0 {
			var parts []openai.ChatMessagePart
			if m.Content != "" {
				parts = append(parts, openai.ChatMessagePart{
					Type: openai.ChatMessagePartTypeText,
					Text: m.Content,
				})
			}
			for _, img := range m.Images {
				mediaType := DetectBase64MediaType(img)
				parts = append(parts, openai.ChatMessagePart{
					Type: openai.ChatMessagePartTypeImageURL,
					ImageURL: &openai.ChatMessageImageURL{
						URL: "data:" + mediaType + ";base64," + img,
					},
				})
			}
			out = append(out, openai.ChatCompletionMessage{
				Role:         role,
				MultiContent: parts,
			})
		} else if m.Content != "" {
			out = append(out, openai.ChatCompletionMessage{
				Role:    role,
				Content: m.Content,
			})
		}
		// A plain turn with no content and no images (e.g. an interrupted assistant
		// reply) is dropped — it carries nothing, and strict OpenAI-compatible
		// servers reject empty messages. Ollama tolerates them; this matches that.
	}
	return out
}

func buildOpenAITools(tools []ToolDef) []openai.Tool {
	out := make([]openai.Tool, len(tools))
	for i, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		params := openai.FunctionDefinition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		}
		out[i] = openai.Tool{Type: openai.ToolTypeFunction, Function: &params}
	}
	return out
}

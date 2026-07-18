package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/config"
)

// OllamaProvider streams from Ollama's native /api/chat endpoint.
// Using the native endpoint (not the OpenAI-compat /v1 layer) lets us set
// num_ctx explicitly and receive complete tool-call objects rather than
// accumulating streamed argument deltas.
type OllamaProvider struct {
	base   string
	apiKey string
}

func NewOllamaProvider(endpoint, apiKey string) *OllamaProvider {
	base := strings.TrimRight(endpoint, "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	return &OllamaProvider{base: base, apiKey: apiKey}
}

func (p *OllamaProvider) Stream(ctx context.Context, req StreamRequest) (<-chan StreamEvent, error) {
	// buildOllamaMessages injects "images" into screenshot tool-result messages so
	// vision-capable Ollama models can actually see the captured screen.
	// The upstream history (req.Messages) is never mutated — images live only in
	// the per-call payload, exactly like the Python sidecar's _pending_screenshot pattern.
	msgs := buildOllamaMessages(req.System, req.Messages)

	// num_ctx is the effective runtime context window for Ollama. It must match
	// what the UI displays (see GetModelInfo) so the user's token gauge and
	// auto-compaction threshold reflect the real limit, not a model's advertised one.
	numCtx := config.GetSidecarSettings().EffectiveOllamaNumCtx()

	mkPayload := func(m []map[string]any) map[string]any {
		p := map[string]any{
			"model":    req.Model,
			"messages": m,
			"stream":   true,
			"options": map[string]any{
				"num_predict": req.MaxTokens,
				"num_ctx":     numCtx,
			},
		}
		// Set the think parameter only when thinking is explicitly requested.
		// Never send think: false — models that don't support the parameter
		// reject it with a 400 error. Omitting it is equivalent to false.
		switch req.ThinkLevel {
		case "high", "medium", "low":
			p["think"] = req.ThinkLevel
		case "true":
			p["think"] = true
		default:
			if req.ThinkingBudget > 0 {
				p["think"] = true
			}
		}
		if len(req.Tools) > 0 {
			p["tools"] = buildOllamaTools(req.Tools)
		}
		return p
	}

	ch := make(chan StreamEvent, 64)
	go func() {
		defer close(ch)
		err := p.stream(ctx, mkPayload(msgs), ch)
		if err == nil || ctx.Err() != nil {
			return
		}
		if strings.HasPrefix(err.Error(), "ollama 400:") {
			// Context too long → retry with progressively fewer messages (keep first
			// user msg + newest half each time, up to 3 attempts).
			if isContextTooLong(err) {
				trimmed := msgs
				for range 3 {
					trimmed = halveMsgs(trimmed)
					if len(trimmed) == 0 {
						break
					}
					if err2 := p.stream(ctx, mkPayload(trimmed), ch); err2 == nil || ctx.Err() != nil {
						return
					} else if !isContextTooLong(err2) {
						ch <- StreamEvent{Type: "error", Err: err2}
						return
					}
				}
				ch <- StreamEvent{Type: "error", Err: err}
				return
			}
			// 400 with images → the model doesn't support vision or the image is too
			// large. Strip images and retry once.
			if msgsHaveImages(msgs) {
				if err2 := p.stream(ctx, mkPayload(stripMsgImages(msgs)), ch); err2 != nil && ctx.Err() == nil {
					ch <- StreamEvent{Type: "error", Err: err2}
				}
				return
			}
		}
		ch <- StreamEvent{Type: "error", Err: err}
	}()
	return ch, nil
}

// isContextTooLong reports whether an Ollama 400 error is a context-window or
// request-body overflow — both are solved by trimming the message history.
func isContextTooLong(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "too long") ||
		strings.Contains(s, "context length") ||
		strings.Contains(s, "context_length") ||
		strings.Contains(s, "request body too large")
}

// halveMsgs drops the oldest half of the middle messages, keeping the first
// message (system/user context) and the most-recent half intact.
func halveMsgs(msgs []map[string]any) []map[string]any {
	if len(msgs) <= 2 {
		return msgs
	}
	keep := len(msgs) / 2
	if keep < 1 {
		keep = 1
	}
	return append(msgs[:1:1], msgs[len(msgs)-keep:]...)
}

// msgsHaveImages reports whether any message in the slice carries an "images" field.
func msgsHaveImages(msgs []map[string]any) bool {
	for _, m := range msgs {
		if m["images"] != nil {
			return true
		}
	}
	return false
}

// stripMsgImages returns a copy of msgs with the "images" field removed from every entry.
func stripMsgImages(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		if m["images"] == nil {
			out[i] = m
			continue
		}
		without := make(map[string]any, len(m))
		for k, v := range m {
			if k != "images" {
				without[k] = v
			}
		}
		out[i] = without
	}
	return out
}

// stream performs one /api/chat call with 429-retry (up to 8 attempts).
func (p *OllamaProvider) stream(ctx context.Context, payload map[string]any, ch chan<- StreamEvent) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	for attempt := range 8 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/api/chat", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if p.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}

		if resp.StatusCode == 429 {
			resp.Body.Close()
			wait := time.Duration(1<<attempt) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode == 400 {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			msg := strings.TrimSpace(string(detail))
			if msg == "" {
				msg = "request body may be too large or malformed"
			}
			return fmt.Errorf("ollama 400: %s", msg)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("ollama returned HTTP %d", resp.StatusCode)
		}

		err = p.readStream(ctx, resp, ch)
		resp.Body.Close()
		return err
	}
	return fmt.Errorf("ollama busy (429) after 8 retries")
}

// readStream reads NDJSON lines from the /api/chat response and emits StreamEvents.
// A 2-minute inactivity watchdog closes the body if no bytes arrive, preventing
// silent hung connections from blocking the executor forever.
func (p *OllamaProvider) readStream(ctx context.Context, resp *http.Response, ch chan<- StreamEvent) error {
	const inactivityTimeout = 2 * time.Minute
	// Closing the body from the watchdog goroutine unblocks scanner.Scan().
	watchdog := time.AfterFunc(inactivityTimeout, func() { resp.Body.Close() })
	defer watchdog.Stop()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	// Accumulate visible text and emit at sentence boundaries so each token
	// event is a complete sentence rather than individual words.
	// Force-flush (emit everything) before tool calls and at stream end.
	var textBuf strings.Builder

	// Inline <think> tag state — used when the model emits thinking inside
	// the content field rather than the native message.thinking field.
	var thinkBuf strings.Builder
	inThink := false

	// emitUpToSentence emits text up to the last natural sentence boundary.
	// With force=true it emits everything regardless of boundary.
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
		// Walk backwards to find the last safe cut point:
		// ". ", "! ", "? ", ".\n", "!\n", "?\n", or "\n\n".
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

	// flushThinkHoldback drains the extractThinkTags holdback buffer.
	// When !inThink the holdback contains visible text that was withheld to
	// guard against a split <think> tag — move it into textBuf so it is
	// included in the next force-flush.  When inThink the holdback is
	// unfinished thinking content — emit it as a thinking event.
	flushThinkHoldback := func() {
		if thinkBuf.Len() == 0 {
			return
		}
		if inThink {
			ch <- StreamEvent{Type: "thinking", Text: thinkBuf.String()}
		} else {
			textBuf.WriteString(thinkBuf.String())
		}
		thinkBuf.Reset()
		inThink = false
	}

	for scanner.Scan() {
		watchdog.Reset(inactivityTimeout) // data arrived — push the deadline forward
		if ctx.Err() != nil {
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var chunk struct {
			Message struct {
				Content   string           `json:"content"`
				Thinking  string           `json:"thinking"`
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
			Done            bool   `json:"done"`
			Error           string `json:"error"`
			PromptEvalCount int    `json:"prompt_eval_count"` // input tokens
			EvalCount       int    `json:"eval_count"`        // output tokens
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			return fmt.Errorf("ollama: %s", chunk.Error)
		}

		// Native thinking field — used by deepseek-r1, qwq, and Ollama models
		// that properly separate thinking from content.
		// Some models wrap the thinking in <think> tags even in this field — strip them.
		if chunk.Message.Thinking != "" {
			raw := chunk.Message.Thinking
			raw = strings.TrimPrefix(raw, "<think>")
			raw = strings.TrimSuffix(raw, "</think>")
			if raw != "" {
				ch <- StreamEvent{Type: "thinking", Text: raw}
			}
		}

		// Content field — either plain text or (for some cloud models like GLM)
		// text with inline <think>...</think> tags. When the native Thinking field
		// is empty, extract inline thinking tags before buffering visible text.
		if chunk.Message.Content != "" {
			if chunk.Message.Thinking == "" {
				visible, thinking := extractThinkTags(&thinkBuf, &inThink, chunk.Message.Content)
				if thinking != "" {
					ch <- StreamEvent{Type: "thinking", Text: thinking}
				}
				if visible != "" {
					textBuf.WriteString(visible)
					emitUpToSentence(false)
				}
			} else {
				textBuf.WriteString(chunk.Message.Content)
				emitUpToSentence(false)
			}
		}

		// Tool calls arrive as complete objects (not deltas).
		// Force-flush buffered text first so the complete sentence precedes tool events.
		for _, tc := range chunk.Message.ToolCalls {
			flushThinkHoldback()
			emitUpToSentence(true)
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			var input map[string]any
			switch v := fn["arguments"].(type) {
			case map[string]any:
				input = v
			case string:
				if v != "" {
					_ = json.Unmarshal([]byte(v), &input)
				}
			}
			// Nil arguments (no-parameter tools, or null/empty sent by Ollama) must
			// be returned as {} not nil — sending null back causes Ollama 400.
			if input == nil {
				input = map[string]any{}
			}
			ch <- StreamEvent{
				Type:      "tool_call",
				ToolID:    uuid.NewString(),
				ToolName:  name,
				ToolInput: input,
			}
		}

		if chunk.Done {
			// Flush the extractThinkTags holdback (visible text tail or unclosed thinking).
			flushThinkHoldback()
			emitUpToSentence(true)
			if chunk.PromptEvalCount > 0 || chunk.EvalCount > 0 {
				ch <- StreamEvent{
					Type:         "done",
					InputTokens:  chunk.PromptEvalCount,
					OutputTokens: chunk.EvalCount,
				}
			}
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return nil // user cancelled — not an error
		}
		return fmt.Errorf("ollama stream stalled (no data for %v) — connection may have dropped", inactivityTimeout)
	}
	return nil
}

func (p *OllamaProvider) DiscoverModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama /api/tags returned %d", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(body.Models))
	for _, m := range body.Models {
		out = append(out, ModelInfo{
			ID:                   m.Name,
			Name:                 m.Name,
			SupportsStreaming:    true,
			SupportsFunctionCall: true,
		})
	}
	return out, nil
}

// GetModelInfo calls /api/show to retrieve context window, vision, and tool capabilities
// for a specific model. Falls back to safe defaults on error.
func (p *OllamaProvider) GetModelInfo(ctx context.Context, model string) ModelInfo {
	payload, _ := json.Marshal(map[string]string{"model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return ModelInfo{ID: model, Name: model, SupportsStreaming: true, SupportsFunctionCall: true}
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ModelInfo{ID: model, Name: model, SupportsStreaming: true, SupportsFunctionCall: true}
	}
	defer resp.Body.Close()

	var body struct {
		Details struct {
			Families []string `json:"families"`
		} `json:"details"`
		ModelInfo    map[string]any `json:"model_info"`
		Capabilities []string       `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ModelInfo{ID: model, Name: model, SupportsStreaming: true, SupportsFunctionCall: true}
	}

	// Context length: scan model_info for any key ending in ".context_length"
	contextLen := 0
	for k, v := range body.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if n, ok := v.(float64); ok && int(n) > contextLen {
				contextLen = int(n)
			}
		}
	}

	// Report the *effective* window the runtime actually enforces, not the model's
	// advertised maximum: Ollama is called with num_ctx (see Stream), so anything
	// beyond it is silently dropped. effective = min(advertised, num_ctx), falling
	// back to num_ctx when the model doesn't advertise a context length.
	numCtx := config.GetSidecarSettings().EffectiveOllamaNumCtx()
	effectiveCtx := numCtx
	if contextLen > 0 && contextLen < numCtx {
		effectiveCtx = contextLen
	}

	// Vision: explicit capability list (Ollama ≥ 0.5) or "clip" in model families
	hasVision := false
	hasTools := false
	for _, c := range body.Capabilities {
		switch c {
		case "vision":
			hasVision = true
		case "tools":
			hasTools = true
		}
	}
	if !hasVision {
		for _, f := range body.Details.Families {
			if f == "clip" || strings.Contains(f, "vision") {
				hasVision = true
				break
			}
		}
	}
	// If capabilities list is absent, assume tools are supported (most modern models)
	if len(body.Capabilities) == 0 {
		hasTools = true
	}

	return ModelInfo{
		ID:                   model,
		Name:                 model,
		ContextWindow:        effectiveCtx,
		SupportsStreaming:    true,
		SupportsFunctionCall: hasTools,
		SupportsVision:       hasVision,
	}
}

// ── message builder ───────────────────────────────────────────────────────────

func buildOllamaMessages(system string, msgs []ChatMessage) []map[string]any {
	var out []map[string]any
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			if len(m.ToolResults) > 0 {
				// Each tool result becomes a separate "tool" role message.
				for _, tr := range m.ToolResults {
					entry := map[string]any{
						"role":    "tool",
						"content": tr.Content,
					}
					if tr.Name != "" {
						entry["name"] = tr.Name
					}
					// Inject the screenshot image so vision-capable Ollama models can
					// actually see it. The stored history never carries ImageData — it
					// is only present on the in-memory ToolResult for the current turn,
					// matching the Python sidecar's _pending_screenshot pattern.
					// If the model doesn't support vision, Stream() will catch the 400
					// and retry after stripping images via stripMsgImages().
					if tr.ImageData != "" {
						entry["images"] = []string{tr.ImageData}
					}
					out = append(out, entry)
				}
			} else {
				entry := map[string]any{"role": "user", "content": m.Content}
				if len(m.Images) > 0 {
					entry["images"] = m.Images
				}
				out = append(out, entry)
			}
		case "assistant":
			entry := map[string]any{"role": "assistant", "content": m.Content}
			if len(m.ToolCalls) > 0 {
				tcs := make([]map[string]any, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					args := tc.Input
					if args == nil {
						args = map[string]any{}
					}
					tcs = append(tcs, map[string]any{
						"function": map[string]any{
							"name":      tc.Name,
							"arguments": args,
						},
					})
				}
				entry["tool_calls"] = tcs
			}
			out = append(out, entry)
		}
	}
	return out
}

func buildOllamaTools(tools []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  schema,
			},
		})
	}
	return out
}

// extractThinkTags scans chunk for <think>...</think> markers, routing thinking
// text to the thinking return value and everything else to visible. State is
// carried across chunk boundaries via buf and inThink pointers so a tag split
// across two chunks is handled correctly.
func extractThinkTags(buf *strings.Builder, inThink *bool, chunk string) (visible, thinking string) {
	const open, close = "<think>", "</think>"
	s := chunk
	for len(s) > 0 {
		if !*inThink {
			// Prepend any holdback from the previous chunk so a <think> tag split
			// across two chunks (e.g. "<thi" … "nk>") is detected correctly.
			if buf.Len() > 0 {
				s = buf.String() + s
				buf.Reset()
			}
			if idx := strings.Index(s, open); idx >= 0 {
				visible += s[:idx]
				s = s[idx+len(open):]
				*inThink = true
			} else {
				// Guard against a split opening tag spanning two chunks.
				safe := len(s)
				if safe > len(open)-1 {
					safe = len(s) - (len(open) - 1)
				} else {
					safe = 0
				}
				visible += s[:safe]
				buf.WriteString(s[safe:]) // hold the tail in case next chunk completes the tag
				s = ""
			}
		} else {
			if idx := strings.Index(s, close); idx >= 0 {
				thinking += buf.String() + s[:idx]
				buf.Reset()
				s = s[idx+len(close):]
				*inThink = false
			} else {
				// Partial close tag may be split — hold tail, emit the safe prefix.
				safe := len(s)
				if safe > len(close)-1 {
					safe = len(s) - (len(close) - 1)
				} else {
					safe = 0
				}
				thinking += buf.String() + s[:safe]
				buf.Reset()
				buf.WriteString(s[safe:])
				s = ""
			}
		}
	}
	return
}


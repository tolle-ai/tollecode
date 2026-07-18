package stdio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/ai"
)

func handleGetMemory(state *ServerState, cmd map[string]any) {
	// TODO Phase 6: read ~/.agent/memory/index.jsonl
	Emit(map[string]any{"type": "memory", "enabled": false, "entries": []any{}})
}

func handleDeleteMemory(state *ServerState, cmd map[string]any) {
	// TODO Phase 6
	Emit(map[string]any{"type": "memory", "enabled": false, "entries": []any{}})
}

func handleToggleMemory(state *ServerState, cmd map[string]any) {
	enabled, _ := cmd["enabled"].(bool)
	// TODO Phase 6: persist to .agent/config.json
	Emit(map[string]any{"type": "memory", "enabled": enabled, "entries": []any{}})
}

func handleAddMemory(state *ServerState, cmd map[string]any) {
	// TODO Phase 6: append to memory index
	Emit(map[string]any{"type": "memory", "enabled": false, "entries": []any{}})
}

func handleQueryMemory(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	query, _ := cmd["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		Emit(map[string]any{"type": "memory_query_result", "text": "Please provide a question or instruction."})
		return
	}

	records := loadIndex(ws)
	if len(records) == 0 {
		Emit(map[string]any{"type": "memory_query_result", "text": "No memory entries found yet. They are created automatically as you work with the agent."})
		return
	}

	// Build memory context block
	var memCtx strings.Builder
	memCtx.WriteString("Workspace memory entries:\n\n")
	for i, r := range records {
		ts := r.Timestamp
		if len(ts) > 10 {
			ts = ts[:10]
		}
		fmt.Fprintf(&memCtx, "%d. [%s] %s\n", i+1, ts, r.Summary)
		if len(r.Keywords) > 0 {
			fmt.Fprintf(&memCtx, "   Keywords: %s\n", strings.Join(r.Keywords, ", "))
		}
		if detail := readDetail(ws, r.File); detail != "" {
			fmt.Fprintf(&memCtx, "   Detail: %s\n", detail)
		}
	}

	// Pick first available provider
	ids := ai.Global.IDs()
	if len(ids) == 0 {
		Emit(map[string]any{"type": "memory_query_result", "text": "No AI provider configured. Add one in Settings to use memory queries."})
		return
	}
	providerID := ids[0]
	provider := ai.Global.Get(providerID)
	model := ai.Global.DefaultModel(providerID)

	systemPrompt := "You are a knowledgeable team lead giving a verbal status update to a colleague. You have access to memory entries from past agent sessions in this workspace. When answering, speak naturally and conversationally — like you're talking, not writing a report. Do NOT use markdown: no headers (##), no bullet points (-), no bold (**), no code blocks. Write in flowing sentences and short paragraphs. Be warm, direct, and human."

	userContent := memCtx.String() + "\n---\n\nColleague asks: " + query

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := provider.Stream(ctx, ai.StreamRequest{
		Model:     model,
		System:    systemPrompt,
		Messages:  []ai.ChatMessage{{Role: "user", Content: userContent}},
		MaxTokens: 1024,
	})
	if err != nil {
		Emit(map[string]any{"type": "memory_query_result", "text": "Failed to reach the AI provider: " + err.Error()})
		return
	}

	var buf strings.Builder
	for ev := range ch {
		switch ev.Type {
		case "token":
			buf.WriteString(ev.Text)
		case "error":
			Emit(map[string]any{"type": "memory_query_result", "text": "AI error: " + ev.Err.Error()})
			return
		}
	}

	text := strings.TrimSpace(buf.String())
	if text == "" {
		text = "No response from the AI provider."
	}
	Emit(map[string]any{"type": "memory_query_result", "text": text})
}

package stdio

import (
	"context"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/session"
)

// handleGetSessionSummary returns file snapshots for a session.
func handleGetSessionSummary(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)

	messageIDs, _ := agent.ListSnapshots(sessionID)
	snapshots := make([]map[string]any, 0, len(messageIDs))
	for _, id := range messageIDs {
		snapshots = append(snapshots, map[string]any{"messageId": id})
	}

	Emit(map[string]any{
		"type":      "session_summary",
		"sessionId": sessionID,
		"snapshots": snapshots,
	})
}

// handleRestoreToMessage restores workspace files from the pre-turn snapshot
// taken before the given message. No git commits are created or destroyed.
func handleRestoreToMessage(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	messageID, _ := cmd["message_id"].(string)
	ws := workspaceFromCmd(state, cmd)
	if ws == "" {
		state.mu.Lock()
		ws = state.Workspace
		state.mu.Unlock()
	}

	if err := agent.RestoreSnapshot(ws, sessionID, messageID); err != nil {
		Emit(map[string]any{"type": "error", "message": "restore failed: " + err.Error()})
		return
	}

	Emit(map[string]any{
		"type":      "session_restored",
		"ok":        true,
		"messageId": messageID,
	})
}

// handleCompactSession runs an LLM summarisation over the session history.
func handleCompactSession(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	ws := workspaceFromCmd(state, cmd)
	if ws == "" {
		state.mu.Lock()
		ws = state.Workspace
		state.mu.Unlock()
	}

	s, err := session.Load(ws, sessionID)
	if err != nil {
		Emit(map[string]any{"type": "compact_session_result", "error": "session not found: " + err.Error()})
		return
	}

	// Build a text dump of the conversation
	var sb strings.Builder
	for _, m := range s.Messages {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role != "" && content != "" {
			sb.WriteString(strings.ToUpper(role))
			sb.WriteString(": ")
			sb.WriteString(content)
			sb.WriteString("\n\n")
		}
	}
	history := sb.String()
	if len(history) > 40000 {
		history = history[:40000]
	}

	provider := ai.Global.Get(s.Provider)
	if provider == nil {
		// Fallback: return a plain text excerpt
		if len(history) > 2000 {
			history = history[:2000] + "…"
		}
		Emit(map[string]any{"type": "compact_session_result", "summary": history})
		return
	}

	prompt := "Summarise the following conversation between a user and an AI coding assistant. " +
		"Focus on: what was accomplished, which files were created or modified, and any outstanding items. " +
		"Be concise (3–5 sentences).\n\n" + history

	msgs := []ai.ChatMessage{{Role: "user", Content: prompt}}
	req := ai.StreamRequest{
		Model:    s.Model,
		System:   "You are a helpful assistant that summarises software development conversations.",
		Messages: msgs,
		// Reasoning ("thinking") models — common on Ollama — can spend a large
		// part of the budget thinking before emitting any final answer, so give
		// summarisation generous headroom rather than truncating mid-thought.
		MaxTokens: 2048,
	}

	ch, err := provider.Stream(context.Background(), req)
	if err != nil {
		Emit(map[string]any{"type": "compact_session_result", "error": err.Error()})
		return
	}

	var out, thinkOut strings.Builder
	for ev := range ch {
		switch ev.Type {
		case "token":
			out.WriteString(ev.Text)
		case "thinking":
			thinkOut.WriteString(ev.Text)
		}
	}
	summary := strings.TrimSpace(out.String())
	// A reasoning model may emit its whole response as "thinking" (or run out of
	// budget mid-thought) and produce little or no final answer. Without a
	// fallback the summary would be empty and the fork would carry no context, so
	// fall back to the thinking text, then to a raw excerpt of the conversation.
	if summary == "" {
		summary = strings.TrimSpace(thinkOut.String())
	}
	if summary == "" {
		summary = history
		if len(summary) > 2000 {
			summary = summary[:2000] + "…"
		}
	}

	// Persist the compact summary on the session header.
	// Original messages are left intact for history viewing.
	// CompactedMessageCount records exactly how many messages existed at compact
	// time — the executor slices Messages[count:] to get post-compact messages,
	// which is simpler and more reliable than timestamp string comparison.
	compactedAt := time.Now().UTC().Format(time.RFC3339)
	_, _ = session.UpdateFields(ws, sessionID, map[string]any{
		"compactedSummary":      summary,
		"compactedAt":           compactedAt,
		"compactedMessageCount": len(s.Messages),
	})

	Emit(map[string]any{"type": "compact_session_result", "summary": summary, "compactedAt": compactedAt})
}

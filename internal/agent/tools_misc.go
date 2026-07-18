package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tolle-ai/tollecode/internal/alerts"
	"github.com/tolle-ai/tollecode/internal/session"
)

func toolAskFollowupQuestion(ctx context.Context, cfg *Config, inp map[string]any) (string, string, bool) {
	question, _ := inp["question"].(string)
	if question == "" {
		return "Error: 'question' is required.", "", true
	}
	var suggestions []string
	if raw, ok := inp["suggestions"].([]any); ok {
		for _, s := range raw {
			if str, ok := s.(string); ok {
				suggestions = append(suggestions, str)
			}
		}
	}
	multiChoice, _ := inp["multi_choice"].(bool)
	if cfg.RequestClarification == nil {
		return "Clarification not available in this context. Proceed with your best judgment.", "", false
	}
	answer, ok := cfg.RequestClarification(ctx, question, suggestions, multiChoice)
	if !ok {
		return "User did not provide clarification (the session was cancelled). Proceed with your best judgment.", "", false
	}

	// Ensure callers always receive a JSON-serialized ClarificationAnswer.
	payload, err := json.Marshal(answer)
	if err != nil {
		return "Error serializing clarification answer.", "", true
	}
	return string(payload), "", false
}

// ClarificationAnswerFromLegacy converts a plain string answer into a structured
// ClarificationAnswer. For backward compatibility, treat the raw string as the
// selected suggestion when it is one of the provided suggestions and there is no
// free text; otherwise store it in Details.
func ClarificationAnswerFromLegacy(answer string, suggestions []string, multiChoice bool) ClarificationAnswer {
	// If the client already sent a JSON object, parse it directly.
	var parsed ClarificationAnswer
	if err := json.Unmarshal([]byte(answer), &parsed); err == nil {
		return parsed
	}

	if multiChoice {
		// Legacy multi-choice clients comma-joined selections; preserve them as details
		// so no data is lost while still allowing future clients to use selected/details.
		return ClarificationAnswer{Selected: []string{}, Details: answer}
	}

	for _, s := range suggestions {
		if s == answer {
			return ClarificationAnswer{Selected: []string{answer}, Details: ""}
		}
	}
	return ClarificationAnswer{Selected: []string{}, Details: answer}
}

// looksLikeClarifyingQuestion reports whether an assistant's plain-text turn is
// (very likely) a clarifying question directed at the user, rather than a normal
// completion. Small/local models often phrase clarifications as prose instead of
// calling ask_followup_question; the executor uses this to nudge them back to the
// tool. Kept conservative to avoid false positives on summaries that merely
// contain a rhetorical question mid-paragraph: the *last* non-empty line must end
// with a question mark.
func looksLikeClarifyingQuestion(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// Find the last non-empty, trimmed line.
	lines := strings.Split(t, "\n")
	var last string
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			last = l
			break
		}
	}
	if last == "" {
		return false
	}
	// Strip common trailing markdown emphasis/quote markers before the check.
	last = strings.TrimRight(last, "*_`\"')] ")
	return strings.HasSuffix(last, "?")
}

// lastQuestionLine returns the last non-empty line of an assistant's prose turn —
// the actual question, when looksLikeClarifyingQuestion matched. Used to surface a
// concise question in the clarification UI when the model refused to call the
// tool, rather than dumping the whole paragraph into the overlay.
func lastQuestionLine(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return "Could you clarify how you'd like to proceed?"
	}
	lines := strings.Split(t, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			// Drop leading markdown list/heading/quote markers.
			return strings.TrimLeft(l, "#>*-_ ")
		}
	}
	return t
}

// clarificationAnswerToText renders a user's clarification answer as the plain
// text fed back into the model after we surfaced a prose question through the UI
// ourselves. A skipped question tells the model to proceed on its own judgment.
func clarificationAnswerToText(a ClarificationAnswer) string {
	parts := make([]string, 0, 2)
	if len(a.Selected) > 0 {
		parts = append(parts, strings.Join(a.Selected, ", "))
	}
	if d := strings.TrimSpace(a.Details); d != "" {
		parts = append(parts, d)
	}
	if len(parts) == 0 {
		return "(The user skipped the question — proceed with your best judgment.)"
	}
	return strings.Join(parts, " — ")
}

func toolCreatePlan(ctx context.Context, cfg *Config, inp map[string]any) string {
	name, _ := inp["name"].(string)
	content, _ := inp["content"].(string)
	if name == "" {
		return "Error: 'name' is required."
	}
	switch cfg.checkPermission(ctx, "file", "create_plan: "+name) {
	case permUnavailable:
		return "File write permission is not available in this context. Do not retry or try alternative approaches. Inform the user if this capability is needed."
	case permDenied:
		if cfg.EmitEvent != nil {
			cfg.EmitEvent(map[string]any{"type": "permission_denied", "tool": "create_plan", "detail": name})
		}
		return "Permission denied by the user. Do NOT retry this operation, do not try alternative approaches, and do not ask for permission again. Move on to tasks that don't require this permission, or inform the user what you need."
	}
	workspace := cfg.Workspace
	// Sanitize name: keep alphanumeric, dash, underscore
	var safe strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			safe.WriteRune(c)
		} else {
			safe.WriteRune('-')
		}
	}
	safeName := safe.String()
	dir := filepath.Join(workspace, ".agent", "plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "Error creating plans directory: " + err.Error()
	}
	path := filepath.Join(dir, safeName+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "Error writing plan: " + err.Error()
	}
	return fmt.Sprintf("Plan created: .agent/plans/%s.md", safeName)
}

func toolFinishTask(cfg *Config, inp map[string]any, isTeamLead bool) string {
	summary, _ := inp["summary"].(string)
	if summary == "" {
		summary = "Task complete."
	}

	// Block finish_task while any delegated sub-agent is still running.
	// This prevents a team lead from marking the session done before delegated
	// work has actually completed, which would leave the UI showing stale
	// active workflows.
	if cfg != nil && cfg.subAgents != nil {
		running := cfg.subAgents.runningEntries()
		if len(running) > 0 {
			lines := []string{
				"BLOCKED: finish_task called while delegated sub-agents are still running.",
				"",
				"You MUST wait for all delegated agents to complete before finishing.",
				"Call wait_for_team to wait for all agents, or wait_for_agent for a specific one, then retry finish_task.",
				"",
				"Still-running agents:",
			}
			for _, e := range running {
				role := e.role
				if role == "" {
					role = e.id
				}
				lines = append(lines, fmt.Sprintf("  - role=%s session_id=%s", role, e.id))
			}
			return strings.Join(lines, "\n")
		}
	}

	todos, err := session.GetTodos(cfg.Workspace, cfg.SessionID)
	if err == nil && len(todos) > 0 {
		var incomplete []session.Todo
		for _, t := range todos {
			if t.Status != "completed" {
				incomplete = append(incomplete, t)
			}
		}
		if len(incomplete) > 0 && !isTeamLead {
			lines := []string{
				"BLOCKED: finish_task called with incomplete todos.",
				"",
				"You MUST continue working. Use TodoWrite to mark completed items as",
				"'completed' and finish any genuinely incomplete ones, then retry finish_task.",
				"",
				"Incomplete todos:",
			}
			for _, t := range incomplete {
				lines = append(lines, fmt.Sprintf("  [%s] (id=%s) %s", t.Status, t.ID, t.Text))
			}
			lines = append(lines, "\nCall TodoWrite with all todos set to 'completed', then call finish_task again.")
			return strings.Join(lines, "\n")
		}
	}
	_, _ = session.UpdateFields(cfg.Workspace, cfg.SessionID, map[string]any{"status": "done"})
	return summary
}

func toolTaskOutOfScope(workspace, sessionID string, inp map[string]any) string {
	reason, _ := inp["reason"].(string)
	if reason == "" {
		reason = "Task is outside this agent's skill set."
	}
	_, _ = session.UpdateFields(workspace, sessionID, map[string]any{"status": "out_of_scope"})
	return "ABORTED: " + reason
}

// toolTodoWrite replaces the session's todo list. allowMultipleInProgress is
// true for a team-lead session (cfg.TeamMemberIDs set), where several todos can
// legitimately be in_progress at once — one per delegated member.
func toolTodoWrite(workspace, sessionID string, inp map[string]any, allowMultipleInProgress bool) string {
	raw, ok := inp["todos"].([]any)
	if !ok {
		return "Error: 'todos' must be a list."
	}
	var todos []session.Todo
	inProgressCount := 0
	seenIDs := make(map[string]bool)
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return "Error: each todo must be an object."
		}
		// `content` is the only required field. Accept a couple of common aliases
		// (`text`, `task`) so a slightly-off tool call still succeeds.
		content, _ := m["content"].(string)
		if content == "" {
			if alt, _ := m["text"].(string); alt != "" {
				content = alt
			} else if alt, _ := m["task"].(string); alt != "" {
				content = alt
			}
		}
		if content == "" {
			return "Error: each todo must have 'content'."
		}
		// `id` is optional — auto-generate a stable, positional id when omitted
		// (and de-dupe) so the agent isn't forced to invent ids.
		id, _ := m["id"].(string)
		if id == "" || seenIDs[id] {
			id = fmt.Sprintf("todo-%d", i+1)
		}
		seenIDs[id] = true
		status, _ := m["status"].(string)
		if status != "pending" && status != "in_progress" && status != "completed" {
			status = "pending"
		}
		priority, _ := m["priority"].(string)
		if priority != "high" && priority != "medium" && priority != "low" {
			priority = "medium"
		}
		if status == "in_progress" {
			inProgressCount++
		}
		todos = append(todos, session.Todo{ID: id, Text: content, Status: status, Priority: priority})
	}
	// Single-in-progress discipline applies to a solo agent only. A team lead
	// tracks one in_progress item per active member, so multiple is expected.
	if !allowMultipleInProgress && inProgressCount > 1 {
		return "Error: only one todo can be 'in_progress' at a time."
	}
	if err := session.SetTodos(workspace, sessionID, todos); err != nil {
		return "Error saving todos: " + err.Error()
	}
	return formatTodoList(todos)
}

func toolTodoRead(workspace, sessionID string) string {
	todos, err := session.GetTodos(workspace, sessionID)
	if err != nil {
		return "No todos."
	}
	return formatTodoList(todos)
}

func formatTodoList(todos []session.Todo) string {
	if len(todos) == 0 {
		return "No todos."
	}
	icons := map[string]string{"pending": "○", "in_progress": "◐", "completed": "●"}
	labels := map[string]string{"high": "H", "medium": "M", "low": "L"}
	lines := []string{"Current todos:"}
	for _, t := range todos {
		icon := icons[t.Status]
		if icon == "" {
			icon = "○"
		}
		pl := labels[t.Priority]
		if pl == "" {
			pl = "M"
		}
		lines = append(lines, fmt.Sprintf("  %s [%s] (id=%s) %s", icon, pl, t.ID, t.Text))
	}
	return strings.Join(lines, "\n")
}

func toolSendAlert(cfg *Config, inp map[string]any) string {
	message, _ := inp["message"].(string)
	if message == "" {
		return "Error: 'message' is required."
	}
	alerts.Publish(cfg.SessionID, cfg.Workspace, message, cfg.SessionID)
	return "Alert sent."
}

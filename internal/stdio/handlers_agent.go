package stdio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
	"github.com/tolle-ai/tollecode/internal/todo"
)

// handleSendMessage starts (or restarts) the agent task for a session.
// If a stream is currently live and the caller does not pass interrupt:true,
// the message is queued and will be processed once the stream ends. This prevents
// accidental interruption of an ongoing agent response when the user types a follow-up.
func handleSendMessage(state *ServerState, cmd map[string]any) {
	message, _ := cmd["message"].(string)
	if message == "" {
		return
	}
	sessionID, _ := cmd["session_id"].(string)
	if sessionID == "" {
		state.mu.Lock()
		sessionID = state.SessionID
		state.mu.Unlock()
	}
	if sessionID == "" {
		Emit(map[string]any{"type": "error", "message": "No active session. Create a session first."})
		return
	}

	workspace := workspaceFromCmd(state, cmd)
	interrupt, _ := cmd["interrupt"].(bool)

	// Explicit interrupt cancels any current turn and starts immediately.
	if interrupt || !isSessionLiveStreaming(sessionID) {
		state.processPendingMessages(sessionID)
		startAgentTurn(state, sessionID, workspace, cmd)
		return
	}

	// Live stream is active and no explicit interrupt: queue the message.
	state.mu.Lock()
	state.pendingMessages[sessionID] = append(state.pendingMessages[sessionID], pendingMessage{cmd: cmd})
	needTimer := !state.pendingMessageTimers[sessionID]
	state.pendingMessageTimers[sessionID] = true
	state.mu.Unlock()

	if needTimer {
		go state.runPendingMessageDelay(sessionID)
	}
}

// isSessionLiveStreaming reports whether sessionID currently has an in-progress
// agent stream. It checks the active-sessions registry and the in-memory bus
// buffer for non-terminal events after the latest terminal event.
func isSessionLiveStreaming(sessionID string) bool {
	active := false
	for _, e := range session.ListActive() {
		if e.SessionID == sessionID && e.Status == "running" {
			active = true
			break
		}
	}
	if !active {
		return false
	}
	return session.Global.HasPendingLiveEvents(sessionID)
}

// processPendingMessages drains any queued messages for sessionID and starts the
// most recent one. Older queued messages are discarded because the user only
// wants the latest prompt to run.
func (s *ServerState) processPendingMessages(sessionID string) {
	s.mu.Lock()
	queue := s.pendingMessages[sessionID]
	if len(queue) == 0 {
		delete(s.pendingMessages, sessionID)
		delete(s.pendingMessageTimers, sessionID)
		s.mu.Unlock()
		return
	}
	pm := queue[len(queue)-1]
	delete(s.pendingMessages, sessionID)
	delete(s.pendingMessageTimers, sessionID)
	s.mu.Unlock()

	cmd := pm.cmd
	workspace := workspaceFromCmd(s, cmd)
	startAgentTurn(s, sessionID, workspace, cmd)
}

// runPendingMessageDelay waits for the live stream to end, then starts any
// queued message. It reschedules itself while the stream remains live and stops
// when the queue is empty or cancelled.
func (s *ServerState) runPendingMessageDelay(sessionID string) {
	delay := config.GetSidecarSettings().EffectiveUserMessageDelay()
	for {
		time.Sleep(delay)

		s.mu.Lock()
		_, hasQueue := s.pendingMessages[sessionID]
		delete(s.pendingMessageTimers, sessionID)
		s.mu.Unlock()

		if !hasQueue {
			return
		}

		if isSessionLiveStreaming(sessionID) {
			// Stream still active: schedule another delay tick.
			s.mu.Lock()
			s.pendingMessageTimers[sessionID] = true
			s.mu.Unlock()
			continue
		}

		s.processPendingMessages(sessionID)
		return
	}
}

// resolveWorkspaceForSession guards against a workspace mismatch: if the resolved
// workspace doesn't actually contain the session file, fall back to the server's
// current workspace when that one does. This prevents a "session not found" error
// when a client creates a session in one workspace but sends the turn with a
// stale or empty workspacePath. If neither contains it, the original is returned
// so the existing not-found error surfaces.
func resolveWorkspaceForSession(state *ServerState, sessionID, workspace string) string {
	if _, err := session.Load(workspace, sessionID); err == nil {
		return workspace
	}
	state.mu.Lock()
	sw := state.Workspace
	state.mu.Unlock()
	if sw != "" && sw != workspace {
		if _, err := session.Load(sw, sessionID); err == nil {
			return sw
		}
	}
	return workspace
}

// startAgentTurn cancels any running task for the session, clears live/bus state,
// and starts a fresh agent goroutine.
func startAgentTurn(state *ServerState, sessionID, workspace string, cmd map[string]any) {
	// Ensure the workspace we run in actually owns this session (handles a client
	// that created it under a different/empty workspacePath than this turn carries).
	// Write it back into cmd so runAgentTask (which re-derives via workspaceFromCmd)
	// and every downstream lookup use the same corrected workspace.
	workspace = resolveWorkspaceForSession(state, sessionID, workspace)
	cmd["workspacePath"] = workspace
	// Cancel any running task for this session BEFORE clearing the live state.
	// The agent goroutine emits a "cancelled" event on context cancellation,
	// which writes to both the live JSONL file and the in-memory bus. We need
	// to wait for those stale events to be written before clearing them.
	//
	// cancelSession is a no-op if there is no registered task for the session,
	// so it is safe to call unconditionally.
	state.cancelSession(sessionID)

	// When the caller explicitly interrupts, any queued pending messages are
	// stale now that this turn is starting immediately. Drain them before
	// clearing the live state so a queued message doesn't replace the command
	// we are about to execute.
	state.processPendingMessages(sessionID)

	// Clear the live file and in-memory bus buffer AFTER the old agent has
	// finished, so any stale "cancelled" / "done" events from the previous turn
	// are removed. We also clear the terminal state so a currently-connected WS
	// client doesn't see a stale terminal and close with code 1000 before the new
	// turn has a chance to publish real events.
	//
	// Order matters: clear the in-memory bus FIRST so any buffered stale terminal
	// cannot be delivered to a currently-connected WS client; then truncate the
	// JSONL replay file. We do not publish session_reset until after this atomic
	// clear, so the client reconnects from a clean offset 0.
	session.Global.ClearBuffer(sessionID)
	session.ClearLiveEvents(sessionID)

	// Write session_reset as the very first event in the now-empty JSONL so
	// any WS client that connects AFTER the interrupt (including a reconnecting
	// client that missed the live bus broadcast) always sees it as the boundary
	// between stale old-turn events and live new-turn events. Without this,
	// a client connecting mid-interrupt would receive Phase-1 old tokens and
	// have no way to tell them apart from new-turn tokens.
	session.AppendLiveEvent(sessionID, map[string]any{
		"type":       "session_reset",
		"session_id": sessionID,
	})

	// Also publish to the live bus so any currently-connected WS client receives
	// the reset signal in Phase 4. Clear the buffer immediately after so newly-
	// connecting clients only see it via Phase-1 (the JSONL written above).
	session.Global.Publish(sessionID, map[string]any{
		"type":       "session_reset",
		"session_id": sessionID,
	})
	session.Global.ClearBuffer(sessionID)

	// If the caller specifies a provider/model, update the session so the
	// executor uses the composer-selected values rather than stale stored ones.
	providerUpdates := map[string]any{"status": "running"}
	if p, ok := cmd["provider"].(string); ok && p != "" {
		providerUpdates["provider"] = p
	}
	if m, ok := cmd["model"].(string); ok && m != "" {
		providerUpdates["model"] = m
	}
	session.UpdateFields(workspace, sessionID, providerUpdates)
	session.RegisterSession(sessionID, workspace, "desktop")
	// Ensure every continued session is represented by a trackable todo task.
	agentID, _ := cmd["agentId"].(string)
	ensureLinkedTodoForSession(workspace, sessionID, messageFromCmd(cmd), agentID, extractTeamMemberIds(cmd))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	state.registerTask(sessionID, cancel, done)

	go func() {
		defer close(done)
		defer state.removeTask(sessionID)
		runAgentTask(ctx, state, sessionID, cmd)
	}()
}

// extractTeamMemberIds parses the teamMemberIds field from a send_message payload.
func extractTeamMemberIds(cmd map[string]any) []string {
	var ids []string
	if rawIDs, ok := cmd["teamMemberIds"].([]any); ok {
		for _, v := range rawIDs {
			if s, ok := v.(string); ok && s != "" {
				ids = append(ids, s)
			}
		}
	}
	return ids
}

// messageFromCmd extracts the message string from a send_message payload.
func messageFromCmd(cmd map[string]any) string {
	m, _ := cmd["message"].(string)
	return m
}

func handleCancel(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	if sessionID == "" {
		state.mu.Lock()
		sessionID = state.SessionID
		state.mu.Unlock()
	}
	// Fire cancel without blocking — the goroutine finishes cleanup in the background.
	// Emitting `cancelled` immediately ensures the stdio loop is never frozen and the
	// UI updates instantly regardless of how long the agent takes to unwind.
	if sessionID != "" {
		state.cancelSessionNoWait(sessionID)
	}
	Emit(map[string]any{"type": "cancelled", "session_id": sessionID})
	if sessionID != "" {
		session.Global.Publish(sessionID, map[string]any{"type": "cancelled", "session_id": sessionID})
		// Auto-close any linked todo so the workflow panel does not stay "running"
		// after the user explicitly stops the agent.
		todo.CloseLinkedTodoBySession(sessionID)
	}
}

func handleToolPermission(state *ServerState, cmd map[string]any) {
	allow, _ := cmd["allow"].(bool)
	allowAll, _ := cmd["allow_all"].(bool)
	sessionID, _ := cmd["session_id"].(string)
	if sessionID == "" {
		state.mu.Lock()
		sessionID = state.SessionID
		state.mu.Unlock()
	}
	ch := state.permQueue(sessionID)
	select {
	case ch <- permResponse{Allow: allow, AllowAll: allowAll}:
	default:
	}
}

func handleClarificationResponse(state *ServerState, cmd map[string]any) {
	requestID, _ := cmd["request_id"].(string)
	if requestID == "" {
		// Fallback: try "requestId" key for compatibility.
		requestID, _ = cmd["requestId"].(string)
	}
	if requestID == "" {
		return
	}

	var answer agent.ClarificationAnswer
	if raw, ok := cmd["answer"].(string); ok && raw != "" {
		// Legacy plain-string answer from older clients.
		suggestions := []string{}
		if rawSugs, ok := cmd["suggestions"].([]any); ok {
			for _, s := range rawSugs {
				if str, ok := s.(string); ok {
					suggestions = append(suggestions, str)
				}
			}
		}
		multiChoice, _ := cmd["multi_choice"].(bool)
		answer = agent.ClarificationAnswerFromLegacy(raw, suggestions, multiChoice)
	} else {
		// Structured answer from new clients.
		if rawSelected, ok := cmd["selected"].([]any); ok {
			for _, s := range rawSelected {
				if str, ok := s.(string); ok {
					answer.Selected = append(answer.Selected, str)
				}
			}
		}
		answer.Details, _ = cmd["details"].(string)
	}

	state.deliverClarificationResponse(requestID, answer)
}

// handleSystemPermissionResponse routes the frontend's answer (granted/denied) back
// to the desktop tool goroutine that's waiting on registerSysPermCh.
func handleSystemPermissionResponse(state *ServerState, cmd map[string]any) {
	requestID, _ := cmd["requestId"].(string)
	granted, _ := cmd["granted"].(bool)
	if requestID != "" {
		state.deliverSysPermResponse(requestID, granted)
	}
}

// handleCheckSystemPermission lets the frontend poll whether accessibility is
// currently granted. The sidecar re-runs its detection logic and replies
// synchronously with a system_permission_status event.
func handleCheckSystemPermission(state *ServerState, cmd map[string]any) {
	permType, _ := cmd["permission_type"].(string)
	var granted bool
	if permType == "accessibility" {
		granted = agent.CheckAccessibilityPermission()
	}
	Emit(map[string]any{
		"type":            "system_permission_status",
		"permission_type": permType,
		"granted":         granted,
	})
}

func handleMemoryPermission(state *ServerState, cmd map[string]any) {
	allow, _ := cmd["allow"].(bool)
	allowAll, _ := cmd["allow_all"].(bool)
	select {
	case state.memQueue <- permResponse{Allow: allow, AllowAll: allowAll}:
	default:
	}
}

// runAgentTask calls the agent executor for one user turn.
func runAgentTask(ctx context.Context, state *ServerState, sessionID string, cmd map[string]any) {
	message, _ := cmd["message"].(string)

	var images []string
	if raw, ok := cmd["images"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				images = append(images, s)
			}
		}
	}

	workspace := workspaceFromCmd(state, cmd)

	// Resolve lead-agent identity and system prompt.
	//
	// Priority order (highest first):
	//  1. Inline fields from the frontend (agentSystemPrompt / agentSkills) — always
	//     accurate because the frontend reads from its own LocalStorage, not agents.json.
	//  2. File-based lookup via LookupAgentCfg — fallback for callers that only pass agentId.
	//
	// This two-layer approach means the sidecar never silently falls back to the
	// generic system prompt when IDs are out of sync between LocalStorage and agents.json.
	var customInstructions string
	var agentSkillNames []string
	var teamMemberIDs []string

	// Display name of the lead/specialist agent. buildSystem anchors the identity
	// block ("You are **X**") on this, so it must be forwarded to the executor for
	// both single agents and team leads.
	agentName, _ := cmd["agentName"].(string)

	// Layer 1 — inline config sent by the frontend.
	if sp, _ := cmd["agentSystemPrompt"].(string); sp != "" {
		customInstructions = sp
	}
	if rawSkills, ok := cmd["agentSkills"].([]any); ok {
		for _, v := range rawSkills {
			if s, ok := v.(string); ok && s != "" {
				agentSkillNames = append(agentSkillNames, s)
			}
		}
	}

	// Layer 2 — file lookup, only when the inline config is missing.
	if customInstructions == "" {
		if agentID, _ := cmd["agentId"].(string); agentID != "" {
			if ac := agent.LookupAgentCfg(agentID); ac != nil {
				if ac.SystemPrompt != "" {
					customInstructions = ac.SystemPrompt
				} else if ac.Role != "" {
					customInstructions = ac.Role
				}
				if len(agentSkillNames) == 0 {
					agentSkillNames = ac.Skills
				}
			}
		}
	}

	// Always read teamMemberIds — needed to activate team-mode tools (delegate_task /
	// wait_for_team) regardless of whether teamContext was also provided.
	if rawIDs, ok := cmd["teamMemberIds"].([]any); ok {
		for _, v := range rawIDs {
			if s, ok := v.(string); ok && s != "" {
				teamMemberIDs = append(teamMemberIDs, s)
			}
		}
	}

	// Team context — build the orchestration roster from member IDs via the shared
	// agent-package builder. It reads the synced agents.json (roles AND skills), so
	// it never mislabels a skilled member as a "general assistant" the way a
	// frontend-built roster can. The pre-built teamContext string from the frontend
	// is only a fallback for callers that pass no member IDs.
	if len(teamMemberIDs) > 0 {
		tc := buildTeamLeadContext(teamMemberIDs)
		if customInstructions != "" {
			customInstructions += "\n\n" + tc
		} else {
			customInstructions = tc
		}
	} else if tc, _ := cmd["teamContext"].(string); tc != "" {
		if customInstructions != "" {
			customInstructions += "\n\n" + tc
		} else {
			customInstructions = tc
		}
	}

	// When an agent has skills defined, activate only those skills for the session.
	// This ensures the agent's system prompt includes its specific skill set.
	if len(agentSkillNames) > 0 {
		session.UpdateFields(workspace, sessionID, map[string]any{"activeSkills": agentSkillNames})
	}

	// hadError is set by agent.Execute and read by the deferred status update.
	var hadError bool
	defer func() {
		finalStatus := "idle"
		if ctx.Err() != nil {
			finalStatus = "cancelled"
		} else if hadError {
			finalStatus = "failed"
		}
		session.UpdateFields(workspace, sessionID, map[string]any{"status": finalStatus})
		session.UnregisterSession(sessionID)
	}()

	state.mu.Lock()
	mode := state.Mode
	thinkingBudget := state.ThinkingBudget
	thinkLevel := state.ThinkLevel
	shellAutoAllow := state.AllowAllShell
	sessionLimit := state.MaxSessionMessages
	state.mu.Unlock()

	if v, ok := cmd["mode"].(string); ok && v != "" {
		mode = v
	}
	// Parse per-message thinking override: bool, string level, or legacy number.
	if t, ok := cmd["thinking"]; ok {
		switch v := t.(type) {
		case bool:
			if v {
				thinkLevel = "true"
			} else {
				thinkLevel = "false"
			}
		case string:
			thinkLevel = v // "true", "false", "low", "medium", "high"
		case float64:
			// Legacy numeric budget — treat >0 as on.
			if v > 0 {
				thinkLevel = "true"
				thinkingBudget = int(v)
			} else {
				thinkLevel = "false"
				thinkingBudget = 0
			}
		}
	}
	// Per-message shell auto-allow override (used by channel background sessions).
	if v, ok := cmd["shell_auto_allow"].(bool); ok && v {
		shellAutoAllow = true
	}
	smartAuthorize := false
	if v, ok := cmd["smart_authorize"].(bool); ok && v {
		smartAuthorize = true
	}
	// Per-message desktop control override (used by /screen slash command).
	desktopPermitted := false
	if v, ok := cmd["desktop_permitted"].(bool); ok && v {
		desktopPermitted = true
	}

	// recordApproval appends the outcome of a permission decision to the
	// tamper-evident audit log, so "who authorized this destructive action, and was
	// it a manual grant or an auto-approval" is answerable after the fact.
	recordApproval := func(command, kind string, allowed, auto bool) {
		if _, err := session.AppendAudit(workspace, sessionID, session.Actor{Label: "desktop"}, "approval", map[string]any{
			"command": session.AuditSummary(command),
			"kind":    kind,
			"allowed": allowed,
			"auto":    auto,
		}); err != nil {
			Emit(map[string]any{"type": "audit_error", "message": err.Error()})
		}
	}

	askPerm := func(ctx context.Context, command string) (allow, allowAll bool) {
		// Extract kind from the command prefix tools use (e.g. "write_file: path" → "write").
		kind := "shell"
		if idx := strings.Index(command, ": "); idx >= 0 {
			prefix := command[:idx]
			if prefix == "write_file" || prefix == "edit_file" || prefix == "create_plan" {
				kind = "write"
			}
		}
		ch := state.permQueue(sessionID)
		event := map[string]any{
			"type":       "permission_request",
			"requestId":  uuid.New().String(),
			"session_id": sessionID,
			"command":    command,
			"kind":       kind,
		}
		Emit(event)
		// Also publish to the session WebSocket bus so Angular WS clients receive it.
		session.Global.Publish(sessionID, event)
		// No timeout: a pending permission prompt must block the agent for as
		// long as it takes the user to respond. The only way out besides an
		// explicit allow/deny is the turn itself being cancelled.
		select {
		case resp := <-ch:
			recordApproval(command, kind, resp.Allow, false)
			return resp.Allow, resp.AllowAll
		case <-ctx.Done():
			recordApproval(command, kind, false, false)
			return false, false
		}
	}

	requestPerm := askPerm
	if smartAuthorize {
		requestPerm = func(ctx context.Context, command string) (allow, allowAll bool) {
			if isSmartSafe(command) {
				recordApproval(command, "shell", true, true) // Smart Authorize auto-approval
				return true, false
			}
			return askPerm(ctx, command)
		}
	}

	requestClarification := func(ctx context.Context, question string, suggestions []string, multiChoice bool) (agent.ClarificationAnswer, bool) {
		requestID := uuid.New().String()
		ch := state.registerClarificationCh(requestID)
		event := map[string]any{
			"type":         "clarification_request",
			"requestId":    requestID,
			"session_id":   sessionID,
			"question":     question,
			"suggestions":  suggestions,
			"multi_choice": multiChoice,
		}
		Emit(event)
		session.Global.Publish(sessionID, event)
		select {
		case resp := <-ch:
			return agent.ClarificationAnswer{Selected: resp.Selected, Details: resp.Details}, true
		case <-ctx.Done():
			// Clean up stale channel on cancellation.
			state.deliverClarificationResponse(requestID, agent.ClarificationAnswer{})
			return agent.ClarificationAnswer{}, false
		}
	}

	requestContinue := func(ctx context.Context, iteration, maxIter int) bool {
		ch := state.iterationConfirmQueue(sessionID)
		event := map[string]any{
			"type":       "iteration_confirm_request",
			"requestId":  uuid.New().String(),
			"session_id": sessionID,
			"iteration":  iteration,
			"max":        maxIter,
		}
		Emit(event)
		session.Global.Publish(sessionID, event)
		select {
		case resp := <-ch:
			return resp.Allow
		case <-time.After(120 * time.Second):
			// Timeout — stop rather than run unbounded.
			return false
		case <-ctx.Done():
			return false
		}
	}

	requestSystemPermission := func(ctx context.Context, permType string) bool {
		requestID := uuid.New().String()
		ch := state.registerSysPermCh(requestID)
		event := map[string]any{
			"type":            "system_permission_required",
			"requestId":       requestID,
			"session_id":      sessionID,
			"permission_type": permType,
		}
		Emit(event)
		session.Global.Publish(sessionID, event)
		select {
		case granted := <-ch:
			return granted
		case <-time.After(120 * time.Second):
			return false
		case <-ctx.Done():
			return false
		}
	}

	takeScreenshot := func(ctx context.Context) (map[string]any, error) {
		requestID := uuid.New().String()
		ch := state.registerScreenshotCh(requestID)
		Emit(map[string]any{
			"type":       "screenshot_request",
			"requestId":  requestID,
			"session_id": sessionID,
		})
		select {
		case payload := <-ch:
			if errMsg, _ := payload["error"].(string); errMsg != "" {
				return nil, fmt.Errorf("screenshot failed: %s", errMsg)
			}
			return payload, nil
		case <-time.After(100 * time.Second):
			return nil, fmt.Errorf("screenshot timed out — Tauri did not respond within 100s")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	emitFn := func(m map[string]any) {
		Emit(m)
	}

	hadError = agent.Execute(ctx, agent.Config{
		SessionID:        sessionID,
		Workspace:        workspace,
		Message:          message,
		Images:           images,
		Mode:             mode,
		ThinkingBudget:   thinkingBudget,
		ThinkLevel:       thinkLevel,
		ShellAutoAllow:   shellAutoAllow,
		DesktopPermitted: desktopPermitted,
		// Browser tools (chromedp) are available in the Lite desktop app — it runs
		// on the user's machine where a display is present, so headless=false Chrome
		// can launch. This lets the agent "run programs and test" via browser_*.
		BrowserAvailable:        true,
		AgentName:               agentName,
		CustomInstructions:      customInstructions,
		TeamMemberIDs:           teamMemberIDs,
		EmitFn:                  emitFn,
		RequestPerm:             requestPerm,
		RequestClarification:    requestClarification,
		RequestContinue:         requestContinue,
		TakeScreenshot:          takeScreenshot,
		RequestSystemPermission: requestSystemPermission,
	})

	// Auto-close any chat-linked TodoTask whose chat session just completed.
	// The frontend's onComplete callback is the primary mechanism, but it can
	// miss the update if the WS/STDIO done event arrives out of order or the
	// component is mid-navigation. The sidecar acts as a reliable fallback:
	// it finds the linked todo by sessionID and patches it directly.
	if ctx.Err() == nil {
		linkedStatus := "done"
		if hadError {
			linkedStatus = "failed"
		}
		closeLinkedTodo(workspace, sessionID, linkedStatus)
	}

	if sessionLimit > 0 && ctx.Err() == nil {
		count := session.MessageCount(workspace, sessionID)
		if count >= sessionLimit {
			h, _, _ := session.LoadTail(workspace, sessionID, 0)
			title := ""
			if h != nil && h.Title != nil {
				title = *h.Title
			}
			Emit(map[string]any{
				"type":          "session_limit_reached",
				"session_id":    sessionID,
				"message_count": count,
				"limit":         sessionLimit,
				"title":         title,
			})
		}
	}
}

// closeLinkedTodo finds any chat-linked TodoTask associated with sessionID and
// marks it done or failed. It handles both team tasks (leadSessionId) and single
// tasks (step.sessionId). This is the sidecar-side fallback for the frontend's
// patchTodoStatus — needed because the WS/STDIO done event can be missed if the
// frontend component is navigating away or the connection is briefly interrupted.
func closeLinkedTodo(workspace, sessionID, status string) {
	// Team task: look up by leadSessionId.
	if t := todo.FindByLeadSession(workspace, sessionID); t != nil && t.Status == "running" {
		for i := range t.Steps {
			if t.Steps[i].Status == "running" {
				t.Steps[i].Status = status
			}
		}
		t.Status = status
		todo.Update(workspace, t)
		emitTodoUpdate(workspace, t)
		return
	}
	// Single-step task: look up by step.sessionId.
	if t, _ := todo.FindByStepSession(workspace, sessionID); t != nil && t.Status == "running" {
		for i := range t.Steps {
			if t.Steps[i].SessionID == sessionID && t.Steps[i].Status == "running" {
				t.Steps[i].Status = status
			}
		}
		t.Status = status
		todo.Update(workspace, t)
		emitTodoUpdate(workspace, t)
	}
}

// isSmartSafe reports whether Smart Authorize can auto-approve an operation
// without prompting. Unlike Ask mode, Smart Authorize trusts routine in-workspace
// work — file writes/edits/plans and read-only shell commands — and only stops to
// ask for potentially destructive shell commands (deletes, network, sudo, …).
// This is what makes Smart Authorize meaningfully different from Ask: the agent
// can edit files and run safe commands freely, pausing only on risky shell calls.
func isSmartSafe(command string) bool {
	// File operations are gated by the workspace sandbox (safeJoin) and are
	// reversible via version control, so Smart Authorize approves them outright.
	if idx := strings.Index(command, ": "); idx >= 0 {
		switch command[:idx] {
		case "write_file", "edit_file", "create_plan":
			return true
		}
	}
	return isCommandSafe(command)
}

// isCommandSafe returns true for read-only shell commands that Smart Authorize
// can auto-approve without prompting the user.
func isCommandSafe(command string) bool {
	// Redirects that write to files are always risky.
	if strings.Contains(command, " > ") || strings.Contains(command, " >> ") {
		return false
	}

	// Extract the first token (the binary name, possibly with a path).
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	bin := fields[0]
	// Strip any path prefix (e.g. /usr/bin/ls → ls).
	if idx := strings.LastIndex(bin, "/"); idx >= 0 {
		bin = bin[idx+1:]
	}

	safeCommands := map[string]bool{
		"ls": true, "ll": true, "la": true, "dir": true,
		"cat": true, "head": true, "tail": true, "less": true, "more": true,
		"find": true, "fd": true, "locate": true,
		"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
		"wc": true, "sort": true, "uniq": true, "tr": true, "cut": true,
		"diff": true, "diff3": true, "comm": true,
		"echo": true, "printf": true, "pwd": true,
		"which": true, "type": true, "where": true, "whereis": true,
		"env": true, "printenv": true,
		"date": true, "uname": true, "hostname": true, "id": true, "whoami": true,
		"stat": true, "file": true, "du": true, "df": true,
		"ps": true, "top": true, "htop": true,
		"jq": true, "yq": true, "xmllint": true,
		"strings": true, "od": true, "xxd": true,
		"lsof": true, "netstat": true,
		"npm": true, "yarn": true, "pnpm": true, // subcommand-gated below
		"pip": true, "pip3": true, // subcommand-gated below
		"git": true, // subcommand-gated below
		"go":  true, // subcommand-gated below
	}

	if !safeCommands[bin] {
		return false
	}

	// For package managers and VCS, only allow read-only subcommands.
	switch bin {
	case "git":
		if len(fields) < 2 {
			return false
		}
		safeGit := map[string]bool{
			"status": true, "log": true, "diff": true, "show": true,
			"branch": true, "tag": true, "stash": true, "ls-files": true,
			"ls-tree": true, "remote": true, "describe": true,
			"shortlog": true, "blame": true, "rev-parse": true,
			"rev-list": true, "reflog": true, "worktree": true,
		}
		return safeGit[fields[1]]

	case "npm", "yarn", "pnpm":
		if len(fields) < 2 {
			return false
		}
		safePkg := map[string]bool{
			"list": true, "ls": true, "info": true, "view": true,
			"outdated": true, "audit": true, "run": true, "test": true,
		}
		return safePkg[fields[1]]

	case "pip", "pip3":
		if len(fields) < 2 {
			return false
		}
		return fields[1] == "list" || fields[1] == "show" || fields[1] == "freeze"

	case "go":
		if len(fields) < 2 {
			return false
		}
		safeGo := map[string]bool{
			"build": true, "test": true, "vet": true, "fmt": true,
			"list": true, "doc": true, "env": true, "version": true,
			"run": true,
		}
		return safeGo[fields[1]]
	}

	return true
}

package stdio

// handlers_todo_tasks.go — server-side todo task scheduling and execution.
//
// STDIO commands (client → sidecar):
//   add_todo_task     — persist a task; instant tasks start immediately
//   list_todo_tasks   — return all tasks for a workspace
//   remove_todo_task  — delete and optionally cancel a task
//   start_todo_task   — immediately run a pending/scheduled task
//   cancel_todo_task  — cancel a running task
//
// STDIO push events (sidecar → client):
//   todo_task_update  — task status changed; client merges into its local store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
	"github.com/tolle-ai/tollecode/internal/todo"
)

// ── STDIO command handlers ────────────────────────────────────────────────────

func handleAddTodoTask(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)

	raw, ok := cmd["task"].(map[string]any)
	if !ok {
		Emit(map[string]any{"type": "error", "message": "add_todo_task: missing task payload"})
		return
	}

	t := todoTaskFromMap(raw, ws)

	// Resolve provider/model for each step from agent configs.
	for i := range t.Steps {
		if t.Steps[i].Provider != "" {
			continue
		}
		if ac := agent.LookupAgentCfg(t.Steps[i].AgentID); ac != nil {
			t.Steps[i].Provider = ac.Provider
			t.Steps[i].Model = ac.Model
		}
	}
	// Resolve lead agent config for team mode.
	if t.Mode == "team" && t.LeadProvider == "" {
		if ac := agent.LookupAgentCfg(t.LeadAgentID); ac != nil {
			t.LeadProvider = ac.Provider
			t.LeadModel = ac.Model
		}
	}

	todo.Add(ws, t)

	Emit(map[string]any{
		"type":          "todo_task_added",
		"workspacePath": ws,
		"task":          todoTaskToMap(t),
	})

	// Instant tasks start immediately in a background goroutine.
	if t.ScheduleType != "scheduled" {
		go runTodoTask(state, t.ID, ws)
	}
}

func handleListTodoTasks(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	tasks := todo.List(ws)
	data := make([]any, len(tasks))
	for i, t := range tasks {
		data[i] = todoTaskToMap(t)
	}
	Emit(map[string]any{
		"type":          "todo_tasks_list",
		"workspacePath": ws,
		"tasks":         data,
	})
}

func handleRemoveTodoTask(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	id, _ := cmd["id"].(string)
	if id == "" {
		return
	}
	// Cancel the running goroutine if one exists (task ID is used as the key).
	state.cancelSession(id)
	todo.Remove(ws, id)
	Emit(map[string]any{
		"type":          "todo_task_removed",
		"workspacePath": ws,
		"id":            id,
	})
}

func handleStartTodoTask(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	id, _ := cmd["id"].(string)
	if id == "" {
		return
	}
	t, ok := todo.Get(ws, id)
	if !ok || t.Status == "running" {
		return
	}
	go runTodoTask(state, id, ws)
}

func handleCancelTodoTask(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	id, _ := cmd["id"].(string)
	if id == "" {
		return
	}
	// cancelSession waits for the goroutine to finish.
	state.cancelSession(id)
	todo.PatchStatus(ws, id, "failed")
	if t, ok := todo.Get(ws, id); ok {
		emitTodoUpdate(ws, t)
	}
}

// handleUpdateTodoTask patches the status of a stored task and its running steps.
// Used by the frontend to mark a linked (chat-created) todo as done or failed
// after the underlying session completes outside the todo runner.
func handleUpdateTodoTask(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	id, _ := cmd["id"].(string)
	status, _ := cmd["status"].(string)
	if id == "" || status == "" {
		return
	}
	t, ok := todo.Get(ws, id)
	if !ok {
		return
	}

	// Guard against premature client-driven "done" when the linked session is still
	// running. The sidecar is the source of truth for todo status; the frontend may
	// observe a stale terminal event from a previous turn or a race between the live
	// event bus and the todo store. If the linked session is still active and has no
	// terminal bus event, ignore the done patch and emit a warning.
	if status == "done" && !todoSessionIsTerminal(t) {
		Emit(map[string]any{
			"type":          "warning",
			"message":       "update_todo_task done ignored: linked session is still running",
			"workspacePath": ws,
			"id":            id,
		})
		return
	}

	t.Status = status
	if stepStatus, ok := cmd["stepStatus"].(string); ok && stepStatus != "" {
		for i := range t.Steps {
			if t.Steps[i].Status == "running" {
				t.Steps[i].Status = stepStatus
			}
		}
	}
	todo.Update(ws, t)
	emitTodoUpdate(ws, t)
}

// handleRerunTodoTask resets a task and all its steps back to pending, then
// immediately starts execution. Used for the "Rerun" action on failed tasks.
func handleRerunTodoTask(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	id, _ := cmd["id"].(string)
	if id == "" {
		return
	}
	t, ok := todo.Get(ws, id)
	if !ok {
		return
	}
	// Cancel any currently-running goroutine for this task.
	state.cancelSession(id)

	t.Status = "pending"
	t.CurrentStepIndex = 0
	t.LeadSessionID = ""
	for i := range t.Steps {
		t.Steps[i].Status = "pending"
		t.Steps[i].SessionID = ""
	}
	todo.Update(ws, t)
	emitTodoUpdate(ws, t)

	go runTodoTask(state, id, ws)
}

// ── Scheduler ─────────────────────────────────────────────────────────────────

// StartTodoScheduler launches a background goroutine that fires due scheduled
// tasks every 30 seconds. Must be called once from Run().
func StartTodoScheduler(state *ServerState) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			for _, t := range todo.DueTasks() {
				go runTodoTask(state, t.ID, t.WorkspacePath)
			}
		}
	}()
}

// ── Task runner ───────────────────────────────────────────────────────────────

// runTodoTask is the top-level runner for a single todo task. It registers the
// goroutine in state.tasks so it can be cancelled, marks the task running, then
// delegates to single or team execution mode.
func runTodoTask(state *ServerState, taskID, workspacePath string) {
	t, ok := todo.Get(workspacePath, taskID)
	if !ok {
		return
	}
	// Guard: don't start if already running (scheduler may fire twice).
	if t.Status == "running" {
		return
	}

	// Apply a wall-clock deadline so tasks never run forever.
	// Wrapping with WithCancel after WithTimeout keeps the cancel function
	// usable by state.cancelSession without exposing the internal timer cancel.
	timeout := config.GetSidecarSettings().EffectiveTaskTimeout()
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), timeout)
	ctx, cancel := context.WithCancel(timeoutCtx)
	done := make(chan struct{})
	state.registerTask(taskID, cancel, done)

	go func() {
		defer close(done)
		defer state.removeTask(taskID)
		defer timeoutCancel() // release timer resources

		t.Status = "running"
		todo.Update(workspacePath, t)
		emitTodoUpdate(workspacePath, t)

		if t.Mode == "team" {
			runTodoTeam(ctx, state, t, workspacePath)
		} else {
			runTodoSingle(ctx, state, t, workspacePath)
		}
	}()
}

// runTodoSingle runs each step sequentially, passing a handoff summary to each
// subsequent step so agents have context from the one before them.
func runTodoSingle(ctx context.Context, state *ServerState, t *todo.Task, workspacePath string) {
	var handoff string

	for i := range t.Steps {
		if ctx.Err() != nil {
			patchStepAndTask(workspacePath, t, i, "failed", "failed", "")
			return
		}

		step := &t.Steps[i]
		if step.Status == "done" {
			continue
		}

		provider, model := resolveStepProviderModel(step)

		// Create a fresh session for this step.
		sess, err := session.Create(
			workspacePath, provider, model, "build",
			session.WithAgentName(stepAgentName(step)),
		)
		if err != nil {
			patchStepAndTask(workspacePath, t, i, "failed", "failed", "")
			return
		}

		// When the step's agent has skills defined, activate only those skills.
		if step.AgentID != "" {
			if ac := agent.LookupAgentCfg(step.AgentID); ac != nil && len(ac.Skills) > 0 {
				session.UpdateFields(workspacePath, sess.ID, map[string]any{"activeSkills": ac.Skills})
			}
		}

		// Mark step running.
		t.Steps[i].Status = "running"
		t.Steps[i].SessionID = sess.ID
		t.CurrentStepIndex = i
		todo.Update(workspacePath, t)
		emitTodoUpdate(workspacePath, t)

		// Prepare session for streaming.
		session.Global.ClearBuffer(sess.ID)
		session.ClearLiveEvents(sess.ID)
		session.UpdateFields(workspacePath, sess.ID, map[string]any{"status": "running"})
		session.RegisterSession(sess.ID, workspacePath, "desktop")

		// Build message with optional handoff from the previous step.
		instruction := coalesce(step.Instruction, t.Description, t.Name)
		message := instruction
		if handoff != "" {
			message = handoff + "\n\nYour task for this step:\n" + instruction
		}

		// Custom instructions from the step's agent config.
		customInstr := resolveCustomInstructions(step.AgentID)

		hadError := agent.Execute(ctx, agent.Config{
			SessionID:          sess.ID,
			Workspace:          workspacePath,
			Message:            message,
			Mode:               "build",
			ShellAutoAllow:     t.ShellAutoAllow,
			CustomInstructions: customInstr,
			EmitFn:             makeStepEmitFn(sess.ID),
			RequestPerm:        makePermFn(state, sess.ID, t.ShellAutoAllow),
		})

		finalSessStatus := "idle"
		if hadError {
			finalSessStatus = "failed"
		}
		session.UpdateFields(workspacePath, sess.ID, map[string]any{"status": finalSessStatus})
		session.UnregisterSession(sess.ID)

		success := !hadError && ctx.Err() == nil
		stepStatus := "done"
		if !success {
			stepStatus = "failed"
		}
		t.Steps[i].Status = stepStatus

		outcome := step.OnComplete
		if !success {
			outcome = step.OnFail
		}
		hasNext := i+1 < len(t.Steps)

		if success && outcome == "next" && hasNext {
			handoff = buildHandoff(workspacePath, sess.ID, i+1)
			todo.Update(workspacePath, t)
			emitTodoUpdate(workspacePath, t)
			continue
		}

		// Task finished (either last step, explicit finish, or failure).
		t.Status = "done"
		if !success {
			t.Status = "failed"
		}
		todo.Update(workspacePath, t)
		emitTodoUpdate(workspacePath, t)
		return
	}

	// All steps completed successfully.
	t.Status = "done"
	todo.Update(workspacePath, t)
	emitTodoUpdate(workspacePath, t)
}

// runTodoTeam runs the lead agent with team context injected into the system prompt.
// Sub-agents are spawned via the lead's spawn_sub_agent tool call (existing mechanism).
func runTodoTeam(ctx context.Context, state *ServerState, t *todo.Task, workspacePath string) {
	provider := t.LeadProvider
	model := t.LeadModel
	// Validate that the stored provider ID exists in the current registry.
	// Stale IDs from a different install (dev vs prod) would cause agent_error.
	if provider != "" {
		if _, ok := ai.Global.ResolveProviderID(provider); !ok {
			provider, model = "", ""
		}
	}
	if provider == "" {
		if ac := agent.LookupAgentCfg(t.LeadAgentID); ac != nil {
			if _, ok := ai.Global.ResolveProviderID(ac.Provider); ok {
				provider, model = ac.Provider, ac.Model
			}
		}
	}
	if provider == "" {
		provider, model = firstProvider()
	}

	teamContext := buildTeamContext(t)

	sess, err := session.Create(
		workspacePath, provider, model, "build",
		session.WithAgentName("lead"),
	)
	if err != nil {
		t.Status = "failed"
		todo.Update(workspacePath, t)
		emitTodoUpdate(workspacePath, t)
		return
	}

	// Track lead session ID and pre-allocate member tracking slices so the
	// frontend canvas shows pending member nodes immediately.
	t.LeadSessionID = sess.ID
	if len(t.MemberSessionIDs) != len(t.TeamAgentIDs) {
		t.MemberSessionIDs = make([]string, len(t.TeamAgentIDs))
	}
	if len(t.MemberStatuses) != len(t.TeamAgentIDs) {
		t.MemberStatuses = make([]string, len(t.TeamAgentIDs))
		for i := range t.MemberStatuses {
			t.MemberStatuses[i] = "pending"
		}
	}
	todo.Update(workspacePath, t)
	emitTodoUpdate(workspacePath, t)

	// Guard the task's terminal status against premature "done" when members
	// are still running. This wrapper is applied after agent.Execute returns.
	setTaskStatus := makeTeamDoneGuard(t.ID, workspacePath, func(s string) {
		t.Status = s
		todo.Update(workspacePath, t)
		emitTodoUpdate(workspacePath, t)
	})

	// When the lead agent has skills defined, activate only those skills.
	if t.LeadAgentID != "" {
		if ac := agent.LookupAgentCfg(t.LeadAgentID); ac != nil && len(ac.Skills) > 0 {
			session.UpdateFields(workspacePath, sess.ID, map[string]any{"activeSkills": ac.Skills})
		}
	}

	session.Global.ClearBuffer(sess.ID)
	session.ClearLiveEvents(sess.ID)
	session.UpdateFields(workspacePath, sess.ID, map[string]any{"status": "running"})
	session.RegisterSession(sess.ID, workspacePath, "desktop")

	customInstr := resolveCustomInstructions(t.LeadAgentID)
	if teamContext != "" {
		customInstr = strings.TrimSpace(customInstr + "\n\n" + teamContext)
	}

	instruction := coalesce(t.Description, t.Name)

	hadError := agent.Execute(ctx, agent.Config{
		SessionID:          sess.ID,
		Workspace:          workspacePath,
		Message:            instruction,
		Mode:               "build",
		ShellAutoAllow:     t.ShellAutoAllow,
		CustomInstructions: customInstr,
		TeamMemberIDs:      t.TeamAgentIDs,
		EmitFn:             makeTeamEmitFn(t.ID, workspacePath, t.TeamAgentIDs),
		RequestPerm:        makePermFn(state, sess.ID, t.ShellAutoAllow),
	})

	success := !hadError && ctx.Err() == nil
	finalSessStatus := "idle"
	if !success {
		finalSessStatus = "failed"
	}
	session.UpdateFields(workspacePath, sess.ID, map[string]any{"status": finalSessStatus})
	session.UnregisterSession(sess.ID)

	// Re-read the task so member updates from makeTeamEmitFn are included.
	if latest, ok := todo.Get(workspacePath, t.ID); ok {
		t = latest
	}
	if success {
		setTaskStatus("done")
	} else {
		setTaskStatus("failed")
		// Mark any member that never completed as failed.
		for i, ms := range t.MemberStatuses {
			if ms == "running" || ms == "pending" {
				t.MemberStatuses[i] = "failed"
			}
		}
		todo.Update(workspacePath, t)
		emitTodoUpdate(workspacePath, t)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// emitTodoUpdate broadcasts the current task state to connected STDIO clients
// and to the in-process session bus (so WS clients also receive it).
func emitTodoUpdate(workspacePath string, t *todo.Task) {
	ev := map[string]any{
		"type":          "todo_task_update",
		"workspacePath": workspacePath,
		"task":          todoTaskToMap(t),
	}
	Emit(ev)
}

// makeStepEmitFn returns an emit function for a step session that writes to
// STDIO and also publishes to the session bus so WS clients see the stream.
func makeStepEmitFn(sessionID string) func(map[string]any) {
	return func(m map[string]any) {
		Emit(m)
	}
}

// makeTeamEmitFn is like makeStepEmitFn but additionally intercepts
// sub_agent_spawned / sub_agent_done events emitted by team member goroutines
// and records each member's session ID + status into the task store so the
// frontend workflow canvas can show per-member progress and navigate to sessions.
func makeTeamEmitFn(taskID, ws string, teamAgentIDs []string) func(map[string]any) {
	// Build a fast lookup: agentID → index in teamAgentIDs.
	idxOf := make(map[string]int, len(teamAgentIDs))
	for i, aid := range teamAgentIDs {
		idxOf[aid] = i
	}

	return func(m map[string]any) {
		Emit(m)

		evType, _ := m["type"].(string)
		isTeamMember, _ := m["teamMember"].(bool)
		if !isTeamMember {
			return
		}

		switch evType {
		case "sub_agent_spawned":
			agentID, _ := m["agentId"].(string)
			subID, _ := m["subSessionId"].(string)
			if agentID == "" || subID == "" {
				return
			}
			idx, ok := idxOf[agentID]
			if !ok {
				return
			}
			todo.PatchMember(ws, taskID, idx, subID, "running")
			if t, ok := todo.Get(ws, taskID); ok {
				emitTodoUpdate(ws, t)
			}

		case "sub_agent_done":
			subID, _ := m["subSessionId"].(string)
			if subID == "" {
				return
			}
			// Find which member this session belongs to.
			t, ok := todo.Get(ws, taskID)
			if !ok {
				return
			}
			for i, sid := range t.MemberSessionIDs {
				if sid == subID {
					status := deriveSubAgentStatus(m)
					todo.PatchMember(ws, taskID, i, "", status)
					if updated, ok := todo.Get(ws, taskID); ok {
						emitTodoUpdate(ws, updated)
					}
					break
				}
			}
		}
	}
}

// makeTeamDoneGuard wraps the todo team runner's terminal status update so that
// the overall task cannot flip to "done" while any member is still recorded as
// running. Pending/unassigned members are allowed (they may never have been delegated),
// but members that already have a session ID assigned must be terminal (done or failed)
// before the overall task can become done.
func makeTeamDoneGuard(taskID, workspacePath string, base func(string)) func(string) {
	return func(status string) {
		if status == "done" {
			if t, ok := todo.Get(workspacePath, taskID); ok {
				for i, ms := range t.MemberStatuses {
					if ms == "running" {
						return
					}
					// If this member has already been spawned but hasn't reported a terminal
					// status yet, keep the overall task running. An empty MemberSessionID means
					// the member was never actually delegated, so it is safe to ignore.
					if t.MemberSessionIDs[i] != "" && ms != "done" && ms != "failed" {
						return
					}
				}
			}
		}
		base(status)
	}
}

// deriveSubAgentStatus extracts the real outcome from a sub_agent_done event,
// defaulting to "done" for backward compatibility if no status field is present.
func deriveSubAgentStatus(m map[string]any) string {
	if s, ok := m["status"].(string); ok && s != "" {
		return s
	}
	return "done"
}

// todoSessionIsTerminal returns true if the linked session for a todo has
// emitted a terminal event (done/cancelled/agent_error) or is no longer registered
// as running in the active-sessions registry.
func todoSessionIsTerminal(t *todo.Task) bool {
	linkedSessionID := ""
	if t.Mode == "team" {
		linkedSessionID = t.LeadSessionID
	}
	if linkedSessionID == "" && len(t.Steps) > 0 {
		linkedSessionID = t.Steps[t.CurrentStepIndex].SessionID
	}
	if linkedSessionID == "" {
		return true
	}
	// If the bus has recorded a terminal event for this session, allow done/failed.
	if session.Global.Terminal(linkedSessionID) != nil {
		return true
	}
	// If the active registry still shows this session as running, consider it non-terminal.
	for _, e := range session.ListActive() {
		if e.SessionID == linkedSessionID && e.Status == "running" {
			return false
		}
	}
	return true
}

// makePermFn returns a permission-request function. When autoAllow is true all
// commands are approved silently. Otherwise the request is emitted to STDIO so a
// connected client can respond via tool_permission; it times out after 60 s.
func makePermFn(state *ServerState, sessionID string, autoAllow bool) func(ctx context.Context, command string) (allow, allowAll bool) {
	return func(ctx context.Context, command string) (allow, allowAll bool) {
		if autoAllow {
			return true, false
		}
		// Extract kind from the command prefix tools use (e.g. "write_file: path" → "write").
		kind := "shell"
		if idx := strings.Index(command, ": "); idx >= 0 {
			prefix := command[:idx]
			if prefix == "write_file" || prefix == "edit_file" || prefix == "create_plan" {
				kind = "write"
			}
		}
		// Route through the normal permission queue — if a client is connected it
		// will see the permission_request event and can respond.
		ch := state.permQueue(sessionID)
		event := map[string]any{
			"type":       "permission_request",
			"requestId":  uuid.New().String(),
			"session_id": sessionID,
			"command":    command,
			"kind":       kind,
		}
		Emit(event)
		session.Global.Publish(sessionID, event)
		select {
		case resp := <-ch:
			return resp.Allow, resp.AllowAll
		case <-time.After(60 * time.Second):
			return false, false
		case <-ctx.Done():
			return false, false
		}
	}
}

func resolveStepProviderModel(step *todo.Step) (provider, model string) {
	// Validate stored provider exists in the current registry before trusting it.
	// A task created in dev mode (UUID from ~/.tollecode-dev/config.json) would
	// cause agent.Execute to emit "provider not configured" when run in production
	// if we returned the stale UUID without this check.
	if step.Provider != "" {
		if _, ok := ai.Global.ResolveProviderID(step.Provider); ok {
			return step.Provider, step.Model
		}
	}
	if ac := agent.LookupAgentCfg(step.AgentID); ac != nil {
		if _, ok := ai.Global.ResolveProviderID(ac.Provider); ok {
			return ac.Provider, ac.Model
		}
	}
	return firstProvider()
}

func resolveCustomInstructions(agentID string) string {
	if ac := agent.LookupAgentCfg(agentID); ac != nil {
		if ac.SystemPrompt != "" {
			return ac.SystemPrompt
		}
		return ac.Role
	}
	return ""
}

func firstProvider() (provider, model string) {
	return ai.Global.BestProvider("", "")
}

func stepAgentName(step *todo.Step) string {
	if ac := agent.LookupAgentCfg(step.AgentID); ac != nil && ac.Name != "" {
		return ac.Name
	}
	return "agent"
}

func buildTeamContext(t *todo.Task) string {
	return buildTeamLeadContext(t.TeamAgentIDs)
}

// buildTeamLeadContext wraps agent.BuildTeamLeadContext for local use.
func buildTeamLeadContext(memberIDs []string) string {
	return agent.BuildTeamLeadContext(memberIDs)
}

// buildHandoff reads the last assistant text from the completed step's session
// and formats it as a handoff preamble for the next agent.
func buildHandoff(workspacePath, sessionID string, completedStepNumber int) string {
	sess, _, err := session.LoadTail(workspacePath, sessionID, 0)
	if err != nil || sess == nil {
		return ""
	}
	// Find the last assistant message with non-empty content.
	var lastText string
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		r := sess.Messages[i]
		if role, _ := r["role"].(string); role == "assistant" {
			if content, _ := r["content"].(string); content != "" {
				lastText = content
				break
			}
		}
	}
	if lastText == "" {
		return ""
	}
	if len(lastText) > 2500 {
		lastText = lastText[:2500] + "…"
	}
	return fmt.Sprintf(
		"=== Handoff from Step %d ===\n\nThe previous step completed successfully. Summary:\n\n%s\n\n=== End of handoff — your task follows ===",
		completedStepNumber, lastText,
	)
}

func patchStepAndTask(workspacePath string, t *todo.Task, stepIdx int, stepStatus, taskStatus, sessionID string) {
	if stepIdx < len(t.Steps) {
		t.Steps[stepIdx].Status = stepStatus
		if sessionID != "" {
			t.Steps[stepIdx].SessionID = sessionID
		}
	}
	t.Status = taskStatus
	todo.Update(workspacePath, t)
	emitTodoUpdate(workspacePath, t)
}

// ensureLinkedTodoForSession creates a todo task linked to a chat session if one
// does not already exist. For single-mode chats it creates a one-step task; for
// team-lead chats it creates a team-mode task with pre-allocated member tracking
// so the workflow UI can attach sub-agent runs immediately. The task is persisted
// and a todo_task_added event is emitted.
//
// When an existing linked todo is found but is no longer 'running' (the user is
// starting a new turn on a session whose previous turn already completed), the
// todo is reset to 'running' so the workflow panel reflects the live state again.
// A session_todo_linked event is always emitted so the frontend can keep its
// session→todo cache current across navigation and panel recreations.
func ensureLinkedTodoForSession(workspacePath, sessionID, message, agentID string, teamMemberIds []string) *todo.Task {
	if workspacePath == "" || sessionID == "" {
		return nil
	}

	// Already linked via leadSessionId — reopen if the previous turn already closed it.
	if existing := todo.FindByLeadSession(workspacePath, sessionID); existing != nil {
		if existing.Status == "done" || existing.Status == "failed" {
			existing.Status = "running"
			todo.Update(workspacePath, existing)
			emitTodoUpdate(workspacePath, existing)
		}
		emitSessionTodoLinked(workspacePath, sessionID, existing.ID)
		return existing
	}
	// Already linked via a step sessionId.
	if existing, _ := todo.FindByStepSession(workspacePath, sessionID); existing != nil {
		if existing.Status == "done" || existing.Status == "failed" {
			existing.Status = "running"
			for i := range existing.Steps {
				if existing.Steps[i].SessionID == sessionID &&
					(existing.Steps[i].Status == "done" || existing.Steps[i].Status == "failed") {
					existing.Steps[i].Status = "running"
				}
			}
			todo.Update(workspacePath, existing)
			emitTodoUpdate(workspacePath, existing)
		}
		emitSessionTodoLinked(workspacePath, sessionID, existing.ID)
		return existing
	}

	// Derive a human-readable task name from the session title or message.
	name := "Continue session"
	if s, _, err := session.LoadTail(workspacePath, sessionID, 0); err == nil && s != nil && s.Title != nil && *s.Title != "" {
		name = *s.Title
	} else if message != "" {
		name = truncateTaskName(message, 40)
	}
	description := message
	if description == "" {
		description = sessionID
	}

	if agentID == "" {
		// Try to resolve the agent from the session's configuration for a cleaner link.
		if s, _, err := session.LoadTail(workspacePath, sessionID, 0); err == nil && s != nil && s.AgentName != "" {
			agentID = s.AgentName
		}
	}

	var t *todo.Task
	if len(teamMemberIds) > 0 {
		memberStatuses := make([]string, len(teamMemberIds))
		for i := range memberStatuses {
			memberStatuses[i] = "pending"
		}
		t = &todo.Task{
			ID:               uuid.NewString(),
			Name:             name,
			Description:      description,
			Mode:             "team",
			Status:           "running",
			LeadSessionID:    sessionID,
			LeadAgentID:      agentID,
			TeamAgentIDs:     append([]string(nil), teamMemberIds...),
			MemberStatuses:   memberStatuses,
			MemberSessionIDs: make([]string, len(teamMemberIds)),
			WorkspacePath:    workspacePath,
			CreatedAt:        time.Now().UTC().Format(time.RFC3339),
			ScheduleType:     "instant",
		}
	} else {
		stepID := uuid.NewString()
		t = &todo.Task{
			ID:            uuid.NewString(),
			Name:          name,
			Description:   description,
			Mode:          "single",
			Status:        "running",
			LeadSessionID: sessionID,
			WorkspacePath: workspacePath,
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			ScheduleType:  "instant",
			Steps: []todo.Step{
				{
					ID:          stepID,
					AgentID:     agentID,
					Instruction: name,
					Status:      "running",
					SessionID:   sessionID,
					OnComplete:  "finish",
					OnFail:      "finish",
				},
			},
		}
	}

	todo.Add(workspacePath, t)

	Emit(map[string]any{
		"type":          "todo_task_added",
		"workspacePath": workspacePath,
		"task":          todoTaskToMap(t),
	})

	emitSessionTodoLinked(workspacePath, sessionID, t.ID)
	return t
}

// emitSessionTodoLinked broadcasts the association between a chat session and its
// linked TodoTask so the frontend can keep its session→todo cache current across
// navigation and panel recreations without waiting for the turn to finish.
func emitSessionTodoLinked(workspacePath, sessionID, todoID string) {
	Emit(map[string]any{
		"type":          "session_todo_linked",
		"session_id":    sessionID,
		"todo_id":       todoID,
		"workspacePath": workspacePath,
	})
}

// truncateTaskName returns a short display name for a todo task derived from a
// user message, keeping it readable in narrow UI columns.
func truncateTaskName(message string, maxLen int) string {
	// Collapse newlines and extra spaces so the preview is one line.
	collapsed := strings.Join(strings.Fields(message), " ")
	if len(collapsed) <= maxLen {
		return collapsed
	}
	return collapsed[:maxLen] + "…"
}

func coalesce(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── Serialisation helpers ─────────────────────────────────────────────────────

func todoTaskFromMap(raw map[string]any, workspacePath string) *todo.Task {
	t := &todo.Task{
		ID:            str(raw["id"]),
		Name:          str(raw["name"]),
		Description:   str(raw["description"]),
		Mode:          str(raw["mode"]),
		Status:        strOr(raw["status"], "pending"),
		CreatedAt:     strOr(raw["createdAt"], time.Now().UTC().Format(time.RFC3339)),
		ScheduleType:  str(raw["scheduleType"]),
		ScheduledAt:   str(raw["scheduledAt"]),
		WorkspacePath: workspacePath,
		LeadAgentID:   str(raw["leadAgentId"]),
		LeadProvider:  str(raw["leadProvider"]),
		LeadModel:     str(raw["leadModel"]),
		LeadSessionID: str(raw["leadSessionId"]),
	}
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if ab, ok := raw["shellAutoAllow"].(bool); ok {
		t.ShellAutoAllow = ab
	}
	// Steps
	if rawSteps, ok := raw["steps"].([]any); ok {
		for _, rs := range rawSteps {
			if rm, ok := rs.(map[string]any); ok {
				s := todo.Step{
					ID:          strOr(rm["id"], uuid.NewString()),
					AgentID:     str(rm["agentId"]),
					Instruction: str(rm["instruction"]),
					OnComplete:  strOr(rm["onComplete"], "finish"),
					OnFail:      strOr(rm["onFail"], "finish"),
					Status:      strOr(rm["status"], "pending"),
					Provider:    str(rm["provider"]),
					Model:       str(rm["model"]),
				}
				t.Steps = append(t.Steps, s)
			}
		}
	}
	// Team agent IDs
	if rawIDs, ok := raw["teamAgentIds"].([]any); ok {
		for _, v := range rawIDs {
			if s, ok := v.(string); ok && s != "" {
				t.TeamAgentIDs = append(t.TeamAgentIDs, s)
			}
		}
	}
	// Member session IDs (team mode runtime state)
	if rawSIDs, ok := raw["memberSessionIds"].([]any); ok {
		for _, v := range rawSIDs {
			s, _ := v.(string)
			t.MemberSessionIDs = append(t.MemberSessionIDs, s)
		}
	}
	// Member statuses (team mode runtime state)
	if rawSts, ok := raw["memberStatuses"].([]any); ok {
		for _, v := range rawSts {
			s, _ := v.(string)
			t.MemberStatuses = append(t.MemberStatuses, s)
		}
	}
	return t
}

func todoTaskToMap(t *todo.Task) map[string]any {
	steps := make([]any, len(t.Steps))
	for i, s := range t.Steps {
		steps[i] = map[string]any{
			"id":          s.ID,
			"agentId":     s.AgentID,
			"instruction": s.Instruction,
			"onComplete":  s.OnComplete,
			"onFail":      s.OnFail,
			"status":      s.Status,
			"sessionId":   s.SessionID,
			"provider":    s.Provider,
			"model":       s.Model,
		}
	}
	teamIDs := t.TeamAgentIDs
	if teamIDs == nil {
		teamIDs = []string{}
	}
	memberSessions := t.MemberSessionIDs
	if memberSessions == nil {
		memberSessions = []string{}
	}
	memberStatuses := t.MemberStatuses
	if memberStatuses == nil {
		memberStatuses = []string{}
	}
	return map[string]any{
		"id":               t.ID,
		"name":             t.Name,
		"description":      t.Description,
		"mode":             t.Mode,
		"steps":            steps,
		"leadAgentId":      t.LeadAgentID,
		"teamAgentIds":     teamIDs,
		"status":           t.Status,
		"currentStepIndex": t.CurrentStepIndex,
		"createdAt":        t.CreatedAt,
		"shellAutoAllow":   t.ShellAutoAllow,
		"scheduleType":     t.ScheduleType,
		"scheduledAt":      t.ScheduledAt,
		"workspacePath":    t.WorkspacePath,
		"leadProvider":     t.LeadProvider,
		"leadModel":        t.LeadModel,
		"leadSessionId":    t.LeadSessionID,
		"memberSessionIds": memberSessions,
		"memberStatuses":   memberStatuses,
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

package httpserver

// todo_runner.go — REST-mode adaptation of stdio/handlers_todo_tasks.go.
// Key difference: emits events to the session bus (for SSE subscribers) instead
// of stdout. Permission prompts publish a pending_permission event to the bus
// and wait for a response via the apiState.permQueue.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/session"
	"github.com/tolle-ai/tollecode/internal/todo"
)

// runTodoTask is the REST-mode top-level runner for a single todo task.
func runTodoTask(state *apiState, taskID, workspacePath string) {
	t, ok := todo.Get(workspacePath, taskID)
	if !ok || t.Status == "running" {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	state.register(taskID, cancel, done)

	go func() {
		defer close(done)
		defer state.remove(taskID)

		t.Status = "running"
		todo.Update(workspacePath, t)
		publishTodoUpdate(workspacePath, t)

		if t.Mode == "team" {
			runAPITodoTeam(ctx, state, t, workspacePath)
		} else {
			runAPITodoSingle(ctx, state, t, workspacePath)
		}
	}()
}

func runAPITodoSingle(ctx context.Context, state *apiState, t *todo.Task, workspacePath string) {
	var handoff string

	for i := range t.Steps {
		if ctx.Err() != nil {
			apiPatchStepAndTask(workspacePath, t, i, "failed", "failed", "")
			return
		}

		step := &t.Steps[i]
		if step.Status == "done" {
			continue
		}

		provider, model := apiResolveStepProviderModel(step, state.defaultProvider, state.defaultModel)

		sess, err := session.Create(
			workspacePath, provider, model, "build",
			session.WithAgentName(stepAgentName(step)),
		)
		if err != nil {
			apiPatchStepAndTask(workspacePath, t, i, "failed", "failed", "")
			return
		}

		// When the step's agent has skills defined, activate only those skills.
		if step.AgentID != "" {
			if ac := agent.LookupAgentCfg(step.AgentID); ac != nil && len(ac.Skills) > 0 {
				session.UpdateFields(workspacePath, sess.ID, map[string]any{"activeSkills": ac.Skills})
			}
		}

		t.Steps[i].Status = "running"
		t.Steps[i].SessionID = sess.ID
		t.CurrentStepIndex = i
		todo.Update(workspacePath, t)
		publishTodoUpdate(workspacePath, t)

		session.ClearLiveEvents(sess.ID)
		session.Global.ClearBuffer(sess.ID)
		session.UpdateFields(workspacePath, sess.ID, map[string]any{"status": "running"})
		session.RegisterSession(sess.ID, workspacePath, "api")

		instruction := apiCoalesce(step.Instruction, t.Description, t.Name)
		message := instruction
		if handoff != "" {
			message = handoff + "\n\nYour task for this step:\n" + instruction
		}

		customInstr := apiResolveCustomInstructions(step.AgentID)

		hadError := agent.Execute(ctx, agent.Config{
			SessionID:          sess.ID,
			Workspace:          workspacePath,
			Message:            message,
			Mode:               "build",
			ShellAutoAllow:     t.ShellAutoAllow,
			CustomInstructions: customInstr,
			EmitFn:             apiStepEmitFn(sess.ID),
			RequestPerm:        apiPermFn(state, sess.ID, t.ShellAutoAllow),
		})

		finalStatus := "idle"
		if hadError {
			finalStatus = "failed"
		}
		session.UpdateFields(workspacePath, sess.ID, map[string]any{"status": finalStatus})
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
			handoff = apiBuildfHandoff(workspacePath, sess.ID, i+1)
			todo.Update(workspacePath, t)
			publishTodoUpdate(workspacePath, t)
			continue
		}

		// Task finished (either last step, explicit finish, or failure).
		t.Status = "done"
		if !success {
			t.Status = "failed"
		}
		todo.Update(workspacePath, t)
		publishTodoUpdate(workspacePath, t)
		return
	}

	t.Status = "done"
	todo.Update(workspacePath, t)
	publishTodoUpdate(workspacePath, t)
}

func runAPITodoTeam(ctx context.Context, state *apiState, t *todo.Task, workspacePath string) {
	provider := t.LeadProvider
	model := t.LeadModel
	if provider == "" {
		if ac := agent.LookupAgentCfg(t.LeadAgentID); ac != nil {
			provider, model = ac.Provider, ac.Model
		}
	}
	if provider == "" {
		provider, model = apiFirstProvider(state.defaultProvider, state.defaultModel)
	}

	teamCtx := apiBuildfTeamContext(t)

	sess, err := session.Create(workspacePath, provider, model, "build", session.WithAgentName("lead"))
	if err != nil {
		t.Status = "failed"
		todo.Update(workspacePath, t)
		publishTodoUpdate(workspacePath, t)
		return
	}

	if t.LeadAgentID != "" {
		if ac := agent.LookupAgentCfg(t.LeadAgentID); ac != nil && len(ac.Skills) > 0 {
			session.UpdateFields(workspacePath, sess.ID, map[string]any{"activeSkills": ac.Skills})
		}
	}

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
	publishTodoUpdate(workspacePath, t)

	setTaskStatus := apiTeamDoneGuard(t.ID, workspacePath, func(s string) {
		t.Status = s
		todo.Update(workspacePath, t)
		publishTodoUpdate(workspacePath, t)
	})

	session.ClearLiveEvents(sess.ID)
	session.Global.ClearBuffer(sess.ID)
	session.UpdateFields(workspacePath, sess.ID, map[string]any{"status": "running"})
	session.RegisterSession(sess.ID, workspacePath, "api")

	customInstr := apiResolveCustomInstructions(t.LeadAgentID)
	if teamCtx != "" {
		customInstr = strings.TrimSpace(customInstr + "\n\n" + teamCtx)
	}

	hadError := agent.Execute(ctx, agent.Config{
		SessionID:          sess.ID,
		Workspace:          workspacePath,
		Message:            apiCoalesce(t.Description, t.Name),
		Mode:               "build",
		ShellAutoAllow:     t.ShellAutoAllow,
		CustomInstructions: customInstr,
		TeamMemberIDs:      t.TeamAgentIDs,
		EmitFn:             apiStepEmitFn(sess.ID),
		EmitEvent:          apiTeamEmitFn(t.ID, workspacePath, t.TeamAgentIDs),
		RequestPerm:        apiPermFn(state, sess.ID, t.ShellAutoAllow),
	})

	success := !hadError && ctx.Err() == nil
	finalStatus := "idle"
	if !success {
		finalStatus = "failed"
	}
	session.UpdateFields(workspacePath, sess.ID, map[string]any{"status": finalStatus})
	session.UnregisterSession(sess.ID)

	if latest, ok := todo.Get(workspacePath, t.ID); ok {
		t = latest
	}
	if success {
		setTaskStatus("done")
	} else {
		setTaskStatus("failed")
		for i, ms := range t.MemberStatuses {
			if ms == "running" || ms == "pending" {
				t.MemberStatuses[i] = "failed"
			}
		}
		todo.Update(workspacePath, t)
		publishTodoUpdate(workspacePath, t)
	}
}

// publishTodoUpdate emits a todo_task_update event to the session bus so any
// SSE client connected to a step session sees the change.
func publishTodoUpdate(workspacePath string, t *todo.Task) {
	for _, s := range t.Steps {
		if s.SessionID != "" {
			session.Global.Publish(s.SessionID, map[string]any{
				"type":          "todo_task_update",
				"workspacePath": workspacePath,
				"task":          t,
			})
		}
	}
}

func apiStepEmitFn(sessionID string) func(map[string]any) {
	return func(m map[string]any) {
		off, _ := session.AppendLiveEvent(sessionID, m)
		m["_off"] = off
		session.Global.Publish(sessionID, m)
	}
}

// apiTeamEmitFn intercepts team member sub-agent events and records per-member
// progress into the todo store, mirroring the stdio makeTeamEmitFn behaviour.
func apiTeamEmitFn(taskID, workspacePath string, teamAgentIDs []string) func(map[string]any) {
	idxOf := make(map[string]int, len(teamAgentIDs))
	for i, aid := range teamAgentIDs {
		idxOf[aid] = i
	}

	return func(m map[string]any) {
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
			if idx, ok := idxOf[agentID]; ok {
				apiPatchMember(workspacePath, taskID, idx, subID, "running")
			}

		case "sub_agent_done":
			subID, _ := m["subSessionId"].(string)
			if subID == "" {
				return
			}
			t, ok := todo.Get(workspacePath, taskID)
			if !ok {
				return
			}
			status := deriveSubAgentStatus(m)
			for i, sid := range t.MemberSessionIDs {
				if sid == subID {
					apiPatchMember(workspacePath, taskID, i, "", status)
					break
				}
			}
		}
	}
}

// apiPatchMember mirrors todo.PatchMember with the same running-member guard.
func apiPatchMember(workspacePath, id string, idx int, sessionID, status string) {
	todo.PatchMember(workspacePath, id, idx, sessionID, status)
	if t, ok := todo.Get(workspacePath, id); ok {
		publishTodoUpdate(workspacePath, t)
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

// apiTeamDoneGuard mirrors makeTeamDoneGuard for the REST runner.
func apiTeamDoneGuard(taskID, workspacePath string, base func(string)) func(string) {
	return func(status string) {
		if status == "done" {
			if t, ok := todo.Get(workspacePath, taskID); ok {
				for i, ms := range t.MemberStatuses {
					if ms == "running" {
						return
					}
					if t.MemberSessionIDs[i] != "" && ms != "done" && ms != "failed" {
						return
					}
				}
			}
		}
		base(status)
	}
}

func apiPermFn(state *apiState, sessionID string, autoAllow bool) func(ctx context.Context, command string) (allow, allowAll bool) {
	return func(ctx context.Context, command string) (allow, allowAll bool) {
		if autoAllow {
			return true, false
		}
		event := map[string]any{
			"type":       "pending_permission",
			"requestId":  uuid.New().String(),
			"session_id": sessionID,
			"command":    command,
		}
		off, _ := session.AppendLiveEvent(sessionID, event)
		_ = off
		session.Global.Publish(sessionID, event)
		select {
		case resp := <-state.permQueue(sessionID):
			return resp.Allow, resp.AllowAll
		case <-time.After(60 * time.Second):
			return false, false
		case <-ctx.Done():
			return false, false
		}
	}
}

func apiResolveStepProviderModel(step *todo.Step, defaultProvider, defaultModel string) (provider, model string) {
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
	if defaultProvider != "" {
		return defaultProvider, defaultModel
	}
	return ai.Global.BestProvider("", "")
}

func stepAgentName(step *todo.Step) string {
	if ac := agent.LookupAgentCfg(step.AgentID); ac != nil && ac.Name != "" {
		return ac.Name
	}
	return "agent"
}

func apiResolveCustomInstructions(agentID string) string {
	if ac := agent.LookupAgentCfg(agentID); ac != nil {
		if ac.SystemPrompt != "" {
			return ac.SystemPrompt
		}
		return ac.Role
	}
	return ""
}

func apiBuildfTeamContext(t *todo.Task) string {
	if len(t.TeamAgentIDs) == 0 {
		return ""
	}
	var lines []string
	for _, mid := range t.TeamAgentIDs {
		if ac := agent.LookupAgentCfg(mid); ac != nil {
			lines = append(lines, fmt.Sprintf("  - %s (agent_id: %q)", ac.Name, mid))
		} else {
			lines = append(lines, fmt.Sprintf("  - %s (agent_id: %q)", mid, mid))
		}
	}
	return fmt.Sprintf(
		"You are the lead of a team. Delegate work to your team members using spawn_sub_agent:\n%s\n"+
			"After spawning all sub-agents call wait_for_subagents to collect results.",
		strings.Join(lines, "\n"),
	)
}

func apiBuildfHandoff(workspacePath, sessionID string, completedStepNumber int) string {
	sess, _, err := session.LoadTail(workspacePath, sessionID, 0)
	if err != nil || sess == nil {
		return ""
	}
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

func apiPatchStepAndTask(workspacePath string, t *todo.Task, stepIdx int, stepStatus, taskStatus, sessionID string) {
	if stepIdx < len(t.Steps) {
		t.Steps[stepIdx].Status = stepStatus
		if sessionID != "" {
			t.Steps[stepIdx].SessionID = sessionID
		}
	}
	t.Status = taskStatus
	todo.Update(workspacePath, t)
	publishTodoUpdate(workspacePath, t)
}

func apiCoalesce(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func apiFirstProvider(defaultProvider, defaultModel string) (provider, model string) {
	if defaultProvider != "" {
		return defaultProvider, defaultModel
	}
	return ai.Global.BestProvider("", "")
}

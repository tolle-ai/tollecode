package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
)

// agentCfg is a minimal view of the agent record stored in agents.json.
type agentCfg struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Color        string   `json:"color"`
	Photo        string   `json:"photo"`
	Gradient     string   `json:"gradient"`
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	SystemPrompt string   `json:"systemPrompt"`
	Skills       []string `json:"skills"`
}

// LookupAgentCfg returns the stored config for agentID, or nil if not found.
func LookupAgentCfg(agentID string) *agentCfg {
	return lookupAgentCfg(agentID)
}

func lookupAgentCfg(agentID string) *agentCfg {
	if agentID == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(config.Home(), "agents.json"))
	if err != nil {
		return nil
	}
	var list []agentCfg
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	for i := range list {
		if list[i].ID == agentID {
			return &list[i]
		}
	}
	return nil
}

type subAgentEntry struct {
	id        string
	role      string
	label     string           // LLM-assigned dependency label (e.g. "coding", "testing")
	output    string           // final assistant text, captured after Execute completes
	taskTitle string           // short display title for the agent card
	color     string           // accent color for the agent card
	saItems   []map[string]any // items from the subagent's last assistant message
	injected  bool             // true once the subagent item has been added to parent items
	done      chan struct{}
}

type subAgentTracker struct {
	mu     sync.Mutex
	agents []*subAgentEntry
}

func newSubAgentTracker() *subAgentTracker {
	return &subAgentTracker{}
}

func (t *subAgentTracker) add(id, role string) *subAgentEntry {
	e := &subAgentEntry{id: id, role: role, done: make(chan struct{})}
	t.mu.Lock()
	t.agents = append(t.agents, e)
	t.mu.Unlock()
	return e
}

// runningEntries returns all entries whose done channel has not been closed yet.
func (t *subAgentTracker) runningEntries() []*subAgentEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	var running []*subAgentEntry
	for _, e := range t.agents {
		select {
		case <-e.done:
			// finished; skip
		default:
			running = append(running, e)
		}
	}
	return running
}

// agentNameIsRunning returns true if there is already an active (not-yet-done)
// subagent entry whose display name matches agentName. Checked by display name
// because add() stores ac.Name in e.role. Tool dispatch is sequential per
// session so no separate atomic add is required.
func (t *subAgentTracker) agentNameIsRunning(agentName string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.agents {
		if e.role != agentName {
			continue
		}
		select {
		case <-e.done:
			// channel closed → this entry finished, keep looking
		default:
			return true // channel still open → agent is running
		}
	}
	return false
}

// waitForLabels blocks until all entries with the given labels are done.
// Called synchronously in the tool-dispatch loop before spawning a dependent agent.
func (t *subAgentTracker) waitForLabels(ctx context.Context, labels []string) bool {
	for _, label := range labels {
		t.mu.Lock()
		var target *subAgentEntry
		for _, e := range t.agents {
			if e.label == label {
				target = e
				break
			}
		}
		t.mu.Unlock()

		if target == nil {
			continue // unknown label — skip rather than block forever
		}
		select {
		case <-target.done:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func (t *subAgentTracker) waitForOne(ctx context.Context, id string) (*subAgentEntry, bool) {
	t.mu.Lock()
	var target *subAgentEntry
	for _, e := range t.agents {
		if e.id == id {
			target = e
			break
		}
	}
	t.mu.Unlock()

	if target == nil {
		return nil, false
	}
	select {
	case <-target.done:
		return target, true
	case <-ctx.Done():
		return nil, false
	}
}

func (t *subAgentTracker) waitAll(ctx context.Context) ([]*subAgentEntry, bool) {
	t.mu.Lock()
	entries := make([]*subAgentEntry, len(t.agents))
	copy(entries, t.agents)
	t.mu.Unlock()

	var completed []*subAgentEntry
	for _, e := range entries {
		select {
		case <-e.done:
			completed = append(completed, e)
		case <-ctx.Done():
			return completed, false
		}
	}
	return completed, true
}

// drainSubAgentItems returns a "kind: subagent" history item for every
// completed subagent that has not yet been injected into the parent items list.
// Non-blocking: entries whose done channel isn't closed are skipped.
func (t *subAgentTracker) drainSubAgentItems() []map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	var result []map[string]any
	for _, e := range t.agents {
		if e.injected {
			continue
		}
		select {
		case <-e.done:
			e.injected = true
			result = append(result, map[string]any{
				"kind":         "subagent",
				"subSessionId": e.id,
				"role":         e.role,
				"taskTitle":    e.taskTitle,
				"color":        e.color,
				"items":        e.saItems,
			})
		default:
		}
	}
	return result
}

// toMaps converts a raw []interface{} (from JSON decode) to []map[string]any.
func toMaps(raw any) []map[string]any {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			result = append(result, m)
		}
	}
	return result
}

// emitToParent writes a lifecycle event to the parent session's full emit
// pipeline (stdout + JSONL live file + session bus) so both WS clients and
// stdio listeners receive it and can replay it after reconnect.
func emitToParent(cfg *Config, event map[string]any) {
	if cfg.EmitEvent != nil {
		cfg.EmitEvent(event)
	} else {
		cfg.EmitFn(event)
		session.Global.Publish(cfg.SessionID, event)
	}
}

// deriveSubAgentOutcome maps a sub-agent execution context and error flag to the
// status reported in the sub_agent_done event. cancelled takes precedence over
// failed so callers that observe context cancellation can tell the difference.
func deriveSubAgentOutcome(ctx context.Context, hadError bool) string {
	if ctx.Err() != nil {
		return "cancelled"
	}
	if hadError {
		return "failed"
	}
	return "done"
}

// BuildTeamLeadContext generates the CustomInstructions injected into a team lead's
// session. It lists each member's full profile and embeds the delegation protocol
// so the lead knows exactly who to delegate to, in what order, and how.
func BuildTeamLeadContext(memberIDs []string) string {
	if len(memberIDs) == 0 {
		return ""
	}

	var memberLines []string
	for _, mid := range memberIDs {
		if ac := lookupAgentCfg(mid); ac != nil {
			// A member is only a "general assistant" when it has NEITHER a role NOR
			// any skills. If it has skills but no explicit role, describe it by its
			// specialisation — never downgrade a skilled agent to a generalist.
			role := ac.Role
			if role == "" {
				if len(ac.Skills) > 0 {
					role = "specialist — " + strings.Join(ac.Skills, ", ")
				} else {
					role = "general assistant"
				}
			}
			line := fmt.Sprintf("  - **%s** (agent_id: %q)\n    Role: %s", ac.Name, mid, role)
			if len(ac.Skills) > 0 {
				line += fmt.Sprintf("\n    Skills: %s", strings.Join(ac.Skills, ", "))
			}
			memberLines = append(memberLines, line)
		} else {
			memberLines = append(memberLines, fmt.Sprintf("  - unknown agent (agent_id: %q)", mid))
		}
	}

	return `You are the team lead and orchestrator. Your job is to plan, coordinate, and synthesise — NOT to do the specialised work yourself.

## Your team members
` + strings.Join(memberLines, "\n") + `

Every member listed above is a SPECIALIST defined by the Role and Skills shown. Treat them by
that declared role. NEVER describe, address, or reason about a member that has a Role or Skills as
a "general assistant" — the roster above is authoritative and overrides any assumption you might
otherwise make about who these agents are.

## Orchestration protocol — follow this exactly

### Step 1: Plan before delegating
Before calling delegate_task even once, think through the full work breakdown:
- What discrete tasks need to be done?
- Which team member's role and skills match each task?
- Which tasks depend on the output of another (sequential) vs can run at the same time (parallel)?

### Step 2: Delegate — never do specialised work yourself
- Use delegate_task for EVERY piece of specialised work.
- Only assign a task to an agent whose role/skills match that work. Do NOT assign a coding task to a reviewer or a testing task to a designer.
- Set task_label on every delegate_task call (e.g. "coding", "review", "testing").
- For tasks that must run in sequence: set wait_for to the labels they depend on. The system waits automatically — you do not need to call wait_for_agent manually.
- For independent tasks: delegate them all at once (multiple delegate_task calls in one response) and then call wait_for_team.

### Step 3: Synthesise
- After all members finish, synthesise their outputs into a coherent result for the user.
- Call finish_task only after every delegated task has completed and you have reviewed the outputs.

### Hard rules
- You MUST NOT write code, run tests, create files, or perform any task a team member is responsible for.
- You MUST NOT call finish_task before all delegated tasks are done.
- If no team member has the right skills for a task, say so clearly — do not improvise.`
}

// deriveAgentRole is a last-resort fallback when the model provides no name.
func deriveAgentRole(task string) string {
	t := strings.ToLower(task)
	switch {
	case containsAny(t, "analyz", "analyse", "inspect", "examin"):
		return "analyst"
	case containsAny(t, "review", "audit", "assess"):
		return "reviewer"
	case containsAny(t, "research", "investigat", "find out", "look up"):
		return "researcher"
	case containsAny(t, "test", " spec ", " qa "):
		return "tester"
	case containsAny(t, "write", "document", "draft", "summar"):
		return "writer"
	case containsAny(t, "fix", "debug", "patch", "resolv"):
		return "debugger"
	case containsAny(t, "refactor", "clean up", "optimiz"):
		return "refactorer"
	case containsAny(t, "plan", "design", "architect"):
		return "planner"
	case containsAny(t, "implement", "build", "creat", "develop"):
		return "developer"
	default:
		return "agent"
	}
}

func containsAny(s string, substrings ...string) bool {
	for _, sub := range substrings {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func toolSpawnSubAgent(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	message, _ := inp["message"].(string)
	if message == "" {
		return "Error: 'message' is required for spawn_sub_agent.", true
	}
	agentID, _ := inp["agent_id"].(string)

	// Model-provided name/role take priority over static derivation.
	modelName, _ := inp["name"].(string)
	modelRole, _ := inp["role"].(string)

	parentSess, err := session.Load(cfg.Workspace, cfg.SessionID)
	if err != nil {
		return "Error loading parent session: " + err.Error(), true
	}

	// Resolve provider/model/instructions from the named agent if provided.
	provider := parentSess.Provider
	model := parentSess.Model
	customInstructions := ""
	agentColor := "#a855f7"
	var agentSkillNames []string

	ac := lookupAgentCfg(agentID)
	if ac != nil {
		if ac.Color != "" {
			agentColor = ac.Color
		}
		if ac.Provider != "" {
			if resolved, ok := ai.Global.ResolveProviderID(ac.Provider); ok {
				provider = resolved
			} else {
				provider = ac.Provider
			}
		}
		if ac.Model != "" {
			model = ac.Model
		}
		if provider != "" && model == "" {
			model = ai.Global.DefaultModel(provider)
		}
		if ac.SystemPrompt != "" {
			customInstructions = ac.SystemPrompt
		} else if ac.Role != "" {
			customInstructions = ac.Role
		}
		agentSkillNames = ac.Skills
	}

	// Resolve display name: configured agent > model-provided > keyword fallback.
	agentName := modelName
	if ac != nil && ac.Name != "" {
		agentName = ac.Name
	}
	if agentName == "" {
		agentName = deriveAgentRole(message)
	}

	agentPhoto := ""
	agentGradient := ""
	agentRole := ""
	if ac != nil {
		agentPhoto = ac.Photo
		agentGradient = ac.Gradient
		agentRole = ac.Role
	}

	// If the model provided a role and no configured agent has a system prompt,
	// use it as the sub-agent's custom instructions.
	if modelRole != "" && customInstructions == "" {
		customInstructions = modelRole
	}

	// When spawning a configured agent, wrap its instructions with identity anchors
	// so it stays in its specialist role (same as toolDelegateTask does for team members).
	if ac != nil && agentName != "" {
		persona := customInstructions
		customInstructions = ""
		if persona != "" {
			customInstructions = persona + "\n\n"
		}
		customInstructions += fmt.Sprintf(
			"You are %s. You operate exclusively within your defined role and skills. "+
				"You do NOT act as a general-purpose assistant or take on work outside your specialty. "+
				"If the assigned task is outside your role or skills, call task_out_of_scope immediately.",
			agentName,
		)
	}

	// Determine which skills to activate for the sub-agent.
	// If the agent config specifies skills, use those exclusively.
	// Otherwise, inherit the parent session's active skills.
	var agentSkills []string
	if len(agentSkillNames) > 0 {
		agentSkills = agentSkillNames
	} else {
		agentSkills = parentSess.ActiveSkills
	}

	subSess, err := session.Create(
		cfg.Workspace,
		provider,
		model,
		parentSess.Mode,
		session.WithParent(cfg.SessionID),
		session.WithSkills(agentSkills),
	)
	if err != nil {
		return "Error creating sub-agent session: " + err.Error(), true
	}

	subID := subSess.ID
	entry := cfg.subAgents.add(subID, agentName)
	parentSessionID := cfg.SessionID

	// Derive a short task title from the first line of the message.
	taskTitle := message
	if i := strings.IndexAny(message, "\n."); i > 0 && i < 80 {
		taskTitle = message[:i]
	} else if len(message) > 80 {
		taskTitle = message[:80] + "…"
	}
	entry.taskTitle = taskTitle
	entry.color = agentColor

	emitToParent(cfg, map[string]any{
		"type":         "sub_agent_spawned",
		"subSessionId": subID,
		"session_id":   parentSessionID,
		"taskTitle":    taskTitle,
		"instructions": message,
		"role":         agentName,
		"color":        agentColor,
		"photo":        agentPhoto,
		"gradient":     agentGradient,
		"agentRole":    agentRole,
	})

	go func() {
		hadError := Execute(ctx, Config{
			SessionID:          subID,
			Workspace:          cfg.Workspace,
			Message:            message,
			Mode:               cfg.Mode,
			ThinkingBudget:     cfg.ThinkingBudget,
			ThinkLevel:         cfg.ThinkLevel,
			DesktopPermitted:   cfg.DesktopPermitted,
			BrowserAvailable:   cfg.BrowserAvailable,
			ShellAutoAllow:     cfg.ShellAutoAllow,
			IsSubAgent:         true,
			AgentName:          agentName,
			CustomInstructions: customInstructions,
			EmitFn:             cfg.EmitFn,
			RequestPerm:        cfg.RequestPerm,
			// Share the parent's permission gate so this sub-agent inherits its
			// grants/denials and never re-prompts for what the parent already allowed.
			gate:           cfg.gate,
			TakeScreenshot: cfg.TakeScreenshot,
		})

		// Capture the sub-agent's final assistant output and items before signalling
		// done, so toolWaitForSubAgents can inject them into the parent turn.
		if tail, _, err := session.LoadTail(cfg.Workspace, subID, 3); err == nil {
			for i := len(tail.Messages) - 1; i >= 0; i-- {
				if role, _ := tail.Messages[i]["role"].(string); role == "assistant" {
					if content, _ := tail.Messages[i]["content"].(string); content != "" {
						entry.output = content
					}
					entry.saItems = toMaps(tail.Messages[i]["items"])
					break
				}
			}
		}

		status := deriveSubAgentOutcome(ctx, hadError)
		close(entry.done)
		emitToParent(cfg, map[string]any{
			"type":         "sub_agent_done",
			"subSessionId": subID,
			"session_id":   parentSessionID,
			"status":       status,
			"output":       entry.output,
			"role":         agentName,
			"color":        agentColor,
			"photo":        agentPhoto,
			"gradient":     agentGradient,
			"agentRole":    agentRole,
		})
	}()

	return fmt.Sprintf(`{"sub_agent_id":%q,"agent":%q,"status":"spawned"}`, subID, agentName), false
}

func toolWaitForSubAgents(ctx context.Context, cfg *Config) (string, bool) {
	entries, ok := cfg.subAgents.waitAll(ctx)
	if !ok {
		return "Cancelled while waiting for sub-agents.", true
	}
	if len(entries) == 0 {
		return "No sub-agents were spawned.", false
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "All %d sub-agent(s) completed. Here are their outputs:\n", len(entries))
	for _, e := range entries {
		label := e.role
		if label == "" {
			label = "sub-agent"
		}
		fmt.Fprintf(&sb, "\n--- %s ---\n", label)
		if e.output != "" {
			sb.WriteString(e.output)
		} else {
			sb.WriteString("(sub-agent produced no text output)")
		}
		sb.WriteString("\n")
	}
	return sb.String(), false
}

// toolWaitForAgent blocks until one specific delegated agent (identified by its
// session ID returned from delegate_task) completes. Returns only that agent's output.
// This enables sequential orchestration: delegate → wait_for_agent → pass output to next delegate.
func toolWaitForAgent(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	agentID, _ := inp["agent_id"].(string)
	if agentID == "" {
		return "Error: 'agent_id' is required. Use the 'delegated_to' value from delegate_task.", true
	}

	entry, found := cfg.subAgents.waitForOne(ctx, agentID)
	if !found {
		if ctx.Err() != nil {
			return "Cancelled while waiting for agent.", true
		}
		return fmt.Sprintf("Error: no delegated agent with id %q found. Check the 'delegated_to' value from delegate_task.", agentID), true
	}

	name := entry.role
	if name == "" {
		name = "agent"
	}
	output := entry.output
	if output == "" {
		output = "(agent produced no text output)"
	}
	return fmt.Sprintf("Agent %q completed.\n\nOutput:\n%s", name, output), false
}

// toolDelegateTask assigns a task to a pre-configured team member (team mode only).
// Unlike spawn_sub_agent, agent_id is required and must be a configured agent.
func toolDelegateTask(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	agentID, _ := inp["agent_id"].(string)
	if agentID == "" {
		return "Error: 'agent_id' is required for delegate_task. Specify which team member should handle this.", true
	}
	task, _ := inp["task"].(string)
	if task == "" {
		return "Error: 'task' is required for delegate_task.", true
	}
	context_, _ := inp["context"].(string)
	taskLabel, _ := inp["task_label"].(string)

	// Block until all declared dependency labels have completed.
	// This enforces sequential ordering even when the LLM emits multiple
	// delegate_task calls in a single response.
	var waitFor []string
	if raw, ok := inp["wait_for"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				waitFor = append(waitFor, s)
			}
		}
	}
	if len(waitFor) > 0 {
		if !cfg.subAgents.waitForLabels(ctx, waitFor) {
			return "Cancelled while waiting for dependencies.", true
		}
		// Inject the completed agents' outputs as additional context.
		var depOutputs strings.Builder
		cfg.subAgents.mu.Lock()
		for _, label := range waitFor {
			for _, e := range cfg.subAgents.agents {
				if e.label == label && e.output != "" {
					fmt.Fprintf(&depOutputs, "\n--- Output from %s (%s) ---\n%s\n", e.role, label, e.output)
				}
			}
		}
		cfg.subAgents.mu.Unlock()
		if depOutputs.Len() > 0 {
			if context_ != "" {
				context_ = context_ + "\n\nPrior agent outputs:" + depOutputs.String()
			} else {
				context_ = "Prior agent outputs:" + depOutputs.String()
			}
		}
	}

	// Validate that the agent is a known team member.
	ac := lookupAgentCfg(agentID)
	if ac == nil {
		return fmt.Sprintf("Error: agent %q is not a configured agent. Check the agent_id.", agentID), true
	}

	// Enforce one active task per agent: reject if this agent is already running.
	if cfg.subAgents.agentNameIsRunning(ac.Name) {
		return fmt.Sprintf(
			"Error: agent %q is already running a task. Use wait_for_agent with its session ID to wait for it to finish before assigning another task.",
			ac.Name,
		), true
	}

	parentSess, err := session.Load(cfg.Workspace, cfg.SessionID)
	if err != nil {
		return "Error loading parent session: " + err.Error(), true
	}

	provider := parentSess.Provider
	model := parentSess.Model
	if ac.Provider != "" {
		if resolved, ok := ai.Global.ResolveProviderID(ac.Provider); ok {
			provider = resolved
		} else {
			provider = ac.Provider
		}
	}
	if ac.Model != "" {
		model = ac.Model
	}
	if provider != "" && model == "" {
		model = ai.Global.DefaultModel(provider)
	}

	// Build a strong specialist identity block for customInstructions.
	// The agent's own systemPrompt/role describes what they do; we wrap it
	// with hard identity anchors so the LLM cannot drift from its persona.
	var ciParts []string
	if ac.SystemPrompt != "" {
		ciParts = append(ciParts, ac.SystemPrompt)
	} else if ac.Role != "" {
		ciParts = append(ciParts, "Your role: "+ac.Role)
	}
	ciParts = append(ciParts, fmt.Sprintf(
		"You are %s. You operate exclusively within your defined role and skills. "+
			"You do NOT act as a general-purpose assistant or take on work outside your specialty. "+
			"If the delegated task is outside your role or skills, call task_out_of_scope immediately.",
		ac.Name,
	))
	customInstructions := strings.Join(ciParts, "\n\n")

	// Build the full message: identity reminder + context + task + completion mandate.
	var msgParts []string
	msgParts = append(msgParts, fmt.Sprintf("You are %s. Complete the task below in full — every step — before calling finish_task.", ac.Name))
	if context_ != "" {
		msgParts = append(msgParts, "Context from lead:\n"+context_)
	}
	msgParts = append(msgParts, "Your task:\n"+task)
	msgParts = append(msgParts, "Do not stop partway through. If a step is blocked, report that specifically rather than calling finish_task on incomplete work.")
	message := strings.Join(msgParts, "\n\n")

	agentColor := ac.Color
	if agentColor == "" {
		agentColor = "#a855f7"
	}

	agentPhoto := ""
	agentGradient := ""
	agentRole := ""
	if ac != nil {
		agentPhoto = ac.Photo
		agentGradient = ac.Gradient
		agentRole = ac.Role
	}

	var agentSkills []string
	if len(ac.Skills) > 0 {
		agentSkills = ac.Skills
	} else {
		agentSkills = parentSess.ActiveSkills
	}

	subSess, err := session.Create(
		cfg.Workspace,
		provider,
		model,
		parentSess.Mode,
		session.WithParent(cfg.SessionID),
		session.WithSkills(agentSkills),
	)
	if err != nil {
		return "Error creating session for team member: " + err.Error(), true
	}

	subID := subSess.ID
	entry := cfg.subAgents.add(subID, ac.Name)
	entry.label = taskLabel
	parentSessionID := cfg.SessionID

	taskTitle := task
	if i := strings.IndexAny(task, "\n."); i > 0 && i < 80 {
		taskTitle = task[:i]
	} else if len(task) > 80 {
		taskTitle = task[:80] + "…"
	}
	entry.taskTitle = taskTitle
	entry.color = agentColor

	emitToParent(cfg, map[string]any{
		"type":         "sub_agent_spawned",
		"subSessionId": subID,
		"session_id":   parentSessionID,
		"taskTitle":    taskTitle,
		"instructions": message,
		"role":         ac.Name,
		"color":        agentColor,
		"photo":        agentPhoto,
		"gradient":     agentGradient,
		"agentRole":    agentRole,
		"teamMember":   true,
		"agentId":      agentID,
	})

	go func() {
		Execute(ctx, Config{
			SessionID:          subID,
			Workspace:          cfg.Workspace,
			Message:            message,
			Mode:               cfg.Mode,
			ThinkingBudget:     cfg.ThinkingBudget,
			ThinkLevel:         cfg.ThinkLevel,
			DesktopPermitted:   cfg.DesktopPermitted,
			BrowserAvailable:   cfg.BrowserAvailable,
			ShellAutoAllow:     cfg.ShellAutoAllow,
			IsSubAgent:         true,
			AgentName:          ac.Name,
			CustomInstructions: customInstructions,
			EmitFn:             cfg.EmitFn,
			RequestPerm:        cfg.RequestPerm,
			// Share the parent's permission gate so this delegated agent inherits
			// its grants/denials and never re-prompts for what the parent allowed.
			gate:           cfg.gate,
			TakeScreenshot: cfg.TakeScreenshot,
		})

		if tail, _, err := session.LoadTail(cfg.Workspace, subID, 3); err == nil {
			for i := len(tail.Messages) - 1; i >= 0; i-- {
				if role, _ := tail.Messages[i]["role"].(string); role == "assistant" {
					if content, _ := tail.Messages[i]["content"].(string); content != "" {
						entry.output = content
					}
					entry.saItems = toMaps(tail.Messages[i]["items"])
					break
				}
			}
		}

		status := deriveSubAgentOutcome(ctx, false)
		close(entry.done)
		emitToParent(cfg, map[string]any{
			"type":         "sub_agent_done",
			"subSessionId": subID,
			"session_id":   parentSessionID,
			"status":       status,
			"output":       entry.output,
			"teamMember":   true,
			"role":         ac.Name,
			"color":        agentColor,
			"photo":        agentPhoto,
			"gradient":     agentGradient,
			"agentRole":    agentRole,
		})
	}()

	return fmt.Sprintf(`{"delegated_to":%q,"agent":%q,"status":"running"}`, subID, ac.Name), false
}

// toolWaitForTeam blocks until all team members delegated via delegate_task finish.
// Reuses the same subAgentTracker as spawn_sub_agent.
func toolWaitForTeam(ctx context.Context, cfg *Config) (string, bool) {
	entries, ok := cfg.subAgents.waitAll(ctx)
	if !ok {
		return "Cancelled while waiting for team members.", true
	}
	if len(entries) == 0 {
		return "No tasks were delegated.", false
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "All %d team member(s) completed. Here are their outputs:\n", len(entries))
	for _, e := range entries {
		name := e.role
		if name == "" {
			name = "team member"
		}
		fmt.Fprintf(&sb, "\n=== %s ===\n", name)
		if e.output != "" {
			sb.WriteString(e.output)
		} else {
			sb.WriteString("(no output)")
		}
		sb.WriteString("\n")
	}
	return sb.String(), false
}

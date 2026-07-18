// Package agent implements the core agentic loop for the Go sidecar.
package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
)

// maxToolIterations is the hard-coded fallback; callers should use
// config.GetSidecarSettings().EffectiveMaxIterations() for the user-configured value.
const maxToolIterations = 100

// maxLiveToolOutput caps how many characters of a tool result are sent back to
// the LLM in the live in-memory history. Large outputs (search results, file
// reads) are stored in full in the session file and shown to the user, but
// truncated here to keep Ollama's request body and context window manageable.
const maxLiveToolOutput = 8_000

// ClarificationAnswer is the structured result returned by ask_followup_question.
// Selected carries the user's chosen suggestions; Details carries optional free-text.
type ClarificationAnswer struct {
	Selected []string `json:"selected"`
	Details  string   `json:"details"`
}

// Config bundles everything Execute needs to run an agent turn.
type Config struct {
	SessionID        string
	Workspace        string
	Message          string
	Mode             string // override session mode; empty = use session's mode
	ThinkingBudget   int
	ThinkLevel       string // Ollama: "", "true", "false", "low", "medium", "high"
	DesktopPermitted bool
	IsSubAgent       bool

	// EmitFn writes one event to the transport (stdout or WebSocket).
	EmitFn func(map[string]any)

	// RequestPerm asks the user to allow a shell command.
	// Returns (allow, allowAll). Nil → always deny.
	RequestPerm func(ctx context.Context, command string) (allow, allowAll bool)

	// RequestClarification pauses the agent and asks the user a clarifying question.
	// suggestions is an optional list of pre-written answers to show as chips.
	// multiChoice allows the user to select multiple suggestions simultaneously.
	// Returns the user's structured answer and ok=true, or (zero ClarificationAnswer, false) on timeout/cancel.
	// Nil → clarification not available (sub-agents, legacy callers).
	RequestClarification func(ctx context.Context, question string, suggestions []string, multiChoice bool) (answer ClarificationAnswer, ok bool)

	// TakeScreenshot triggers Tauri to capture the screen.
	// Returns the payload {image, width, height} or an error.
	// Nil → screenshot not available.
	TakeScreenshot func(ctx context.Context) (map[string]any, error)

	// EmitEvent is the full emit function (stdout + JSONL + session bus).
	// Set by Execute(); tools should use this instead of EmitFn so that
	// events like screen_event are visible to WebSocket clients.
	EmitEvent func(map[string]any)

	// subAgents tracks sub-agents spawned during this turn (internal).
	subAgents *subAgentTracker

	// TeamMemberIDs, when non-empty, switches the agent into team-lead mode.
	// In this mode spawn_sub_agent is replaced by delegate_task/wait_for_team
	// so the lead delegates to configured team members instead of spawning
	// ad-hoc sub-agents.
	TeamMemberIDs []string

	// ShellAutoAllow bypasses the per-command permission prompt for run_shell.
	// Set true to auto-allow all shell commands without asking the user.
	ShellAutoAllow bool

	// BrowserAvailable enables the browser tool set.
	// True in dev mode; false in channels (physical screen control only).
	BrowserAvailable bool

	// AgentName is the display name of a configured agent running as a team member.
	// When set, buildSystem puts the agent's identity at the very top of the system
	// prompt so the LLM anchors on its specialist role before reading anything else.
	AgentName string

	// CustomInstructions is the agent's persona/systemPrompt text.
	// Placed at the top of the system prompt when AgentName is set; appended otherwise.
	CustomInstructions string

	// Audit identity — attributed to actions in the tamper-evident audit log.
	// Any field may be empty on surfaces without that identity (e.g. desktop).
	UserID     string
	TenantID   string
	ActorLabel string

	// OverrideModel, when non-empty, replaces the session's stored model for this turn only.
	// Used by the CLI agent picker to honour per-agent model preferences.
	OverrideModel string

	// Images are user-attached base64-encoded images for the current turn.
	// Not persisted to disk — injected only into the in-memory user ChatMessage.
	Images []string

	// ProviderID is the ID of the session's active provider.
	// Set by Execute() after loading the session; used by RAG tools for embeddings.
	ProviderID string

	// ExtDispatch, when non-nil, is called before the built-in tool switch in Dispatch.
	// If it returns handled=true the built-in switch is skipped entirely.
	// Pro edition uses this to route "integration__*" tool calls.
	ExtDispatch ExtDispatchFunc

	// SystemOverride, when non-nil, replaces the output of buildSystem() entirely.
	// Pro edition uses this to inject identity, integrations, and persona instructions.
	SystemOverride *string

	// MemoryEnabled is set by Execute() from the workspace memory config.
	// When true, the save_memory tool is included and the system prompt mentions it.
	MemoryEnabled bool

	// RequestSystemPermission asks the frontend to guide the user to grant a macOS
	// system permission. permType is "accessibility" (mouse/keyboard control).
	// Returns true when the user grants it, false on timeout or denial.
	// Nil → permission flow not available (sub-agents, legacy callers).
	RequestSystemPermission func(ctx context.Context, permType string) bool

	// RequestContinue is called when ConfirmContinue is enabled and the agent
	// reaches the confirmation threshold. It pauses the loop and asks the user
	// whether to keep going. Returns true to continue, false to stop.
	// Nil → always continue (no prompt).
	RequestContinue func(ctx context.Context, iteration, maxIter int) bool

	// gate holds this turn's permission grants/denials. It is shared by pointer
	// with every sub-agent spawned during the turn (see subagent.go) so an "allow
	// all" or "deny" made by any agent is inherited by the rest instead of
	// re-prompting. Initialised by Execute when nil.
	gate *permGate

	// Screen coordinate mapping — populated after the first screenshot.
	// Coordinates the LLM sees are in image space (lastScreenImgW × lastScreenImgH).
	// Coordinates CoreGraphics mouse events use are in logical screen space.
	// Scale: logical_x = img_x * lastScreenLogicalW / lastScreenImgW
	lastScreenImgW     int
	lastScreenImgH     int
	lastScreenLogicalW int
	lastScreenLogicalH int
}

// auditActor builds the actor attribution for the tamper-evident audit log,
// falling back to the agent name when no explicit label was supplied.
func (c *Config) auditActor() session.Actor {
	label := c.ActorLabel
	if label == "" {
		label = c.AgentName
	}
	return session.Actor{UserID: c.UserID, TenantID: c.TenantID, Label: label}
}

// permGate holds a turn's permission grants and denials. One gate is shared by a
// session turn and every sub-agent it spawns, so a grant ("allow all") or denial
// made by any agent is honoured by the rest instead of re-prompting — sub-agents
// inherit the parent's permissions.
//
// The mutex also serialises the prompt itself: when several parallel sub-agents
// reach a permission check at once, only the first shows a prompt while holding
// the lock; the others block, then re-read the now-updated state and proceed
// without a second prompt. This prevents duplicate stacked prompts (and, in the
// CLI, the concurrent raw-mode terminal corruption they caused).
type permGate struct {
	mu     sync.Mutex
	shell  bool // "allow all" granted for shell commands
	file   bool // "allow all" granted for file writes/edits/plans
	denied bool // user denied — auto-deny everything else this turn
}

// permDecision is the outcome of a permission check.
type permDecision int

const (
	permAllowed     permDecision = iota // proceed
	permDenied                          // user denied (now or earlier) — caller emits permission_denied
	permUnavailable                     // no prompter wired up in this context
)

// checkPermission runs the shared, serialised permission gate for one operation.
// kind is "shell" or "file"; promptArg is the description passed to RequestPerm.
// A decision is made at most once per turn across the whole agent tree: an "allow
// all" or denial by any agent (parent or sub-agent) short-circuits the rest.
func (c *Config) checkPermission(ctx context.Context, kind, promptArg string) permDecision {
	if c.ShellAutoAllow {
		return permAllowed
	}
	g := c.gate
	if g == nil {
		// Defensive: a caller that didn't route through Execute (or a zero-value
		// Config in a test) gets a local gate so behaviour is still coherent.
		g = &permGate{}
		c.gate = g
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.denied {
		return permDenied
	}
	if (kind == "file" && g.file) || (kind != "file" && g.shell) {
		return permAllowed
	}
	if c.RequestPerm == nil {
		return permUnavailable
	}

	allow, allowAll := c.RequestPerm(ctx, promptArg)
	if !allow {
		g.denied = true
		return permDenied
	}
	if allowAll {
		// "Allow all" covers both shell and file operations for the turn, matching
		// the user's expectation that clicking it stops all further prompts.
		g.shell = true
		g.file = true
	}
	return permAllowed
}

// awaitPermissionGate blocks until no permission prompt is currently in
// flight anywhere in this agent's tree (this agent or a concurrently running
// sub-agent sharing the same gate). checkPermission only guards run_shell,
// write_file, edit_file, and create_plan — every other tool (browser control,
// desktop mouse/keyboard, email, calendar, memory…) never calls it at all. On
// its own that let a sub-agent freely run those unguarded tools while a
// sibling's prompt sat unanswered — a pending prompt must freeze the whole
// agent, not just the specific call that triggered it. Dispatch calls this
// before every tool, gated or not, so that guarantee holds universally.
func (c *Config) awaitPermissionGate() {
	g := c.gate
	if g == nil {
		return
	}
	g.mu.Lock()
	g.mu.Unlock()
}

// ExtDispatchFunc is the pro-edition extension hook type for tool dispatch.
// If handled is true the built-in switch is skipped.
type ExtDispatchFunc func(ctx context.Context, cfg *Config, toolName string, inp map[string]any) (output, imageData string, isError, handled bool)

// Execute runs one user turn of the agentic loop.
// It loads the session, streams the LLM, executes tools in a loop,
// persists messages, and emits all events via cfg.EmitFn.
// Returns true when the turn ended due to a provider / agent error so callers
// can mark the session as "failed" rather than "idle".
func Execute(ctx context.Context, cfg Config) (hadError bool) {
	// Initialize sub-agent tracker for this turn.
	cfg.subAgents = newSubAgentTracker()

	// Ensure a permission gate exists. Sub-agents receive the parent's gate (see
	// subagent.go) so they inherit its grants/denials; a top-level turn with no
	// gate yet gets a fresh one.
	if cfg.gate == nil {
		cfg.gate = &permGate{}
	}

	// Sub-agents are one-shot: tear down any browser tab they opened when they
	// finish so tabs don't leak across many delegations. The main session's tab
	// persists across turns and is cleaned up on delete_session instead.
	if cfg.IsSubAgent {
		defer DestroyBrowserSession(cfg.SessionID)
	}

	emit := func(m map[string]any) {
		m["session_id"] = cfg.SessionID
		cfg.EmitFn(m)
		// Persist to live file before publishing so WS clients can replay with _off.
		off, _ := session.AppendLiveEvent(cfg.SessionID, m)
		m["_off"] = off
		session.Global.Publish(cfg.SessionID, m)
	}
	// Expose the full emit to tools so screen_event etc. reach WS clients.
	cfg.EmitEvent = emit

	// Load session
	s, err := session.Load(cfg.Workspace, cfg.SessionID)
	if err != nil {
		emit(map[string]any{"type": "agent_error", "message": "session not found: " + cfg.SessionID})
		return true
	}

	// Resolve provider — support both literal IDs and type aliases like
	// "anthropic" or "ollama-cloud".
	resolvedID := s.Provider
	if resolved, ok := ai.Global.ResolveProviderID(resolvedID); ok {
		resolvedID = resolved
	}
	provider := ai.Global.Get(resolvedID)
	if provider == nil {
		ai.Global.Reload()
		if resolved2, ok := ai.Global.ResolveProviderID(s.Provider); ok {
			resolvedID = resolved2
		}
		provider = ai.Global.Get(resolvedID)
	}
	if provider == nil {
		emit(map[string]any{"type": "agent_error", "message": "provider not configured: " + s.Provider})
		return true
	}
	// Patch the session if the provider was a type alias that got resolved.
	if resolvedID != s.Provider {
		s.Provider = resolvedID
		session.UpdateFields(cfg.Workspace, cfg.SessionID, map[string]any{"provider": resolvedID})
	}
	cfg.ProviderID = resolvedID

	// Defensive: if the session model is empty (e.g. created before provider/model
	// were properly persisted), resolve the best default model for the provider.
	// This prevents passing model="" to the LLM API, which causes 404s from
	// providers like Ollama Cloud.
	if s.Model == "" {
		_, bestModel := ai.Global.BestProvider(resolvedID, "")
		if bestModel != "" {
			s.Model = bestModel
			session.UpdateFields(cfg.Workspace, cfg.SessionID, map[string]any{"model": bestModel})
		}
	}
	// Apply per-turn model override (e.g. from CLI agent picker) without persisting.
	if cfg.OverrideModel != "" {
		s.Model = cfg.OverrideModel
	}

	mode := s.Mode
	if cfg.Mode != "" {
		mode = cfg.Mode
	}

	// Build history and system prompt.
	cfg.MemoryEnabled = isMemoryEnabled(cfg.Workspace)
	msgs := s.Messages

	// Cap raw messages to the last 60 (≈30 turns) before building the AI history
	// so the context window doesn't grow unbounded across long sessions.
	const maxHistory = 60

	// If the session has been compacted, rebuild context from the summary + any
	// messages added after the compact point. CompactedMessageCount gives us a
	// reliable index-based split (no fragile timestamp string comparison).
	// For sessions compacted before this field was added, fall back to timestamps.
	//
	// The summary is injected as a user→assistant exchange so that the next real
	// user message forms a valid alternating sequence. A bare "user" summary
	// followed by another "user" message is rejected by most providers.
	if s.CompactedSummary != "" {
		var postCompact []map[string]any
		if s.CompactedMessageCount > 0 && s.CompactedMessageCount <= len(msgs) {
			// Primary path: exact index-based split.
			postCompact = msgs[s.CompactedMessageCount:]
		} else {
			// Fallback for sessions compacted before CompactedMessageCount was stored.
			for _, m := range msgs {
				ts, _ := m["timestamp"].(string)
				if ts > s.CompactedAt {
					postCompact = append(postCompact, m)
				}
			}
		}
		// Reserve 2 slots for the summary pair; trim postCompact to fit maxHistory.
		if len(postCompact) > maxHistory-2 {
			postCompact = postCompact[len(postCompact)-(maxHistory-2):]
		}
		summaryPair := []map[string]any{
			{"role": "user", "content": "What was discussed in our previous conversation?"},
			{"role": "assistant", "content": "[Previous session context — summarized]\n\n" + s.CompactedSummary},
		}
		msgs = append(summaryPair, postCompact...)
	} else if len(msgs) > maxHistory {
		msgs = msgs[len(msgs)-maxHistory:]
	}
	history := buildHistory(msgs)
	// Load active skills from the session and resolve their full content.
	var activeSkills []SkillDef
	if s.ActiveSkills != nil && len(s.ActiveSkills) > 0 {
		activeSkills = LoadActiveSkills(cfg.Workspace, s.ActiveSkills)
	}

	// Recall: pull the memories most relevant to this turn's request and inject
	// them into the system prompt. Sub-agents skip recall — they run against a
	// caller-supplied brief, not the workspace's accumulated history.
	recalled := ""
	if cfg.MemoryEnabled && !cfg.IsSubAgent {
		recalled = RecallMemory(cfg.Workspace, cfg.Message, 5)
	}

	system := buildSystem(cfg.Workspace, mode, cfg.MemoryEnabled, cfg.DesktopPermitted, cfg.AgentName, cfg.CustomInstructions, len(cfg.TeamMemberIDs) > 0, activeSkills, recalled)
	if cfg.SystemOverride != nil {
		system = *cfg.SystemOverride
	}

	// Detect interrupted prior turn: if the last stored message is an assistant
	// message that was cut short, inject a continuation prompt instead of the
	// raw user message so the LLM resumes rather than restarts.
	userContent := cfg.Message
	if len(s.Messages) > 0 {
		last := s.Messages[len(s.Messages)-1]
		if role, _ := last["role"].(string); role == "assistant" {
			if interrupted, _ := last["interrupted"].(bool); interrupted {
				userContent = "Your previous response was interrupted. Continue from where you left off. " +
					"Do NOT repeat work already done — resume the task.\n\nOriginal request: " + cfg.Message
			}
		}
	}

	// Persist the incoming user message (always store the raw request)
	userMsgID := uuid.NewString()
	TryGitSnapshot(cfg.Workspace, cfg.SessionID, userMsgID)
	userMsg := session.Message{
		ID:        userMsgID,
		Role:      "user",
		Content:   cfg.Message,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	_ = session.AppendMessage(cfg.Workspace, cfg.SessionID, userMsg)

	// Append to in-memory history for this call (may use the resume prompt).
	// Images are attached only to the in-memory entry — not persisted to disk.
	history = append(history, ai.ChatMessage{Role: "user", Content: userContent, Images: cfg.Images})

	emit(map[string]any{"type": "status", "status": "thinking"})

	finalText, thinking, toolUses, items, totalIn, totalOut, loopError := runLoop(ctx, cfg, s, mode, system, history, provider, emit)

	// Persist assistant message if we have any content.
	// Mark as interrupted when cancelled so the next turn can inject a resume prompt.
	if finalText != "" || len(toolUses) > 0 || len(items) > 0 {
		assistantMsg := session.Message{
			ID:          uuid.NewString(),
			Role:        "assistant",
			Content:     finalText,
			Thinking:    thinking,
			Timestamp:   time.Now().UTC().Format(time.RFC3339Nano),
			Provider:    s.Provider,
			Model:       s.Model,
			ToolUses:    toolUses,
			Items:       items,
			Interrupted: ctx.Err() != nil,
		}
		_ = session.AppendMessage(cfg.Workspace, cfg.SessionID, assistantMsg)
	}

	if totalIn > 0 || totalOut > 0 {
		_ = session.AddTokenUsage(cfg.Workspace, cfg.SessionID, totalIn, totalOut)
	}

	// Per-turn audit summary: which provider/model handled the turn, token cost,
	// tool count, and how it ended. Answers "what did the AI do this turn".
	turnOutcome := "completed"
	switch {
	case ctx.Err() != nil:
		turnOutcome = "cancelled"
	case loopError != "":
		turnOutcome = "error"
	}
	if _, err := session.AppendAudit(cfg.Workspace, cfg.SessionID, cfg.auditActor(), "turn", map[string]any{
		"provider":     s.Provider,
		"model":        s.Model,
		"inputTokens":  totalIn,
		"outputTokens": totalOut,
		"toolCount":    len(toolUses),
		"outcome":      turnOutcome,
	}); err != nil {
		emit(map[string]any{"type": "audit_error", "message": err.Error()})
	}

	if ctx.Err() == nil {
		if loopError != "" {
			// Provider / LLM error — emit a terminal agent_error so the WS
			// handler closes and the client stops showing a loading state.
			emit(map[string]any{"type": "agent_error", "message": loopError})
			return true
		}
		emit(map[string]any{"type": "done", "inputTokens": totalIn, "outputTokens": totalOut})
		if cfg.MemoryEnabled && !cfg.IsSubAgent {
			go autoSaveSessionMemory(cfg.Workspace, cfg.Message, finalText, items, provider, s.Model, turnOutcome)
		}
	}
	return false
}

// effectiveContextWindow returns the runtime context window for the given
// provider/model without a network call. For Ollama it is the configured num_ctx
// (the value the runtime actually enforces); for Anthropic/OpenAI it is the
// static per-model table value. Falls back to a conservative 128k.
func effectiveContextWindow(provider ai.Provider, model string) int {
	var window int
	switch p := provider.(type) {
	case *ai.OllamaProvider:
		window = config.GetSidecarSettings().EffectiveOllamaNumCtx()
	case *ai.AnthropicProvider:
		window = ai.AnthropicModelInfo(model).ContextWindow
	case *ai.OpenAIProvider:
		window = ai.OpenAIModelInfo(model).ContextWindow
	default:
		_ = p
	}
	if window <= 0 {
		window = 128_000
	}
	return window
}

// runLoop is the agentic streaming + tool-execution loop.
// Returns the final text, accumulated thinking, all tool-use records, content
// items, token counts, and — on provider/LLM failure — a non-empty loopError
// string that Execute will forward as a terminal agent_error event.
func runLoop(
	ctx context.Context,
	cfg Config,
	s *session.APISession,
	mode, system string,
	history []ai.ChatMessage,
	provider ai.Provider,
	emit func(map[string]any),
) (finalText, thinking string, toolUses []map[string]any, items []map[string]any, totalIn, totalOut int, loopError string) {
	var thinkingBuf strings.Builder

	// Secret vault for this run. Under egress redact mode the guardrail swaps
	// secrets for reversible handles on the way out to the model; the model echoes
	// a handle back in tool input, and we substitute the real value back in below,
	// immediately before dispatch. Without this the agent would run the literal
	// placeholder as a credential and every authenticated call would fail.
	// In off/log mode nothing is ever aliased, so the vault stays empty and every
	// reveal is a no-op.
	ctx = ai.WithSecretVault(ctx, ai.NewSecretVault())
	vault := ai.SecretVaultFrom(ctx)

	// lastInputTokens tracks how many input tokens the previous iteration consumed.
	// When it grows large we proactively trim history before the next call.
	var lastInputTokens int
	// todoReinjectCount tracks how many times we've nudged the agent back to work
	// on incomplete todos. Capped at 3 to avoid infinite nudge loops.
	var todoReinjectCount int

	// clarifyNudgeCount tracks how many times we've nudged the agent to re-ask a
	// plain-text clarifying question via the ask_followup_question tool. Capped at
	// 1: small/local models often phrase clarifications as prose instead of calling
	// the tool, which leaves the UI without an interactive overlay. One nudge
	// recovers the tool path without risking a loop if the model keeps refusing.
	var clarifyNudgeCount int
	// autoClarifyCount tracks how many times we've surfaced a prose question through
	// the clarification UI ourselves after the nudge failed. Capped so a model that
	// insists on prose can't loop forever — but the question is never silently left
	// as passive text the user might scroll past and miss.
	var autoClarifyCount int

	// Resolve user-configured iteration limits at run time.
	sidecarSettings := config.GetSidecarSettings()
	iterMax := sidecarSettings.EffectiveMaxIterations()
	confirmThreshold := sidecarSettings.EffectiveConfirmThreshold()
	confirmContinue := sidecarSettings.ConfirmContinue
	// Track whether we've already fired the confirm prompt this session to
	// avoid spamming it on every subsequent iteration after the threshold.
	confirmedContinue := false

	// Effective context window for this provider/model. Used as a last-resort
	// safety net: the frontend auto-compacts first, but if a turn still arrives
	// near the window we trim the oldest history rather than overflow the model.
	ctxWindow := effectiveContextWindow(provider, s.Model)
	// Trim threshold: 92% of the window, leaving headroom for tools + the reply.
	trimAt := ctxWindow * 92 / 100

	for iter := 0; iter < iterMax; iter++ {
		// Check cancellation before each LLM call
		if ctx.Err() != nil {
			emit(map[string]any{"type": "cancelled"})
			return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, ""
		}

		// Confirm-continue: pause at threshold and ask the user whether to keep going.
		// We fire once per session (confirmedContinue gates re-entry). If the user
		// denies, we stop cleanly rather than failing with an error.
		if confirmContinue && !confirmedContinue && iter >= confirmThreshold && cfg.RequestContinue != nil {
			confirmedContinue = true
			emit(map[string]any{
				"type":      "iteration_confirm_request",
				"iteration": iter,
				"max":       iterMax,
			})
			if !cfg.RequestContinue(ctx, iter, iterMax) {
				emit(map[string]any{"type": "done", "inputTokens": totalIn, "outputTokens": totalOut})
				return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, ""
			}
		}

		// Proactively trim history when the previous call approached the model's
		// effective context window. This is a last resort — the frontend auto-compacts
		// well before this — so we keep it lossy-but-cheap rather than summarising here.
		if lastInputTokens > trimAt && len(history) > 4 {
			// Drop the oldest quarter of middle messages, keeping the first user
			// message and the most-recent entries where the current task lives.
			drop := len(history) / 4
			history = append(history[:1:1], history[1+drop:]...)
		}

		// User-configurable output budget (sidecar_settings.maxOutputTokens); each
		// provider clamps it down to its model's real ceiling, so a large value is safe.
		maxTok := config.GetSidecarSettings().EffectiveMaxOutputTokens()
		if cfg.ThinkingBudget > 0 && cfg.ThinkingBudget >= maxTok {
			maxTok = cfg.ThinkingBudget + 5000
		}
		req := ai.StreamRequest{
			Model:          s.Model,
			System:         system,
			Messages:       history,
			Tools:          WorkspaceTools(ctx, cfg.Workspace, cfg.MemoryEnabled, cfg.DesktopPermitted, cfg.BrowserAvailable, cfg.TeamMemberIDs),
			MaxTokens:      maxTok,
			ThinkingBudget: cfg.ThinkingBudget,
			ThinkLevel:     cfg.ThinkLevel,
		}

		// Pass ctx directly — each provider manages its own inactivity timeout.
		// The user can cancel at any time via the stop button.
		ch, err := provider.Stream(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				emit(map[string]any{"type": "cancelled"})
				return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, ""
			}
			msg := fmt.Sprintf("provider error: %v", err)
			emit(map[string]any{"type": "error", "message": msg})
			return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, msg
		}

		var textBuf strings.Builder
		var localThinking strings.Builder
		var toolCalls []ai.StreamEvent
		var turnThinking []ai.ThinkingBlock // Anthropic thinking blocks to replay this turn
		var finishReason string
		var streamErrMsg string

		for ev := range ch {
			if ctx.Err() != nil {
				break
			}
			switch ev.Type {
			case "token":
				textBuf.WriteString(ev.Text)
				emit(map[string]any{"type": "token", "content": ev.Text})
			case "thinking":
				localThinking.WriteString(ev.Text)
				emit(map[string]any{"type": "thinking", "content": ev.Text})
			case "thinking_block":
				// Anthropic extended thinking: keep the completed block so it can be
				// replayed on the follow-up request when this turn calls a tool.
				turnThinking = append(turnThinking, ai.ThinkingBlock{
					Thinking:  ev.Text,
					Signature: ev.Signature,
					Redacted:  ev.Redacted,
				})
			case "tool_call":
				// Substitute real secrets back in for any vault handle the model
				// echoed. Done here, at the single point the tool call enters the
				// loop, so dispatch, history, audit, and the UI all agree — and so
				// history keeps raw values, which the guardrail re-aliases on the
				// next outbound request.
				ev.ToolInput = vault.RevealInput(ev.ToolInput)
				toolCalls = append(toolCalls, ev)
				emit(map[string]any{
					"type":      "tool_call",
					"tool":      ev.ToolName,
					"toolInput": ev.ToolInput,
				})
			case "done":
				finishReason = ev.FinishReason
				totalIn += ev.InputTokens
				totalOut += ev.OutputTokens
				lastInputTokens = ev.InputTokens
			case "error":
				msg := "LLM error"
				if ev.Err != nil {
					msg = ev.Err.Error()
				}
				emit(map[string]any{"type": "error", "message": msg})
				streamErrMsg = msg
			}
		}
		if ctx.Err() != nil {
			// Capture any partial text streamed before the cancel so the session
			// file records it and the next turn can resume rather than restart.
			if textBuf.Len() > 0 {
				partial := textBuf.String()
				finalText += partial
				items = appendOrMergeItem(items, "text", partial)
			}
			emit(map[string]any{"type": "cancelled"})
			return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, ""
		}
		if streamErrMsg != "" {
			return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, streamErrMsg
		}

		// Accumulate thinking
		if localThinking.Len() > 0 {
			thinkingBuf.WriteString(localThinking.String())
			items = appendOrMergeItem(items, "thinking", localThinking.String())
		}

		// Accumulate text
		if textBuf.Len() > 0 {
			items = appendOrMergeItem(items, "text", textBuf.String())
		}

		// Build assistant turn for history. Thinking blocks are carried in-memory
		// only (Anthropic requires them replayed for the current turn's tool_use;
		// older turns are auto-filtered, so they are not persisted to disk).
		assistantTurn := ai.ChatMessage{Role: "assistant", Content: textBuf.String(), ThinkingBlocks: turnThinking}
		for _, tc := range toolCalls {
			assistantTurn.ToolCalls = append(assistantTurn.ToolCalls, ai.ToolCall{
				ID:    tc.ToolID,
				Name:  tc.ToolName,
				Input: tc.ToolInput,
			})
		}
		history = append(history, assistantTurn)

		// If the model hit the token limit and produced no tool calls, prompt it
		// to continue from where it left off (mirrors the Python sidecar pattern).
		if finishReason == "max_tokens" && len(toolCalls) == 0 {
			history = append(history, ai.ChatMessage{
				Role:    "user",
				Content: "Continue exactly where you left off.",
			})
			continue
		}

		// If the model asked the user a clarifying question as plain text instead
		// of calling ask_followup_question, nudge it once to re-ask via the tool so
		// the UI renders the interactive clarification overlay and blocks the
		// composer until the user answers. Only when the tool is actually available.
		if len(toolCalls) == 0 && cfg.RequestClarification != nil &&
			clarifyNudgeCount < 1 && mode != "plan" &&
			looksLikeClarifyingQuestion(textBuf.String()) {
			clarifyNudgeCount++
			history = append(history, ai.ChatMessage{
				Role: "user",
				Content: "You asked the user a clarifying question in your message text. " +
					"Do NOT ask questions as plain text. You MUST call the ask_followup_question tool " +
					"to ask the user, passing `question` and 2-4 concrete `suggestions`. " +
					"Re-ask the same question now by calling ask_followup_question — output no other text.",
			})
			continue
		}

		// The nudge was already spent and the model STILL asked in prose. Stop
		// fighting it: surface the question through the clarification UI ourselves
		// so it is never left as passive text the user might scroll past. We ask the
		// raw question (free-text answer, no fabricated suggestions), then feed the
		// user's answer back so the turn continues as if the tool had been called.
		if len(toolCalls) == 0 && cfg.RequestClarification != nil &&
			clarifyNudgeCount >= 1 && autoClarifyCount < 2 && mode != "plan" &&
			looksLikeClarifyingQuestion(textBuf.String()) {
			autoClarifyCount++
			if ans, ok := cfg.RequestClarification(ctx, lastQuestionLine(textBuf.String()), nil, false); ok {
				history = append(history, ai.ChatMessage{
					Role:    "user",
					Content: clarificationAnswerToText(ans),
				})
				continue
			}
		}

		// If no tool calls, check for incomplete todos before treating this as done.
		// Genuine failures (cancelled, provider error, max iterations) have separate
		// return paths above — this guard only fires on a clean natural exit.
		// Team-lead agents are exempt: their session todos represent delegated work
		// that team members complete, not work the lead must do itself.
		if len(toolCalls) == 0 {
			if mode != "plan" && todoReinjectCount < 3 && len(cfg.TeamMemberIDs) == 0 &&
				cfg.Workspace != "" && cfg.SessionID != "" {
				todos, _ := session.GetTodos(cfg.Workspace, cfg.SessionID)
				if incomplete := incompleteTodos(todos); len(incomplete) > 0 {
					todoReinjectCount++
					history = append(history, ai.ChatMessage{
						Role:    "user",
						Content: buildTodoContinuationPrompt(incomplete),
					})
					continue
				}
			}
			finalText = textBuf.String()
			return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, ""
		}

		// Execute tools and build tool-result turn
		toolResultTurn := ai.ChatMessage{Role: "user"}
		var finishSummary string
		for _, tc := range toolCalls {
			if ctx.Err() != nil {
				break
			}
			emit(map[string]any{
				"type":      "tool_use_start",
				"tool":      tc.ToolName,
				"toolInput": tc.ToolInput,
			})
			output, imageData, isError := Dispatch(ctx, &cfg, tc.ToolName, tc.ToolInput)

			record := map[string]any{
				"id":      tc.ToolID,
				"tool":    tc.ToolName,
				"input":   tc.ToolInput,
				"output":  output,
				"success": !isError,
			}
			toolUses = append(toolUses, record)

			// Tamper-evident audit: record that this tool ran, with a bounded input
			// summary and outcome. Answers "which tools ran, on whose behalf".
			if _, err := session.AppendAudit(cfg.Workspace, cfg.SessionID, cfg.auditActor(), "tool_exec", map[string]any{
				"tool":    tc.ToolName,
				"input":   session.AuditSummary(tc.ToolInput),
				"success": !isError,
			}); err != nil {
				emit(map[string]any{"type": "audit_error", "message": err.Error()})
			}
			items = append(items, map[string]any{"kind": "tool", "toolUse": record})

			// After wait_for_* completes, persist completed subagent cards into the
			// parent items so they survive session reload and appear in history.
			if !isError && (tc.ToolName == "wait_for_team" || tc.ToolName == "wait_for_subagents" || tc.ToolName == "wait_for_agent") {
				for _, sa := range cfg.subAgents.drainSubAgentItems() {
					items = append(items, sa)
				}
			}

			// When the tool produced an image, add a separate image item so the
			// UI renders the screenshot inline (mirrors the Python sidecar pattern).
			if imageData != "" {
				source := "browser"
				if tc.ToolName == "screenshot" {
					source = "desktop"
				}
				imgItem := map[string]any{
					"kind":   "image",
					"image":  imageData,
					"source": source,
				}
				// Extract PNG dimensions (bytes 16–23 of IHDR) for display metadata.
				if decoded, err := base64.StdEncoding.DecodeString(imageData); err == nil && len(decoded) >= 24 {
					imgItem["width"] = int(decoded[16])<<24 | int(decoded[17])<<16 | int(decoded[18])<<8 | int(decoded[19])
					imgItem["height"] = int(decoded[20])<<24 | int(decoded[21])<<16 | int(decoded[22])<<8 | int(decoded[23])
				}
				items = append(items, imgItem)
			}

			emit(map[string]any{
				"type":       "tool_result",
				"tool":       tc.ToolName,
				"toolOutput": output,
				"success":    !isError,
			})

			liveContent := output
			if len(liveContent) > maxLiveToolOutput {
				liveContent = liveContent[:maxLiveToolOutput] + fmt.Sprintf("\n[truncated at %d chars]", maxLiveToolOutput)
			}
			toolResultTurn.ToolResults = append(toolResultTurn.ToolResults, ai.ToolResult{
				ToolUseID:      tc.ToolID,
				Name:           tc.ToolName,
				Content:        liveContent,
				IsError:        isError,
				ImageData:      imageData,
				ImageMediaType: "image/png",
			})

			// finish_task succeeded (not blocked by incomplete todos) — capture the
			// summary so we can exit without a follow-up LLM call that produces no text.
			if (tc.ToolName == "finish_task" || tc.ToolName == "finish") &&
				!isError && !strings.HasPrefix(output, "BLOCKED") {
				finishSummary = output
			}

			// task_out_of_scope — the agent has determined the request is outside its
			// skill set; exit the loop immediately so it cannot attempt the work anyway.
			if tc.ToolName == "task_out_of_scope" && !isError {
				finalText = output
				items = appendOrMergeItem(items, "text", output)
				return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, ""
			}
		}

		// If finish_task fired successfully this iteration, exit the loop now.
		// The summary IS the final text — no additional LLM call needed, and
		// making one only produces an empty/thinking-only response.
		if finishSummary != "" {
			finalText = finishSummary
			items = appendOrMergeItem(items, "text", finishSummary)
			return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, ""
		}

		history = append(history, toolResultTurn)
	}

	const iterErr = "agent stopped after too many tool iterations"
	emit(map[string]any{"type": "error", "message": iterErr})
	return finalText, thinkingBuf.String(), toolUses, items, totalIn, totalOut, iterErr
}

func incompleteTodos(todos []session.Todo) []session.Todo {
	var out []session.Todo
	for _, t := range todos {
		if t.Status != "completed" {
			out = append(out, t)
		}
	}
	return out
}

func buildTodoContinuationPrompt(incomplete []session.Todo) string {
	lines := []string{
		"NOTICE: You stopped without completing all todos. You MUST continue working.",
		"",
		"Incomplete todos:",
	}
	for _, t := range incomplete {
		lines = append(lines, fmt.Sprintf("  [%s] (id=%s) %s", t.Status, t.ID, t.Text))
	}
	lines = append(lines, "",
		"Instructions:",
		"1. Call TodoWrite to set the next todo to 'in_progress'.",
		"2. Do the work.",
		"3. Call TodoWrite to mark it 'completed'.",
		"4. Repeat until all todos are 'completed'.",
		"5. Only then call finish_task.",
	)
	return strings.Join(lines, "\n")
}

// appendOrMergeItem appends a text or thinking block to items, merging with the last if same kind.
func appendOrMergeItem(items []map[string]any, kind, content string) []map[string]any {
	if len(items) > 0 {
		last := items[len(items)-1]
		if last["kind"] == kind {
			last["content"] = last["content"].(string) + content
			return items
		}
	}
	return append(items, map[string]any{"kind": kind, "content": content})
}

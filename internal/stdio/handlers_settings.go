package stdio

import (
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
)

// handleGetSidecarSettings returns current agent-level settings to the frontend.
func handleGetSidecarSettings(_ *ServerState, _ map[string]any) {
	s := config.GetSidecarSettings()
	Emit(map[string]any{
		"type":                     "sidecar_settings",
		"maxToolIterations":        s.MaxToolIterations,
		"confirmContinue":          s.ConfirmContinue,
		"confirmContinueThreshold": s.ConfirmContinueThreshold,
		"ollamaNumCtx":             s.EffectiveOllamaNumCtx(),
		"maxOutputTokens":          s.EffectiveMaxOutputTokens(),
		"egressMode":               s.EffectiveEgressMode(),
	})
}

// handleSetSidecarSettings persists updated agent-level settings from the frontend.
func handleSetSidecarSettings(_ *ServerState, cmd map[string]any) {
	current := config.GetSidecarSettings()

	if v, ok := cmd["maxToolIterations"].(float64); ok {
		current.MaxToolIterations = int(v)
	}
	if v, ok := cmd["confirmContinue"].(bool); ok {
		current.ConfirmContinue = v
	}
	if v, ok := cmd["confirmContinueThreshold"].(float64); ok {
		current.ConfirmContinueThreshold = int(v)
	}
	if v, ok := cmd["ollamaNumCtx"].(float64); ok {
		current.OllamaNumCtx = int(v)
	}
	if v, ok := cmd["maxOutputTokens"].(float64); ok {
		current.MaxOutputTokens = int(v)
	}
	if v, ok := cmd["egressMode"].(string); ok {
		current.EgressMode = v
	}

	if err := config.SaveSidecarSettings(current); err != nil {
		Emit(map[string]any{"type": "error", "message": "failed to save settings: " + err.Error()})
		return
	}
	ai.SyncEgressFromSettings() // apply the (possibly changed) guardrail mode to live requests now
	Emit(map[string]any{
		"type":                     "sidecar_settings",
		"maxToolIterations":        current.MaxToolIterations,
		"confirmContinue":          current.ConfirmContinue,
		"confirmContinueThreshold": current.ConfirmContinueThreshold,
		"ollamaNumCtx":             current.EffectiveOllamaNumCtx(),
		"maxOutputTokens":          current.EffectiveMaxOutputTokens(),
		"egressMode":               current.EffectiveEgressMode(),
	})
}

// handleIterationConfirmResponse routes the user's continue/stop choice back
// to the agent goroutine that is paused waiting for it.
func handleIterationConfirmResponse(state *ServerState, cmd map[string]any) {
	allow, _ := cmd["allow"].(bool)
	sessionID, _ := cmd["session_id"].(string)
	if sessionID == "" {
		state.mu.Lock()
		sessionID = state.SessionID
		state.mu.Unlock()
	}
	state.deliverIterationConfirm(sessionID, allow)

	// Also publish so Angular WS clients that listen on the session bus receive it.
	session.Global.Publish(sessionID, map[string]any{
		"type":       "iteration_confirm_response",
		"session_id": sessionID,
		"allow":      allow,
	})
}

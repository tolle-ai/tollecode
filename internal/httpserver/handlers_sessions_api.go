package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/session"
)

func mountSessions(r chi.Router, state *apiState, cfg ServerConfig) {
	r.Get("/sessions", listSessionsAPI(cfg))
	r.Post("/sessions", createSessionAPI(state))
	r.Get("/sessions/{id}", getSessionAPI(state))
	r.Delete("/sessions/{id}", deleteSessionAPI(state))
	r.Post("/sessions/{id}/run", runSessionAPI(state, cfg))
	r.Post("/sessions/{id}/cancel", cancelSessionAPI(state))
	r.Get("/sessions/{id}/events", streamSessionEventsAPI)

	// Pause-resume for interactive flows
	r.Post("/sessions/{id}/permission-response", permissionResponseAPI(state))
	r.Post("/sessions/{id}/clarification-response", clarificationResponseAPI(state))
}

// ── CRUD ──────────────────────────────────────────────────────────────────────

func listSessionsAPI(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, ok := resolveWorkspacePath(wsID)
		if !ok {
			writeErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}
		sessions, err := session.List(wsPath)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, sessions)
	}
}

func createSessionAPI(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			WorkspaceID string `json:"workspace_id"`
			AgentID     string `json:"agent_id"`
			Provider    string `json:"provider"`
			Model       string `json:"model"`
			Mode        string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		wsPath, ok := resolveWorkspacePath(body.WorkspaceID)
		if !ok {
			writeErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}

		provider := body.Provider
		model := body.Model
		if provider == "" && body.AgentID != "" {
			if ac := agent.LookupAgentCfg(body.AgentID); ac != nil {
				provider, model = ac.Provider, ac.Model
			}
		}
		if provider == "" {
			provider, model = apiFirstProvider(state.defaultProvider, state.defaultModel)
		}
		mode := body.Mode
		if mode == "" {
			mode = "build"
		}

		var opts []session.CreateOption
		if body.AgentID != "" {
			if ac := agent.LookupAgentCfg(body.AgentID); ac != nil {
				opts = append(opts, session.WithAgentName(ac.Name))
				// When an agent has skills defined, activate only those skills.
				if len(ac.Skills) > 0 {
					opts = append(opts, session.WithSkills(ac.Skills))
				}
			}
		}

		sess, err := session.Create(wsPath, provider, model, mode, opts...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		state.trackSession(sess.ID, wsPath)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, sess)
	}
}

func getSessionAPI(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsPath := resolveSessionWorkspace(state, id, r)
		if wsPath == "" {
			writeErr(w, http.StatusBadRequest, "workspace_id required (or create session via POST /v1/sessions first)")
			return
		}
		sess, err := session.Load(wsPath, id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, sess)
	}
}

func deleteSessionAPI(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsPath := resolveSessionWorkspace(state, id, r)
		if wsPath == "" {
			writeErr(w, http.StatusBadRequest, "workspace_id required")
			return
		}
		state.cancel(id)
		agent.DestroyBrowserSession(id)
		session.Delete(wsPath, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

// ── Run (SSE streaming) ───────────────────────────────────────────────────────

func runSessionAPI(state *apiState, cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsPath := resolveSessionWorkspace(state, id, r)
		if wsPath == "" {
			writeErr(w, http.StatusBadRequest, "workspace_id required")
			return
		}

		var body struct {
			Message       string   `json:"message"`
			Images        []string `json:"images"`
			Mode          string   `json:"mode"`
			ShellAutoAllow *bool   `json:"shellAutoAllow"`
			TeamMemberIDs []string `json:"teamMemberIds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if body.Message == "" {
			writeErr(w, http.StatusBadRequest, "message is required")
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Cancel any previous run on this session.
		state.cancel(id)
		session.ClearLiveEvents(id)
		session.Global.ClearBuffer(id)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")

		var writeMu sync.Mutex
		emitSSE := func(event map[string]any) {
			data, err := json.Marshal(event)
			if err != nil {
				return
			}
			writeMu.Lock()
			fmt.Fprintf(w, "data: %s\n\n", data) //nolint:errcheck
			flusher.Flush()
			writeMu.Unlock()
		}

		shellAutoAllow := cfg.Tools.ShellAllowed
		if body.ShellAutoAllow != nil {
			shellAutoAllow = *body.ShellAutoAllow
		}

		ctx, cancelFn := context.WithCancel(r.Context())
		done := make(chan struct{})
		state.register(id, cancelFn, done)

		session.UpdateFields(wsPath, id, map[string]any{"status": "running"})
		session.RegisterSession(id, wsPath, "api")

		go func() {
			defer close(done)
			defer state.remove(id)
			defer session.UnregisterSession(id)

			customInstructions := ""
			if len(body.TeamMemberIDs) > 0 {
				customInstructions = agent.BuildTeamLeadContext(body.TeamMemberIDs)
			}

			agentCfg := agent.Config{
				SessionID:          id,
				Workspace:          wsPath,
				Message:            body.Message,
				Images:             body.Images,
				ShellAutoAllow:     shellAutoAllow,
				TeamMemberIDs:      body.TeamMemberIDs,
				CustomInstructions: customInstructions,
				EmitFn: func(event map[string]any) {
					off, _ := session.AppendLiveEvent(id, event)
					event["_off"] = off
					session.Global.Publish(id, event)
					emitSSE(event)
				},
				RequestPerm: func(ctx context.Context, command string) (bool, bool) {
					event := map[string]any{
						"type":       "pending_permission",
						"session_id": id,
						"command":    command,
					}
					off, _ := session.AppendLiveEvent(id, event)
					event["_off"] = off
					session.Global.Publish(id, event)
					emitSSE(event)

					ch := state.permQueue(id)
					select {
					case resp := <-ch:
						return resp.Allow, resp.AllowAll
					case <-time.After(60 * time.Second):
						return false, false
					case <-ctx.Done():
						return false, false
					}
				},
				RequestClarification: func(ctx context.Context, question string, suggestions []string, multiChoice bool) (agent.ClarificationAnswer, bool) {
					event := map[string]any{
						"type":         "clarification_needed",
						"session_id":   id,
						"question":     question,
						"suggestions":  suggestions,
						"multi_choice": multiChoice,
					}
					off, _ := session.AppendLiveEvent(id, event)
					event["_off"] = off
					session.Global.Publish(id, event)
					emitSSE(event)

					ch := state.clarificationQueue(id)
					select {
					case resp := <-ch:
						return agent.ClarificationAnswerFromLegacy(resp.Answer, suggestions, multiChoice), true
					case <-ctx.Done():
						return agent.ClarificationAnswer{}, false
					}
				},
			}
			if body.Mode != "" {
				agentCfg.Mode = body.Mode
			}

			agent.Execute(ctx, agentCfg)
		}()

		<-done
	}
}

func cancelSessionAPI(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		// Non-blocking: signal cancellation and return immediately so the client
		// receives the 204 right away without waiting for the goroutine to unwind.
		state.cancelNoWait(id)
		session.Global.Publish(id, map[string]any{"type": "cancelled", "session_id": id})
		w.WriteHeader(http.StatusNoContent)
	}
}

// ── SSE event stream (reconnectable) ─────────────────────────────────────────

// streamSessionEventsAPI replicates the WebSocket 4-phase logic over SSE so
// mobile/REST clients can reconnect after a network drop using ?from=<offset>.
func streamSessionEventsAPI(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	var fromOffset int64
	if v := r.URL.Query().Get("from"); v != "" {
		fromOffset, _ = strconv.ParseInt(v, 10, 64)
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	sendEvent := func(event map[string]any) bool {
		data, err := json.Marshal(event)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		if err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// ── Phase 1: replay from live file ───────────────────────────────────────
	phase1, _ := session.TailLiveEvents(sessionID, fromOffset)
	sentTerminal := false
	for _, le := range phase1.Events {
		ev := cloneWithOff(le.Event, le.Offset)
		if !sendEvent(ev) {
			return
		}
		if t, _ := ev["type"].(string); session.IsTerminalType(t) {
			sentTerminal = true
		}
	}
	if sentTerminal {
		return
	}

	// ── Phase 2: subscribe to in-memory bus ──────────────────────────────────
	ch, unsub := session.Global.Subscribe(sessionID)
	var unsubOnce sync.Once
	safeUnsub := func() { unsubOnce.Do(unsub) }
	defer safeUnsub()

	// ── Phase 3: catch-up ────────────────────────────────────────────────────
	phase3, _ := session.TailLiveEvents(sessionID, phase1.EndOffset)
	for _, le := range phase3.Events {
		ev := cloneWithOff(le.Event, le.Offset)
		if !sendEvent(ev) {
			safeUnsub()
			return
		}
		if t, _ := ev["type"].(string); session.IsTerminalType(t) {
			sentTerminal = true
		}
	}
	if sentTerminal {
		safeUnsub()
		return
	}

	lastSentOffset := phase3.EndOffset
	if lastSentOffset == 0 {
		lastSentOffset = phase1.EndOffset
	}

	// ── Phase 4: live stream ─────────────────────────────────────────────────
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				sendEvent(map[string]any{"type": "cancelled", "session_id": sessionID, "reason": "connection_lost"}) //nolint:errcheck
				return
			}
			if lastSentOffset > 0 {
				if off, isInt := event["_off"].(int64); isInt && off < lastSentOffset {
					continue
				}
			}
			if !sendEvent(event) {
				return
			}
			if t, _ := event["type"].(string); session.IsTerminalType(t) {
				return
			}
		}
	}
}

// ── Interactive pause-resume ─────────────────────────────────────────────────

func permissionResponseAPI(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var body struct {
			Allow    bool `json:"allow"`
			AllowAll bool `json:"allowAll"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if !state.deliverPerm(id, body.Allow, body.AllowAll) {
			writeErr(w, http.StatusNotFound, "no pending permission request for this session")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func clarificationResponseAPI(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var body struct {
			Answer string `json:"answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if !state.deliverClarification(id, body.Answer) {
			writeErr(w, http.StatusNotFound, "no pending clarification for this session")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolveSessionWorkspace looks up the workspace path for a session.
// Checks apiState first, then falls back to workspace_id query param.
func resolveSessionWorkspace(state *apiState, sessionID string, r *http.Request) string {
	if ws, ok := state.workspaceFor(sessionID); ok {
		return ws
	}
	wsID := r.URL.Query().Get("workspace_id")
	if wsID == "" {
		return ""
	}
	ws, _ := resolveWorkspacePath(wsID)
	return ws
}

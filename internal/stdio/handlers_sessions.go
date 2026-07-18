package stdio

import (
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/session"
)

func handleGetActiveSessions(_ *ServerState, _ map[string]any) {
	entries := session.ListActive()
	var data []any
	for _, e := range entries {
		// Channel sessions are scoped to the channel thread; exclude them from
		// the dev-mode active-sessions list.
		if e.Initiator == "channel" {
			continue
		}
		data = append(data, e)
	}
	if data == nil {
		data = []any{}
	}
	Emit(map[string]any{"type": "active_sessions", "data": data})
}

func handleGetSessions(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	sessions, _ := session.List(ws)
	data := make([]any, len(sessions))
	for i, s := range sessions {
		data[i] = s
	}
	// Echo back the requested workspacePath verbatim so the UI can correlate this
	// response with the request that triggered it. The `sessions` event is
	// broadcast to every listener, so without a key concurrent get_sessions
	// requests (e.g. while switching workspaces) would resolve each other's
	// responses and show the wrong workspace's history.
	reqWs, _ := cmd["workspacePath"].(string)
	Emit(map[string]any{"type": "sessions", "data": data, "workspacePath": reqWs})
}

func handleNewSession(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	providerID, _ := cmd["provider"].(string)
	if providerID == "" {
		providerID, _ = cmd["provider_id"].(string)
	}
	model, _ := cmd["model"].(string)
	mode, _ := cmd["mode"].(string)
	if mode == "" {
		state.mu.Lock()
		mode = state.Mode
		state.mu.Unlock()
	}
	agentName, _ := cmd["agentName"].(string)

	var skills []string
	if rawSkills, ok := cmd["skills"].([]any); ok {
		for _, s := range rawSkills {
			if str, ok := s.(string); ok {
				skills = append(skills, str)
			}
		}
	}

	if providerID == "" {
		providerID, model = ai.Global.BestProvider("", "")
	} else if providerID != "terminal" {
		// Resolve type aliases (e.g. "anthropic" or "ollama-cloud") to a real provider ID.
		if resolved, ok := ai.Global.ResolveProviderID(providerID); ok {
			providerID = resolved
		}
		if model == "" {
			model = ai.Global.DefaultModel(providerID)
		}
	}

	opts := []session.CreateOption{
		session.WithRole(""),
		session.WithAgentName(agentName),
	}
	if len(skills) > 0 {
		opts = append(opts, session.WithSkills(skills))
	}

	s, err := session.Create(ws, providerID, model, mode, opts...)
	if err != nil {
		Emit(map[string]any{"type": "error", "message": "create session failed: " + err.Error()})
		return
	}

	state.mu.Lock()
	state.SessionID = s.ID
	state.Mode = mode
	state.ActiveSkills = s.ActiveSkills
	// Remember the workspace this session was created in so a follow-up
	// send_message that omits (or sends a stale/empty) workspacePath still
	// resolves to the same workspace — otherwise Load() looks in the wrong
	// directory and the brand-new session 404s with "session not found".
	if ws != "" {
		state.Workspace = ws
	}
	state.mu.Unlock()

	Emit(map[string]any{"type": "session_created", "session": s, "session_id": s.ID})
}

func handleLoadSession(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	ws := workspaceFromCmd(state, cmd)
	tail := 30
	if v, ok := cmd["tail"].(float64); ok {
		tail = int(v)
	}
	skip := 0
	if v, ok := cmd["skip"].(float64); ok {
		skip = int(v)
	}

	// Paginate backward: return the page before the currently loaded tail.
	if skip > 0 {
		s, hasMore, err := session.LoadPage(ws, sessionID, skip, tail)
		if err != nil {
			Emit(map[string]any{"type": "error", "message": "session not found: " + sessionID})
			return
		}
		Emit(map[string]any{
			"type":       "session_loaded",
			"session":    s,
			"session_id": s.ID,
			"prepend":    true,
			"has_more":   hasMore,
		})
		return
	}

	s, total, err := session.LoadTail(ws, sessionID, tail)
	if err != nil {
		Emit(map[string]any{"type": "error", "message": "session not found: " + sessionID})
		return
	}

	state.mu.Lock()
	state.SessionID = s.ID
	state.Mode = s.Mode
	state.ActiveSkills = s.ActiveSkills
	state.mu.Unlock()

	// Capture the authoritative header message count BEFORE we overwrite it with
	// the tail sentinel. The frontend needs this to slice post-compact messages
	// without relying on fragile timestamp string comparison.
	headerMsgCount := 0
	if s.MessageCount != nil {
		headerMsgCount = *s.MessageCount
	}
	postCompactMessageCount := 0
	if s.CompactedMessageCount > 0 && s.CompactedMessageCount <= headerMsgCount {
		postCompactMessageCount = headerMsgCount - s.CompactedMessageCount
	}

	hasMore := total > tail
	s.MessageCount = &total

	// Return buffered live events so the UI can display in-progress work
	// immediately, before the WebSocket connection completes.
	liveEvents := []any{}
	if s.Status == "running" {
		if tr, err := session.TailLiveEvents(s.ID, 0); err == nil {
			for _, le := range tr.Events {
				ev := make(map[string]any, len(le.Event)+1)
				for k, v := range le.Event {
					ev[k] = v
				}
				ev["_off"] = le.Offset
				liveEvents = append(liveEvents, ev)
			}
		}
	}

	Emit(map[string]any{
		"type":                    "session_loaded",
		"session":                 s,
		"session_id":              s.ID,
		"liveEvents":              liveEvents,
		"has_more":                hasMore,
		"post_compact_msg_count":  postCompactMessageCount,
	})
}

func handleDeleteSession(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	ws := workspaceFromCmd(state, cmd)
	state.cancelSession(sessionID)
	agent.DestroyBrowserSession(sessionID)
	session.Delete(ws, sessionID)
	Emit(map[string]any{"type": "session_deleted", "session_id": sessionID})
}

func handleUpdateSession(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	ws := workspaceFromCmd(state, cmd)
	if sessionID == "" {
		state.mu.Lock()
		sessionID = state.SessionID
		state.mu.Unlock()
	}
	if sessionID == "" {
		Emit(map[string]any{"type": "error", "message": "update_session requires session_id"})
		return
	}
	fields := map[string]any{}
	for _, f := range []string{"provider", "model", "mode", "title", "status"} {
		if v, ok := cmd[f].(string); ok && v != "" {
			fields[f] = v
		}
	}
	s, err := session.UpdateFields(ws, sessionID, fields)
	if err != nil {
		Emit(map[string]any{"type": "error", "message": "update_session failed: " + err.Error()})
		return
	}
	Emit(map[string]any{"type": "session_updated", "session": s, "session_id": sessionID})
}

func handleAppendMessage(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	ws := workspaceFromCmd(state, cmd)
	msgRaw, _ := cmd["message"].(map[string]any)
	if sessionID == "" || msgRaw == nil {
		Emit(map[string]any{"type": "message_appended", "ok": false, "session_id": sessionID})
		return
	}
	role, _ := msgRaw["role"].(string)
	content, _ := msgRaw["content"].(string)
	id, _ := msgRaw["id"].(string)
	if id == "" {
		id = uuid.NewString()
	}
	ts, _ := msgRaw["timestamp"].(string)
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339Nano)
	}
	err := session.AppendMessage(ws, sessionID, session.Message{
		ID: id, Role: role, Content: content, Timestamp: ts,
	})
	if err != nil {
		Emit(map[string]any{"type": "message_appended", "ok": false, "session_id": sessionID, "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "message_appended", "ok": true, "session_id": sessionID})
}

func handleSwitchModel(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	ws := workspaceFromCmd(state, cmd)
	providerID, _ := cmd["provider_id"].(string)
	model, _ := cmd["model"].(string)
	// Resolve type aliases (e.g. "anthropic" or "ollama-cloud") to a real provider ID.
	if providerID != "" {
		if resolved, ok := ai.Global.ResolveProviderID(providerID); ok {
			providerID = resolved
		}
	}
	if model == "" && providerID != "" {
		model = ai.Global.DefaultModel(providerID)
	}
	session.UpdateFields(ws, sessionID, map[string]any{"provider": providerID, "model": model})
	Emit(map[string]any{
		"type":        "model_switched",
		"session_id":  sessionID,
		"provider_id": providerID,
		"model":       model,
	})
}

func handleGetEarlierMessages(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	ws := workspaceFromCmd(state, cmd)
	loadedCount := 30
	if v, ok := cmd["loaded_count"].(float64); ok {
		loadedCount = int(v)
	}
	page := 30
	if v, ok := cmd["page"].(float64); ok {
		page = int(v)
	}

	s, err := session.Load(ws, sessionID)
	if err != nil {
		Emit(map[string]any{"type": "earlier_messages", "session_id": sessionID, "messages": []any{}, "total": 0})
		return
	}
	all := s.Messages
	total := len(all)
	end := total - loadedCount
	if end < 0 {
		end = 0
	}
	start := end - page
	if start < 0 {
		start = 0
	}
	Emit(map[string]any{
		"type":       "earlier_messages",
		"session_id": sessionID,
		"messages":   all[start:end],
		"total":      total,
	})
}

func handleGetSubagentSession(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["session_id"].(string)
	state.mu.Lock()
	ws := state.Workspace
	state.mu.Unlock()

	s, err := session.Load(ws, sessionID)
	if err != nil {
		Emit(map[string]any{
			"type":       "subagent_session",
			"session_id": sessionID,
			"messages":   []any{},
			"meta":       map[string]any{},
		})
		return
	}
	Emit(map[string]any{
		"type":       "subagent_session",
		"session_id": sessionID,
		"messages":   s.Messages,
		"meta": map[string]any{
			"role":  s.Role,
			"color": s.Color,
			"title": s.Title,
			"model": s.Model,
		},
	})
}

func handleGetTodos(state *ServerState, cmd map[string]any) {
	state.mu.Lock()
	sessionID := state.SessionID
	ws := state.Workspace
	state.mu.Unlock()

	todos, err := session.GetTodos(ws, sessionID)
	if err != nil || len(todos) == 0 {
		Emit(map[string]any{"type": "todos", "session_id": sessionID, "data": []any{}})
		return
	}
	data := make([]any, len(todos))
	for i, t := range todos {
		data[i] = map[string]any{
			"id":       t.ID,
			"text":     t.Text,
			"status":   t.Status,
			"checked":  t.Checked,
			"priority": t.Priority,
		}
	}
	Emit(map[string]any{"type": "todos", "session_id": sessionID, "data": data})
}

// workspaceFromCmd returns the workspace path from the command or falls back to state.
func workspaceFromCmd(state *ServerState, cmd map[string]any) string {
	if ws, ok := cmd["workspacePath"].(string); ok && ws != "" {
		return ws
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.Workspace
}

package stdio

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
)

// Channel messages are stored per-channel as JSONL:
//   <workspace>/.agent/channels/<channelId>.jsonl
// Each line is a ChatMessage object.

type channelMsg struct {
	ID          string         `json:"id"`
	From        string         `json:"from"`
	SenderName  string         `json:"senderName"`
	AvatarColor string         `json:"avatarColor"`
	Time        string         `json:"time"`
	Type        string         `json:"type"`
	Text        string         `json:"text,omitempty"`
	ResultLabel string         `json:"resultLabel,omitempty"`
	ResultStatus string        `json:"resultStatus,omitempty"`
	SessionID   string         `json:"sessionId,omitempty"`
	Extra       map[string]any `json:"-"`
}

func channelPath(workspace, channelID string) string {
	return filepath.Join(workspace, ".agent", "channels", sanitizeID(channelID)+".jsonl")
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func loadChannelMessages(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			msgs = append(msgs, m)
		}
	}
	return msgs, nil
}

func appendChannelMessage(path string, msg map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return enc.Encode(msg)
}

func handleChannelsGetMessages(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	channelID, _ := cmd["channelId"].(string)
	path := channelPath(ws, channelID)
	msgs, _ := loadChannelMessages(path)
	if msgs == nil {
		msgs = []map[string]any{}
	}
	Emit(map[string]any{"type": "channel_messages", "channelId": channelID, "messages": msgs})
}

func handleChannelsSendMessage(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	channelID, _ := cmd["channelId"].(string)
	text, _ := cmd["text"].(string)
	// Angular sends `from_` to avoid Python keyword conflict.
	from, _ := cmd["from_"].(string)
	if from == "" {
		from, _ = cmd["from"].(string)
	}
	if from == "" {
		from = "user"
	}
	senderName, _ := cmd["senderName"].(string)
	if senderName == "" {
		senderName = "You"
	}
	avatarColor, _ := cmd["avatarColor"].(string)
	if avatarColor == "" {
		avatarColor = "#7B5CF5"
	}
	avatarInitial, _ := cmd["avatarInitial"].(string)
	if avatarInitial == "" {
		avatarInitial = "Y"
	}

	msg := map[string]any{
		"id":           uuid.NewString(),
		"from":         from,
		"senderName":   senderName,
		"avatarColor":  avatarColor,
		"avatarInitial": avatarInitial,
		"time":         time.Now().UTC().Format(time.RFC3339),
		"type":         "text",
		"text":         text,
	}
	path := channelPath(ws, channelID)
	_ = appendChannelMessage(path, msg)
	Emit(map[string]any{"type": "channel_message_sent", "channelId": channelID, "message": msg})
}

func handleChannelsDeleteMessage(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	channelID, _ := cmd["channelId"].(string)
	messageID, _ := cmd["messageId"].(string)
	path := channelPath(ws, channelID)
	msgs, _ := loadChannelMessages(path)
	filtered := msgs[:0]
	for _, m := range msgs {
		if m["id"] != messageID {
			filtered = append(filtered, m)
		}
	}
	rewriteChannelMessages(path, filtered)
	Emit(map[string]any{"type": "channel_message_deleted", "channelId": channelID, "messageId": messageID})
}

func handleChannelsDelete(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	channelID, _ := cmd["channelId"].(string)
	os.Remove(channelPath(ws, channelID))
	removeChannelMeta(ws, channelID)
	Emit(map[string]any{"type": "channel_deleted", "channelId": channelID})
}

func handleChannelsNotifyDone(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	channelID, _ := cmd["channelId"].(string)
	if channelID == "" {
		channelID, _ = cmd["agentName"].(string)
	}
	status, _ := cmd["status"].(string)
	summary, _ := cmd["summary"].(string)
	sessionID, _ := cmd["sessionId"].(string)
	agentName, _ := cmd["agentName"].(string)
	agentColor, _ := cmd["agentColor"].(string)
	agentPhoto, _ := cmd["agentPhoto"].(string)
	agentGradient, _ := cmd["agentGradient"].(string)
	agentRole, _ := cmd["agentRole"].(string)
	if agentColor == "" {
		agentColor = "#7B5CF5"
	}

	label := "Task complete"
	if summary != "" {
		label = summary
	}
	msgType := "result"
	msg := map[string]any{
		"id":            uuid.NewString(),
		"from":          "agent",
		"senderName":    agentName,
		"avatarColor":   agentColor,
		"avatarPhoto":   agentPhoto,
		"avatarGradient": agentGradient,
		"avatarRole":    agentRole,
		"time":          time.Now().UTC().Format(time.RFC3339),
		"type":          msgType,
		"resultLabel":   label,
		"resultStatus":  status,
		"sessionId":     sessionID,
	}

	path := channelPath(ws, channelID)
	_ = appendChannelMessage(path, msg)
	Emit(map[string]any{"type": "channel_message", "channelId": channelID, "message": msg})
}

func handleChannelsCommand(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	channelID, _ := cmd["channelId"].(string)
	command, _ := cmd["command"].(string)
	args, _ := cmd["args"].(string)

	base := map[string]any{
		"type":          "channel_command_result",
		"channelId":     channelID,
		"id":            uuid.NewString(),
		"from":          "agent",
		"senderName":    "Tollecode",
		"avatarInitial": "T",
		"avatarColor":   "#7B5CF5",
		"time":          time.Now().UTC().Format(time.RFC3339),
	}

	switch command {
	case "help":
		base["commandType"] = "help"
		base["commandData"] = []map[string]any{
			{"name": "help", "args": "", "description": "Show all available commands"},
			{"name": "usage", "args": "[view] [--from DATE] [--to DATE]", "description": "Token usage charts & analytics"},
			{"name": "sessions", "args": "", "description": "List recent sessions"},
			{"name": "skills", "args": "", "description": "List available skills"},
			{"name": "memory", "args": "[list|search <query>]", "description": "Browse workspace memory"},
			{"name": "start", "args": "[instructions]", "description": "Start a session"},
			{"name": "stop", "args": "", "description": "Stop the current session"},
			{"name": "task", "args": "<instruction>", "description": "Assign a task to the agent inline"},
			{"name": "screen", "args": "<instruction>", "description": "Control the physical screen with an agent"},
		}

	case "sessions":
		sessions, _ := session.List(ws)
		data := make([]map[string]any, 0, len(sessions))
		for _, s := range sessions {
			cnt := 0
			if s.MessageCount != nil {
				cnt = *s.MessageCount
			}
			data = append(data, map[string]any{
				"id":           s.ID,
				"agentName":    s.AgentName,
				"model":        s.Model,
				"status":       s.Status,
				"messageCount": cnt,
			})
		}
		base["commandType"] = "sessions"
		base["commandData"] = data

	case "usage":
		base["commandType"] = "usage"
		base["commandData"] = aggregateUsage(ws)

	case "skills":
		base["commandType"] = "skills"
		base["commandData"] = []map[string]any{}

	case "memory":
		parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
		sub := ""
		if len(parts) > 0 {
			sub = parts[0]
		}
		base["commandType"] = "memory"
		base["commandData"] = map[string]any{
			"subcommand": sub,
			"query":      strings.Join(parts[1:], " "),
			"data":       []map[string]any{},
		}

	default:
		base["commandType"] = "ack"
		base["text"] = fmt.Sprintf("Command /%s is not implemented yet.", command)
	}

	Emit(base)
}

// aggregateUsage scans session JSONL headers to summarise token usage.
func aggregateUsage(ws string) map[string]any {
	sessions, _ := session.List(ws)

	type bucket struct {
		inputTokens  int
		outputTokens int
		calls        int
	}

	daily := map[string]*bucket{}
	byModel := map[string]*bucket{}
	byProvider := map[string]*bucket{}
	var totalIn, totalOut, totalCalls int

	for _, s := range sessions {
		if s.InputTokens == 0 && s.OutputTokens == 0 {
			continue
		}
		day := s.CreatedAt
		if len(day) >= 10 {
			day = day[:10]
		}
		if daily[day] == nil {
			daily[day] = &bucket{}
		}
		daily[day].inputTokens += s.InputTokens
		daily[day].outputTokens += s.OutputTokens
		daily[day].calls++

		if s.Model != "" {
			if byModel[s.Model] == nil {
				byModel[s.Model] = &bucket{}
			}
			byModel[s.Model].inputTokens += s.InputTokens
			byModel[s.Model].outputTokens += s.OutputTokens
			byModel[s.Model].calls++
		}
		if s.Provider != "" {
			if byProvider[s.Provider] == nil {
				byProvider[s.Provider] = &bucket{}
			}
			byProvider[s.Provider].inputTokens += s.InputTokens
			byProvider[s.Provider].outputTokens += s.OutputTokens
			byProvider[s.Provider].calls++
		}
		totalIn += s.InputTokens
		totalOut += s.OutputTokens
		totalCalls++
	}

	// Build sorted daily rows
	dailyRows := make([]map[string]any, 0, len(daily))
	for date, b := range daily {
		dailyRows = append(dailyRows, map[string]any{
			"date":          date,
			"input_tokens":  b.inputTokens,
			"output_tokens": b.outputTokens,
			"total_tokens":  b.inputTokens + b.outputTokens,
			"cost":          0.0,
			"calls":         b.calls,
		})
	}
	sort.Slice(dailyRows, func(i, j int) bool {
		return dailyRows[i]["date"].(string) < dailyRows[j]["date"].(string)
	})

	modelRows := make([]map[string]any, 0, len(byModel))
	for model, b := range byModel {
		modelRows = append(modelRows, map[string]any{
			"model":         model,
			"input_tokens":  b.inputTokens,
			"output_tokens": b.outputTokens,
			"total_tokens":  b.inputTokens + b.outputTokens,
			"cost":          0.0,
			"calls":         b.calls,
		})
	}
	sort.Slice(modelRows, func(i, j int) bool {
		return modelRows[i]["total_tokens"].(int) > modelRows[j]["total_tokens"].(int)
	})

	providerRows := make([]map[string]any, 0, len(byProvider))
	for provider, b := range byProvider {
		providerRows = append(providerRows, map[string]any{
			"provider":      provider,
			"input_tokens":  b.inputTokens,
			"output_tokens": b.outputTokens,
			"total_tokens":  b.inputTokens + b.outputTokens,
			"cost":          0.0,
			"calls":         b.calls,
		})
	}
	sort.Slice(providerRows, func(i, j int) bool {
		return providerRows[i]["total_tokens"].(int) > providerRows[j]["total_tokens"].(int)
	})

	return map[string]any{
		"totals": map[string]any{
			"input_tokens":  totalIn,
			"output_tokens": totalOut,
			"total_tokens":  totalIn + totalOut,
			"cost":          0.0,
			"calls":         totalCalls,
		},
		"daily":       dailyRows,
		"by_model":    modelRows,
		"by_provider": providerRows,
	}
}

func handleChannelsChat(state *ServerState, cmd map[string]any) {
	channelID, _ := cmd["channelId"].(string)
	ws := workspaceFromCmd(state, cmd)
	text, _ := cmd["text"].(string)
	provider, _ := cmd["provider"].(string)
	model, _ := cmd["model"].(string)
	agentName, _ := cmd["agentName"].(string)
	agentColor, _ := cmd["agentColor"].(string)
	agentInitial, _ := cmd["agentInitial"].(string)
	agentPhoto, _ := cmd["agentPhoto"].(string)
	agentGradient, _ := cmd["agentGradient"].(string)
	agentRole, _ := cmd["agentRole"].(string)
	forceDesktop, _ := cmd["forceDesktop"].(bool)
	if agentName == "" {
		agentName = "Agent"
	}
	if agentColor == "" {
		agentColor = "#7B5CF5"
	}
	if agentInitial == "" {
		agentInitial = "A"
	}

	if provider == "" || model == "" {
		Emit(map[string]any{"type": "channel_chat_error", "channelId": channelID, "error": "No provider or model selected."})
		return
	}

	// Prevent duplicate concurrent chats for the same channel.
	if !state.startChannelChat(channelID) {
		return // silently ignore — the Angular guard should stop duplicates before they arrive
	}

	sess, err := session.Create(ws, provider, model, "build",
		session.WithAgentName(agentName),
		session.WithColor(agentColor),
		session.WithChannelSession(), // keeps this session out of the dev-mode list
	)
	if err != nil {
		Emit(map[string]any{"type": "channel_chat_error", "channelId": channelID, "error": "Failed to create session: " + err.Error()})
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	state.registerTask(sess.ID, cancel, done)

	go func() {
		defer close(done)
		defer state.removeTask(sess.ID)
		defer session.UnregisterSession(sess.ID)
		defer state.endChannelChat(channelID)

		session.RegisterSession(sess.ID, ws, "channel")

		var accumulated strings.Builder
		var hadError bool

		emitFn := func(m map[string]any) {
			t, _ := m["type"].(string)
			switch t {
			case "token":
				content, _ := m["content"].(string)
				accumulated.WriteString(content)
				Emit(map[string]any{"type": "channel_token", "channelId": channelID, "content": content})
			case "tool_use_start":
				m["channelId"] = channelID
				Emit(m)
			case "screen_event":
				m["channelId"] = channelID
				Emit(m)
			case "done":
				// Save the assistant reply to the channel and emit channel_chat_done.
				savedMsg := map[string]any{
					"id":             uuid.NewString(),
					"from":           "agent",
					"senderName":     agentName,
					"avatarColor":    agentColor,
					"avatarInitial":  agentInitial,
					"avatarPhoto":    agentPhoto,
					"avatarGradient": agentGradient,
					"avatarRole":     agentRole,
					"time":           time.Now().UTC().Format(time.RFC3339),
					"type":           "text",
					"text":           accumulated.String(),
				}
				path := channelPath(ws, channelID)
				_ = appendChannelMessage(path, savedMsg)
				Emit(map[string]any{"type": "channel_chat_done", "channelId": channelID, "message": savedMsg})
			case "agent_error":
				// Terminal error from Execute. (The intermediate "error" event from
				// runLoop is for WS clients only — Execute re-emits it as agent_error.)
				hadError = true
				msg, _ := m["message"].(string)
				if msg == "" {
					msg = "An error occurred."
				}
				Emit(map[string]any{"type": "channel_chat_error", "channelId": channelID, "error": msg})
			case "cancelled":
				Emit(map[string]any{"type": "channel_chat_error", "channelId": channelID, "error": "cancelled"})
			}
		}

		// Look up agent config: desktop permission + custom instructions.
		desktopPermitted := forceDesktop
		customInstructions := ""
		for _, a := range loadAgents() {
			if a.Name == agentName {
				for _, p := range a.Permissions {
					if p == "desktop" {
						desktopPermitted = true
					}
				}
				// Build custom instructions from systemPrompt and/or role.
				if a.SystemPrompt != "" {
					customInstructions = a.SystemPrompt
				} else if a.Role != "" {
					customInstructions = a.Role
				}
				break
			}
		}

		var takeScreenshot func(context.Context) (map[string]any, error)
		if desktopPermitted {
			takeScreenshot = func(sctx context.Context) (map[string]any, error) {
				requestID := uuid.New().String()
				ch := state.registerScreenshotCh(requestID)
				Emit(map[string]any{
					"type":       "screenshot_request",
					"requestId":  requestID,
					"session_id": sess.ID,
				})
				select {
				case payload := <-ch:
					if errMsg, _ := payload["error"].(string); errMsg != "" {
						return nil, fmt.Errorf("screenshot failed: %s", errMsg)
					}
					return payload, nil
				case <-time.After(100 * time.Second):
					return nil, fmt.Errorf("screenshot timed out — Tauri did not respond within 100s")
				case <-sctx.Done():
					return nil, sctx.Err()
				}
			}
		}

		// Chat channels are unattended, so there is no interactive approval step.
		// Autonomous shell is therefore OFF unless an operator explicitly enables
		// ChannelShellAutoAllow — this fences off the injected-content -> run_shell
		// chain by default. Desktop tools are only offered when the agent has the
		// "desktop" permission.
		hadError = agent.Execute(ctx, agent.Config{
			SessionID:          sess.ID,
			Workspace:          ws,
			Message:            text,
			Mode:               "build",
			EmitFn:             emitFn,
			ShellAutoAllow:     config.GetSidecarSettings().ChannelShellAutoAllow,
			BrowserAvailable:   false,
			DesktopPermitted:   desktopPermitted,
			TakeScreenshot:     takeScreenshot,
			CustomInstructions: customInstructions,
		})

		if hadError && ctx.Err() == nil {
			// agent_error was already emitted via emitFn above; update session status.
			session.UpdateFields(ws, sess.ID, map[string]any{"status": "failed"})
		} else if ctx.Err() != nil {
			session.UpdateFields(ws, sess.ID, map[string]any{"status": "cancelled"})
		} else {
			session.UpdateFields(ws, sess.ID, map[string]any{"status": "idle"})
		}
	}()
}

func rewriteChannelMessages(path string, msgs []map[string]any) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, m := range msgs {
		_ = enc.Encode(m)
	}
}

// Package channels provides shared helpers used by all chat-platform gateways.
package channels

import (
	"context"
	"log"
	"strings"

	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/session"
)

// TurnConfig is the per-message configuration passed to RunAgentTurn.
type TurnConfig struct {
	Platform       string // "telegram" | "discord" | "slack" | "whatsapp" | "signal"
	ChatID         string // platform-specific conversation identifier
	WorkspacePath  string
	Provider       string
	Model          string
	ShellAutoAllow bool
	Message        string

	// RequestPerm, when set, overrides the default permission handler.
	// When nil, RunAgentTurn falls back to auto-allow/deny based on ShellAutoAllow.
	// The function should show a prompt to the user, wait for approval, and return
	// (allow, allowAll). "kind" is "shell" or "write". "detail" is e.g. the
	// command or file path.
	RequestPerm func(ctx context.Context, kind, detail string) (allow, allowAll bool)
}

// RunAgentTurn finds-or-creates a session for the chat, runs one agent turn,
// and returns the assistant's reply text.  It handles all session lifecycle
// bookkeeping (ClearLiveEvents, RegisterSession, status updates).
func RunAgentTurn(ctx context.Context, cfg TurnConfig) string {
	sessID, err := EnsureSession(cfg.Platform, cfg.ChatID, cfg.WorkspacePath, cfg.Provider, cfg.Model)
	if err != nil {
		log.Printf("[%s] EnsureSession(%s): %v", cfg.Platform, cfg.ChatID, err)
		return ""
	}

	var replyBuf strings.Builder
	agentCfg := agent.Config{
		SessionID:      sessID,
		Workspace:      cfg.WorkspacePath,
		Message:        cfg.Message,
		Mode:           "build",
		ShellAutoAllow: cfg.ShellAutoAllow,
		EmitFn: func(ev map[string]any) {
			switch t, _ := ev["type"].(string); t {
			case "token":
				if c, _ := ev["content"].(string); c != "" {
					replyBuf.WriteString(c)
				}
			case "agent_error":
				// Terminal error from Execute — surface to the user.
				// (The intermediate "error" event from runLoop is for WS clients
				// only; Execute re-emits it as agent_error for channel consumers.)
				if msg, _ := ev["message"].(string); msg != "" {
					log.Printf("[%s] agent error: %s", cfg.Platform, msg)
					replyBuf.WriteString("Sorry, I ran into an error: " + msg)
				}
			}
		},
		RequestPerm: func(ctx context.Context, command string) (bool, bool) {
			if cfg.RequestPerm != nil {
				// Determine the kind from the command string prefix.
				// Tools format the detail as "run_shell: <cmd>", "write_file: <path>", etc.
				kind := "shell"
				detail := command
				if idx := strings.Index(command, ": "); idx >= 0 {
					prefix := command[:idx]
					detail = command[idx+2:]
					switch prefix {
					case "write_file", "edit_file", "create_plan":
						kind = "write"
					case "run_shell":
						kind = "shell"
					}
				}
				return cfg.RequestPerm(ctx, kind, detail)
			}
			return cfg.ShellAutoAllow, false
		},
	}

	session.ClearLiveEvents(sessID)
	session.Global.ClearBuffer(sessID)
	session.UpdateFields(cfg.WorkspacePath, sessID, map[string]any{"status": "running"})
	session.RegisterSession(sessID, cfg.WorkspacePath, "channel")

	agent.Execute(ctx, agentCfg)

	session.UpdateFields(cfg.WorkspacePath, sessID, map[string]any{"status": "idle"})
	session.UnregisterSession(sessID)

	reply := strings.TrimSpace(replyBuf.String())
	if reply == "" {
		reply = "Done."
	}
	return reply
}

// EnsureSession returns the existing session ID for (platform, chatID) or creates
// a new channel session in workspacePath using the given provider/model.
// If provider or model is empty, it falls back to the best available provider
// via ai.Global.BestProvider so we never pass an empty model to the LLM.
func EnsureSession(platform, chatID, workspacePath, provider, model string) (string, error) {
	if provider == "" || model == "" {
		bestProv, bestModel := ai.Global.BestProvider(provider, model)
		if provider == "" {
			provider = bestProv
		}
		if model == "" {
			model = bestModel
		}
	}
	if b := Find(platform, chatID); b != nil {
		if existingSess, err := session.Load(workspacePath, b.SessionID); err == nil {
			// Patch provider/model on the existing session if they were previously empty.
			// We check BOTH the binding fields and the session fields, because a
			// previous patch may have updated the binding but failed to update the
			// session file (e.g. disk error, race). If the session's model is empty
			// even though the binding is not, we still patch.
			bindingNeedsPatch := (b.Provider == "" && provider != "") || (b.Model == "" && model != "")
			sessionNeedsPatch := (existingSess.Provider == "" && provider != "") || (existingSess.Model == "" && model != "")
			if bindingNeedsPatch || sessionNeedsPatch {
				session.UpdateFields(workspacePath, b.SessionID, map[string]any{
					"provider": provider,
					"model":    model,
				})
				if bindingNeedsPatch {
					b.Provider = provider
					b.Model = model
					Save(b)
				}
			}
			return b.SessionID, nil
		}
	}
	sess, err := session.Create(workspacePath, provider, model, "build",
		session.WithChannelSession())
	if err != nil {
		return "", err
	}
	Save(&Binding{
		Platform:      platform,
		ChatID:        chatID,
		SessionID:     sess.ID,
		WorkspacePath: workspacePath,
		Provider:      provider,
		Model:         model,
	})
	return sess.ID, nil
}

// SplitMessage splits text into chunks of at most maxLen runes, breaking at
// word or newline boundaries where possible.
func SplitMessage(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		end := maxLen
		if end > len(runes) {
			end = len(runes)
		}
		if end < len(runes) {
			for i := end; i > end-200 && i > 0; i-- {
				if runes[i] == '\n' || runes[i] == ' ' {
					end = i
					break
				}
			}
		}
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}

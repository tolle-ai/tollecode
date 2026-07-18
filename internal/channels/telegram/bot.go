// Package telegram implements a Telegram bot gateway via the Bot API long-polling
// interface. No external library is required — all calls are plain HTTPS to
// api.telegram.org.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/channels"
)

// Config holds everything the bot needs to start.
type Config struct {
	Token          string
	WorkspacePath  string
	Provider       string
	Model          string
	AgentID        string
	ShellAutoAllow bool
	// MentionOnly: when true, respond in group chats only when @-mentioned.
	// Private (DM) chats are always handled.
	MentionOnly bool

	// TurnFunc, when set, overrides the default channels.RunAgentTurn execution.
	// The self-hosted deployment injects a runner that drives the DB-native agent
	// chat path (provider/tools resolved per org+agent) instead of the local
	// filesystem agent engine. chatID is used as the conversation/thread key.
	// When nil, the bot falls back to channels.RunAgentTurn (desktop behaviour).
	TurnFunc func(ctx context.Context, chatID, text string) string

	// Greeting is the reply sent for the /start command. Populated with the
	// assigned agent's name/greeting so the bot introduces itself in-character
	// rather than as generic "TolleCode". Empty falls back to a default.
	Greeting string
}

// Start begins the long-polling loop. It blocks until ctx is cancelled.
func Start(ctx context.Context, cfg Config) {
	if cfg.Token == "" {
		log.Println("[telegram] no token configured — gateway disabled")
		return
	}
	b := &bot{
		cfg:          cfg,
		client:       &http.Client{Timeout: 40 * time.Second},
		permRequests: make(map[string]chan permResponse),
	}
	log.Printf("[telegram] gateway started (workspace=%s)", cfg.WorkspacePath)
	b.poll(ctx)
}

// ── bot internals ─────────────────────────────────────────────────────────────

type permResponse struct {
	Allow    bool
	AllowAll bool
}

type bot struct {
	cfg    Config
	client *http.Client

	username string // populated by getMe on startup

	// permRequests maps request IDs to channels that the RequestPerm callback
	// waits on. The callback_query handler writes to the channel when the user
	// taps an inline button.
	permMu      sync.Mutex
	permRequests map[string]chan permResponse
}

func (b *bot) poll(ctx context.Context) {
	// Resolve bot username for @mention detection in group chats.
	if me, err := b.getMe(ctx); err == nil {
		b.username = me
		log.Printf("[telegram] authenticated as @%s — starting long-poll", me)
	} else {
		log.Printf("[telegram] getMe failed: %v (check bot token)", err)
	}
	// Clear any registered webhook. Telegram rejects getUpdates with a 409
	// Conflict while a webhook is active, which silently blocks long-polling —
	// a common cause of "bot connected but never replies".
	b.deleteWebhook(ctx)
	var offset int64
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[telegram] getUpdates error: %v — retrying in 5s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			b.handleUpdate(ctx, u)
		}
	}
}

func (b *bot) handleUpdate(ctx context.Context, u tgUpdate) {
	// Handle callback queries (inline button presses for permission prompts).
	if cb := u.CallbackQuery; cb != nil {
		b.handleCallbackQuery(ctx, *cb)
		return
	}

	msg := u.Message
	if msg == nil {
		msg = u.EditedMessage
	}
	if msg == nil || msg.Text == "" {
		return
	}

	chatID := fmt.Sprintf("%d", msg.Chat.ID)
	userText := strings.TrimSpace(msg.Text)

	// Handle /start. Priority:
	//   1. An explicit configured greeting → send verbatim.
	//   2. An assigned agent (TurnFunc) → let the model introduce itself in-character
	//      (its name/persona is in the system prompt) so it never replies as "TolleCode".
	//   3. Desktop fallback with no assigned agent.
	if strings.HasPrefix(userText, "/start") {
		if b.cfg.Greeting != "" {
			b.sendMessage(ctx, chatID, b.cfg.Greeting)
			return
		}
		if b.cfg.TurnFunc != nil {
			b.sendChatAction(ctx, chatID, "typing")
			go func() {
				reply := strings.TrimSpace(b.cfg.TurnFunc(ctx, chatID,
					"A user just started the chat. Greet them warmly and briefly introduce yourself and how you can help."))
				for _, chunk := range channels.SplitMessage(reply, 4096) {
					if strings.TrimSpace(chunk) != "" {
						b.sendMessage(ctx, chatID, chunk)
					}
				}
			}()
			return
		}
		b.sendMessage(ctx, chatID, "Hi! I'm TolleCode. Send me a message to get started.")
		return
	}

	// In group/supergroup chats, apply mention_only filter.
	isGroup := msg.Chat.Type == "group" || msg.Chat.Type == "supergroup"
	if isGroup && b.cfg.MentionOnly {
		if !b.isMentioned(msg) {
			return
		}
		// Strip the @botusername mention so the agent sees clean text.
		if b.username != "" {
			userText = strings.TrimSpace(strings.ReplaceAll(userText, "@"+b.username, ""))
		}
		if userText == "" {
			return
		}
	}

	// Run the agent turn in a goroutine so the poll loop keeps running and can
	// receive callback_query updates (for permission prompts).
	b.sendChatAction(ctx, chatID, "typing")

	go func() {
		var reply string
		if b.cfg.TurnFunc != nil {
			// Self-hosted path: run the turn via the DB-native agent chat engine.
			reply = b.cfg.TurnFunc(ctx, chatID, userText)
		} else {
			reply = channels.RunAgentTurn(ctx, channels.TurnConfig{
				Platform:       "telegram",
				ChatID:         chatID,
				WorkspacePath:  b.cfg.WorkspacePath,
				Provider:       b.cfg.Provider,
				Model:          b.cfg.Model,
				ShellAutoAllow: b.cfg.ShellAutoAllow,
				Message:        userText,
				RequestPerm:    b.makeRequestPerm(chatID),
			})
		}

		if strings.TrimSpace(reply) == "" {
			return
		}
		for _, chunk := range channels.SplitMessage(reply, 4096) {
			b.sendMessage(ctx, chatID, chunk)
		}
	}()
}

// makeRequestPerm returns a permission-request function for the agent config.
// It sends a message with inline keyboard buttons to the Telegram chat and
// waits for the user to respond. The poll loop processes the callback query
// and resolves the pending channel.
func (b *bot) makeRequestPerm(chatID string) func(ctx context.Context, kind, detail string) (bool, bool) {
	return func(ctx context.Context, kind, detail string) (bool, bool) {
		reqID := uuid.NewString()[:8]

		// Build prompt text.
		label := "shell command"
		if kind == "write" {
			label = "file write"
		}
		text := fmt.Sprintf("⚠️ *Permission Request*\n\nThe agent wants to run a %s:\n```\n%s\n```\n\nTap a button below to approve or deny.", label, detail)

		// Inline keyboard with Allow / Deny / Allow All buttons.
		// Labels are specific to the kind so the user knows what "all" means.
		allowAllLabel := "✅ Allow All Shell"
		if kind == "write" {
			allowAllLabel = "✅ Allow All Writes"
		}
		keyboard := map[string]any{
			"inline_keyboard": [][]map[string]any{
				{
					{"text": "✅ Allow", "callback_data": "perm:allow:" + reqID},
					{"text": "❌ Deny", "callback_data": "perm:deny:" + reqID},
				},
				{
					{"text": allowAllLabel, "callback_data": "perm:allowall:" + reqID},
				},
			},
		}

		// Register the pending channel before sending the message
		// so we don't miss a fast response.
		ch := make(chan permResponse, 1)
		b.permMu.Lock()
		b.permRequests[reqID] = ch
		b.permMu.Unlock()
		defer func() {
			b.permMu.Lock()
			delete(b.permRequests, reqID)
			b.permMu.Unlock()
		}()

		// Send the permission prompt message with buttons.
		b.sendMessageWithKeyboard(ctx, chatID, text, keyboard)

		// Wait for the user to respond via callback query, or timeout.
		select {
		case resp := <-ch:
			return resp.Allow, resp.AllowAll
		case <-time.After(120 * time.Second):
			b.sendMessage(ctx, chatID, "_Permission request timed out._")
			return false, false
		case <-ctx.Done():
			return false, false
		}
	}
}

// handleCallbackQuery processes an inline button press from a permission prompt.
func (b *bot) handleCallbackQuery(ctx context.Context, cb tgCallbackQuery) {
	data := cb.Data
	if !strings.HasPrefix(data, "perm:") {
		// Not a permission callback — acknowledge and ignore.
		b.answerCallbackQuery(ctx, cb.ID, "")
		return
	}

	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 3 {
		b.answerCallbackQuery(ctx, cb.ID, "Invalid request")
		return
	}
	action, reqID := parts[1], parts[2]

	// Look up the pending permission request.
	b.permMu.Lock()
	ch, ok := b.permRequests[reqID]
	b.permMu.Unlock()

	if !ok {
		b.answerCallbackQuery(ctx, cb.ID, "Request expired")
		return
	}

	// Resolve the permission request.
	resp := permResponse{}
	switch action {
	case "allow":
		resp.Allow = true
		b.answerCallbackQuery(ctx, cb.ID, "✅ Approved")
	case "allowall":
		resp.Allow = true
		resp.AllowAll = true
		b.answerCallbackQuery(ctx, cb.ID, "✅ Approved (all future)")
	case "deny":
		resp.Allow = false
		b.answerCallbackQuery(ctx, cb.ID, "❌ Denied")
	default:
		b.answerCallbackQuery(ctx, cb.ID, "Unknown action")
		return
	}

	// Non-blocking write — the channel is buffered with size 1.
	select {
	case ch <- resp:
	default:
	}
}

// ── Telegram Bot API ──────────────────────────────────────────────────────────

type tgUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	EditedMessage *tgMessage       `json:"edited_message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgCallbackQuery struct {
	ID   string   `json:"id"`
	From tgUser   `json:"from"`
	Data string   `json:"data"`
}

type tgMessage struct {
	MessageID int64        `json:"message_id"`
	Text      string       `json:"text"`
	Chat      tgChat       `json:"chat"`
	From      tgUser       `json:"from"`
	Entities  []tgEntity   `json:"entities"`
}

type tgEntity struct {
	Type   string `json:"type"`   // "mention", "text_mention", etc.
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

func (b *bot) apiURL(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", b.cfg.Token, method)
}

func (b *bot) getUpdates(ctx context.Context, offset int64) ([]tgUpdate, error) {
	params := url.Values{}
	params.Set("offset", fmt.Sprintf("%d", offset))
	params.Set("timeout", "30")
	params.Set("allowed_updates", `["message","edited_message","callback_query"]`)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.apiURL("getUpdates")+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool       `json:"ok"`
		Description string     `json:"description"`
		Result      []tgUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK {
		desc := result.Description
		if desc == "" {
			desc = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("getUpdates rejected: %s", desc)
	}
	return result.Result, nil
}

// deleteWebhook removes any webhook registered for this bot so long-polling can
// receive updates. Idempotent — a no-op when no webhook is set.
func (b *bot) deleteWebhook(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL("deleteWebhook"), nil)
	if err != nil {
		return
	}
	resp, err := b.client.Do(req)
	if err != nil {
		log.Printf("[telegram] deleteWebhook error: %v", err)
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

func (b *bot) sendMessage(ctx context.Context, chatID, text string) {
	b.sendMessageWithKeyboard(ctx, chatID, text, nil)
}

// sendMessageWithKeyboard sends a message with an optional inline keyboard.
// If keyboard is nil, no keyboard is attached.
func (b *bot) sendMessageWithKeyboard(ctx context.Context, chatID, text string, keyboard map[string]any) {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
	}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.apiURL("sendMessage"), bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		log.Printf("[telegram] sendMessage error: %v", err)
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

// answerCallbackQuery acknowledges an inline button press with an optional
// text notification shown briefly to the user.
func (b *bot) answerCallbackQuery(ctx context.Context, queryID, text string) {
	payload := map[string]any{
		"callback_query_id": queryID,
	}
	if text != "" {
		payload["text"] = text
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.apiURL("answerCallbackQuery"), bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

func (b *bot) sendChatAction(ctx context.Context, chatID, action string) {
	payload, _ := json.Marshal(map[string]any{"chat_id": chatID, "action": action})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.apiURL("sendChatAction"), bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

// getMe calls getMe and returns the bot's username.
func (b *bot) getMe(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.apiURL("getMe"), nil)
	if err != nil {
		return "", err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Result.Username, nil
}

// isMentioned returns true when the message contains a @mention entity that
// matches the bot's username, or a text_mention pointing to the bot.
func (b *bot) isMentioned(msg *tgMessage) bool {
	if b.username == "" {
		return false
	}
	mention := "@" + b.username
	for _, e := range msg.Entities {
		if e.Type == "mention" {
			runes := []rune(msg.Text)
			if e.Offset+e.Length <= len(runes) {
				if string(runes[e.Offset:e.Offset+e.Length]) == mention {
					return true
				}
			}
		}
	}
	return false
}

// ── helpers ───────────────────────────────────────────────────────────────────

// splitMessage splits text into chunks of at most maxLen runes,
// breaking at word or newline boundaries where possible.
func splitMessage(text string, maxLen int) []string {
	return channels.SplitMessage(text, maxLen)
}
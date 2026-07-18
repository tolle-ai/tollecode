// Package slack implements a Slack bot gateway using the Slack Events API (webhook mode).
// Slack POSTs event callbacks to the server; we reply via the Web API.
//
// Setup in the Slack app dashboard:
//  1. Enable Event Subscriptions → Request URL: https://your-server/v1/channels/slack/events
//  2. Subscribe to bot events: message.im, app_mention
//  3. Add OAuth scopes: chat:write, im:history, app_mentions:read
//  4. Install to workspace and copy the Bot Token (xoxb-…)
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/channels"
)

// Config holds everything the Slack gateway needs.
type Config struct {
	BotToken       string
	SigningSecret  string
	WorkspacePath  string
	Provider       string
	Model          string
	AgentID        string
	ShellAutoAllow bool
}

// Handler returns an http.HandlerFunc that handles all Slack Events API requests
// (both the url_verification challenge and live event callbacks).
func Handler(cfg Config) http.HandlerFunc {
	b := &bot{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}}
	return b.serveHTTP
}

// ── bot internals ─────────────────────────────────────────────────────────────

type bot struct {
	cfg  Config
	http *http.Client
}

func (b *bot) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	if err := b.verifySignature(r, body); err != nil {
		log.Printf("[slack] signature verification failed: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var envelope slackEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Respond to the url_verification challenge immediately.
	if envelope.Type == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge}) //nolint:errcheck
		return
	}

	// Acknowledge all other events immediately (Slack requires < 3 s response).
	w.WriteHeader(http.StatusOK)

	if envelope.Type != "event_callback" {
		return
	}

	ev := envelope.Event
	// Ignore bot messages to avoid loops.
	if ev.BotID != "" || ev.SubType == "bot_message" {
		return
	}

	// Handle DMs (message.im) and channel @mentions (app_mention).
	if ev.Type != "message" && ev.Type != "app_mention" {
		return
	}

	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return
	}
	// Strip the @-mention from app_mention events so the agent sees clean text.
	if ev.Type == "app_mention" {
		text = stripMentionPrefix(text)
	}

	// Detach from the request context: once serveHTTP returns Go cancels r.Context(),
	// but the agent run must outlive the HTTP handler.
	go b.handleEvent(context.WithoutCancel(r.Context()), ev.Channel, text)
}

func (b *bot) handleEvent(ctx context.Context, channelID, text string) {
	reply := channels.RunAgentTurn(ctx, channels.TurnConfig{
		Platform:       "slack",
		ChatID:         channelID,
		WorkspacePath:  b.cfg.WorkspacePath,
		Provider:       b.cfg.Provider,
		Model:          b.cfg.Model,
		ShellAutoAllow: b.cfg.ShellAutoAllow,
		Message:        text,
	})
	for _, chunk := range channels.SplitMessage(reply, 3000) {
		b.postMessage(ctx, channelID, chunk)
	}
}

// ── Slack signature verification ─────────────────────────────────────────────

func (b *bot) verifySignature(r *http.Request, body []byte) error {
	if b.cfg.SigningSecret == "" {
		return nil // verification disabled (dev mode)
	}
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return fmt.Errorf("missing signature headers")
	}
	// Replay protection: reject requests older than 5 minutes.
	var tsInt int64
	fmt.Sscanf(ts, "%d", &tsInt)
	if abs(time.Now().Unix()-tsInt) > 300 {
		return fmt.Errorf("request timestamp too old")
	}
	baseString := "v0:" + ts + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(b.cfg.SigningSecret))
	mac.Write([]byte(baseString)) //nolint:errcheck
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ── Slack Web API ─────────────────────────────────────────────────────────────

type slackEnvelope struct {
	Type      string     `json:"type"`
	Challenge string     `json:"challenge"`
	Event     slackEvent `json:"event"`
}

type slackEvent struct {
	Type    string `json:"type"`
	SubType string `json:"subtype"`
	BotID   string `json:"bot_id"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
	User    string `json:"user"`
}

func (b *bot) postMessage(ctx context.Context, channelID, text string) {
	body, _ := json.Marshal(map[string]string{
		"channel": channelID,
		"text":    text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.BotToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		log.Printf("[slack] postMessage error: %v", err)
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

// ── helpers ───────────────────────────────────────────────────────────────────

// stripMentionPrefix removes a leading <@USERID> mention from app_mention event text.
func stripMentionPrefix(text string) string {
	if !strings.HasPrefix(text, "<@") {
		return text
	}
	end := strings.Index(text, ">")
	if end == -1 {
		return text
	}
	return strings.TrimSpace(text[end+1:])
}

func splitMessage(text string, maxLen int) []string {
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

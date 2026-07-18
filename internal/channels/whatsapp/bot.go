// Package whatsapp implements a WhatsApp bot gateway via the Meta Cloud API.
// No external library is required — all communication uses stdlib net/http.
//
// Meta webhook setup:
//  1. Create a Meta app at developers.facebook.com
//  2. Add WhatsApp product → phone number
//  3. Configure webhook: URL = https://your-server/v1/channels/whatsapp/events
//     Verify token = the value you set in config
//  4. Subscribe to messages webhook field
package whatsapp

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

const graphAPI = "https://graph.facebook.com/v19.0"

// Config holds everything the WhatsApp gateway needs.
type Config struct {
	PhoneNumberID  string
	AccessToken    string
	// AppSecret is used to verify the X-Hub-Signature-256 header on incoming webhooks.
	AppSecret      string
	VerifyToken    string
	WorkspacePath  string
	Provider       string
	Model          string
	AgentID        string
	ShellAutoAllow bool
}

// Handler returns an http.HandlerFunc for both GET (verification) and POST (events).
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
	switch r.Method {
	case http.MethodGet:
		b.handleVerification(w, r)
	case http.MethodPost:
		b.handleEvent(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVerification responds to Meta's hub.challenge webhook verification request.
func (b *bot) handleVerification(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") == "subscribe" && q.Get("hub.verify_token") == b.cfg.VerifyToken {
		fmt.Fprint(w, q.Get("hub.challenge"))
		return
	}
	http.Error(w, "forbidden", http.StatusForbidden)
}

func (b *bot) handleEvent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	if err := b.verifySignature(r, body); err != nil {
		log.Printf("[whatsapp] signature verification failed: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Acknowledge immediately — Meta requires a 200 within 20 seconds.
	w.WriteHeader(http.StatusOK)

	var notification waNotification
	if err := json.Unmarshal(body, &notification); err != nil {
		return
	}

	for _, entry := range notification.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" {
				continue
			}
			for _, msg := range change.Value.Messages {
				if msg.Type != "text" {
					continue
				}
				from := msg.From
				msgID := msg.ID
				text := strings.TrimSpace(msg.Text.Body)
				if text == "" {
					continue
				}
				// Detach from request context: handler returns 200 immediately,
				// which cancels r.Context() before the agent run completes.
				go b.processMessage(context.WithoutCancel(r.Context()), from, msgID, text)
			}
		}
	}
}

func (b *bot) processMessage(ctx context.Context, from, msgID, text string) {
	// Mark the specific message as read using its wamid before running the agent.
	b.markRead(ctx, msgID)

	reply := channels.RunAgentTurn(ctx, channels.TurnConfig{
		Platform:       "whatsapp",
		ChatID:         from,
		WorkspacePath:  b.cfg.WorkspacePath,
		Provider:       b.cfg.Provider,
		Model:          b.cfg.Model,
		ShellAutoAllow: b.cfg.ShellAutoAllow,
		Message:        text,
	})
	for _, chunk := range channels.SplitMessage(reply, 4000) {
		b.sendMessage(ctx, from, chunk)
	}
}

// ── Meta signature verification ───────────────────────────────────────────────

func (b *bot) verifySignature(r *http.Request, body []byte) error {
	if b.cfg.AppSecret == "" {
		return nil // verification disabled — dev/local mode
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}
	// Meta signs with the App Secret, not the access token.
	mac := hmac.New(sha256.New, []byte(b.cfg.AppSecret))
	mac.Write(body) //nolint:errcheck
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// ── Meta Cloud API ────────────────────────────────────────────────────────────

type waNotification struct {
	Entry []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Messages []struct {
					ID   string `json:"id"`
					From string `json:"from"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

func (b *bot) sendMessage(ctx context.Context, to, text string) {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]string{"body": text},
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/%s/messages", graphAPI, b.cfg.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		log.Printf("[whatsapp] sendMessage error: %v", err)
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

func (b *bot) markRead(ctx context.Context, messageID string) {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"status":            "read",
		"message_id":        messageID,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/%s/messages", graphAPI, b.cfg.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
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

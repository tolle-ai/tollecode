// Package discord implements a Discord bot gateway via the Discord Gateway WebSocket API.
// No external library is required — all WebSocket and REST calls use stdlib + gorilla/websocket.
//
// Op-code flow: HELLO (10) → IDENTIFY (2) → READY → heartbeat loop → MESSAGE_CREATE events.
//
// Intents used:
//   - GUILDS            (1 << 0  =     1)
//   - GUILD_MESSAGES    (1 << 9  =   512)
//   - MESSAGE_CONTENT   (1 << 15 = 32768)  requires privileged intent in the developer portal
//   - DIRECT_MESSAGES   (1 << 12 =  4096)
//
// Total intent bitmask: 37377
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tolle-ai/tollecode/internal/channels"
)

const (
	gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
	apiBase    = "https://discord.com/api/v10"
	intents    = 1 | 512 | 4096 | 32768 // GUILDS | GUILD_MESSAGES | DIRECT_MESSAGES | MESSAGE_CONTENT
)

// Config holds everything the bot needs.
type Config struct {
	Token          string // include "Bot " prefix
	WorkspacePath  string
	Provider       string
	Model          string
	AgentID        string
	ShellAutoAllow bool
	MentionOnly    bool
	// TurnFunc, when set, overrides the default channels.RunAgentTurn execution
	// (used by the self-host DB-native path). It receives the channel id and text
	// and returns the reply.
	TurnFunc func(ctx context.Context, chatID, text string) string
}

// Start connects to the Discord Gateway and runs the event loop.
// Blocks until ctx is cancelled. Reconnects automatically on disconnect.
func Start(ctx context.Context, cfg Config) {
	if cfg.Token == "" {
		log.Println("[discord] no token configured — gateway disabled")
		return
	}
	b := &bot{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}}
	log.Printf("[discord] gateway started (workspace=%s)", cfg.WorkspacePath)
	for ctx.Err() == nil {
		if err := b.connect(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[discord] reconnecting in 5s: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// ── bot internals ─────────────────────────────────────────────────────────────

type bot struct {
	cfg    Config
	http   *http.Client
	selfID string // populated after READY
}

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	T  string          `json:"t,omitempty"`
	S  *int            `json:"s,omitempty"`
}

func (b *bot) connect(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, gatewayURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	var (
		seq           *int
		heartbeatStop = make(chan struct{})
		mu            sync.Mutex
		wg            sync.WaitGroup
	)

	send := func(p payload) error {
		mu.Lock()
		defer mu.Unlock()
		return conn.WriteJSON(p)
	}

	for {
		var p payload
		if err := conn.ReadJSON(&p); err != nil {
			close(heartbeatStop)
			wg.Wait()
			return fmt.Errorf("read: %w", err)
		}
		if p.S != nil {
			seq = p.S
		}

		switch p.Op {
		case 10: // HELLO
			var hello struct {
				HeartbeatInterval int `json:"heartbeat_interval"`
			}
			json.Unmarshal(p.D, &hello) //nolint:errcheck
			// Send IDENTIFY
			identify := map[string]any{
				"token":   b.cfg.Token,
				"intents": intents,
				"properties": map[string]string{
					"os":      "linux",
					"browser": "tollecode",
					"device":  "tollecode",
				},
			}
			idata, _ := json.Marshal(identify)
			if err := send(payload{Op: 2, D: idata}); err != nil {
				return fmt.Errorf("identify: %w", err)
			}
			// Start heartbeat loop
			interval := time.Duration(hello.HeartbeatInterval) * time.Millisecond
			wg.Add(1)
			go func() {
				defer wg.Done()
				t := time.NewTicker(interval)
				defer t.Stop()
				for {
					select {
					case <-heartbeatStop:
						return
					case <-ctx.Done():
						return
					case <-t.C:
						var seqVal any
						if seq != nil {
							seqVal = *seq
						}
						d, _ := json.Marshal(seqVal)
						mu.Lock()
						conn.WriteJSON(payload{Op: 1, D: d}) //nolint:errcheck
						mu.Unlock()
					}
				}
			}()

		case 0: // DISPATCH
			switch p.T {
			case "READY":
				var ready struct {
					User struct {
						ID string `json:"id"`
					} `json:"user"`
				}
				json.Unmarshal(p.D, &ready) //nolint:errcheck
				b.selfID = ready.User.ID
				log.Printf("[discord] READY (bot id=%s)", b.selfID)

			case "MESSAGE_CREATE":
				var msg discordMessage
				if err := json.Unmarshal(p.D, &msg); err != nil {
					continue
				}
				// Ignore our own messages
				if msg.Author.ID == b.selfID || msg.Author.Bot {
					continue
				}
				go b.handleMessage(ctx, msg)
			}

		case 7: // RECONNECT
			close(heartbeatStop)
			wg.Wait()
			return fmt.Errorf("server requested reconnect")

		case 9: // INVALID SESSION
			close(heartbeatStop)
			wg.Wait()
			return fmt.Errorf("invalid session")

		case 11: // HEARTBEAT ACK — no action needed
		}
	}
}

func (b *bot) handleMessage(ctx context.Context, msg discordMessage) {
	// DM channels have type 1; guild channels have type 0.
	isDM := msg.GuildID == ""

	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return
	}

	// In guild channels, apply mention_only filter.
	if !isDM && b.cfg.MentionOnly {
		if !b.isMentioned(msg) {
			return
		}
		// Strip the @mention from the message so the agent sees clean text.
		text = b.stripMention(text)
	}

	b.sendTyping(ctx, msg.ChannelID)

	var reply string
	if b.cfg.TurnFunc != nil {
		reply = b.cfg.TurnFunc(ctx, msg.ChannelID, text)
	} else {
		reply = channels.RunAgentTurn(ctx, channels.TurnConfig{
			Platform:       "discord",
			ChatID:         msg.ChannelID,
			WorkspacePath:  b.cfg.WorkspacePath,
			Provider:       b.cfg.Provider,
			Model:          b.cfg.Model,
			ShellAutoAllow: b.cfg.ShellAutoAllow,
			Message:        text,
		})
	}
	// Discord message limit is 2000 chars.
	for _, chunk := range channels.SplitMessage(reply, 2000) {
		b.sendMessage(ctx, msg.ChannelID, chunk)
	}
}

func (b *bot) isMentioned(msg discordMessage) bool {
	for _, u := range msg.Mentions {
		if u.ID == b.selfID {
			return true
		}
	}
	return false
}

func (b *bot) stripMention(text string) string {
	if b.selfID == "" {
		return text
	}
	text = strings.ReplaceAll(text, "<@"+b.selfID+">", "")
	text = strings.ReplaceAll(text, "<@!"+b.selfID+">", "")
	return strings.TrimSpace(text)
}

// ── Discord REST ──────────────────────────────────────────────────────────────

type discordMessage struct {
	ID        string        `json:"id"`
	ChannelID string        `json:"channel_id"`
	// GuildID is absent on DMs; present on guild channel messages.
	GuildID   string        `json:"guild_id"`
	Content   string        `json:"content"`
	Author    discordUser   `json:"author"`
	Mentions  []discordUser `json:"mentions"`
}

type discordUser struct {
	ID  string `json:"id"`
	Bot bool   `json:"bot"`
}

func (b *bot) sendMessage(ctx context.Context, channelID, content string) {
	body, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/channels/"+channelID+"/messages", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", b.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		log.Printf("[discord] sendMessage error: %v", err)
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

func (b *bot) sendTyping(ctx context.Context, channelID string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/channels/"+channelID+"/typing", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", b.cfg.Token)
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

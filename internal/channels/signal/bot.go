// Package signal implements a Signal gateway via the signal-cli daemon.
//
// Prerequisites on the host:
//
//	brew install signal-cli          # macOS
//	apt-get install signal-cli       # Debian/Ubuntu
//
// Register the number once:
//
//	signal-cli -a +15551234567 register
//	signal-cli -a +15551234567 verify <code>
//
// signal-cli is then started by this gateway as a subprocess in JSON daemon mode.
// We write send commands to its stdin and read incoming message events from stdout.
package signal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/channels"
)

// Config holds everything the Signal gateway needs.
type Config struct {
	// PhoneNumber is the Signal account number (e.g. "+15551234567").
	PhoneNumber    string
	// CLIPath is the path to the signal-cli binary (default: "signal-cli").
	CLIPath        string
	WorkspacePath  string
	Provider       string
	Model          string
	AgentID        string
	ShellAutoAllow bool
	// TurnFunc, when set, overrides the default channels.RunAgentTurn execution
	// (used by the self-host DB-native path).
	TurnFunc func(ctx context.Context, chatID, text string) string
}

// Start launches signal-cli in JSON daemon mode and processes incoming messages.
// Blocks until ctx is cancelled. Restarts the subprocess automatically on crash.
func Start(ctx context.Context, cfg Config) {
	if cfg.PhoneNumber == "" {
		log.Println("[signal] no phone number configured — gateway disabled")
		return
	}
	cliPath := cfg.CLIPath
	if cliPath == "" {
		cliPath = "signal-cli"
	}
	b := &bot{cfg: cfg, cliPath: cliPath}
	log.Printf("[signal] gateway started (number=%s)", cfg.PhoneNumber)
	for ctx.Err() == nil {
		if err := b.run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[signal] subprocess exited (%v) — restarting in 5s", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// ── bot internals ─────────────────────────────────────────────────────────────

type bot struct {
	cfg     Config
	cliPath string
	stdin   io.Writer // set while subprocess is running
}

func (b *bot) run(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, b.cliPath,
		"-a", b.cfg.PhoneNumber,
		"--output", "json",
		"daemon", "--no-receive-stdout",
	)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start signal-cli: %w", err)
	}
	b.stdin = stdinPipe

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev signalEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Envelope.DataMessage != nil && ev.Envelope.DataMessage.Message != "" {
			from := ev.Envelope.Source
			text := strings.TrimSpace(ev.Envelope.DataMessage.Message)
			go b.handleMessage(ctx, from, text)
		}
	}

	stdinPipe.Close()
	return cmd.Wait()
}

func (b *bot) handleMessage(ctx context.Context, from, text string) {
	var reply string
	if b.cfg.TurnFunc != nil {
		reply = b.cfg.TurnFunc(ctx, from, text)
	} else {
		reply = channels.RunAgentTurn(ctx, channels.TurnConfig{
			Platform:       "signal",
			ChatID:         from,
			WorkspacePath:  b.cfg.WorkspacePath,
			Provider:       b.cfg.Provider,
			Model:          b.cfg.Model,
			ShellAutoAllow: b.cfg.ShellAutoAllow,
			Message:        text,
		})
	}
	b.sendMessage(ctx, from, reply)
}

// sendMessage writes a JSON send command to signal-cli's stdin.
func (b *bot) sendMessage(_ context.Context, to, text string) {
	if b.stdin == nil {
		return
	}
	cmd := map[string]any{
		"jsonrpc": "2.0",
		"method":  "send",
		"id":      fmt.Sprintf("%d", time.Now().UnixNano()),
		"params": map[string]any{
			"recipient": []string{to},
			"message":   text,
		},
	}
	line, err := json.Marshal(cmd)
	if err != nil {
		return
	}
	line = append(line, '\n')
	b.stdin.Write(line) //nolint:errcheck
}

// ── signal-cli JSON event types ───────────────────────────────────────────────

type signalEvent struct {
	Envelope signalEnvelope `json:"envelope"`
}

type signalEnvelope struct {
	Source      string              `json:"source"`
	DataMessage *signalDataMessage  `json:"dataMessage"`
}

type signalDataMessage struct {
	Message string `json:"message"`
}

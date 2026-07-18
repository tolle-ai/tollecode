//go:build !windows

package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunShellForegroundCancelStopsPromptly reproduces the "can't stop a running
// shell command" bug: the command spawns a child (sleep) that inherits the
// stdout pipe. Cancelling the context must kill the whole process group so the
// read loop unblocks and the tool returns quickly — not after the child's own
// 30s lifetime.
func TestRunShellForegroundCancelStopsPromptly(t *testing.T) {
	cfg := &Config{Workspace: t.TempDir(), Mode: "build", ShellAutoAllow: true}
	ctx, cancel := context.WithCancel(context.Background())

	type result struct {
		out   string
		isErr bool
	}
	done := make(chan result, 1)
	go func() {
		out, isErr := toolRunShell(ctx, cfg, map[string]any{"command": "echo running; sleep 30"})
		done <- result{out, isErr}
	}()

	time.Sleep(400 * time.Millisecond) // let the command start
	cancel()

	select {
	case r := <-done:
		if !strings.Contains(strings.ToLower(r.out), "cancel") {
			t.Errorf("expected a cancellation message, got: %q", r.out)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("toolRunShell did not return within 8s of cancel — the command could not be stopped")
	}
}

// TestRunShellForegroundCapturesOutput guards the writer-based refactor: normal
// output (including stderr and a trailing line without a newline) must still be
// captured faithfully.
func TestRunShellForegroundCapturesOutput(t *testing.T) {
	cfg := &Config{Workspace: t.TempDir(), Mode: "build", ShellAutoAllow: true}
	out, isErr := toolRunShell(context.Background(), cfg, map[string]any{
		"command": "printf 'line1\\nline2\\n'; printf 'to-stderr\\n' 1>&2; printf 'noeol'",
	})
	if isErr {
		t.Fatalf("unexpected error result: %q", out)
	}
	for _, want := range []string{"line1", "line2", "to-stderr", "noeol"} {
		if !strings.Contains(out, want) {
			t.Errorf("captured output missing %q; got: %q", want, out)
		}
	}
}

// TestRunShellForegroundLingeringChildDoesNotHang covers normal completion where
// a backgrounded child keeps the stdout pipe open after the shell exits. Without
// cmd.WaitDelay the read loop blocks for the child's full lifetime; with it the
// tool returns shortly after the shell exits.
func TestRunShellForegroundLingeringChildDoesNotHang(t *testing.T) {
	cfg := &Config{Workspace: t.TempDir(), Mode: "build", ShellAutoAllow: true}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		toolRunShell(context.Background(), cfg, map[string]any{"command": "sleep 30 & echo done"})
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 12*time.Second {
			t.Fatalf("toolRunShell took %v — a lingering child hung the read loop", elapsed)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("toolRunShell hung past 12s on a lingering background child")
	}
}

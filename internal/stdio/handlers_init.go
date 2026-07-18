package stdio

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tolle-ai/tollecode/internal/session"
	"github.com/tolle-ai/tollecode/internal/todo"
)

func handleInit(state *ServerState, cmd map[string]any) {
	// Accept both "workspacePath" (frontend convention) and "workspace" (fallback).
	ws, _ := cmd["workspacePath"].(string)
	if ws == "" {
		ws, _ = cmd["workspace"].(string)
	}
	if ws != "" && ws != "/" {
		cleanPath := filepath.Clean(ws)
		if cleanPath != "." && cleanPath != ".." {
			// Ensure the workspace directory exists on disk (same as register_workspace).
			if err := os.MkdirAll(cleanPath, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "[handleInit] MkdirAll(%s) failed: %v\n", cleanPath, err)
			}
			if err := os.MkdirAll(filepath.Join(cleanPath, ".agent"), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "[handleInit] MkdirAll(.agent) failed: %v\n", err)
			}
			state.mu.Lock()
			state.Workspace = cleanPath
			state.mu.Unlock()
		}
	}
	// Fall back to cwd if workspace was never set.
	state.mu.Lock()
	if state.Workspace == "" {
		if cwd, err := os.Getwd(); err == nil {
			state.Workspace = cwd
		}
	}
	w := state.Workspace
	state.mu.Unlock()
	// Remove stale "running" entries from dead sidecar processes.
	session.PurgeDead()

	Emit(map[string]any{"type": "ready", "workspace": w})
}

func handleRegisterWorkspace(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["path"].(string)
	name, _ := cmd["name"].(string)
	if name == "" && wsPath != "" {
		name = filepath.Base(wsPath)
	}
	ok := false
	errMsg := ""
	if wsPath != "" {
		// Ensure the workspace directory exists on disk (mirrors the HTTP server's
		// createWorkspace behaviour).  Reject obviously invalid paths.
		cleanPath := filepath.Clean(wsPath)
		if cleanPath != "/" && cleanPath != "." && cleanPath != ".." {
			if err := os.MkdirAll(cleanPath, 0o755); err != nil {
				errMsg = fmt.Sprintf("mkdir %s: %v", cleanPath, err)
				fmt.Fprintf(os.Stderr, "[handleRegisterWorkspace] MkdirAll(%s) failed: %v\n", cleanPath, err)
			} else {
				// Create the .agent subdirectory used for memory, todos, etc.
				if err := os.MkdirAll(filepath.Join(cleanPath, ".agent"), 0o755); err != nil {
					fmt.Fprintf(os.Stderr, "[handleRegisterWorkspace] MkdirAll(.agent) failed: %v\n", err)
				}
				state.mu.Lock()
				state.Workspace = cleanPath
				state.mu.Unlock()
				// Eagerly load the todo store so the scheduler can find pending tasks.
				todo.LoadWorkspace(cleanPath)
				// Reset tasks that were mid-execution when the sidecar last died.
				todo.ResetStaleRunning(cleanPath)
				// Reset chat sessions left in "running" by a force-quit/crash so the
				// frontend doesn't hydrate them as perpetually-streaming (see
				// session.ResetStaleRunning). PurgeDead() in handleInit has already
				// pruned the in-memory active registry by this point.
				session.ResetStaleRunning(cleanPath)
				ok = true
			}
		}
	}
	resp := map[string]any{"type": "register_workspace", "ok": ok, "path": wsPath, "name": name}
	if errMsg != "" {
		resp["error"] = errMsg
	}
	Emit(resp)
}

func handleSetMode(state *ServerState, cmd map[string]any) {
	mode, _ := cmd["mode"].(string)
	if mode == "" {
		mode = "build"
	}
	state.mu.Lock()
	state.Mode = mode
	sid := state.SessionID
	ws := state.Workspace
	state.mu.Unlock()

	if sid != "" {
		// TODO Phase 2: session_store.UpdateField(ws, sid, "mode", mode)
		_ = ws
	}
	Emit(map[string]any{"type": "mode_set", "mode": mode})
}

func handleSetThinking(state *ServerState, cmd map[string]any) {
	// Support both old budget-based API and new boolean/level API.
	// "thinking" key: bool or string level ("true","false","low","medium","high")
	// "budget" key (legacy, Anthropic only): string shorthand or raw number
	thinkLevel := ""
	budget := 0

	if t, ok := cmd["thinking"]; ok {
		switch v := t.(type) {
		case bool:
			if v {
				thinkLevel = "true"
			} else {
				thinkLevel = "false"
			}
		case string:
			thinkLevel = v
		case float64:
			if v > 0 {
				thinkLevel = "true"
				budget = int(v)
			} else {
				thinkLevel = "false"
			}
		}
	} else {
		// Legacy budget-only path.
		budgetMap := map[string]int{
			"0": 0, "off": 0,
			"1k": 1024, "4k": 4096, "10k": 10000, "32k": 32000,
		}
		switch v := cmd["budget"].(type) {
		case string:
			budget = budgetMap[v]
			if budget > 0 {
				thinkLevel = "true"
			} else {
				thinkLevel = "false"
			}
		case float64:
			budget = int(v)
			if budget > 0 {
				thinkLevel = "true"
			} else {
				thinkLevel = "false"
			}
		}
	}

	state.mu.Lock()
	state.ThinkLevel = thinkLevel
	state.ThinkingBudget = budget
	state.mu.Unlock()
	Emit(map[string]any{"type": "thinking_set", "think_level": thinkLevel, "budget": budget})
}

func handleSetShellAutoAllow(state *ServerState, cmd map[string]any) {
	enabled, _ := cmd["enabled"].(bool)
	state.mu.Lock()
	state.AllowAllShell = enabled
	state.mu.Unlock()
	Emit(map[string]any{"type": "shell_auto_allow_set", "enabled": enabled})
}

func handleSetSessionLimit(state *ServerState, cmd map[string]any) {
	limit := 0
	switch v := cmd["limit"].(type) {
	case float64:
		limit = int(v)
	case int:
		limit = v
	}
	state.mu.Lock()
	state.MaxSessionMessages = limit
	state.mu.Unlock()
	Emit(map[string]any{"type": "session_limit_set", "limit": limit})
}

var _ = os.Stderr // keep import (used by fmt.Fprintf above)

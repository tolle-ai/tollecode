// Package cli implements the interactive TolleCode CLI.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/mcp"
	"github.com/tolle-ai/tollecode/internal/session"
)

// Config holds startup options for the CLI.
type Config struct {
	Workspace      string
	Mode           string // "build" or "plan"
	ThinkingBudget int
	ProviderID     string
	Model          string
	Task           string              // non-empty → one-shot mode
	SessionID      string              // non-empty → resume this session prefix
	AgentArg       string              // pre-select agent by name or ID
	TeamArg        string              // pre-select team by name or ID
	Skills         []string // skill names to activate on the session (cloud mode)
}

// Run starts the interactive CLI or executes a one-shot task and exits.
func Run(cfg Config) {
	// Apply the persisted egress-guardrail mode (and any TOLLECODE_EGRESS override)
	// before any request — the CLI doesn't go through the stdio state constructor
	// where the desktop/web modes do this.
	ai.SyncEgressFromSettings()
	// Route findings through the loader-safe, deduping sink — the ai default
	// writes raw stderr, which corrupts the status line (see egresssink.go).
	installEgressSink()

	// Auto-connect locally-running MCP backends (e.g. Blender, Unity) for the interactive
	// REPL, one-shot --task, and `launch` — the same as Lite. Server modes
	// (serve/selfhost) never call cli.Run, so they don't probe localhost.
	mcp.EnableAutoDiscovery = true

	ws := resolveWorkspace(cfg.Workspace)
	mode := cfg.Mode
	if mode == "" {
		mode = "build"
	}

	if cfg.Task != "" {
		runOneShot(ws, mode, cfg)
		return
	}

	repl := NewTolleREPL(ws, mode, cfg.ThinkingBudget, cfg.ProviderID, cfg.Model)
	repl.presetSessionID = cfg.SessionID
	// Apply pre-selected agent/team from flags.
	if sel := resolveAgentArg(cfg.AgentArg, cfg.TeamArg); sel != nil {
		repl.applyAgentSelection(sel)
	}
	if err := repl.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "tollecode: %v\n", err)
		os.Exit(1)
	}
}

// runOneShot executes one task non-interactively and exits.
func runOneShot(ws, mode string, cfg Config) {
	ai.Global.Reload()
	ids := ai.Global.IDs()
	if len(ids) == 0 {
		fmt.Fprintf(os.Stderr, "tollecode: no providers configured — run tollecode configure\n")
		os.Exit(1)
	}

	pid := cfg.ProviderID
	if pid == "" {
		pid = ids[0]
	}
	mdl := cfg.Model
	if mdl == "" {
		mdl = ai.Global.DefaultModel(pid)
	}

	// Resolve session — resume existing or create new
	var sess session.APISession
	if cfg.SessionID != "" {
		if loaded := findSessionByPrefix(ws, cfg.SessionID); loaded != nil {
			sess = *loaded
		}
	}
	if sess.ID == "" {
		var err error
		sess, err = session.Create(ws, pid, mdl, mode, session.WithRole(""))
		if err != nil {
			fmt.Fprintf(os.Stderr, "tollecode: create session: %v\n", err)
			os.Exit(1)
		}
	}

	sessions, _ := session.List(ws)
	PrintIntro(ws, len(sessions))
	PrintReady(pid, mdl, mode)

	renderer := NewStreamRenderer()
	renderer.SetSession(ws, sess.ID)
	renderer.StartLoader(thinkingBudgetLabel(cfg.ThinkingBudget))

	emitFn := func(m map[string]any) { renderer.HandleEvent(m) }
	requestPerm := func(_ context.Context, command string) (bool, bool) {
		renderer.loader.pause()
		fmt.Printf(
			"\n  %s%s▶  Shell command%s\n  %s$ %s%s\n\n",
			colorPrimary, ansiBold, ansiReset,
			ansiDim, command, ansiReset,
		)
		switch runChoicePrompt("Do you want to proceed?", permissionChoices, 0) {
		case 0:
			fmt.Printf("  %s✓ Allowed%s\n\n", colorGreen, ansiReset)
			return true, false
		case 1:
			fmt.Printf("  %s✓ Always allowed this session%s\n\n", colorGreen, ansiReset)
			return true, true
		default: // "No" or cancelled (Esc/Ctrl-C)
			fmt.Printf("  %s✗ Denied%s\n\n", ansiDim+colorRed, ansiReset)
			return false, false
		}
	}

	requestClarification := func(_ context.Context, question string, suggestions []string, _ bool) (agent.ClarificationAnswer, bool) {
		renderer.loader.pause()
		fmt.Printf(
			"\n  %s%s?  Clarification needed%s\n  %s%s%s\n\n",
			colorPrimary, ansiBold, ansiReset,
			ansiBold, question, ansiReset,
		)
		if len(suggestions) > 0 {
			options := append(append([]string{}, suggestions...), "Type a different answer…")
			idx := runChoicePrompt("", options, 0)
			if idx < 0 {
				return agent.ClarificationAnswer{}, false
			}
			if idx < len(suggestions) {
				fmt.Printf("  %s❯ %s%s\n\n", colorPrimary, suggestions[idx], ansiReset)
				return agent.ClarificationAnswer{Selected: []string{suggestions[idx]}}, true
			}
			// "Type a different answer…" → free-text below.
		}
		ans := readFreeText(colorPrimary)
		if ans == "" {
			return agent.ClarificationAnswer{}, false
		}
		fmt.Println()
		return agent.ClarificationAnswer{Selected: []string{ans}}, true
	}

	// Resolve agent/team selection for one-shot mode. resolveAgentExec yields the
	// agent identity + skills (single agent) or the lead identity + orchestration
	// roster (team), matching the interactive REPL path.
	var agentName, customInstructions string
	var teamMemberIDs, agentSkills []string
	var overrideModel string
	if sel := resolveAgentArg(cfg.AgentArg, cfg.TeamArg); sel != nil {
		agentName, customInstructions, agentSkills, teamMemberIDs, overrideModel = sel.resolveAgentExec()
	}

	// Merge any explicitly-requested skills (cloud mode) with the agent's own skills.
	activeSkills := append(append([]string{}, cfg.Skills...), agentSkills...)
	if len(activeSkills) > 0 {
		if _, err := session.UpdateFields(ws, sess.ID, map[string]any{"activeSkills": activeSkills}); err != nil {
			fmt.Fprintf(os.Stderr, "tollecode: activate skills: %v\n", err)
		}
	}

	agent.Execute(context.Background(), agent.Config{
		SessionID:            sess.ID,
		Workspace:            ws,
		Message:              cfg.Task,
		Mode:                 mode,
		ThinkingBudget:       cfg.ThinkingBudget,
		EmitFn:               emitFn,
		RequestPerm:          requestPerm,
		RequestClarification: requestClarification,
		AgentName:            agentName,
		CustomInstructions:   customInstructions,
		TeamMemberIDs:        teamMemberIDs,
		OverrideModel:        overrideModel,
		// Browser tools available in one-shot CLI runs too — same local-display
		// rationale as the interactive REPL.
		BrowserAvailable: true,
	})

	renderer.StopLoader()
}

func resolveWorkspace(ws string) string {
	if ws == "" {
		var err error
		ws, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "tollecode: %v\n", err)
			os.Exit(1)
		}
	}
	ws = filepath.Clean(ws)
	if info, err := os.Stat(ws); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "tollecode: workspace not found: %s\n", ws)
		os.Exit(1)
	}
	return ws
}

func findSessionByPrefix(ws, prefix string) *session.APISession {
	sessions, _ := session.List(ws)
	for _, s := range sessions {
		if len(s.ID) >= len(prefix) && s.ID[:len(prefix)] == prefix {
			loaded, err := session.Load(ws, s.ID)
			if err == nil {
				return loaded
			}
		}
	}
	return nil
}

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/chzyer/readline"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/session"
)

var thinkingPresets = map[string]int{
	"0": 0, "off": 0,
	"1k": 1024, "4k": 4096,
	"10k": 10000, "32k": 32000,
}

// thinkingBudgetLabel renders a turn's thinking budget for the loader's hint
// parenthetical (e.g. "thinking 32k"), or "" when thinking is off.
func thinkingBudgetLabel(budget int) string {
	if budget <= 0 {
		return ""
	}
	for k, v := range thinkingPresets {
		if v == budget && k != "0" && k != "off" {
			return "thinking " + k
		}
	}
	return fmt.Sprintf("thinking %d", budget)
}

// TolleREPL is the interactive CLI loop.
type TolleREPL struct {
	workspace       string
	mode            string
	thinkingBudget  int
	providerID      string
	model           string
	sess            *session.APISession
	renderer        *StreamRenderer
	running         bool
	rl              *readline.Instance
	paste           *bracketedPasteReader // stdin wrapper backing r.rl; holds stashed pastes
	composer        *composer             // pinned bottom composer (nil-safe; no-op off a TTY)
	presetSessionID string                // resume this session ID prefix on startup

	// Active agent/team selection (set via % picker or /agent command).
	// agentName / agentSystemPrompt / agentSkills hold the RESOLVED executor identity:
	// for a single agent they are the agent's own; for a team they are the lead agent's
	// identity plus the orchestration roster. teamMemberIDs is set only for teams.
	agentID           string
	agentName         string
	agentSystemPrompt string
	agentSkills       []string
	agentModel        string // model override from agent config
	teamID            string
	teamName          string
	teamMemberIDs     []string
}

func NewTolleREPL(workspace, mode string, thinkingBudget int, providerID, model string) *TolleREPL {
	return &TolleREPL{
		workspace:      workspace,
		mode:           mode,
		thinkingBudget: thinkingBudget,
		providerID:     providerID,
		model:          model,
		renderer:       NewStreamRenderer(),
	}
}

// Run starts the REPL. Blocks until the user exits.
func (r *TolleREPL) Run(ctx context.Context) error {
	sessions, _ := session.List(r.workspace)
	PrintIntro(r.workspace, len(sessions))

	// Provider + model selection
	if r.providerID == "" || r.model == "" {
		pid, mdl, err := r.selectProviderModel()
		if err != nil || pid == "" {
			fmt.Printf("  %s⚠  No providers configured. Add a provider in ~/.tollecode/config.json%s\n",
				colorRed, ansiReset)
			return nil
		}
		r.providerID = pid
		if r.model == "" {
			r.model = mdl
		}
	}

	// Resume existing session or create a new one
	if r.presetSessionID != "" {
		if loaded := findSessionByPrefix(r.workspace, r.presetSessionID); loaded != nil {
			r.sess = loaded
			r.model = loaded.Model
			r.mode = loaded.Mode
		}
	}
	if r.sess == nil {
		s, err := session.Create(r.workspace, r.providerID, r.model, r.mode, session.WithRole(""))
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		r.sess = &s
	}

	PrintReady(r.providerID, r.model, r.mode)

	// Pinned bottom composer: keeps the input row + hint row fixed at the
	// bottom of the terminal via a scroll region, with all output scrolling
	// above. Registered before readline's deferred Close so the region is
	// dropped last (defers run LIFO). No-op off a TTY / tiny terminals.
	r.composer = newComposer()
	r.composer.onIdleResize = func() {
		if rl := r.rl; rl != nil {
			rl.Refresh()
		}
	}
	r.composer.setup()
	defer r.composer.teardown()

	// Set up readline — creation is extracted to newReadline() so handleAtPicker
	// can close/recreate the instance around the raw-mode file picker without
	// readline's background goroutine competing for stdin bytes.
	r.rl = r.newReadline()
	if r.rl == nil {
		return fmt.Errorf("readline init failed")
	}
	defer r.closeReadline()
	r.running = true

	for r.running {
		// r.rl may have been replaced by handleAtPicker; recreate if nil.
		if r.rl == nil {
			r.rl = r.newReadline()
			if r.rl == nil {
				break
			}
		}
		activeSkills := []string{}
		if r.sess != nil {
			activeSkills = r.sess.ActiveSkills
		}
		pinned := r.composer.isActive()
		if pinned {
			// Refresh the pinned rows (mode/model/agent can change between
			// turns) and hand the input row to readline.
			r.composer.setHints(r.mode, r.model, activeSkills, r.activeAgentLabel())
			r.composer.beginIdleInput()
		} else {
			fmt.Println()
			PrintComposerFooter(r.model, r.mode, activeSkills, r.activeAgentLabel())
		}
		r.rl.SetPrompt(r.makePrompt())
		line, err := r.rl.Readline()
		if pinned {
			r.composer.endIdleInput()
		}
		if err != nil {
			// EOF (Ctrl-D), Ctrl-C, or a standalone Esc (mapped to interrupt).
			fmt.Printf("\n  %sBye.%s\n", ansiDim, ansiReset)
			break
		}
		// Echo the submitted line into the content flow before expanding paste
		// chips — the compact chip, not a whole pasted file, is what should
		// land in the scrollback. (The inline prompt used to leave this echo
		// behind by itself; the pinned input row is cleared on submit.)
		if pinned && strings.TrimSpace(line) != "" {
			echoUserLine(strings.TrimSpace(line))
		}
		// Swap any pasted-content placeholder chips back to the real text.
		if r.paste != nil {
			line = r.paste.expand(line)
		}
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		r.dispatch(ctx, text)

		// Messages typed into the composer and Enter-queued during the turn
		// auto-send in order (each may itself queue more while it runs).
		for r.running {
			msg, ok := r.composer.dequeue()
			if !ok {
				break
			}
			echoUserLine(msg)
			r.dispatch(ctx, msg)
		}
		// Unsubmitted during-turn text survives into the next prompt.
		if partial := r.composer.takeBuffer(); partial != "" && r.rl != nil {
			r.rl.WriteStdin([]byte(partial))
		}
	}
	return nil
}

// dispatch routes one line of user input — typed at the prompt or queued in
// the composer during a turn — to the matching handler.
func (r *TolleREPL) dispatch(ctx context.Context, text string) {
	if strings.HasPrefix(text, "/") {
		r.handleCommand(ctx, text)
	} else if text == "?" {
		// Matches the composer footer's "? for shortcuts" hint.
		PrintHelp()
	} else if r.isAtPickerTrigger(text) {
		r.handleAtPicker(text)
	} else if r.isAgentPickerTrigger(text) {
		r.handleAgentPicker(text)
	} else {
		r.send(ctx, text)
	}
}

// newReadline creates a fresh readline instance with the standard config.
func (r *TolleREPL) newReadline() *readline.Instance {
	histDir := os.Getenv("HOME") + "/.tollecode"
	_ = os.MkdirAll(histDir, 0o755)
	// Wrap stdin so multi-line pastes are captured whole (as a placeholder chip)
	// instead of the first embedded newline submitting the line (see paste.go).
	paste := newBracketedPasteReader(readline.Stdin)
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          r.makePrompt(),
		HistoryFile:     histDir + "/cli_history",
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		Stdin:           paste,
		// Bound readline's erase-to-end-of-screen so its refresh cycle can't
		// wipe the pinned hint row below the input row (see rlstdout.go).
		Stdout: newRLBoundedStdout(r.composer, os.Stdout),
	})
	if err != nil {
		return nil
	}
	r.paste = paste
	enableBracketedPaste()
	return rl
}

// closeReadline disables bracketed-paste mode and tears down the readline
// instance. Pair it with newReadline (which re-enables paste mode) around the
// raw-mode @/% pickers so their input isn't wrapped in paste markers.
func (r *TolleREPL) closeReadline() {
	if r.rl == nil {
		return
	}
	disableBracketedPaste()
	r.rl.Close()
	r.rl = nil
}

// rl wraps an ANSI sequence in readline's non-printable markers so readline
// correctly tracks the visible width of the prompt.
func rl(ansi string) string { return "\x01" + ansi + "\x02" }

func (r *TolleREPL) makePrompt() string {
	// The \n is printed outside the prompt string so readline doesn't count it.
	// Plan (read-only, "thinking") gets the brand accent; build (active,
	// executing) gets green — the same two colors used for mode everywhere else.
	if r.mode == "plan" {
		return "  " + rl(colorPrimary+ansiBold) + "❯" + rl(ansiReset) + " "
	}
	return "  " + rl(colorGreen+ansiBold) + "❯" + rl(ansiReset) + " "
}

func (r *TolleREPL) printPromptNewline() {
	fmt.Println()
}

// ── Provider / model selection ────────────────────────────────────────────────

func (r *TolleREPL) selectProviderModel() (pid, model string, err error) {
	ai.Global.Reload()
	ids := ai.Global.IDs()
	if len(ids) == 0 {
		return "", "", fmt.Errorf("no providers configured")
	}
	sort.Strings(ids)

	if len(ids) == 1 {
		pid = ids[0]
		fmt.Printf("  %sProvider:%s  %s\n", ansiDim, ansiReset, pid)
	} else {
		pid = r.promptProvider(ids)
		if pid == "" {
			return "", "", fmt.Errorf("no provider selected")
		}
	}

	models := r.modelsForProvider(pid)
	defaultModel := ai.Global.DefaultModel(pid)

	if len(models) == 0 {
		return pid, defaultModel, nil
	}
	if len(models) == 1 {
		fmt.Printf("  %sModel:%s     %s\n", ansiDim, ansiReset, models[0])
		return pid, models[0], nil
	}
	model = r.promptModel(pid, models, defaultModel)
	if model == "" {
		model = defaultModel
	}
	return pid, model, nil
}

func (r *TolleREPL) modelsForProvider(pid string) []string {
	cfg, ok := ai.Global.Config(pid)
	if !ok {
		return nil
	}
	var models []string
	for _, m := range cfg.Models {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		if name != "" {
			models = append(models, name)
		}
	}
	return models
}

// fetchLiveModels calls the provider API for the current list of models.
// Falls back to the config-defined models if the live fetch fails or returns nothing.
func (r *TolleREPL) fetchLiveModels(ctx context.Context) []string {
	adapter := ai.Global.Get(r.providerID)
	if adapter != nil {
		fmt.Printf("  %sFetching available models…%s", ansiDim, ansiReset)
		live, err := adapter.DiscoverModels(ctx)
		fmt.Printf("\r%s\r", strings.Repeat(" ", 40)) // clear the line
		if err == nil && len(live) > 0 {
			models := make([]string, len(live))
			for i, m := range live {
				models[i] = m.ID
			}
			return models
		}
		if err != nil {
			fmt.Printf("  %s⚠  Could not reach endpoint — using configured models.%s\n", ansiDim, ansiReset)
		}
	}
	return r.modelsForProvider(r.providerID)
}

func (r *TolleREPL) promptProvider(ids []string) string {
	options := make([]string, len(ids))
	for i, id := range ids {
		cfg, _ := ai.Global.Config(id)
		name := cfg.Name
		if name == "" {
			name = id
		}
		if typeLabel := providerTypeLabel(cfg.Type); typeLabel != "" {
			options[i] = fmt.Sprintf("%s  (%s)", name, typeLabel)
		} else {
			options[i] = name
		}
	}
	idx := runChoicePrompt("Select a provider", options, 0)
	if idx < 0 {
		return ids[0] // cancelled → default to the first provider
	}
	return ids[idx]
}

func (r *TolleREPL) promptModel(pid string, models []string, defaultModel string) string {
	options := make([]string, len(models))
	defaultIdx := 0
	for i, m := range models {
		if m == defaultModel {
			defaultIdx = i
			options[i] = m + "  (default)"
		} else {
			options[i] = m
		}
	}
	idx := runChoicePrompt("Select a model · "+pid, options, defaultIdx)
	if idx < 0 {
		return defaultModel // cancelled → keep the default
	}
	return models[idx]
}

// ── @ file picker ─────────────────────────────────────────────────────────────

// isAtPickerTrigger returns true when the user's entire input is "@" or "@query"
// (no spaces). That signals they want the interactive file picker, not a send.
func (r *TolleREPL) isAtPickerTrigger(text string) bool {
	return strings.HasPrefix(text, "@") && !strings.Contains(text[1:], " ")
}

// handleAtPicker opens the interactive file picker. If the user selects a file,
// its path is pre-filled into the next readline prompt as "@path " so they can
// append their question before sending.
//
// readline runs a background goroutine that holds exclusive raw-mode control of
// stdin. We must close it before calling RunFilePicker (which also needs raw
// stdin), then recreate it afterward — otherwise readline's goroutine silently
// consumes every keypress the picker is waiting for.
func (r *TolleREPL) handleAtPicker(text string) {
	query := strings.TrimPrefix(text, "@")

	// Release readline's stdin/terminal lock (also disables paste mode).
	r.closeReadline()

	selected := RunFilePicker(r.workspace, query)

	// Recreate readline so the main loop can continue normally.
	r.rl = r.newReadline()

	if selected == "" {
		return
	}
	fmt.Printf("  %s○  @%s%s\n", ansiDim, selected, ansiReset)
	// Pre-fill the next readline prompt; WriteStdin is consumed before actual stdin.
	// Paths containing spaces are quoted so expandAtRefs can still resolve them.
	ref := "@" + selected
	if strings.Contains(selected, " ") {
		ref = `@"` + selected + `"`
	}
	if r.rl != nil {
		r.rl.WriteStdin([]byte(ref + " "))
	}
}

// ── % agent/team picker ───────────────────────────────────────────────────────

// isAgentPickerTrigger returns true when the entire input is "%" or "%query"
// (no spaces), signalling that the user wants the interactive agent/team picker.
func (r *TolleREPL) isAgentPickerTrigger(text string) bool {
	return strings.HasPrefix(text, "%") && !strings.Contains(text[1:], " ")
}

// handleAgentPicker opens the interactive agent/team picker.
// Same close-readline / reopen pattern as handleAtPicker.
func (r *TolleREPL) handleAgentPicker(text string) {
	query := strings.TrimPrefix(text, "%")

	// Give explicit feedback when there is nothing to pick, so "%" never looks
	// like a silent no-op (the picker itself returns nil on an empty list).
	if len(agentPickerCollect()) == 0 {
		fmt.Printf("  %s◎  No agents or teams configured yet — create one in the desktop app.%s\n",
			ansiDim, ansiReset)
		return
	}

	r.closeReadline()

	result := RunAgentPicker(query)

	r.rl = r.newReadline()

	if result == nil {
		return
	}
	r.applyAgentSelection(result)
}

// applyAgentSelection stores the picked agent/team on the REPL and prints confirmation.
// Both single agents and teams resolve through resolveAgentExec so the executor always
// receives the agent identity + skills (single) or the lead identity + orchestration
// roster (team).
func (r *TolleREPL) applyAgentSelection(res *AgentPickerResult) {
	agentName, customInstructions, skills, teamMemberIDs, model := res.resolveAgentExec()
	r.agentName = agentName
	r.agentSystemPrompt = customInstructions
	r.agentSkills = skills
	r.agentModel = model
	r.teamMemberIDs = teamMemberIDs

	if res.Kind == "team" {
		r.agentID = ""
		r.teamID = res.ID
		r.teamName = res.Name
		fmt.Printf("  %s◎  Team → %s%s%s  %s(lead: %s, %d members)%s\n",
			ansiDim, colorPrimary, res.Name, ansiReset,
			ansiDim, orDash(agentName), len(teamMemberIDs), ansiReset)
	} else {
		r.agentID = res.ID
		r.teamID = ""
		r.teamName = ""
		modelNote := ""
		if model != "" {
			modelNote = fmt.Sprintf("  %s(model: %s)%s", ansiDim, model, ansiReset)
		}
		fmt.Printf("  %s◎  Agent → %s%s%s%s\n",
			ansiDim, colorPrimary, res.Name, ansiReset, modelNote)
	}
}

// orDash returns s, or "—" when s is empty (used for optional display fields).
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// clearAgentSelection resets agent/team state back to general.
func (r *TolleREPL) clearAgentSelection() {
	r.agentID = ""
	r.agentName = ""
	r.agentSystemPrompt = ""
	r.agentSkills = nil
	r.agentModel = ""
	r.teamID = ""
	r.teamName = ""
	r.teamMemberIDs = nil
	fmt.Printf("  %s◎  Agent → General%s\n", ansiDim, ansiReset)
}

// activeAgentLabel returns a short label for the toolbar (empty string = general).
func (r *TolleREPL) activeAgentLabel() string {
	if r.teamName != "" {
		return r.teamName
	}
	return r.agentName
}

// ── Slash command handling ────────────────────────────────────────────────────

func (r *TolleREPL) handleCommand(ctx context.Context, raw string) {
	parts := strings.SplitN(strings.TrimPrefix(raw, "/"), " ", 2)
	cmd := strings.ToLower(parts[0])
	arg := ""
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}

	switch cmd {
	case "exit", "quit", "q":
		fmt.Printf("  %sBye.%s\n", ansiDim, ansiReset)
		r.running = false

	case "help":
		PrintHelp()

	case "clear":
		// The full-screen clear wipes the pinned rows too; the scroll region
		// itself stays programmed and the home position is inside it, so the
		// banners land in the content area — then repaint the pinned rows.
		fmt.Print("\033[2J\033[H")
		PrintIntro(r.workspace, 0)
		PrintReady(r.providerID, r.model, r.mode)
		r.composer.redraw()

	case "mode":
		if arg == "plan" || arg == "build" {
			r.mode = arg
			if r.sess != nil {
				session.UpdateFields(r.workspace, r.sess.ID, map[string]any{"mode": arg})
				r.sess.Mode = arg
			}
			modeColor := colorGreen
			if arg == "plan" {
				modeColor = colorPrimary
			}
			fmt.Printf("  %s%sMode → %s%s\n", ansiBold, modeColor, strings.ToUpper(arg), ansiReset)
		} else if arg != "" {
			fmt.Printf("  %sUnknown mode '%s'. Use plan or build.%s\n", colorRed, arg, ansiReset)
		} else {
			modeColor := colorGreen
			if r.mode == "plan" {
				modeColor = colorPrimary
			}
			fmt.Printf("  %s%sCurrent mode: %s%s\n", ansiBold, modeColor, strings.ToUpper(r.mode), ansiReset)
		}

	case "model":
		if arg != "" {
			r.model = arg
			if r.sess != nil {
				session.UpdateFields(r.workspace, r.sess.ID, map[string]any{"model": arg})
				r.sess.Model = arg
			}
			fmt.Printf("  Model → %s\n", arg)
		} else {
			models := r.fetchLiveModels(ctx)
			if len(models) == 0 {
				fmt.Printf("  Current model: %s\n", r.model)
				return
			}
			newModel := r.promptModel(r.providerID, models, r.model)
			if newModel != "" && newModel != r.model {
				r.model = newModel
				if r.sess != nil {
					session.UpdateFields(r.workspace, r.sess.ID, map[string]any{"model": newModel})
					r.sess.Model = newModel
				}
				fmt.Printf("  Model → %s\n", newModel)
			}
		}

	case "provider":
		ai.Global.Reload()
		ids := ai.Global.IDs()
		sort.Strings(ids)
		if len(ids) == 0 {
			fmt.Printf("  %sNo providers configured.%s\n", ansiDim, ansiReset)
			return
		}
		newPID := r.promptProvider(ids)
		if newPID == "" {
			return
		}
		models := r.modelsForProvider(newPID)
		newModel := ai.Global.DefaultModel(newPID)
		if len(models) > 1 {
			newModel = r.promptModel(newPID, models, newModel)
		}
		r.providerID = newPID
		r.model = newModel
		if r.sess != nil {
			session.UpdateFields(r.workspace, r.sess.ID, map[string]any{
				"provider": newPID, "model": newModel,
			})
			r.sess.Provider = newPID
			r.sess.Model = newModel
		}
		fmt.Printf("  Provider → %s  ·  %s\n", newPID, newModel)

	case "thinking":
		if budget, ok := thinkingPresets[strings.ToLower(arg)]; ok {
			r.thinkingBudget = budget
			label := arg
			if arg == "0" || arg == "" {
				label = "off"
			}
			fmt.Printf("  %sThinking budget → %s%s\n", ansiDim, label, ansiReset)
		} else if arg != "" {
			keys := make([]string, 0)
			for k := range thinkingPresets {
				if k != "off" {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			fmt.Printf("  %sOptions: %s%s\n", colorRed, strings.Join(keys, ", "), ansiReset)
		} else {
			current := "off"
			for k, v := range thinkingPresets {
				if v == r.thinkingBudget && k != "0" && k != "off" {
					current = k
					break
				}
			}
			fmt.Printf("  %sThinking budget: %s%s\n", ansiDim, current, ansiReset)
		}

	case "new":
		s, err := session.Create(r.workspace, r.providerID, r.model, r.mode, session.WithRole(""))
		if err != nil {
			fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
			return
		}
		r.sess = &s
		fmt.Printf("  %sNew session: %s…%s\n", ansiDim, s.ID[:8], ansiReset)

	case "sessions":
		r.printSessions()

	case "session":
		if arg == "" {
			if r.sess != nil {
				fmt.Printf("  %sCurrent session: %s%s\n", ansiDim, r.sess.ID, ansiReset)
			}
		} else {
			r.switchSession(arg)
		}

	case "memory":
		// A free-text argument ("/memory what did we ship yesterday") is a
		// natural-language question — answer it with a spoken status summary
		// over the memories in the requested date range. Structured
		// subcommands (list/view/search/…) still go to handleMemoryCmd.
		if isMemoryQuery(arg) {
			r.summarizeMemory(ctx, arg)
		} else {
			handleMemoryCmd(r.workspace, arg)
		}

	case "skill":
		sessID := ""
		if r.sess != nil {
			sessID = r.sess.ID
		}
		handleSkillCmd(r.workspace, sessID, arg)

	case "agents":
		// Interactive agent manager (create / edit / assign skills / delete). Uses
		// raw-mode pickers, so tear down readline for the duration and rebuild after.
		r.closeReadline()
		r.manageAgents()
		r.rl = r.newReadline()

	case "teams":
		r.closeReadline()
		r.manageTeams()
		r.rl = r.newReadline()

	case "skills":
		r.closeReadline()
		r.manageSkills()
		r.rl = r.newReadline()

	case "screen":
		if arg == "" {
			fmt.Printf("  %sUsage: /screen <task description>%s\n", colorRed, ansiReset)
			fmt.Printf("  %sExample: /screen click the red button in the top right%s\n", ansiDim, ansiReset)
		} else {
			r.runScreenTask(ctx, arg)
		}

	case "todo", "todos":
		if r.sess == nil {
			fmt.Printf("  %sNo active session.%s\n", ansiDim, ansiReset)
		} else {
			printTodos(r.workspace, r.sess.ID)
		}

	case "usage":
		printUsage(r.workspace)

	case "configure", "config":
		handleConfigure(r.workspace)
		ai.Global.Reload()

	case "settings":
		handleConfigureSettings()

	case "agent":
		if arg == "" || arg == "%" {
			// Open interactive picker (same close/reopen pattern as handleAtPicker).
			r.closeReadline()
			result := RunAgentPicker("")
			r.rl = r.newReadline()
			if result != nil {
				r.applyAgentSelection(result)
			}
		} else if arg == "clear" || arg == "reset" || arg == "general" {
			r.clearAgentSelection()
		} else {
			// Match by name prefix or ID from the loaded entries.
			entries := agentPickerCollect()
			matched := agentPickerFilter(entries, arg)
			if len(matched) == 0 {
				fmt.Printf("  %sNo agent or team matching %q%s\n", colorRed, arg, ansiReset)
			} else {
				r.applyAgentSelection(pickerEntryToResult(matched[0]))
			}
		}

	default:
		fmt.Printf("  %sUnknown command /%s. Type /help for options.%s\n",
			colorRed, cmd, ansiReset)
	}
}

// ── Sessions ──────────────────────────────────────────────────────────────────

func (r *TolleREPL) printSessions() {
	sessions, err := session.List(r.workspace)
	if err != nil || len(sessions) == 0 {
		fmt.Printf("  %sNo sessions yet.%s\n", ansiDim, ansiReset)
		return
	}

	fmt.Println()
	fmt.Printf("  %s%s%-10s  %-45s  %-28s  %-5s  %s%s\n",
		ansiBold, colorPrimary, "ID", "Title", "Model", "Mode", "Updated", ansiReset)
	fmt.Println(drawRule())

	limit := 20
	if len(sessions) < limit {
		limit = len(sessions)
	}
	for _, s := range sessions[:limit] {
		marker := ""
		if r.sess != nil && s.ID == r.sess.ID {
			marker = " ←"
		}
		modeColor := colorGreen
		if s.Mode == "plan" {
			modeColor = colorPrimary
		}
		title := "—"
		if s.Title != nil && *s.Title != "" {
			title = *s.Title
		}
		if len(title) > 44 {
			title = title[:44]
		}
		updated := ""
		if s.UpdatedAt != "" && len(s.UpdatedAt) >= 16 {
			updated = strings.Replace(s.UpdatedAt[:16], "T", " ", 1)
		}
		fmt.Printf("  %s%-10s%s  %-45s  %s%-28s%s  %s%s%s  %s%s\n",
			ansiDim, s.ID[:8]+marker, ansiReset,
			title,
			ansiDim, s.Model, ansiReset,
			ansiBold+modeColor, strings.ToUpper(s.Mode), ansiReset,
			ansiDim, updated+ansiReset)
	}
	fmt.Println()
}

func (r *TolleREPL) switchSession(prefix string) {
	sessions, _ := session.List(r.workspace)
	var matches []session.APISession
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, prefix) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		fmt.Printf("  %sNo session matching '%s'.%s\n", colorRed, prefix, ansiReset)
		return
	}
	if len(matches) > 1 {
		fmt.Printf("  %sAmbiguous prefix — %d sessions match.%s\n", colorRed, len(matches), ansiReset)
		return
	}
	loaded, err := session.Load(r.workspace, matches[0].ID)
	if err != nil {
		fmt.Printf("  %s%s✗  Could not load session.%s\n", ansiBold, colorRed, ansiReset)
		return
	}
	r.sess = loaded
	r.model = loaded.Model
	r.mode = loaded.Mode
	msgCount := len(loaded.Messages)
	fmt.Printf("  %sResumed %s…  %d messages%s\n",
		ansiDim, loaded.ID[:8], msgCount, ansiReset)

	// Show any sub-agent history for this session
	var subagents []map[string]any
	for _, msg := range loaded.Messages {
		if role, _ := msg["role"].(string); role == "subagent" {
			subagents = append(subagents, msg)
		}
	}
	PrintSubagentCards(subagents)
}

// ── Message send ──────────────────────────────────────────────────────────────

func (r *TolleREPL) send(ctx context.Context, message string) {
	if r.sess == nil {
		fmt.Printf("  %s%s✗  No active session.%s\n", ansiBold, colorRed, ansiReset)
		return
	}

	// Expand @path references — read file/dir content and prepend to message.
	if expanded, resolved := expandAtRefs(r.workspace, message); len(resolved) > 0 {
		for _, p := range resolved {
			fmt.Printf("  %s○  @%s%s\n", ansiDim, p, ansiReset)
		}
		message = expanded
	}

	r.renderer.Reset()
	r.renderer.SetSession(r.workspace, r.sess.ID)
	r.renderer.StartLoader(thinkingBudgetLabel(r.thinkingBudget))

	requestPerm := func(_ context.Context, command string) (allow, allowAll bool) {
		r.renderer.loader.pause()
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
		r.renderer.loader.pause()
		fmt.Printf(
			"\n  %s%s?  Clarification needed%s\n  %s%s%s\n\n",
			colorPrimary, ansiBold, ansiReset,
			ansiBold, question, ansiReset,
		)
		if len(suggestions) > 0 {
			// Arrow-navigable list; a trailing row drops to free-text input.
			options := append(append([]string{}, suggestions...), "Type a different answer…")
			idx := runChoicePrompt("", options, 0)
			if idx < 0 {
				fmt.Printf("  %sSkipping clarification.%s\n\n", ansiDim, ansiReset)
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
			fmt.Printf("  %sSkipping clarification.%s\n\n", ansiDim, ansiReset)
			return agent.ClarificationAnswer{}, false
		}
		fmt.Println()
		return agent.ClarificationAnswer{Selected: []string{ans}}, true
	}

	emitFn := func(m map[string]any) {
		r.renderer.HandleEvent(m)
	}

	// Resolve model: agent-config override → session model.
	model := r.model
	if r.agentModel != "" {
		model = r.agentModel
	}

	// Activate the selected agent's (or team lead's) skills on the session so the
	// executor injects them into the system prompt — same mechanism the desktop uses.
	if len(r.agentSkills) > 0 {
		session.UpdateFields(r.workspace, r.sess.ID, map[string]any{"activeSkills": r.agentSkills})
	}

	// Make Ctrl-C cancel just this turn — stopping the agent loop and killing any
	// running shell command via the cancellable context — instead of doing
	// nothing (or killing the whole CLI). A second Ctrl-C force-quits. Esc also
	// cancels the turn, via the cbreak key watcher.
	turnCtx, cancel := context.WithCancel(ctx)
	stopInterrupt := installTurnInterrupt(cancel, r.renderer.loader)
	keys := startKeyWatch(cancel, r.composer)

	agent.Execute(turnCtx, agent.Config{
		SessionID:            r.sess.ID,
		Workspace:            r.workspace,
		Message:              message,
		Mode:                 r.mode,
		ThinkingBudget:       r.thinkingBudget,
		EmitFn:               emitFn,
		RequestPerm:          requestPerm,
		RequestClarification: requestClarification,
		AgentName:            r.agentName,
		CustomInstructions:   r.agentSystemPrompt,
		TeamMemberIDs:        r.teamMemberIDs,
		OverrideModel:        model,
		// Browser tools (chromedp) available in the interactive CLI — it runs
		// locally where a display is present, so the agent can drive a real
		// browser to test what it builds.
		BrowserAvailable: true,
	})

	keys.stop()
	stopInterrupt()
	cancel()
	r.renderer.StopLoader()
	printTurnDuration(r.renderer.loader.elapsed())
}

// printTurnDuration reports how long the turn ran, printed into the content
// flow once the loader's status line is gone (covers done/cancelled/error
// turns uniformly since it runs after Execute returns).
func printTurnDuration(d time.Duration) {
	if d <= 0 {
		return
	}
	fmt.Printf("  %s✻ Toiled for %s%s\n", ansiDim, formatLoaderElapsed(d), ansiReset)
}

// installTurnInterrupt routes SIGINT (Ctrl-C) to cancel the current agent turn
// rather than terminating the process. The first Ctrl-C cancels; a second one
// force-quits so the user is never stuck if cancellation is slow. The returned
// stop func restores default signal handling and must be called when the turn
// ends.
func installTurnInterrupt(cancel context.CancelFunc, loader *gradientLoader) (stop func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	done := make(chan struct{})
	go func() {
		count := 0
		for {
			select {
			case <-sigCh:
				count++
				if count == 1 {
					loader.pause()
					fmt.Printf("\n  %s⎋ interrupting… (Ctrl-C again to force quit)%s\n", ansiDim, ansiReset)
					cancel()
				} else {
					restoreTerminalOnExit() // os.Exit skips deferred stop()
					os.Exit(130)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(done)
	}
}

// ── Screen control ────────────────────────────────────────────────────────────

// runScreenTask executes a desktop-control task in the current session.
// Shell commands are auto-allowed (user invoked /screen autonomously),
// and the native OS screenshot is used as the TakeScreenshot provider.
func (r *TolleREPL) runScreenTask(ctx context.Context, task string) {
	if r.sess == nil {
		fmt.Printf("  %s%s✗  No active session.%s\n", ansiBold, colorRed, ansiReset)
		return
	}

	fmt.Printf("  %s◉  Desktop control: %s%s\n", ansiDim, task, ansiReset)
	fmt.Printf("  %sShell commands auto-allowed. Press Ctrl-C to interrupt.%s\n\n", ansiDim, ansiReset)

	r.renderer.Reset()
	r.renderer.SetSession(r.workspace, r.sess.ID)
	r.renderer.StartLoader(thinkingBudgetLabel(r.thinkingBudget))

	emitFn := func(m map[string]any) {
		r.renderer.HandleEvent(m)
	}

	takeScreenshot := func(sctx context.Context) (map[string]any, error) {
		imgData, w, h, err := agent.NativeCaptureScreen()
		if err != nil {
			return nil, err
		}
		return map[string]any{"image": imgData, "width": w, "height": h}, nil
	}

	agent.Execute(ctx, agent.Config{
		SessionID:        r.sess.ID,
		Workspace:        r.workspace,
		Message:          task,
		Mode:             r.mode,
		ThinkingBudget:   r.thinkingBudget,
		EmitFn:           emitFn,
		DesktopPermitted: true,
		BrowserAvailable: true,
		ShellAutoAllow:   true,
		TakeScreenshot:   takeScreenshot,
	})

	r.renderer.StopLoader()
	printTurnDuration(r.renderer.loader.elapsed())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func providerTypeLabel(ptype string) string {
	labels := map[string]string{
		"anthropic":    "anthropic",
		"openai":       "openai",
		"ollama":       "ollama local",
		"ollama-cloud": "ollama cloud",
		"custom":       "custom",
	}
	if l, ok := labels[ptype]; ok {
		return l
	}
	return ptype
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/cli"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/httpserver"
	"github.com/tolle-ai/tollecode/internal/liteauth"
	"github.com/tolle-ai/tollecode/internal/selfhostgate"
	"github.com/tolle-ai/tollecode/internal/session"
	"github.com/tolle-ai/tollecode/internal/stdio"
	"github.com/tolle-ai/tollecode/internal/webmode"
)

var thinkingMap = map[string]int{
	"0": 0, "off": 0,
	"1k": 1024, "4k": 4096,
	"10k": 10000, "32k": 32000,
}

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// ── root (interactive REPL) ───────────────────────────────────────────────────

func rootCmd() *cobra.Command {
	var (
		workspace   string
		provider    string
		model       string
		mode        string
		thinking    string
		task        string
		sessionID   string
		agentArg    string
		teamArg     string
		devMode     bool
		stdioMode   bool
		showVersion bool
	)

	root := &cobra.Command{
		Use:          "tollecode",
		Short:        "TolleCode — AI coding assistant",
		SilenceUsage: true,
		// Runs before every subcommand (configure, launch, sessions, the REPL, …).
		// --dev must take effect first so we reconcile the right data dir; then we
		// pull in any providers the Lite desktop/web app configured into config.json
		// so the whole CLI shares the same providers as Lite.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if devMode {
				config.SetDevMode()
			}
			// Reconcile Lite config so it cuts across CLI / desktop / web.
			// Providers: config.json ↔ KV lite_providers. Teams: teams.json ↔
			// KV lite_teams. (Agents already cut across via the Lite app's
			// agents_list ↔ agents.json sync, and lite_agents is a keyed object
			// rather than an array, so it's intentionally not reconciled here.)
			ai.Global.SyncFromLiteKV()
			config.ReconcileKVArrayWithFile("lite_teams", filepath.Join(config.Home(), "teams.json"))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				fmt.Println("tollecode v" + cli.Version)
				return nil
			}
			if devMode {
				config.SetDevMode()
			}
			if stdioMode {
				stdio.Run()
				return nil
			}
			budget := thinkingMap[strings.ToLower(thinking)]
			cli.Run(cli.Config{
				Workspace:      workspace,
				Mode:           mode,
				ProviderID:     provider,
				Model:          model,
				Task:           task,
				SessionID:      sessionID,
				ThinkingBudget: budget,
				AgentArg:       agentArg,
				TeamArg:        teamArg,
			})
			return nil
		},
	}

	root.Flags().StringVarP(&workspace, "workspace", "w", "", "Workspace directory (default: cwd)")
	root.Flags().StringVarP(&provider, "provider", "p", "", "Provider ID from ~/.tollecode/config.json")
	root.Flags().StringVarP(&model, "model", "m", "", "Model name")
	root.Flags().StringVar(&mode, "mode", "build", "Agent mode: plan|build")
	root.Flags().StringVar(&thinking, "thinking", "0", "Thinking budget: 0|1k|4k|10k|32k")
	root.Flags().StringVarP(&task, "task", "t", "", "Run a single task and exit (non-interactive)")
	root.Flags().StringVarP(&sessionID, "session", "s", "", "Resume an existing session by ID prefix")
	root.Flags().StringVarP(&agentArg, "agent", "a", "", "Start with this agent selected (name or ID)")
	root.Flags().StringVar(&teamArg, "team", "", "Start with this team selected (name or ID)")
	root.Flags().BoolVar(&devMode, "dev", false, "Use ~/.tollecode-dev data directory")
	root.Flags().BoolVar(&stdioMode, "stdio", false, "Run as JSON-over-stdio server (Tauri IPC mode)")
	root.Flags().BoolVarP(&showVersion, "version", "v", false, "Show version and exit")

	root.AddCommand(launchCmd())
	root.AddCommand(sessionsCmd())
	root.AddCommand(configureCmd())
	// Agent settings are also a top-level command (`tollecode settings`), not just
	// `tollecode configure settings` — settings aren't provider config.
	root.AddCommand(configureSettingsCmd())
	root.AddCommand(uninstallCmd())
	root.AddCommand(serveCmd())
	root.AddCommand(webCmd())
	root.AddCommand(authCmd())
	root.AddCommand(cloudCommands()...)

	return root
}

// ── auth (local Lite 2FA account) ─────────────────────────────────────────────

func authCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage the local Lite 2FA account (TOTP) used by desktop + web",
	}
	cmd.AddCommand(authStatusCmd())
	cmd.AddCommand(authResetCmd())
	return cmd
}

func authStatusCmd() *cobra.Command {
	var devMode bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show where the account is stored and whether one exists",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if devMode {
				config.SetDevMode()
			}
			fmt.Printf("Store: %s\n", liteauth.StorePath())
			if u := liteauth.LocalUser(); u != nil {
				fmt.Printf("Account: %s <%s> (registered — login asks for the 6-digit code)\n", u.Name, u.Email)
			} else {
				fmt.Println("Account: none (next login starts a fresh registration)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&devMode, "dev", false, "Use ~/.tollecode-dev data directory")
	return cmd
}

func authResetCmd() *cobra.Command {
	var devMode bool
	cmd := &cobra.Command{
		Use:          "reset",
		Short:        "Delete the local account (TOTP secret + all sessions)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if devMode {
				config.SetDevMode()
			}
			removed, err := liteauth.Reset()
			if err != nil {
				return fmt.Errorf("reset auth: %w", err)
			}
			if removed {
				fmt.Printf("Cleared the account and all sessions at %s.\n", liteauth.StorePath())
				fmt.Println("The next login starts from registration. Restart `tollecode web` if it's running.")
			} else {
				fmt.Printf("No account file at %s (nothing to reset).\n", liteauth.StorePath())
				fmt.Println("If a browser still shows a registered account, the running server uses a different TOLLECODE_HOME or OS user — run this as that user / with that env.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&devMode, "dev", false, "Use ~/.tollecode-dev data directory")
	return cmd
}

// ── web ─────────────────────────────────────────────────────────────────────

func webCmd() *cobra.Command {
	var (
		port      int
		noOpen    bool
		devMode   bool
		resetAuth bool
	)

	cmd := &cobra.Command{
		Use:   "web",
		Short: "Run Tollecode Lite in the browser (no desktop app)",
		Long: `Run Tollecode Lite as a local web app.

Serves the Lite UI over HTTP and speaks the same command protocol the desktop
app uses — no Tauri window. Opens your browser automatically.

Examples:
  tollecode web
  tollecode web --port 5180
  tollecode web --no-open
  tollecode web --reset-auth   # forget the TOTP account, register fresh`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if devMode {
				config.SetDevMode()
				ai.Global.Reload()
			}

			if resetAuth {
				removed, err := liteauth.Reset()
				if err != nil {
					return fmt.Errorf("reset auth: %w", err)
				}
				if removed {
					fmt.Printf("[web] auth reset — cleared the account and all sessions at %s; the next login starts from registration\n", liteauth.StorePath())
				} else {
					fmt.Printf("[web] --reset-auth: no account file at %s (nothing to reset — check TOLLECODE_HOME / the user running the server)\n", liteauth.StorePath())
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				sig := make(chan os.Signal, 1)
				signal.Notify(sig, os.Interrupt)
				<-sig
				cancel()
			}()

			return webmode.Run(ctx, port, !noOpen)
		},
	}

	cmd.Flags().IntVar(&port, "port", 5180, "HTTP port to serve on")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Do not open the browser automatically")
	cmd.Flags().BoolVar(&devMode, "dev", false, "Use ~/.tollecode-dev data directory")
	cmd.Flags().BoolVar(&resetAuth, "reset-auth", false, "Clear the stored TOTP account + sessions before starting (fresh registration)")

	return cmd
}

// ── launch ────────────────────────────────────────────────────────────────────

func launchCmd() *cobra.Command {
	var (
		workspace string
		model     string
		mode      string
		thinking  string
		task      string
		sessionID string
	)

	cmd := &cobra.Command{
		Use:   "launch <provider>",
		Short: "Launch with a specific provider and model",
		Long: `Launch directly with a specific provider and model.

Examples:
  tollecode launch anthropic --model claude-sonnet-4-6
  tollecode launch openai --model gpt-4o
  tollecode launch ollama:local --model llama3
  tollecode launch ollama:cloud --model kimi-k2
  tollecode launch my-custom-id`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ai.Global.Reload()
			pid := resolveProviderSpec(args[0])
			if pid == "" {
				explainLaunchError(args[0])
				os.Exit(1)
			}
			budget := thinkingMap[strings.ToLower(thinking)]
			cli.Run(cli.Config{
				Workspace:      workspace,
				Mode:           mode,
				ProviderID:     pid,
				Model:          model,
				Task:           task,
				SessionID:      sessionID,
				ThinkingBudget: budget,
			})
			return nil
		},
	}

	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Workspace directory (default: cwd)")
	cmd.Flags().StringVarP(&model, "model", "m", "", "Model name")
	cmd.Flags().StringVar(&mode, "mode", "build", "Agent mode: plan|build")
	cmd.Flags().StringVar(&thinking, "thinking", "0", "Thinking budget: 0|1k|4k|10k|32k")
	cmd.Flags().StringVarP(&task, "task", "t", "", "Run a single task and exit")
	cmd.Flags().StringVarP(&sessionID, "session", "s", "", "Resume a session by ID prefix")

	return cmd
}

func resolveProviderSpec(spec string) string {
	specToType := map[string]string{
		"anthropic":    "anthropic",
		"openai":       "openai",
		"ollama":       "ollama",
		"ollama:local": "ollama",
		"ollama:cloud": "ollama-cloud",
		"custom":       "custom",
	}
	for _, id := range ai.Global.IDs() {
		if id == spec {
			return id
		}
	}
	targetType, ok := specToType[strings.ToLower(spec)]
	if !ok {
		return ""
	}
	ids := ai.Global.IDs()
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	for _, cfg := range loadRawProviderConfigs() {
		id, _ := cfg["id"].(string)
		t, _ := cfg["type"].(string)
		if t == targetType && idSet[id] {
			return id
		}
	}
	return ""
}

func explainLaunchError(spec string) {
	fmt.Printf("\n  \033[1;31m✗  No provider found for '%s'.\033[0m\n\n", spec)
	ids := ai.Global.IDs()
	if len(ids) == 0 {
		fmt.Println("  No providers configured. Run:")
		fmt.Println("  \033[1;35mtollecode configure\033[0m  to add one.")
		return
	}
	fmt.Println("  Configured providers:")
	for _, cfg := range loadRawProviderConfigs() {
		id, _ := cfg["id"].(string)
		name, _ := cfg["name"].(string)
		if name == "" {
			name = id
		}
		ptype, _ := cfg["type"].(string)
		hint := map[string]string{
			"anthropic":    "anthropic",
			"openai":       "openai",
			"ollama":       "ollama:local",
			"ollama-cloud": "ollama:cloud",
			"custom":       "custom",
		}[ptype]
		if hint == "" {
			hint = id
		}
		fmt.Printf("  \033[1;35mtollecode launch %s\033[0m  \033[2m← %s\033[0m\n", hint, name)
	}
	fmt.Println()
}

func loadRawProviderConfigs() []map[string]any {
	data, err := os.ReadFile(filepath.Join(config.Home(), "config.json"))
	if err != nil {
		return nil
	}
	var cfgs []map[string]any
	_ = json.Unmarshal(data, &cfgs)
	return cfgs
}

// ── sessions ──────────────────────────────────────────────────────────────────

func sessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "sessions",
		Short:        "Manage TolleCode sessions",
		SilenceUsage: true,
	}
	cmd.AddCommand(sessionsListCmd())
	cmd.AddCommand(sessionsShowCmd())
	cmd.AddCommand(sessionsDeleteCmd())
	return cmd
}

func sessionsListCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List all sessions in the workspace",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := resolveWS(workspace)
			sessions, _ := session.List(ws)
			if len(sessions) == 0 {
				fmt.Println("  \033[2mNo sessions found.\033[0m")
				return nil
			}
			fmt.Println()
			printSessionsTable(sessions, "")
			fmt.Printf("\n  \033[2m%d session(s)\033[0m\n\n", len(sessions))
			return nil
		},
	}
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Workspace directory (default: cwd)")
	return cmd
}

func sessionsShowCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:          "show <id>",
		Short:        "Show messages in a session",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := resolveWS(workspace)
			s := findSessionCLI(ws, args[0])
			if s == nil {
				os.Exit(1)
			}

			const (
				p3    = "\033[38;2;124;58;237m"
				p2    = "\033[38;2;168;85;247m"
				dim   = "\033[2m"
				reset = "\033[0m"
				bold  = "\033[1m"
				cyan  = "\033[38;2;125;211;252m"
			)

			fmt.Println()
			fmt.Printf("  %s%sSession %s%s\n", bold, p3, s.ID[:8], reset)
			fmt.Printf("  %s%s%s\n", p3+dim, strings.Repeat("─", 76), reset)
			fmt.Printf("  %smodel:%s    %s\n", dim, reset, s.Model)
			fmt.Printf("  %sprovider:%s %s\n", dim, reset, s.Provider)
			fmt.Printf("  %smode:%s     %s\n", dim, reset, s.Mode)
			created := s.CreatedAt
			if len(created) > 19 {
				created = created[:19]
			}
			fmt.Printf("  %screated:%s  %s\n\n", dim, reset, strings.Replace(created, "T", " ", 1))

			for _, msg := range s.Messages {
				role, _ := msg["role"].(string)
				content, _ := msg["content"].(string)
				content = strings.TrimSpace(content)
				if content == "" {
					continue
				}
				if role == "user" {
					fmt.Printf("  %s%sYou%s\n", bold, cyan, reset)
				} else {
					fmt.Printf("  %s%sTolleCode%s\n", bold, p2, reset)
				}
				if len(content) > 500 {
					content = content[:500] + "…"
				}
				fmt.Printf("  %s\n\n", content)
			}

			var subagents []map[string]any
			for _, msg := range s.Messages {
				if r, _ := msg["role"].(string); r == "subagent" {
					subagents = append(subagents, msg)
				}
			}
			cli.PrintSubagentCards(subagents)
			return nil
		},
	}
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "")
	return cmd
}

func sessionsDeleteCmd() *cobra.Command {
	var (
		workspace string
		yes       bool
	)
	cmd := &cobra.Command{
		Use:          "delete <id>",
		Short:        "Delete a session",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := resolveWS(workspace)
			s := findSessionCLI(ws, args[0])
			if s == nil {
				os.Exit(1)
			}
			title := "—"
			if s.Title != nil && *s.Title != "" {
				title = *s.Title
			}
			if !yes {
				fmt.Printf("  Delete session %s  '%s'? (y/N): ", s.ID[:8], title)
				var ans string
				fmt.Scan(&ans)
				if strings.ToLower(strings.TrimSpace(ans)) != "y" {
					fmt.Println("  \033[2mAborted.\033[0m")
					return nil
				}
			}
			if session.Delete(ws, s.ID) {
				fmt.Printf("  \033[2mDeleted session %s.\033[0m\n", s.ID[:8])
			} else {
				fmt.Println("  \033[31mDelete failed.\033[0m")
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	return cmd
}

// ── configure ─────────────────────────────────────────────────────────────────

func configureCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "configure",
		Short:        "Add or manage AI providers",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cli.RunConfigure("")
			return nil
		},
	}
	cmd.AddCommand(configureListCmd())
	cmd.AddCommand(configureAddCmd())
	cmd.AddCommand(configureRemoveCmd())
	cmd.AddCommand(configureSetKeyCmd())
	cmd.AddCommand(configureSettingsCmd())
	return cmd
}

func configureListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "List configured providers",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ai.Global.Reload()
			ids := ai.Global.IDs()
			if len(ids) == 0 {
				fmt.Println("  \033[2mNo providers configured. Run: tollecode configure\033[0m")
				return nil
			}
			sort.Strings(ids)
			fmt.Println()
			fmt.Printf("  \033[1;38;2;124;58;237m%-14s  %-22s  %-14s  %s\033[0m\n",
				"ID", "Name", "Type", "Status")
			fmt.Printf("  \033[38;2;91;33;182m\033[2m%s\033[0m\n", strings.Repeat("─", 72))
			for _, id := range ids {
				cfg, _ := ai.Global.Config(id)
				status := "enabled"
				if !cfg.Enabled {
					status = "\033[2mdisabled\033[0m"
				}
				idStr := id
				if len(idStr) > 13 {
					idStr = idStr[:13]
				}
				nameStr := cfg.Name
				if len(nameStr) > 22 {
					nameStr = nameStr[:22]
				}
				fmt.Printf("  %-14s  %-22s  \033[38;2;168;85;247m%-14s\033[0m  %s\n",
					idStr, nameStr, cfg.Type, status)
			}
			fmt.Println()
			return nil
		},
	}
}

func configureAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "add",
		Short:        "Add a new provider interactively",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cli.RunConfigureAdd()
			return nil
		},
	}
}

func configureRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "remove [id]",
		Short:        "Remove a provider",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				cli.RunConfigureRemoveByID(args[0])
			} else {
				cli.RunConfigureRemoveInteractive()
			}
			return nil
		},
	}
}

func configureSetKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "set-key <provider-id> <api-key>",
		Short:        "Update the API key for a provider",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cli.RunConfigureSetKey(args[0], args[1])
			return nil
		},
	}
}

func configureSettingsCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "settings",
		Short:        "View or change agent settings (max iterations, confirm-continue, egress guardrail)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cli.RunConfigureSettings()
			return nil
		},
	}
}

// ── uninstall ─────────────────────────────────────────────────────────────────

func uninstallCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:          "uninstall",
		Short:        "Remove the tollecode binary and optionally its data directory",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			const (
				bold  = "\033[1m"
				red   = "\033[1;31m"
				dim   = "\033[2m"
				reset = "\033[0m"
			)

			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("could not locate binary: %w", err)
			}

			dataDir := config.Home()

			fmt.Println()
			fmt.Printf("  %sThis will remove:%s\n", bold, reset)
			fmt.Printf("  %s  binary:  %s%s\n", dim, reset, exe)
			fmt.Printf("  %s  data:    %s%s  %s(sessions, config, memory)%s\n", dim, reset, dataDir, dim, reset)
			fmt.Println()

			if !yes {
				fmt.Printf("  %sContinue? (y/N):%s ", bold, reset)
				var ans string
				fmt.Scan(&ans)
				if strings.ToLower(strings.TrimSpace(ans)) != "y" {
					fmt.Printf("  %sAborted.%s\n\n", dim, reset)
					return nil
				}
			}

			// Remove the binary first (before wiping the data dir, since the
			// binary may live inside it at ~/.tollecode/bin/tollecode).
			// On Unix a running binary can be unlinked while in use — the inode
			// stays open until the process exits. We rename to a .old sibling
			// first so the slot is freed immediately; the actual inode is removed
			// on process exit (or immediately if the fs allows it).
			tmp := exe + ".old"
			if err := os.Rename(exe, tmp); err != nil {
				fmt.Fprintf(os.Stderr, "  %s✗  Could not remove binary: %v%s\n", red, err, reset)
				fmt.Fprintf(os.Stderr, "  %sRemove manually: %s%s\n\n", dim, exe, reset)
			} else {
				if err := os.Remove(tmp); err != nil {
					// Best-effort: on Windows the file may be locked until the process exits.
					_ = scheduleRemoveOnExit(tmp)
				}
				fmt.Printf("  %s  Removed %s%s\n", dim, exe, reset)
			}

			// Clean up any /usr/local/bin symlink that points to this binary.
			symlink := "/usr/local/bin/tollecode"
			if target, err := os.Readlink(symlink); err == nil && target == exe {
				if err := os.Remove(symlink); err != nil {
					fmt.Fprintf(os.Stderr, "  %s✗  Could not remove symlink %s: %v%s\n", red, symlink, err, reset)
					fmt.Fprintf(os.Stderr, "  %sRemove manually: sudo rm %s%s\n", dim, symlink, reset)
				} else {
					fmt.Printf("  %s  Removed %s%s\n", dim, symlink, reset)
				}
			}

			// Remove data directory last.
			if err := os.RemoveAll(dataDir); err != nil {
				fmt.Fprintf(os.Stderr, "  %s✗  Could not remove data directory: %v%s\n", red, err, reset)
			} else {
				fmt.Printf("  %s  Removed %s%s\n", dim, dataDir, reset)
			}
			fmt.Println()
			fmt.Printf("  %sTolleCode uninstalled.%s\n\n", bold, reset)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	return cmd
}

// scheduleRemoveOnExit deletes path after the current process exits.
// On Windows the running binary is file-locked, so we spawn a detached cmd.exe
// that waits briefly then deletes it. On Unix the rename already unlinked it,
// so this is a no-op fallback.
func scheduleRemoveOnExit(path string) error {
	if runtime.GOOS == "windows" {
		script := fmt.Sprintf(`ping -n 3 127.0.0.1 >nul & del /f /q "%s"`, path)
		cmd := exec.Command("cmd", "/c", script)
		cmd.SysProcAttr = sysProcDetach()
		return cmd.Start()
	}
	return os.Remove(path)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func resolveWS(ws string) string {
	if ws != "" {
		return ws
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tollecode: %v\n", err)
		os.Exit(1)
	}
	return cwd
}

func findSessionCLI(ws, prefix string) *session.APISession {
	sessions, _ := session.List(ws)
	var matches []session.APISession
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, prefix) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		fmt.Printf("  \033[31mNo session matching '%s'.\033[0m\n", prefix)
		return nil
	}
	if len(matches) > 1 {
		fmt.Printf("  \033[33mAmbiguous prefix — %d sessions match.\033[0m\n", len(matches))
		return nil
	}
	loaded, err := session.Load(ws, matches[0].ID)
	if err != nil {
		fmt.Printf("  \033[31m✗  Could not load session '%s'.\033[0m\n", prefix)
		return nil
	}
	return loaded
}

func printSessionsTable(sessions []session.APISession, currentID string) {
	const (
		p3    = "\033[38;2;124;58;237m"
		dim   = "\033[2m"
		reset = "\033[0m"
		bold  = "\033[1m"
		green = "\033[38;2;74;222;128m"
		yell  = "\033[38;2;251;189;35m"
	)
	fmt.Printf("  %s%s%-10s  %-45s  %-28s  %-5s  %s%s\n",
		bold, p3, "ID", "Title", "Model", "Mode", "Updated", reset)
	fmt.Printf("  %s%s%s\n", p3+dim, strings.Repeat("─", 76), reset)

	limit := 40
	if len(sessions) < limit {
		limit = len(sessions)
	}
	for _, s := range sessions[:limit] {
		marker := ""
		if s.ID == currentID {
			marker = " ←"
		}
		mc := green
		if s.Mode == "plan" {
			mc = yell
		}
		title := "—"
		if s.Title != nil && *s.Title != "" {
			title = *s.Title
		}
		if len(title) > 44 {
			title = title[:44]
		}
		updated := ""
		if len(s.UpdatedAt) >= 16 {
			updated = strings.Replace(s.UpdatedAt[:16], "T", " ", 1)
		}
		fmt.Printf("  %s%-10s%s  %-45s  %s%-28s%s  %s%s%-5s%s  %s%s\n",
			dim, s.ID[:8]+marker, reset,
			title,
			dim, s.Model, reset,
			bold, mc, strings.ToUpper(s.Mode), reset,
			dim, updated+reset)
	}
}

// ── serve (self-host API server) ──────────────────────────────────────────────

func serveCmd() *cobra.Command {
	var (
		configPath string
		port       int
		devMode    bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the self-host REST + WebSocket API server",
		Long: `Start the Tollecode self-host API server.

Loads configuration from ~/.tollecode/tollecode.yaml by default.
Use --config to specify a different path or --port to override the port.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if devMode {
				config.SetDevMode()
				ai.Global.Reload() // reload providers from the dev config directory
			}

			// Resolve config path.
			if configPath == "" {
				configPath = filepath.Join(config.Home(), "tollecode.yaml")
			}

			cfg, err := httpserver.LoadConfig(configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[tollecode] Warning: could not load config from %s: %v\n", configPath, err)
				cfg = httpserver.DefaultServerConfig()
			}

			// CLI --port overrides config file.
			if port != 0 {
				cfg.Port = port
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Handle SIGINT/SIGTERM for clean shutdown.
			go func() {
				sig := make(chan os.Signal, 1)
				signal.Notify(sig, os.Interrupt)
				<-sig
				cancel()
			}()

			// Load ~/.tollecode/.env so the server works without manual env export.
			config.LoadDotEnv()

			// Selfhost mode: DATABASE_URL triggers the PostgreSQL-backed, JWT-authenticated
			// API plus the embedded Angular web UI (setup wizard, login, dashboard) and the
			// DB-backed chat gateways. Compiled in only under the `selfhost` build tag —
			// open-source builds decline here and fall through to the community API.
			if handled, serveErr := selfhostgate.TryServe(ctx, cfg, true); handled {
				if serveErr != nil {
					fmt.Fprintf(os.Stderr, "[tollecode] serve error: %v\n", serveErr)
					os.Exit(1)
				}
				return nil
			}

			if err := httpserver.StartAPI(ctx, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "[tollecode] serve error: %v\n", err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "Path to tollecode.yaml config file (default: ~/.tollecode/tollecode.yaml)")
	cmd.Flags().IntVar(&port, "port", 0, "HTTP server port (overrides config file)")
	cmd.Flags().BoolVar(&devMode, "dev", false, "Use ~/.tollecode-dev data directory")

	return cmd
}

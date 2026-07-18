package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/channels/discord"
	"github.com/tolle-ai/tollecode/internal/channels/signal"
	"github.com/tolle-ai/tollecode/internal/channels/slack"
	"github.com/tolle-ai/tollecode/internal/channels/telegram"
	"github.com/tolle-ai/tollecode/internal/channels/whatsapp"
	"github.com/tolle-ai/tollecode/internal/lsp"
)

// Start binds an HTTP server on a random free loopback port and returns the port.
// Used by stdio (desktop/VSCode) mode — loopback only, no auth.
// The server shuts down cleanly when ctx is cancelled.
func Start(ctx context.Context) (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)
	MountLocalRoutes(r)

	srv := &http.Server{Handler: r}
	go srv.Serve(l)
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	return port, nil
}

// MountLocalRoutes mounts the single-user local routes (session/channel
// streaming WebSockets, the LSP proxy, and inline completion) onto r. These are
// the loopback, no-auth routes shared by stdio (desktop/VSCode) mode and web
// mode. Callers add their own middleware, command bridge, and static UI.
func MountLocalRoutes(r chi.Router) {
	r.Get("/ws/session/{sessionId}", handleSessionWS)
	r.Get("/channels/ws/{channelId}", handleChannelWS)

	// LSP proxy — one WebSocket connection per language server process.
	r.Get("/lsp/ws/{lang}", func(w http.ResponseWriter, r *http.Request) {
		lsp.HandleWS(w, r, chi.URLParam(r, "lang"))
	})
	r.Get("/lsp/check", handleLSPCheck)
	r.Get("/lsp/registry", func(w http.ResponseWriter, r *http.Request) {
		lsp.HandleRegistry(w, r)
	})
	r.Get("/lsp/install/{id}", func(w http.ResponseWriter, r *http.Request) {
		lsp.HandleInstall(w, r, chi.URLParam(r, "id"))
	})
	r.Get("/lsp/runtime/{lang}", func(w http.ResponseWriter, r *http.Request) {
		lsp.HandleRuntimeCheck(w, r, chi.URLParam(r, "lang"))
	})

	r.Post("/autocomplete", handleAutocomplete)
	r.Post("/explain", handleExplain)
}

// StartAPI binds the full REST + WebSocket API server on the configured address.
// Used by --serve mode. Blocks until ctx is cancelled.
func StartAPI(ctx context.Context, cfg ServerConfig) error {
	addr := cfg.Addr()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// Merge YAML providers into ai.Global so they're available to the agent.
	if len(cfg.Providers) > 0 {
		ai.Global.InjectConfigs(ConvertProviders(cfg.Providers))
	}

	// Apply the egress guardrail mode. The stdio and CLI entrypoints do this
	// themselves; without it here, serve mode stayed on the compile-time default
	// (log) no matter what egressMode or TOLLECODE_EGRESS said.
	ai.SyncEgressFromSettings()

	state := newAPIState(cfg.DefaultProvider, cfg.DefaultModel)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	// WebSocket routes (no auth — kept for local tooling compatibility)
	r.Get("/ws/session/{sessionId}", handleSessionWS)
	r.Get("/channels/ws/{channelId}", handleChannelWS)

	// Slack events webhook — Slack signs its own requests; no Bearer auth needed.
	if cfg.Channels.Slack.BotToken != "" {
		r.Post("/v1/channels/slack/events", slack.Handler(slackCfg(cfg, state)))
	}

	// WhatsApp webhook — Meta signs its own requests; no Bearer auth needed.
	if cfg.Channels.WhatsApp.PhoneNumberID != "" {
		wh := whatsapp.Handler(whatsappCfg(cfg, state))
		r.Get("/v1/channels/whatsapp/events", wh)
		r.Post("/v1/channels/whatsapp/events", wh)
	}

	// REST API — all routes behind auth middleware
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware(cfg.APIKeys))
		r.Use(userContextMiddleware)

		r.Route("/v1", func(r chi.Router) {
			mountWorkspaces(r, cfg)
			mountAgents(r)
			mountUsers(r)
			mountHumanTeams(r)
			mountSessions(r, state, cfg)
			mountTodos(r, state)
			mountSchedules(r, state)
			mountProviders(r)
			mountTools(r)
			mountAlerts(r)
			mountFS(r, cfg)
			mountSkills(r, state, cfg)
		})
	})

	fmt.Printf("[tollecode] API server listening on http://%s\n", addr)

	// Cron scheduler — fires ScheduleType=cron todo tasks on time.
	startCronScheduler(ctx, state)

	// Chat-platform gateways.
	startGateways(ctx, cfg, state)

	srv := &http.Server{Handler: r}
	go srv.Serve(l)
	<-ctx.Done()
	srv.Shutdown(context.Background())
	return nil
}

// startGateways launches all configured chat platform gateways as goroutines.
func startGateways(ctx context.Context, cfg ServerConfig, state *apiState) {
	resolveWS := func(workspaceID string) string {
		if ws, ok := resolveWorkspacePath(workspaceID); ok {
			return ws
		}
		if len(cfg.Workspaces) > 0 {
			return cfg.Workspaces[0].Path
		}
		return ""
	}
	// resolveProviderModel resolves provider/model with this priority:
	//   1. Channel-level provider/model (explicit pin in YAML)
	//   2. Agent config provider/model (if agent_id is set)
	//   3. Server-level default_provider / default_model
	//   4. Best available provider (tier-based fallback)
	// In all cases, if model is empty after resolution, it's filled in from
	// the provider's configured default model via BestProvider.
	resolveProviderModel := func(channelProvider, channelModel, agentID string) (provider, model string) {
		if channelProvider != "" {
			provider, model = channelProvider, channelModel
		} else if agentID != "" {
			if ac := agent.LookupAgentCfg(agentID); ac != nil && ac.Provider != "" {
				provider, model = ac.Provider, ac.Model
			}
		}
		if provider == "" {
			return apiFirstProvider(cfg.DefaultProvider, cfg.DefaultModel)
		}
		// Provider was set but model may be empty — resolve from provider defaults.
		if model == "" {
			_, model = ai.Global.BestProvider(provider, "")
		}
		return provider, model
	}

	if t := cfg.Channels.Telegram; t.Token != "" {
		provider, model := resolveProviderModel(t.Provider, t.Model, t.AgentID)
		go telegram.Start(ctx, telegram.Config{
			Token:          t.Token,
			WorkspacePath:  resolveWS(t.WorkspaceID),
			Provider:       provider,
			Model:          model,
			AgentID:        t.AgentID,
			ShellAutoAllow: t.ShellAutoAllow,
			MentionOnly:    t.MentionOnly,
		})
	}

	if d := cfg.Channels.Discord; d.Token != "" {
		provider, model := resolveProviderModel(d.Provider, d.Model, d.AgentID)
		go discord.Start(ctx, discord.Config{
			Token:          d.Token,
			WorkspacePath:  resolveWS(d.WorkspaceID),
			Provider:       provider,
			Model:          model,
			AgentID:        d.AgentID,
			ShellAutoAllow: d.ShellAutoAllow,
			MentionOnly:    d.MentionOnly,
		})
	}

	if s := cfg.Channels.Signal; s.PhoneNumber != "" {
		provider, model := resolveProviderModel(s.Provider, s.Model, s.AgentID)
		go signal.Start(ctx, signal.Config{
			PhoneNumber:    s.PhoneNumber,
			CLIPath:        s.CLIPath,
			WorkspacePath:  resolveWS(s.WorkspaceID),
			Provider:       provider,
			Model:          model,
			AgentID:        s.AgentID,
			ShellAutoAllow: s.ShellAutoAllow,
		})
	}
}

// slackCfg builds a slack.Config from the server config.
func slackCfg(cfg ServerConfig, _ *apiState) slack.Config {
	s := cfg.Channels.Slack
	ws, _ := resolveWorkspacePath(s.WorkspaceID)
	if ws == "" && len(cfg.Workspaces) > 0 {
		ws = cfg.Workspaces[0].Path
	}
	provider, model := "", ""
	if s.Provider != "" {
		provider, model = s.Provider, s.Model
	} else if s.AgentID != "" {
		if ac := agent.LookupAgentCfg(s.AgentID); ac != nil && ac.Provider != "" {
			provider, model = ac.Provider, ac.Model
		}
	}
	if provider == "" {
		provider, model = apiFirstProvider(cfg.DefaultProvider, cfg.DefaultModel)
	}
	// Provider was set but model may be empty — resolve from provider defaults.
	if model == "" && provider != "" {
		_, model = ai.Global.BestProvider(provider, "")
	}
	return slack.Config{
		BotToken:       s.BotToken,
		SigningSecret:  s.SigningSecret,
		WorkspacePath:  ws,
		Provider:       provider,
		Model:          model,
		AgentID:        s.AgentID,
		ShellAutoAllow: s.ShellAutoAllow,
	}
}

// whatsappCfg builds a whatsapp.Config from the server config.
func whatsappCfg(cfg ServerConfig, _ *apiState) whatsapp.Config {
	wa := cfg.Channels.WhatsApp
	ws, _ := resolveWorkspacePath(wa.WorkspaceID)
	if ws == "" && len(cfg.Workspaces) > 0 {
		ws = cfg.Workspaces[0].Path
	}
	provider, model := "", ""
	if wa.Provider != "" {
		provider, model = wa.Provider, wa.Model
	} else if wa.AgentID != "" {
		if ac := agent.LookupAgentCfg(wa.AgentID); ac != nil && ac.Provider != "" {
			provider, model = ac.Provider, ac.Model
		}
	}
	if provider == "" {
		provider, model = apiFirstProvider(cfg.DefaultProvider, cfg.DefaultModel)
	}
	// Provider was set but model may be empty — resolve from provider defaults.
	if model == "" && provider != "" {
		_, model = ai.Global.BestProvider(provider, "")
	}
	return whatsapp.Config{
		PhoneNumberID:  wa.PhoneNumberID,
		AccessToken:    wa.AccessToken,
		AppSecret:      wa.AppSecret,
		VerifyToken:    wa.VerifyToken,
		WorkspacePath:  ws,
		Provider:       provider,
		Model:          model,
		AgentID:        wa.AgentID,
		ShellAutoAllow: wa.ShellAutoAllow,
	}
}

// handleLSPCheck returns which language servers are installed on this machine,
// plus the shell PATH dirs the sidecar resolved at startup (useful for debugging
// "server not found" issues — if a dir is missing, the user's shell didn't export it).
func handleLSPCheck(w http.ResponseWriter, r *http.Request) {
	installed := lsp.Detect()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"installed":    installed,
		"shell":        lsp.ShellUsed(),
		"process_path": strings.Split(os.Getenv("PATH"), ":"),
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

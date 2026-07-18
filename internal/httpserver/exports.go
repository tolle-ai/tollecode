package httpserver

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/channels/slack"
	"github.com/tolle-ai/tollecode/internal/channels/whatsapp"
)

// StartAPIWithExtensions is identical to StartAPI but calls extRoutes(router, cfg)
// after all community routes are mounted, allowing callers (e.g. the pro binary)
// to add additional routes to the same server and port.
func StartAPIWithExtensions(ctx context.Context, cfg ServerConfig, extRoutes func(r chi.Router, cfg ServerConfig)) error {
	addr := cfg.Addr()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	if len(cfg.Providers) > 0 {
		ai.Global.InjectConfigs(ConvertProviders(cfg.Providers))
	}

	state := newAPIState(cfg.DefaultProvider, cfg.DefaultModel)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	r.Get("/ws/session/{sessionId}", handleSessionWS)
	r.Get("/channels/ws/{channelId}", handleChannelWS)

	if cfg.Channels.Slack.BotToken != "" {
		r.Post("/v1/channels/slack/events", slack.Handler(slackCfg(cfg, state)))
	}
	if cfg.Channels.WhatsApp.PhoneNumberID != "" {
		wh := whatsapp.Handler(whatsappCfg(cfg, state))
		r.Get("/v1/channels/whatsapp/events", wh)
		r.Post("/v1/channels/whatsapp/events", wh)
	}

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

			// Pro extension point
			if extRoutes != nil {
				extRoutes(r, cfg)
			}
		})
	})

	fmt.Printf("[tollecode] API server listening on http://%s\n", addr)

	startCronScheduler(ctx, state)
	startGateways(ctx, cfg, state)

	srv := &http.Server{Handler: r}
	go srv.Serve(l)
	<-ctx.Done()
	srv.Shutdown(context.Background())
	return nil
}

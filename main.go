package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/tolle-ai/tollecode/internal/cli"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/httpserver"
	"github.com/tolle-ai/tollecode/internal/selfhostgate"
	"github.com/tolle-ai/tollecode/internal/stdio"
)

func main() {
	stdioMode := flag.Bool("stdio", false, "Run as JSON-over-stdio server (Tauri IPC mode)")
	cliMode   := flag.Bool("cli", false, "Run as interactive CLI")
	devMode   := flag.Bool("dev", false, "Use ~/.tollecode-dev data directory")
	port      := flag.Int("port", 47821, "HTTP server port (non-stdio mode)")
	workspace := flag.String("workspace", "", "Workspace directory (default: cwd)")
	provider  := flag.String("provider", "", "Provider ID")
	model     := flag.String("model", "", "Model name")
	agentMode := flag.String("mode", "build", "Agent mode: plan|build")
	task      := flag.String("task", "", "Run a single task and exit (non-interactive)")
	sessionID := flag.String("session", "", "Resume an existing session by ID prefix")
	thinking  := flag.String("thinking", "0", "Thinking budget: 0|1k|4k|10k|32k")
	flag.Parse()

	thinkingMap := map[string]int{
		"0": 0, "off": 0,
		"1k": 1024, "4k": 4096, "10k": 10000, "32k": 32000,
	}
	budget := thinkingMap[*thinking]

	if *devMode {
		config.SetDevMode()
	}

	if *stdioMode {
		stdio.Run()
		return
	}

	if *cliMode || *task != "" {
		cli.Run(cli.Config{
			Workspace:      *workspace,
			Mode:           *agentMode,
			ProviderID:     *provider,
			Model:          *model,
			Task:           *task,
			SessionID:      *sessionID,
			ThinkingBudget: budget,
		})
		return
	}

	// HTTP serve mode — full REST + SSE API server
	cfgPath := "tollecode.yaml"
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		cfgPath = fmt.Sprintf("%s/server.yaml", os.Getenv("HOME"))
	}
	cfg, err := httpserver.LoadConfig(cfgPath)
	if err != nil {
		cfg = httpserver.DefaultServerConfig()
		cfg.Port = *port
	} else {
		if *port != 47821 {
			cfg.Port = *port
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

	// Load ~/.tollecode/.env so the server works without manual env export.
	config.LoadDotEnv()

	// Selfhost mode: DATABASE_URL triggers the PostgreSQL-backed, JWT-authenticated
	// API. Compiled in only under the `selfhost` build tag — open-source builds
	// decline here and fall through to the community API below.
	if handled, serveErr := selfhostgate.TryServe(ctx, cfg, false); handled {
		if serveErr != nil {
			fmt.Fprintf(os.Stderr, "[sidecar] serve error: %v\n", serveErr)
			os.Exit(1)
		}
		return
	}

	if err := httpserver.StartAPI(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[sidecar] serve error: %v\n", err)
		os.Exit(1)
	}
}

// Package webmode runs Tollecode Lite as a local web app: the same Go sidecar
// that Tauri drives over stdio, but serving the Angular UI over HTTP and
// speaking the command protocol over a WebSocket (/ws/cmd) instead of Tauri
// IPC. This is what `tollecode web` launches — a browser-based Lite that runs
// the way self-host does, without the Tauri desktop shell.
package webmode

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/httpserver"
	"github.com/tolle-ai/tollecode/internal/liteaccess"
	"github.com/tolle-ai/tollecode/internal/liteauth"
	"github.com/tolle-ai/tollecode/internal/mcp"
	"github.com/tolle-ai/tollecode/internal/stdio"
	"github.com/tolle-ai/tollecode/internal/webauth"
)

// The Lite Angular build is copied into dist/browser at release time (see
// scripts/build-lite-web.sh). A placeholder index.html keeps go:embed — and
// therefore the build — working before the real UI has been staged.
//
//go:embed dist/browser
var embeddedUI embed.FS

var uiRoot = func() (fs.FS, bool) {
	sub, err := fs.Sub(embeddedUI, "dist/browser")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}

// Run starts the web-mode server on 127.0.0.1:port and (optionally) opens the
// default browser at it. Blocks until ctx is cancelled.
func Run(ctx context.Context, port int, open bool) error {
	// Lite web: auto-connect locally-running MCP backends (e.g. Blender, Unity).
	mcp.EnableAutoDiscovery = true

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("web mode: cannot bind %s: %w", addr, err)
	}

	// One-time import of the desktop app's config (providers, agents, teams,
	// workspaces, …) into the shared KV so `tollecode web` mirrors the desktop
	// Lite app on first run. No-op once the shared store has data.
	config.SeedLiteKVFromDesktop()

	state := stdio.NewWebState()

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	// CORS reflects only loopback origins (never a wildcard) — see webauth.
	r.Use(webauth.CORS(port))

	// Auth over HTTP: the access-key door and the TOTP handshake. These live on
	// HTTP (not /ws/cmd) because only an HTTP response can set the httpOnly
	// session/grant cookies that gate everything else.
	mountAuthRoutes(r)

	// The command protocol the Lite UI speaks (was Tauri invoke → now /ws/cmd).
	// The socket authenticates from the tc_access + tc_session cookies presented
	// on its handshake.
	stdio.MountCmdBridge(r, state, port)

	// Agent streaming, LSP, and completion WebSockets/endpoints. Unlike desktop
	// stdio mode (trusted local IPC on a random port), the web transport is
	// reachable over the network, so these are gated on a loopback Origin and a
	// valid Lite session token.
	r.Group(func(pr chi.Router) {
		pr.Use(webauth.RequireSession(port))
		httpserver.MountLocalRoutes(pr)
	})

	// Background scheduler for server-side todo tasks, same as stdio.Run().
	stdio.StartTodoScheduler(state)

	// Embedded Angular SPA (falls through to index.html for client-side routes).
	// Served unauthenticated so the login screen itself can load.
	mountUI(r)

	srv := &http.Server{Handler: r}
	go srv.Serve(l)
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	url := fmt.Sprintf("http://%s", addr)
	if _, ok := uiRoot(); !ok {
		fmt.Printf("[web] serving on %s (UI not embedded — API/WS only)\n", url)
	} else {
		fmt.Printf("[web] Tollecode Lite running at %s\n", url)
	}

	// Declare which data dir this process actually reads. When `tollecode auth
	// reset` "does nothing", it's almost always because the reset ran against a
	// different dir (another OS user / TOLLECODE_HOME) than the running server —
	// this line makes the real path unambiguous.
	acct := "none — next login registers"
	if u := liteauth.LocalUser(); u != nil {
		acct = fmt.Sprintf("%s <%s>", u.Name, u.Email)
	}
	fmt.Printf("[web] data dir: %s\n", config.Home())
	fmt.Printf("[web] auth store: %s  (account: %s)\n", liteauth.StorePath(), acct)

	// The door is on: no browser gets past the login screen without this key, so
	// print it here — the machine running the server is the one place it can be
	// obtained. Personal-PC installs leave the key off and never see this.
	if liteaccess.Required() {
		fmt.Printf("[web] server access key required — enter this in each browser to unlock access:\n    %s\n", liteaccess.Key())
	}

	if open {
		// Give the listener a moment before launching the browser.
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(url)
		}()
	}

	<-ctx.Done()
	return nil
}

// mountUI serves the embedded SPA: exact file matches are served directly,
// everything else falls back to index.html so the Angular router handles the
// route. No-op when no UI is embedded (API/WS still work).
func mountUI(r chi.Router) {
	sub, ok := uiRoot()
	if !ok {
		return
	}
	server := http.FileServer(http.FS(sub))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		p := strings.TrimPrefix(req.URL.Path, "/")
		if p != "" {
			if f, err := sub.Open(p); err == nil {
				f.Close()
				server.ServeHTTP(w, req)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		req.URL.Path = "/"
		server.ServeHTTP(w, req)
	})
}

// openBrowser opens url in the platform default browser, best-effort.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}

package stdio

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/tolle-ai/tollecode/internal/liteaccess"
	"github.com/tolle-ai/tollecode/internal/liteauth"
	"github.com/tolle-ai/tollecode/internal/webauth"
)

// The web bridge exposes the exact JSON command protocol used over stdio (the
// Tauri IPC transport) as a WebSocket endpoint at /ws/cmd. A browser-based Lite
// build connects here instead of calling Tauri's invoke(); commands are handled
// by the same dispatch() switch and events flow back through the shared Emit()
// sink. This is what lets "tollecode web" reuse the desktop command surface
// unchanged.
//
// Security. Because this transport is exposed over the network (loopback, and
// whatever the user forwards it to), it enforces two things the desktop stdio
// transport doesn't need: a loopback Origin check on the handshake (blocks a
// malicious website from driving the sidecar via the browser), and a Lite
// session token on every command that isn't part of the login handshake. A
// connection only becomes "authenticated" by presenting a valid session token —
// at handshake (?token=) or on a command — so the TOTP login is a real gate, not
// a cosmetic UI screen. Broadcast events reach authenticated connections only,
// and auth responses go connection-local, so a passive listener can neither read
// another socket's data nor capture the login token.

// cmdUpgrader upgrades /ws/cmd. Origin is validated before Upgrade is called (see
// the handler), so CheckOrigin here is a permissive pass-through.
var cmdUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4 * 1024,
	WriteBufferSize: 64 * 1024,
}

// cmdHub tracks the connected /ws/cmd clients and fans Emit() events out to all
// of them. In practice web mode has a single browser tab, but supporting N
// clients keeps a second tab (or a reconnect race) from missing events.
type cmdHub struct {
	mu    sync.RWMutex
	conns map[*cmdConn]struct{}
}

type cmdConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex  // gorilla allows only one concurrent writer
	authed  atomic.Bool // set once a valid session token is presented (the lock)
	keyOK   atomic.Bool // set once the server access key (the door) is satisfied
}

// writeJSON marshals v and writes it to this connection only. Used for
// connection-local replies (auth responses) that must not be broadcast.
func (c *cmdConn) writeJSON(v any) {
	line, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[web] reply marshal error: %v\n", err)
		return
	}
	c.writeMu.Lock()
	c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	werr := c.ws.WriteMessage(websocket.TextMessage, line)
	c.writeMu.Unlock()
	if werr != nil {
		c.ws.Close()
	}
}

func newCmdHub() *cmdHub {
	return &cmdHub{conns: make(map[*cmdConn]struct{})}
}

func (h *cmdHub) add(c *cmdConn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *cmdHub) remove(c *cmdConn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// broadcast delivers one event to every AUTHENTICATED client. Unauthenticated
// connections (a browser mid-login, or a listener that never logged in) receive
// no broadcast events — only the connection-local replies to their own auth
// commands. Dead sockets are dropped silently; the read loop handles teardown.
func (h *cmdHub) broadcast(event map[string]any) {
	line, err := json.Marshal(event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[web] emit marshal error: %v\n", err)
		return
	}
	h.mu.RLock()
	conns := make([]*cmdConn, 0, len(h.conns))
	for c := range h.conns {
		if c.authed.Load() {
			conns = append(conns, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range conns {
		c.writeMu.Lock()
		c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		werr := c.ws.WriteMessage(websocket.TextMessage, line)
		c.writeMu.Unlock()
		if werr != nil {
			c.ws.Close()
		}
	}
}

// NewWebState creates a fresh server state for web mode, equivalent to the state
// stdio.Run() builds for the desktop IPC transport.
func NewWebState() *ServerState {
	return newServerState()
}

// MountCmdBridge mounts the /ws/cmd command WebSocket onto r and installs the
// broadcasting emit sink so events reach authenticated browsers. It shares the
// single provided ServerState across every connection, matching the desktop
// model where one sidecar process serves one user. port is the loopback port the
// server listens on, used to validate the handshake Origin.
func MountCmdBridge(r chi.Router, state *ServerState, port int) {
	hub := newCmdHub()
	SetEmitSink(hub.broadcast)

	r.Get("/ws/cmd", func(w http.ResponseWriter, req *http.Request) {
		// Reject cross-origin browser handshakes before upgrading. A page the
		// sidecar didn't serve (any real website) carries a non-loopback Origin.
		if !webauth.OriginAllowed(req, port) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		ws, err := cmdUpgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		conn := &cmdConn{ws: ws}
		// Authenticate the socket from the httpOnly cookies the browser sends on
		// the WebSocket upgrade: the access-key grant (the door) and the TOTP
		// session token (the lock). Neither secret is ever in a URL or in
		// JavaScript's reach — the login/unlock happen over the /web HTTP endpoints,
		// which set these cookies, and the socket is (re)opened afterwards.
		conn.keyOK.Store(liteaccess.GrantOK(webauth.AccessGrantFromRequest(req)))
		if ok, _ := liteauth.ValidateSession(webauth.SessionTokenFromRequest(req)); ok {
			conn.authed.Store(true)
		}
		hub.add(conn)
		defer func() {
			hub.remove(conn)
			ws.Close()
		}()

		// Tell the freshly-connected client the sidecar is up. The desktop
		// transport gets this via the stdout "server_started" line at startup;
		// a browser that connects later needs it on connect instead.
		conn.writeJSON(map[string]any{"type": "server_started"})

		ws.SetReadLimit(8 * 1024 * 1024)
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if len(data) == 0 {
				continue
			}
			var cmd map[string]any
			if err := json.Unmarshal(data, &cmd); err != nil {
				fmt.Fprintf(os.Stderr, "[web] bad JSON on /ws/cmd: %v\n", err)
				continue
			}
			typ, _ := cmd["type"].(string)

			// The socket is gated entirely by the handshake cookies: the door
			// (access-key grant) then the lock (TOTP session). Auth itself no longer
			// runs over this socket — it's HTTP now (only HTTP can set the cookies) —
			// so there are no in-band credentials to accept here.
			if !conn.keyOK.Load() {
				conn.writeJSON(map[string]any{"type": typ, "error": "access key required"})
				continue
			}
			if !conn.authed.Load() {
				conn.writeJSON(map[string]any{"type": typ, "error": "authentication required"})
				continue
			}

			// Reuse the desktop dispatch switch verbatim so the command surface
			// stays identical across transports.
			dispatch(state, cmd)
		}
	})
}

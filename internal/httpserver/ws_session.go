package httpserver

import (
	"bytes"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/tolle-ai/tollecode/internal/session"
)

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  1024,
	WriteBufferSize: 32 * 1024,
}

const writeTimeout = 10 * time.Second

// handleSessionWS streams agent events to a WebSocket client.
//
// A session WebSocket is a long-lived CHANNEL, not a per-turn connection. It is
// opened when the client selects a session and stays open across turns: a
// terminal event (done / cancelled / agent_error) ends a TURN but NOT the
// channel, so a follow-up message streams back on the same socket with no
// reconnect or handshake delay. The channel closes only when the client
// disconnects (or the process shuts down).
//
// Connection lifecycle (four phases):
//
//  1. Replay the live JSONL file from the client's last-known offset so no
//     events are lost across reconnects.
//  2. Subscribe to the in-memory bus (which replays its own ring buffer) so
//     events that arrived between Phase 1 and now are covered.
//  3. A catch-up read of the live file closes the race window between the end
//     of Phase 1 and the Phase 2 subscription.
//  4. Stream live bus events, deduplicated against the file offset already
//     sent. A `session_reset` (emitted when a new turn truncates the live log)
//     drops the dedup threshold so the next turn's low-offset events flow. The
//     loop exits only on client disconnect or process shutdown.
func handleSessionWS(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")

	var fromOffset int64
	if v := r.URL.Query().Get("from_offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			fromOffset = n
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Serialise all writes: gorilla/websocket allows only one concurrent writer.
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		return conn.WriteJSON(v)
	}
	sendClose := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}

	// Tell the just-connected client whether a turn is already live on this
	// session, BEFORE any replay. A second browser or tab that opens a session
	// mid-turn uses this to stream the in-flight events instead of swallowing them
	// while it waits for a session_reset boundary that fired before it connected —
	// which is what otherwise leaves "only the browser that sent the message"
	// showing the stream. An idle session reports running=false and the client
	// keeps its normal skip-until-next-turn behaviour.
	if err := writeJSON(map[string]any{
		"type":       "stream_status",
		"session_id": sessionID,
		"running":    session.IsRunning(sessionID),
	}); err != nil {
		return
	}

	// ── Phase 1: replay from live file ───────────────────────────────────────

	// A terminal encountered while replaying belongs to a finished TURN; we
	// forward it (so the UI shows done) but do NOT close — the channel stays
	// open, idle, ready for the next turn. Phase 4 handles channel teardown.
	phase1, _ := session.TailLiveEvents(sessionID, fromOffset)
	for _, le := range phase1.Events {
		ev := cloneWithOff(le.Event, le.Offset)
		if err := writeJSON(ev); err != nil {
			return
		}
	}

	// ── Phase 2: subscribe to in-memory bus ──────────────────────────────────
	//
	// SubscribeFrom replays only buffered events whose _off >= phase1.EndOffset,
	// so events already delivered via the Phase 1 file replay are not re-sent
	// by the in-memory ring buffer. This closes the duplicate window that
	// caused "old messages stream in" on reconnect.
	ch, unsub := session.Global.SubscribeFrom(sessionID, phase1.EndOffset)
	var unsubOnce sync.Once
	safeUnsub := func() { unsubOnce.Do(unsub) }
	defer safeUnsub()

	// ── Phase 3: catch-up between Phase 1 end and Phase 2 subscribe ──────────

	phase3, _ := session.TailLiveEvents(sessionID, phase1.EndOffset)
	for _, le := range phase3.Events {
		ev := cloneWithOff(le.Event, le.Offset)
		if err := writeJSON(ev); err != nil {
			safeUnsub()
			return
		}
	}

	// Deduplication threshold: any bus event with _off below this was already
	// sent via the file replay above. A `session_reset` in Phase 4 resets it to
	// 0 so a fresh turn's low-offset events are not skipped.
	lastSentOffset := phase3.EndOffset
	if lastSentOffset == 0 {
		lastSentOffset = phase1.EndOffset
	}

	// NOTE: we intentionally do NOT close here when the session already has a
	// stored terminal. The channel stays open and idle so the next turn streams
	// on this same socket — that is the whole point of the channel model.

	// ── Read goroutine: pings + client-disconnect detection ──────────────────

	// clientGone is closed when the peer disconnects (close frame, TCP reset,
	// or read-deadline expiry).
	clientGone := make(chan struct{})
	go func() {
		// Close clientGone BEFORE safeUnsub so the Phase 4 select sees
		// <-clientGone first and exits cleanly, rather than seeing !ok on ch
		// (from safeUnsub closing it) and mistakenly sending synthetic cancelled
		// to a client that has already disconnected.
		defer safeUnsub()
		defer close(clientGone)
		conn.SetReadLimit(4096)
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			// Reset the inactivity deadline on every frame (ping or otherwise).
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			switch mt {
			case websocket.CloseMessage:
				// Client sent a graceful close — we're done.
				return
			case websocket.TextMessage:
				if bytes.Contains(msg, []byte(`"ping"`)) {
					writeMu.Lock()
					conn.SetWriteDeadline(time.Now().Add(writeTimeout))
					conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong"}`))
					writeMu.Unlock()
				}
			}
		}
	}()

	// ── Phase 4: live stream from the bus ────────────────────────────────────

	for {
		select {
		case <-clientGone:
			return
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				// Bus subscription closed without a terminal event (e.g. the
				// process is shutting down). Send a synthetic cancelled so the
				// client knows the stream ended and can stop showing a spinner.
				synthetic := map[string]any{
					"type":       "cancelled",
					"session_id": sessionID,
					"reason":     "connection_lost",
				}
				writeJSON(synthetic) //nolint:errcheck
				sendClose()
				return
			}

			// A new turn truncated the live JSONL and reset offsets back to ~0
			// (see startAgentTurn). Drop the dedup threshold so the new turn's
			// low-offset events are forwarded instead of being skipped as
			// "already sent" against the previous turn's high offset.
			if t, _ := event["type"].(string); t == "session_reset" {
				lastSentOffset = 0
			}

			// Deduplicate: skip events already sent via Phase 1 / Phase 3 file replay.
			if lastSentOffset > 0 {
				if off, isInt := event["_off"].(int64); isInt && off < lastSentOffset {
					continue
				}
			}

			if err := writeJSON(event); err != nil {
				return
			}

			// A terminal ends the TURN, not the CHANNEL. Forward it (the client
			// flips the UI to done/idle) but keep the socket open so the next
			// turn streams here without a reconnect. The channel closes only via
			// clientGone / request-context / bus-closed (handled above).
		}
	}
}

// cloneWithOff returns a shallow copy of m with the _off field set to offset.
func cloneWithOff(m map[string]any, offset int64) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out["_off"] = offset
	return out
}

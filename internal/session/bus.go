package session

import "sync"

const replayBufSize = 256

// Bus is an in-process pub/sub for session events.
// The stdio emitter publishes every agent event here;
// HTTP WebSocket handlers subscribe to receive them.
type Bus struct {
	mu        sync.Mutex
	subs      map[string][]chan map[string]any
	buffers   map[string][]map[string]any
	terminals map[string]map[string]any // last terminal event per session (done/cancelled/agent_error)
}

// Global is the singleton bus used by the whole process.
var Global = &Bus{
	subs:      make(map[string][]chan map[string]any),
	buffers:   make(map[string][]map[string]any),
	terminals: make(map[string]map[string]any),
}

// IsTerminalType returns true for event types that end the agent stream.
// run_done/run_failed are terminal for workflow-run streams, which reuse this bus.
func IsTerminalType(t string) bool {
	return t == "done" || t == "cancelled" || t == "agent_error" ||
		t == "run_done" || t == "run_failed" || t == "run_waiting"
}

// ClearBuffer discards buffered events (and terminal state) for sessionID.
// Call this before starting a new agent turn so reconnecting WS clients
// don't replay stale events from a previous turn.
func (b *Bus) ClearBuffer(sessionID string) {
	b.mu.Lock()
	delete(b.buffers, sessionID)
	delete(b.terminals, sessionID)
	b.mu.Unlock()
}

// Subscribe returns a buffered channel that receives all future events for
// sessionID, and a cancel function that must be called to stop delivery.
// Any events already buffered since the last ClearBuffer are replayed
// immediately so clients that connect after agent start don't miss them.
func (b *Bus) Subscribe(sessionID string) (chan map[string]any, func()) {
	ch := make(chan map[string]any, 512)
	b.mu.Lock()
	// Replay buffered events into the new channel before registering.
	for _, ev := range b.buffers[sessionID] {
		select {
		case ch <- ev:
		default:
		}
	}
	b.subs[sessionID] = append(b.subs[sessionID], ch)
	b.mu.Unlock()

	return ch, b.cancelFn(sessionID, ch)
}

// cancelFn returns the unsubscribe/cleanup closure for a subscriber channel.
// It removes ch from the session's subscriber list and closes it. Shared by
// Subscribe and SubscribeFrom so the teardown logic stays identical.
func (b *Bus) cancelFn(sessionID string, ch chan map[string]any) func() {
	return func() {
		b.mu.Lock()
		chans := b.subs[sessionID]
		for i, c := range chans {
			if c == ch {
				b.subs[sessionID] = append(chans[:i], chans[i+1:]...)
				break
			}
		}
		if len(b.subs[sessionID]) == 0 {
			delete(b.subs, sessionID)
		}
		b.mu.Unlock()
		close(ch)
	}
}

// SubscribeFrom is like Subscribe but only replays buffered events whose _off
// is >= fromOffset. A WS client that already received file-replayed events up
// to fromOffset (e.g. Phase 1 replay) uses this so the in-memory ring buffer
// does not re-deliver those same events as duplicates ("old messages stream
// in" on reconnect).
//
// When fromOffset is 0, every buffered event is replayed (matching the
// historical Subscribe behavior). Events without an _off field (off == 0)
// are always replayed when fromOffset == 0, and skipped when fromOffset > 0
// only if their resolved offset is below the threshold — which is correct
// because a real event always carries an _off >= 1 once the live log is
// appending.
func (b *Bus) SubscribeFrom(sessionID string, fromOffset int64) (chan map[string]any, func()) {
	ch := make(chan map[string]any, 512)
	b.mu.Lock()
	// Replay only buffered events at or beyond fromOffset so a reconnecting
	// client that already saw Phase 1 file replay does not get duplicates.
	for _, ev := range b.buffers[sessionID] {
		if fromOffset > 0 {
			off, _ := ev["_off"].(int64)
			if off < fromOffset {
				continue
			}
		}
		select {
		case ch <- ev:
		default:
		}
	}
	b.subs[sessionID] = append(b.subs[sessionID], ch)
	b.mu.Unlock()

	return ch, b.cancelFn(sessionID, ch)
}

// Terminal returns a shallow copy of the last terminal event for sessionID,
// or nil if the session is still running or has never started.
func (b *Bus) Terminal(sessionID string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	src := b.terminals[sessionID]
	if src == nil {
		return nil
	}
	cp := make(map[string]any, len(src))
	for k, v := range src {
		cp[k] = v
	}
	return cp
}

// Publish sends event to every subscriber for sessionID and appends it to
// the replay buffer so late-connecting subscribers can catch up.
// Terminal events (done/cancelled/agent_error) are also stored so the WS
// handler can close immediately if the session already finished.
// Non-blocking: slow subscribers drop events rather than blocking the agent.
func (b *Bus) Publish(sessionID string, event map[string]any) {
	t, _ := event["type"].(string)

	b.mu.Lock()
	buf := b.buffers[sessionID]
	buf = append(buf, event)
	if len(buf) > replayBufSize {
		buf = buf[len(buf)-replayBufSize:]
	}
	b.buffers[sessionID] = buf

	if IsTerminalType(t) {
		b.terminals[sessionID] = event
	}

	// Copy the slice so we can release the lock before sending.
	chans := make([]chan map[string]any, len(b.subs[sessionID]))
	copy(chans, b.subs[sessionID])
	b.mu.Unlock()

	for _, ch := range chans {
		safeSend(ch, event)
	}
}

// HasPendingLiveEvents reports true if the newest buffered event for sessionID
// is non-terminal, which means an agent stream is still in progress. Terminal
// events (done/cancelled/agent_error) mark the end of the stream; any older
// non-terminal events after the newest terminal are ignored.
func (b *Bus) HasPendingLiveEvents(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf := b.buffers[sessionID]
	if len(buf) == 0 {
		// No buffered events at all. Use the terminal store as a hint: if there's
		// no terminal and no buffer, we conservatively say not pending.
		return false
	}
	latest := buf[len(buf)-1]
	t, _ := latest["type"].(string)
	return !IsTerminalType(t)
}
// A panic would occur if unsub() closed ch between the time Publish()
// captured the channel slice and when the send is attempted.
func safeSend(ch chan map[string]any, event map[string]any) {
	defer func() { recover() }() //nolint:errcheck
	select {
	case ch <- event:
	default:
	}
}

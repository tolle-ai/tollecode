package stdio

import (
	"context"
	"sync"

	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/ai"
)

// sessionTask tracks a running agent goroutine for one session.
type sessionTask struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// ServerState holds the mutable IPC state for the stdio server.
type ServerState struct {
	mu sync.Mutex

	Workspace      string
	SessionID      string
	Mode           string
	ThinkingBudget int    // Anthropic budget (kept for Anthropic provider)
	ThinkLevel     string // Ollama: "", "true", "false", "low", "medium", "high"
	ActiveSkills   []string

	// per-session running tasks keyed by session_id
	tasks map[string]*sessionTask

	// screenshot responses keyed by requestId
	screenshotCh map[string]chan map[string]any

	// activeChatChannels prevents two parallel agent goroutines for the same channel.
	activeChatChannels map[string]struct{}

	// shell permission queues keyed by session_id
	permQueues map[string]chan permResponse

	// memory permission queue (single, per-server)
	memQueue chan permResponse
	AllowAllMemory bool

	// AllowAllShell auto-approves run_shell commands without showing a permission prompt.
	AllowAllShell bool

	// MaxSessionMessages triggers session_limit_reached when a session exceeds this
	// number of messages. 0 = unlimited.
	MaxSessionMessages int

	// clarification queues keyed by request_id — one pending question at a time.
	clarificationChannels map[string]chan clarificationResponse

	// system permission responses keyed by requestId (one per pending desktop tool call).
	systemPermQueues map[string]chan bool

	// iterationConfirmQueues holds one channel per session waiting for the user
	// to respond to an iteration_confirm_request. Allow = continue, deny = stop.
	iterationConfirmQueues map[string]chan permResponse

	// pendingMessages queues user messages that arrived while a session was still
	// live streaming. They are collapsed to the latest message once the stream ends.
	pendingMessages map[string][]pendingMessage

	// pendingMessageTimers tracks whether a delayed flush goroutine is already in
	// flight for a session, preventing multiple concurrent timers.
	pendingMessageTimers map[string]bool
}

// pendingMessage holds one user message that arrived while a session was still
// live streaming, plus the full send_message payload needed to start the turn.
type pendingMessage struct {
	cmd map[string]any
}

type permResponse struct {
	Allow    bool
	AllowAll bool
}

type clarificationResponse struct {
	Selected []string
	Details  string
}

func newServerState() *ServerState {
	// Apply the persisted egress-guardrail mode before serving any request. Both
	// the desktop (stdio) and web run modes construct state here.
	ai.SyncEgressFromSettings()
	return &ServerState{
		Mode:                   "build",
		ThinkingBudget:         0,
		ThinkLevel:             "",
		tasks:                  make(map[string]*sessionTask),
		screenshotCh:           make(map[string]chan map[string]any),
		permQueues:             make(map[string]chan permResponse),
		memQueue:               make(chan permResponse, 1),
		clarificationChannels:  make(map[string]chan clarificationResponse),
		activeChatChannels:     make(map[string]struct{}),
		systemPermQueues:       make(map[string]chan bool),
		iterationConfirmQueues: make(map[string]chan permResponse),
		pendingMessages:        make(map[string][]pendingMessage),
		pendingMessageTimers:   make(map[string]bool),
	}
}

// startChannelChat registers channelID as active. Returns false if already running.
func (s *ServerState) startChannelChat(channelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.activeChatChannels[channelID]; ok {
		return false
	}
	s.activeChatChannels[channelID] = struct{}{}
	return true
}

// endChannelChat unregisters channelID so a new chat can start.
func (s *ServerState) endChannelChat(channelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.activeChatChannels, channelID)
}

// cancelSession cancels a running task and blocks until the goroutine exits.
// Use this before starting a replacement task to prevent two goroutines for the
// same session from running concurrently (e.g. handleSendMessage).
func (s *ServerState) cancelSession(sessionID string) {
	s.mu.Lock()
	t, ok := s.tasks[sessionID]
	delete(s.tasks, sessionID)
	// Any queued messages for this session are stale now that we're cancelling
	// synchronously. Drop them so they don't start after this call returns.
	delete(s.pendingMessages, sessionID)
	delete(s.pendingMessageTimers, sessionID)
	s.mu.Unlock()
	if ok {
		t.cancel()
		<-t.done
	}
}

// cancelSessionNoWait fires context cancellation and returns immediately.
// The task stays in the map so handleSendMessage can still find it and wait
// before starting a replacement. The goroutine's deferred removeTask cleans it up.
// Use this for user-initiated stops so the stdio read loop is never blocked.
func (s *ServerState) cancelSessionNoWait(sessionID string) {
	s.mu.Lock()
	t, ok := s.tasks[sessionID]
	// User explicitly cancelled: drop any queued messages so they don't restart
	// the stream after the user already asked for it to stop.
	delete(s.pendingMessages, sessionID)
	delete(s.pendingMessageTimers, sessionID)
	s.mu.Unlock()
	if ok {
		t.cancel()
	}
}

// registerTask stores a new task for the session.
func (s *ServerState) registerTask(sessionID string, cancel context.CancelFunc, done <-chan struct{}) {
	s.mu.Lock()
	s.tasks[sessionID] = &sessionTask{cancel: cancel, done: done}
	s.mu.Unlock()
}

// removeTask deletes a finished task from the registry.
func (s *ServerState) removeTask(sessionID string) {
	s.mu.Lock()
	delete(s.tasks, sessionID)
	s.mu.Unlock()
}

// registerScreenshotCh registers a channel that will receive the screenshot response.
func (s *ServerState) registerScreenshotCh(requestID string) chan map[string]any {
	ch := make(chan map[string]any, 1)
	s.mu.Lock()
	s.screenshotCh[requestID] = ch
	s.mu.Unlock()
	return ch
}

// deliverScreenshot routes a screenshot_response from stdin to the waiting goroutine.
func (s *ServerState) deliverScreenshot(requestID string, payload map[string]any) bool {
	s.mu.Lock()
	ch, ok := s.screenshotCh[requestID]
	if ok {
		delete(s.screenshotCh, requestID)
	}
	s.mu.Unlock()
	if ok {
		ch <- payload
		return true
	}
	return false
}

// permQueue returns (creating if needed) the permission channel for a session.
func (s *ServerState) permQueue(sessionID string) chan permResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.permQueues[sessionID]; !ok {
		s.permQueues[sessionID] = make(chan permResponse, 1)
	}
	return s.permQueues[sessionID]
}

// registerClarificationCh creates a one-shot channel that receives the user's
// answer to a clarification_request. Keyed by the requestId sent to the frontend.
func (s *ServerState) registerClarificationCh(requestID string) chan clarificationResponse {
	ch := make(chan clarificationResponse, 1)
	s.mu.Lock()
	s.clarificationChannels[requestID] = ch
	s.mu.Unlock()
	return ch
}

// deliverClarificationResponse routes a clarification_response from the frontend to
// the waiting goroutine. Returns false when no listener is registered (duplicate/stale).
func (s *ServerState) deliverClarificationResponse(requestID string, answer agent.ClarificationAnswer) bool {
	s.mu.Lock()
	ch, ok := s.clarificationChannels[requestID]
	if ok {
		delete(s.clarificationChannels, requestID)
	}
	s.mu.Unlock()
	if ok {
		ch <- clarificationResponse{Selected: answer.Selected, Details: answer.Details}
		return true
	}
	return false
}

// iterationConfirmQueue returns (creating if needed) the confirm channel for a session.
func (s *ServerState) iterationConfirmQueue(sessionID string) chan permResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.iterationConfirmQueues[sessionID]; !ok {
		s.iterationConfirmQueues[sessionID] = make(chan permResponse, 1)
	}
	return s.iterationConfirmQueues[sessionID]
}

// deliverIterationConfirm routes an iteration_confirm_response to the waiting goroutine.
func (s *ServerState) deliverIterationConfirm(sessionID string, allow bool) {
	ch := s.iterationConfirmQueue(sessionID)
	select {
	case ch <- permResponse{Allow: allow}:
	default:
	}
}

// registerSysPermCh creates a one-shot channel that receives the user's response
// to a system_permission_required event. Keyed by the requestId sent to the frontend.
func (s *ServerState) registerSysPermCh(requestID string) chan bool {
	ch := make(chan bool, 1)
	s.mu.Lock()
	s.systemPermQueues[requestID] = ch
	s.mu.Unlock()
	return ch
}

// deliverSysPermResponse routes a system_permission_response from the frontend to
// the waiting goroutine. Returns false when no listener is registered (duplicate/stale).
func (s *ServerState) deliverSysPermResponse(requestID string, granted bool) bool {
	s.mu.Lock()
	ch, ok := s.systemPermQueues[requestID]
	if ok {
		delete(s.systemPermQueues, requestID)
	}
	s.mu.Unlock()
	if ok {
		ch <- granted
		return true
	}
	return false
}

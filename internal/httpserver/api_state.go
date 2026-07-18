package httpserver

import (
	"context"
	"sync"
)

// apiState tracks mutable runtime state for the REST API server:
// running agent goroutines, pending permission prompts, and clarification queues.
type apiState struct {
	mu sync.Mutex

	// Server-level default provider/model (from tollecode.yaml default_provider / default_model).
	// Used by apiFirstProvider when no channel or agent override is set.
	defaultProvider string
	defaultModel    string

	// running agent turns keyed by session ID
	tasks map[string]*apiTask

	// pending shell-permission prompts keyed by session ID
	permQueues map[string]chan apiPermResponse

	// pending clarification questions keyed by session ID
	clarificationQueues map[string]chan apiClarificationResponse

	// session ID → workspace path (populated on session create/run)
	sessionWorkspaces map[string]string
}

type apiTask struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

type apiPermResponse struct {
	Allow    bool
	AllowAll bool
}

type apiClarificationResponse struct {
	Answer string
}

func newAPIState(defaultProvider, defaultModel string) *apiState {
	return &apiState{
		defaultProvider:    defaultProvider,
		defaultModel:       defaultModel,
		tasks:              make(map[string]*apiTask),
		permQueues:         make(map[string]chan apiPermResponse),
		clarificationQueues: make(map[string]chan apiClarificationResponse),
		sessionWorkspaces:   make(map[string]string),
	}
}

func (s *apiState) trackSession(sessionID, workspacePath string) {
	s.mu.Lock()
	s.sessionWorkspaces[sessionID] = workspacePath
	s.mu.Unlock()
}

func (s *apiState) workspaceFor(sessionID string) (string, bool) {
	s.mu.Lock()
	ws, ok := s.sessionWorkspaces[sessionID]
	s.mu.Unlock()
	return ws, ok
}

func (s *apiState) register(sessionID string, cancel context.CancelFunc, done <-chan struct{}) {
	s.mu.Lock()
	s.tasks[sessionID] = &apiTask{cancel: cancel, done: done}
	s.mu.Unlock()
}

func (s *apiState) remove(sessionID string) {
	s.mu.Lock()
	delete(s.tasks, sessionID)
	s.mu.Unlock()
}

// cancel cancels a running task and blocks until the goroutine exits.
// Use before starting a replacement task to prevent concurrent goroutines.
func (s *apiState) cancel(sessionID string) {
	s.mu.Lock()
	t, ok := s.tasks[sessionID]
	delete(s.tasks, sessionID)
	s.mu.Unlock()
	if ok {
		t.cancel()
		<-t.done
	}
}

// cancelNoWait fires context cancellation and returns immediately.
// The task stays in the map so a subsequent run call can still find and wait for it.
// Use for user-initiated stops where responsiveness matters more than waiting for cleanup.
func (s *apiState) cancelNoWait(sessionID string) {
	s.mu.Lock()
	t, ok := s.tasks[sessionID]
	s.mu.Unlock()
	if ok {
		t.cancel()
	}
}

func (s *apiState) isRunning(sessionID string) bool {
	s.mu.Lock()
	_, ok := s.tasks[sessionID]
	s.mu.Unlock()
	return ok
}

func (s *apiState) permQueue(sessionID string) chan apiPermResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.permQueues[sessionID]; !ok {
		s.permQueues[sessionID] = make(chan apiPermResponse, 1)
	}
	return s.permQueues[sessionID]
}

func (s *apiState) deliverPerm(sessionID string, allow, allowAll bool) bool {
	s.mu.Lock()
	ch, ok := s.permQueues[sessionID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- apiPermResponse{Allow: allow, AllowAll: allowAll}:
		return true
	default:
		return false
	}
}

func (s *apiState) clarificationQueue(sessionID string) chan apiClarificationResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clarificationQueues[sessionID]; !ok {
		s.clarificationQueues[sessionID] = make(chan apiClarificationResponse, 1)
	}
	return s.clarificationQueues[sessionID]
}

func (s *apiState) deliverClarification(sessionID, answer string) bool {
	s.mu.Lock()
	ch, ok := s.clarificationQueues[sessionID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- apiClarificationResponse{Answer: answer}:
		return true
	default:
		return false
	}
}

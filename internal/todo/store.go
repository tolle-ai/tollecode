// Package todo provides persistent storage and scheduling for TodoTask workflows.
// Tasks are stored per-workspace in .agent/todo_tasks.json and survive sidecar restarts.
package todo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Step mirrors the Angular TodoStep shape plus resolved execution fields.
type Step struct {
	ID          string `json:"id"`
	AgentID     string `json:"agentId"`
	Instruction string `json:"instruction,omitempty"`
	OnComplete  string `json:"onComplete"` // "next" | "finish"
	OnFail      string `json:"onFail"`     // "next" | "finish"
	Status      string `json:"status"`     // "pending" | "running" | "done" | "failed" | "skipped"
	SessionID   string `json:"sessionId,omitempty"`
	// Provider/model resolved at task-creation time from agent config.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Task mirrors the Angular TodoTask shape plus sidecar execution metadata.
type Task struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Mode             string   `json:"mode"` // "single" | "team"
	Steps            []Step   `json:"steps,omitempty"`
	LeadAgentID      string   `json:"leadAgentId,omitempty"`
	TeamAgentIDs     []string `json:"teamAgentIds,omitempty"`
	Status           string   `json:"status"` // "pending" | "running" | "done" | "failed"
	CurrentStepIndex int      `json:"currentStepIndex"`
	CreatedAt        string   `json:"createdAt"`
	ShellAutoAllow   bool     `json:"shellAutoAllow,omitempty"`
	ScheduleType     string   `json:"scheduleType,omitempty"` // "instant" | "scheduled" | "cron"
	ScheduledAt      string   `json:"scheduledAt,omitempty"`  // RFC3339
	Cron             string   `json:"cron,omitempty"`         // cron expression (5-field)
	WorkspacePath    string   `json:"workspacePath"`
	// Resolved provider/model for team-mode lead agent.
	LeadProvider string `json:"leadProvider,omitempty"`
	LeadModel    string `json:"leadModel,omitempty"`
	// Session ID of the lead agent's execution session (team mode only).
	LeadSessionID string `json:"leadSessionId,omitempty"`
	// MemberSessionIDs tracks the session created for each team member (indexed by
	// position in TeamAgentIDs). Empty string means not yet delegated.
	MemberSessionIDs []string `json:"memberSessionIds,omitempty"`
	// MemberStatuses tracks individual member execution status, same index order as TeamAgentIDs.
	MemberStatuses []string `json:"memberStatuses,omitempty"`
}

const tasksFile = "todo_tasks.json"

type workspaceStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	path  string
}

// stores is the global registry of per-workspace stores.
var stores sync.Map // workspacePath → *workspaceStore

// LoadWorkspace eagerly loads (or no-ops if already loaded) the task store for a
// workspace. Call this when the client sends register_workspace so the scheduler
// can find tasks without waiting for the first add/list command.
func LoadWorkspace(workspacePath string) {
	storeFor(workspacePath)
}

func storeFor(workspacePath string) *workspaceStore {
	if v, ok := stores.Load(workspacePath); ok {
		return v.(*workspaceStore)
	}
	s := &workspaceStore{
		tasks: make(map[string]*Task),
		path:  filepath.Join(workspacePath, ".agent", tasksFile),
	}
	s.load()
	actual, _ := stores.LoadOrStore(workspacePath, s)
	return actual.(*workspaceStore)
}

func (s *workspaceStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var tasks []*Task
	if json.Unmarshal(data, &tasks) != nil {
		return
	}
	for _, t := range tasks {
		s.tasks[t.ID] = t
	}
}

func (s *workspaceStore) save() {
	s.mu.RLock()
	tasks := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		cp := *t
		tasks = append(tasks, &cp)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	_ = os.WriteFile(s.path, data, 0o644)
}

// Add persists a new task for the workspace.
func Add(workspacePath string, t *Task) {
	s := storeFor(workspacePath)
	s.mu.Lock()
	s.tasks[t.ID] = t
	s.mu.Unlock()
	s.save()
}

// List returns all tasks for the workspace (copies, safe to mutate).
func List(workspacePath string) []*Task {
	s := storeFor(workspacePath)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		cp := *t
		out = append(out, &cp)
	}
	return out
}

// Get returns a copy of a single task, and false if not found.
func Get(workspacePath, id string) (*Task, bool) {
	s := storeFor(workspacePath)
	s.mu.RLock()
	t, ok := s.tasks[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// Update replaces the stored task (full replacement).
func Update(workspacePath string, t *Task) {
	s := storeFor(workspacePath)
	cp := *t
	s.mu.Lock()
	s.tasks[t.ID] = &cp
	s.mu.Unlock()
	s.save()
}

// Remove deletes a task from the store.
func Remove(workspacePath, id string) {
	s := storeFor(workspacePath)
	s.mu.Lock()
	delete(s.tasks, id)
	s.mu.Unlock()
	s.save()
}

// PatchMember atomically updates a single team member's session ID and/or status
// at the given index (position in TeamAgentIDs). Slices are grown as needed.
// Pass an empty string to leave either field unchanged.
func PatchMember(workspacePath, id string, idx int, sessionID, status string) {
	s := storeFor(workspacePath)
	s.mu.Lock()
	if t, ok := s.tasks[id]; ok {
		for len(t.MemberSessionIDs) <= idx {
			t.MemberSessionIDs = append(t.MemberSessionIDs, "")
		}
		for len(t.MemberStatuses) <= idx {
			t.MemberStatuses = append(t.MemberStatuses, "pending")
		}
		if sessionID != "" {
			t.MemberSessionIDs[idx] = sessionID
		}
		if status != "" {
			t.MemberStatuses[idx] = status
		}
		// If any member is still running, the overall task must stay running too.
		// This keeps the workflow UI honest when a lead marks some members done
		// while others are still working.
		if t.Status == "done" {
			for _, ms := range t.MemberStatuses {
				if ms == "running" {
					t.Status = "running"
					break
				}
			}
		}
	}
	s.mu.Unlock()
	s.save()
}

// FindByLeadSession returns the first team-mode task whose LeadSessionID
// matches sessionID, or nil if none is found.
// Used by runAgentTask to auto-close a chat-linked todo when the lead session ends.
func FindByLeadSession(workspacePath, sessionID string) *Task {
	if sessionID == "" {
		return nil
	}
	s := storeFor(workspacePath)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tasks {
		if t.LeadSessionID == sessionID {
			cp := *t
			return &cp
		}
	}
	return nil
}

// FindByStepSession returns the first task that has a step whose SessionID
// matches sessionID, along with that step's index. Returns nil if not found.
// Used by runAgentTask to auto-close a chat-linked single-step todo.
func FindByStepSession(workspacePath, sessionID string) (*Task, int) {
	if sessionID == "" {
		return nil, -1
	}
	s := storeFor(workspacePath)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tasks {
		for i, step := range t.Steps {
			if step.SessionID == sessionID {
				cp := *t
				return &cp, i
			}
		}
	}
	return nil, -1
}

// PatchStatus atomically updates just the Status field.
func PatchStatus(workspacePath, id, status string) {
	s := storeFor(workspacePath)
	s.mu.Lock()
	if t, ok := s.tasks[id]; ok {
		t.Status = status
	}
	s.mu.Unlock()
	s.save()
}

// ResetStaleRunning resets tasks that are stuck in "running" after a sidecar
// crash. Linked todos (mode=="single" with step.sessionId set, or mode=="team"
// with leadSessionId set) are left alone — the frontend will patch them via
// patchTodoStatus when their chat session completes. True runner tasks whose
// steps never got a session ID are reset to "pending" so they can be restarted.
func ResetStaleRunning(workspacePath string) {
	s := storeFor(workspacePath)
	s.mu.Lock()
	changed := false
	for _, t := range s.tasks {
		if t.Status != "running" {
			continue
		}
		// Keep team tasks that have a live lead session reference — those are
		// linked todos from chat and will be resolved by the frontend.
		if t.Mode == "team" && t.LeadSessionID != "" {
			continue
		}
		// Keep single tasks where at least one step has a session ID — the
		// frontend will call patchTodoStatus when the chat session finishes.
		hasLinkedSession := false
		for _, step := range t.Steps {
			if step.SessionID != "" {
				hasLinkedSession = true
				break
			}
		}
		if hasLinkedSession {
			continue
		}
		// True runner task stuck mid-run: reset to pending so it can be restarted.
		t.Status = "pending"
		for i := range t.Steps {
			if t.Steps[i].Status == "running" {
				t.Steps[i].Status = "pending"
				t.Steps[i].SessionID = ""
			}
		}
		t.LeadSessionID = ""
		changed = true
	}
	s.mu.Unlock()
	if changed {
		s.save()
	}
}

// DueTasks returns tasks that are pending+scheduled and past their scheduledAt time.
// It ranges over every loaded workspace store, so LoadWorkspace must have been called
// for each workspace that should participate in scheduling.
func DueTasks() []*Task {
	now := time.Now().UTC()
	var due []*Task
	stores.Range(func(_, v any) bool {
		ws := v.(*workspaceStore)
		ws.mu.RLock()
		for _, t := range ws.tasks {
			if t.Status != "pending" || t.ScheduleType != "scheduled" || t.ScheduledAt == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339, t.ScheduledAt)
			if err != nil {
				continue
			}
			if !at.After(now) {
				cp := *t
				due = append(due, &cp)
			}
		}
		ws.mu.RUnlock()
		return true
	})
	return due
}

// Workspaces returns the loaded workspace paths. It is safe to call concurrently.
func Workspaces() []string {
	var out []string
	stores.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok {
			out = append(out, s)
		}
		return true
	})
	return out
}

// CloseLinkedTodoBySession marks any todo whose step or lead session matches sessionID
// as failed and persists it. It returns the number of todos updated.
func CloseLinkedTodoBySession(sessionID string) int {
	updated := 0
	for _, ws := range Workspaces() {
		for _, t := range List(ws) {
			matched := false
			if t.Mode == "team" && t.LeadSessionID == sessionID {
				matched = true
			} else {
				for _, step := range t.Steps {
					if step.SessionID == sessionID {
						matched = true
						break
					}
				}
			}
			if !matched || t.Status == "done" || t.Status == "failed" {
				continue
			}
			t.Status = "failed"
			for i := range t.Steps {
				if t.Steps[i].Status == "running" {
					t.Steps[i].Status = "failed"
				}
			}
			for i := range t.MemberStatuses {
				if t.MemberStatuses[i] == "running" {
					t.MemberStatuses[i] = "failed"
				}
			}
			Update(ws, t)
			updated++
		}
	}
	return updated
}

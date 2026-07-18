package httpserver

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/todo"
)

func mountTodos(r chi.Router, state *apiState) {
	r.Get("/todos", listTodos(state))
	r.Post("/todos", createTodo(state))
	r.Get("/todos/{id}", getTodo(state))
	r.Patch("/todos/{id}", patchTodo(state))
	r.Delete("/todos/{id}", deleteTodo(state))
	r.Post("/todos/{id}/run", runTodo(state))
	r.Post("/todos/{id}/cancel", cancelTodo(state))
}

func listTodos(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := r.URL.Query().Get("workspace_id")
		statusFilter := r.URL.Query().Get("status")

		wsPath, ok := resolveWorkspacePath(wsID)
		if !ok {
			writeErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}

		tasks := todo.List(wsPath)
		out := tasks
		if statusFilter != "" {
			filtered := out[:0]
			for _, t := range out {
				if t.Status == statusFilter {
					filtered = append(filtered, t)
				}
			}
			out = filtered
		}
		if out == nil {
			out = []*todo.Task{}
		}
		writeJSON(w, out)
	}
}

func createTodo(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, ok := resolveWorkspacePath(wsID)
		if !ok {
			writeErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}

		var t todo.Task
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if t.Name == "" {
			writeErr(w, http.StatusBadRequest, "name is required")
			return
		}
		t.ID = uuid.NewString()
		t.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		t.Status = "pending"
		t.WorkspacePath = wsPath
		if t.Mode == "" {
			t.Mode = "single"
		}

		// Resolve provider/model for each step from agent configs.
		for i := range t.Steps {
			if t.Steps[i].Provider == "" {
				if ac := agent.LookupAgentCfg(t.Steps[i].AgentID); ac != nil {
					t.Steps[i].Provider = ac.Provider
					t.Steps[i].Model = ac.Model
				}
			}
		}
		if t.Mode == "team" && t.LeadProvider == "" {
			if ac := agent.LookupAgentCfg(t.LeadAgentID); ac != nil {
				t.LeadProvider = ac.Provider
				t.LeadModel = ac.Model
			}
		}

		todo.Add(wsPath, &t)

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, t)

		// Instant tasks start immediately.
		if t.ScheduleType != "scheduled" {
			go runTodoTask(state, t.ID, wsPath)
		}
	}
}

func getTodo(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)

		t, found := findTask(id, wsPath)
		if !found {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, t)
	}
}

func patchTodo(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)

		t, found := findTask(id, wsPath)
		if !found {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}

		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if v, ok := patch["name"].(string); ok {
			t.Name = v
		}
		if v, ok := patch["status"].(string); ok {
			t.Status = v
		}
		if v, ok := patch["scheduleType"].(string); ok {
			t.ScheduleType = v
		}
		if v, ok := patch["scheduledAt"].(string); ok {
			t.ScheduledAt = v
		}
		todo.Update(t.WorkspacePath, t)
		writeJSON(w, t)
	}
}

func deleteTodo(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)

		t, found := findTask(id, wsPath)
		if !found {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		state.cancel(id)
		todo.Remove(t.WorkspacePath, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func runTodo(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)

		t, found := findTask(id, wsPath)
		if !found {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if t.Status == "running" {
			writeErr(w, http.StatusConflict, "already running")
			return
		}
		go runTodoTask(state, id, t.WorkspacePath)
		writeJSON(w, map[string]string{"status": "started"})
	}
}

func cancelTodo(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)

		t, found := findTask(id, wsPath)
		if !found {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		state.cancel(id)
		todo.PatchStatus(t.WorkspacePath, id, "failed")
		w.WriteHeader(http.StatusNoContent)
	}
}

// findTask looks up a task by ID across a given workspace path (or all workspaces
// if wsPath is empty).
func findTask(id, wsPath string) (*todo.Task, bool) {
	if wsPath != "" {
		return todo.Get(wsPath, id)
	}
	// Scan all registered local workspaces.
	globalRegistry.mu.RLock()
	paths := make([]string, len(globalRegistry.localWorkspaces))
	for i, w := range globalRegistry.localWorkspaces {
		paths[i] = w.Path
	}
	globalRegistry.mu.RUnlock()
	for _, p := range paths {
		if t, ok := todo.Get(p, id); ok {
			return t, true
		}
	}
	return nil, false
}

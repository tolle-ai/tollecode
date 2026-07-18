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

// A schedule is a todo.Task with ScheduleType="cron" and a Cron expression.
// We store them in the same todo_tasks.json but filter by ScheduleType.

func mountSchedules(r chi.Router, state *apiState) {
	r.Get("/schedules", listSchedules(state))
	r.Post("/schedules", createSchedule(state))
	r.Get("/schedules/{id}", getSchedule(state))
	r.Patch("/schedules/{id}", patchSchedule(state))
	r.Delete("/schedules/{id}", deleteSchedule(state))
	r.Post("/schedules/{id}/run", triggerSchedule(state))
}

func listSchedules(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, ok := resolveWorkspacePath(wsID)
		if !ok {
			writeErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}
		var out []*todo.Task
		for _, t := range todo.List(wsPath) {
			if t.ScheduleType == "cron" {
				out = append(out, t)
			}
		}
		if out == nil {
			out = []*todo.Task{}
		}
		writeJSON(w, out)
	}
}

func createSchedule(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, ok := resolveWorkspacePath(wsID)
		if !ok {
			writeErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}

		var body struct {
			AgentID        string `json:"agentId"`
			Name           string `json:"name"`
			Cron           string `json:"cron"`
			Message        string `json:"message"`
			ShellAutoAllow bool   `json:"shellAutoAllow"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if body.Cron == "" || body.Message == "" {
			writeErr(w, http.StatusBadRequest, "cron and message are required")
			return
		}

		name := body.Name
		if name == "" {
			name = body.Message
			if len(name) > 60 {
				name = name[:60] + "…"
			}
		}

		provider, model := "", ""
		if ac := agent.LookupAgentCfg(body.AgentID); ac != nil {
			provider, model = ac.Provider, ac.Model
		}
		if provider == "" {
			provider, model = apiFirstProvider(state.defaultProvider, state.defaultModel)
		}

		t := &todo.Task{
			ID:            uuid.NewString(),
			Name:          name,
			Description:   body.Message,
			Mode:          "single",
			Status:        "pending",
			ScheduleType:  "cron",
			Cron:          body.Cron,
			WorkspacePath: wsPath,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
			ShellAutoAllow: body.ShellAutoAllow,
			Steps: []todo.Step{
				{
					ID:          uuid.NewString(),
					AgentID:     body.AgentID,
					Instruction: body.Message,
					OnComplete:  "finish",
					OnFail:      "finish",
					Status:      "pending",
					Provider:    provider,
					Model:       model,
				},
			},
			LeadProvider: provider,
			LeadModel:    model,
		}

		todo.Add(wsPath, t)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, t)
	}
}

func getSchedule(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)
		t, found := findTask(id, wsPath)
		if !found || t.ScheduleType != "cron" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, t)
	}
}

func patchSchedule(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)
		t, found := findTask(id, wsPath)
		if !found || t.ScheduleType != "cron" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}

		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if v, ok := patch["status"].(string); ok {
			t.Status = v
		}
		if v, ok := patch["name"].(string); ok {
			t.Name = v
		}
		if v, ok := patch["cron"].(string); ok && v != "" {
			t.Cron = v
		}
		todo.Update(t.WorkspacePath, t)
		writeJSON(w, t)
	}
}

func deleteSchedule(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)
		t, found := findTask(id, wsPath)
		if !found || t.ScheduleType != "cron" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		state.cancel(id)
		todo.Remove(t.WorkspacePath, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func triggerSchedule(state *apiState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)
		t, found := findTask(id, wsPath)
		if !found || t.ScheduleType != "cron" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if t.Status == "running" {
			writeErr(w, http.StatusConflict, "already running")
			return
		}
		// Reset step statuses for a fresh run.
		for i := range t.Steps {
			t.Steps[i].Status = "pending"
			t.Steps[i].SessionID = ""
		}
		t.Status = "pending"
		todo.Update(t.WorkspacePath, t)
		go runTodoTask(state, id, t.WorkspacePath)
		writeJSON(w, map[string]string{"status": "started"})
	}
}

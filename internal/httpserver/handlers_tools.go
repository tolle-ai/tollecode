package httpserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/mcp"
)

func mountTools(r chi.Router) {
	r.Get("/tools", listTools)
	r.Post("/tools", registerTool)
	r.Delete("/tools/{name}", deleteTool)
}

func listTools(w http.ResponseWriter, r *http.Request) {
	workspace := r.URL.Query().Get("workspace_id")
	wsPath := ""
	if workspace != "" {
		wsPath, _ = resolveWorkspacePath(workspace)
	}

	tools := agent.WorkspaceTools(context.Background(), wsPath, false, false, false, nil)
	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := make([]toolInfo, len(tools))
	for i, t := range tools {
		out[i] = toolInfo{Name: t.Name, Description: t.Description}
	}
	writeJSON(w, out)
}

func registerTool(w http.ResponseWriter, r *http.Request) {
	workspace := r.URL.Query().Get("workspace_id")
	wsPath := ""
	if workspace != "" {
		wsPath, _ = resolveWorkspacePath(workspace)
	}
	if wsPath == "" {
		writeErr(w, http.StatusBadRequest, "workspace_id is required")
		return
	}

	var tool mcp.CustomTool
	if err := json.NewDecoder(r.Body).Decode(&tool); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if tool.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	tool.Enabled = true

	tools, _ := mcp.LoadCustomTools(wsPath)
	// Replace if name already exists, otherwise append.
	replaced := false
	for i, t := range tools {
		if t.Name == tool.Name {
			tools[i] = tool
			replaced = true
			break
		}
	}
	if !replaced {
		tools = append(tools, tool)
	}
	if err := mcp.SaveCustomTools(wsPath, tools); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, tool)
}

func deleteTool(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	workspace := r.URL.Query().Get("workspace_id")
	wsPath := ""
	if workspace != "" {
		wsPath, _ = resolveWorkspacePath(workspace)
	}
	if wsPath == "" {
		writeErr(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	tools, _ := mcp.LoadCustomTools(wsPath)
	filtered := tools[:0]
	for _, t := range tools {
		if t.Name != name {
			filtered = append(filtered, t)
		}
	}
	if err := mcp.SaveCustomTools(wsPath, filtered); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package httpserver

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/todo"
)

// WorkspaceKind distinguishes local workspaces (directories on this machine)
// from remote workspaces (connections to external servers).
type WorkspaceKind string

const (
	WorkspaceLocal  WorkspaceKind = "local"
	WorkspaceRemote WorkspaceKind = "remote"
)

// workspaceRegistry is the in-memory registry of named workspaces.
// It maintains two separate slices — one for local workspaces (persisted to
// workspaces.json) and one for remote workspaces (persisted to
// remote-workspaces.json).
type workspaceRegistry struct {
	mu                sync.RWMutex
	localWorkspaces   []WorkspaceEntry
	remoteWorkspaces  []WorkspaceEntry
}

// WorkspaceEntry is a registered workspace.
// Kind is "local" or "remote". Local workspaces have a Path on this machine;
// remote workspaces have a URL pointing to an external server.
type WorkspaceEntry struct {
	ID        string        `json:"id"`
	Kind      WorkspaceKind `json:"kind"`
	Path      string        `json:"path,omitempty"`      // local: directory path; remote: unused
	Name      string        `json:"name"`
	URL       string        `json:"url,omitempty"`       // remote: server URL
	APIKey    string        `json:"apiKey,omitempty"`    // remote: server API key (stored for convenience)
	CreatedAt string        `json:"createdAt"`
}

var globalRegistry = &workspaceRegistry{}

func initWorkspaceRegistry(cfg ServerConfig) {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()

	// Load local workspaces from disk, then merge YAML config entries.
	localEntries := loadWorkspacesFile(localWorkspacesFilePath())
	idx := make(map[string]bool)
	for _, e := range localEntries {
		idx[e.ID] = true
	}
	for _, wc := range cfg.Workspaces {
		if _, ok := idx[wc.ID]; !ok {
			localEntries = append(localEntries, WorkspaceEntry{
				ID:        wc.ID,
				Kind:      WorkspaceLocal,
				Path:      wc.Path,
				Name:      wc.Name,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			})
		}
		todo.LoadWorkspace(wc.Path)
	}
	globalRegistry.localWorkspaces = localEntries
	saveWorkspacesFile(localWorkspacesFilePath(), localEntries)

	// Load remote workspaces from disk (no YAML config for these).
	remoteEntries := loadWorkspacesFile(remoteWorkspacesFilePath())
	globalRegistry.remoteWorkspaces = remoteEntries
	// Don't save if empty — only save when the user creates one.
	if len(remoteEntries) > 0 {
		saveWorkspacesFile(remoteWorkspacesFilePath(), remoteEntries)
	}
}

func resolveWorkspacePath(id string) (string, bool) {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	for _, w := range globalRegistry.localWorkspaces {
		if w.ID == id {
			return w.Path, true
		}
	}
	return "", false
}

func mountWorkspaces(r chi.Router, cfg ServerConfig) {
	initWorkspaceRegistry(cfg)

	// Local workspaces (directories on this machine)
	r.Get("/workspaces", listWorkspaces)
	r.Post("/workspaces", createWorkspaceHandler(cfg))
	r.Get("/workspaces/{id}", getWorkspace)
	r.Delete("/workspaces/{id}", deleteWorkspace)

	// Remote workspaces (connections to external servers)
	r.Get("/workspaces/remote", listRemoteWorkspaces)
	r.Post("/workspaces/remote", createRemoteWorkspaceHandler)
	r.Get("/workspaces/remote/{id}", getRemoteWorkspace)
	r.Delete("/workspaces/remote/{id}", deleteRemoteWorkspace)
}

// ── Local workspace handlers ────────────────────────────────────────────────

func listWorkspaces(w http.ResponseWriter, r *http.Request) {
	globalRegistry.mu.RLock()
	ws := make([]WorkspaceEntry, len(globalRegistry.localWorkspaces))
	copy(ws, globalRegistry.localWorkspaces)
	globalRegistry.mu.RUnlock()
	writeJSON(w, ws)
}

func createWorkspaceHandler(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID   string `json:"id"`
			Path string `json:"path"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
			http.Error(w, `{"error":"path is required"}`, http.StatusBadRequest)
			return
		}
		if body.ID == "" {
			body.ID = uuid.NewString()
		}

		// Ensure the workspace directory exists on disk.
		// Reject obviously invalid paths.
		cleanPath := filepath.Clean(body.Path)
		if cleanPath == "/" || cleanPath == "." || cleanPath == ".." {
			writeErr(w, http.StatusBadRequest, "invalid workspace path")
			return
		}

		// Check path is within allowed roots (unless allow_root_fs is enabled)
		if !isAllowed(cleanPath, cfg.AllowedRoots, cfg.AllowRootFS) {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("path %s is outside allowed roots", cleanPath))
			return
		}

		if err := os.MkdirAll(cleanPath, 0o755); err != nil {
			log.Printf("[createWorkspace] MkdirAll(%s) failed: %v", cleanPath, err)
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("could not create directory %s: %v", cleanPath, err))
			return
		}
		// Create the .agent subdirectory used for memory, todos, etc.
		if err := os.MkdirAll(filepath.Join(cleanPath, ".agent"), 0o755); err != nil {
			log.Printf("[createWorkspace] MkdirAll(.agent) failed: %v", err)
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("could not create .agent directory: %v", err))
			return
		}

		e := WorkspaceEntry{
			ID:        body.ID,
			Kind:      WorkspaceLocal,
			Path:      cleanPath,
			Name:      body.Name,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		globalRegistry.mu.Lock()
		globalRegistry.localWorkspaces = append(globalRegistry.localWorkspaces, e)
		saveWorkspacesFile(localWorkspacesFilePath(), globalRegistry.localWorkspaces)
		globalRegistry.mu.Unlock()

		todo.LoadWorkspace(cleanPath)

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, e)
	}
}

func getWorkspace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	for _, e := range globalRegistry.localWorkspaces {
		if e.ID == id {
			writeJSON(w, e)
			return
		}
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

func deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()
	filtered := globalRegistry.localWorkspaces[:0]
	found := false
	for _, e := range globalRegistry.localWorkspaces {
		if e.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, e)
	}
	if !found {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	globalRegistry.localWorkspaces = filtered
	saveWorkspacesFile(localWorkspacesFilePath(), filtered)
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote workspace handlers ───────────────────────────────────────────────

func listRemoteWorkspaces(w http.ResponseWriter, r *http.Request) {
	globalRegistry.mu.RLock()
	ws := make([]WorkspaceEntry, len(globalRegistry.remoteWorkspaces))
	copy(ws, globalRegistry.remoteWorkspaces)
	globalRegistry.mu.RUnlock()
	writeJSON(w, ws)
}

func createRemoteWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		URL    string `json:"url"`
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if body.URL == "" {
		http.Error(w, `{"error":"url is required"}`, http.StatusBadRequest)
		return
	}
	if body.ID == "" {
		body.ID = uuid.NewString()
	}

	e := WorkspaceEntry{
		ID:        body.ID,
		Kind:      WorkspaceRemote,
		Name:      body.Name,
		URL:       body.URL,
		APIKey:    body.APIKey,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	globalRegistry.mu.Lock()
	globalRegistry.remoteWorkspaces = append(globalRegistry.remoteWorkspaces, e)
	saveWorkspacesFile(remoteWorkspacesFilePath(), globalRegistry.remoteWorkspaces)
	globalRegistry.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, e)
}

func getRemoteWorkspace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	for _, e := range globalRegistry.remoteWorkspaces {
		if e.ID == id {
			writeJSON(w, e)
			return
		}
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

func deleteRemoteWorkspace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()
	filtered := globalRegistry.remoteWorkspaces[:0]
	found := false
	for _, e := range globalRegistry.remoteWorkspaces {
		if e.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, e)
	}
	if !found {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	globalRegistry.remoteWorkspaces = filtered
	saveWorkspacesFile(remoteWorkspacesFilePath(), filtered)
	w.WriteHeader(http.StatusNoContent)
}

// ── persistence ───────────────────────────────────────────────────────────────

func homeDir() string {
	home := config.Home()
	if !filepath.IsAbs(home) {
		home, _ = filepath.Abs(home)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		log.Printf("[homeDir] could not create home directory %s: %v", home, err)
	}
	return home
}

// localWorkspacesFilePath returns the path for local workspace entries.
func localWorkspacesFilePath() string {
	return filepath.Join(homeDir(), "workspaces.json")
}

// remoteWorkspacesFilePath returns the path for remote workspace entries.
func remoteWorkspacesFilePath() string {
	return filepath.Join(homeDir(), "remote-workspaces.json")
}

func loadWorkspacesFile(path string) []WorkspaceEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []WorkspaceEntry
	_ = json.Unmarshal(data, &entries)
	// Migrate: entries without a Kind default to "local"
	for i := range entries {
		if entries[i].Kind == "" {
			entries[i].Kind = WorkspaceLocal
		}
	}
	return entries
}

func saveWorkspacesFile(path string, entries []WorkspaceEntry) {
	data, _ := json.MarshalIndent(entries, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}
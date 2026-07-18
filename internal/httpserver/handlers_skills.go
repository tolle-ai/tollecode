package httpserver

// handlers_skills.go — REST API for skill management and execution.
//
// Skills are Markdown files with YAML frontmatter stored in:
//   global:    ~/.tollecode/skills/<name>.md
//   workspace: {ws}/.agent/skills/<name>.md
//
// Routes:
//   GET    /v1/skills             list all skills (global + workspace)
//   POST   /v1/skills             create a skill
//   GET    /v1/skills/{name}      get a skill by name
//   PATCH  /v1/skills/{name}      update a skill
//   DELETE /v1/skills/{name}      delete a skill
//   POST   /v1/skills/{name}/run  run a skill (SSE streaming)

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
)

func mountSkills(r chi.Router, state *apiState, cfg ServerConfig) {
	r.Get("/skills", listSkillsHandler(cfg))
	r.Post("/skills", createSkillHandler(cfg))
	r.Get("/skills/{name}", getSkillHandler(cfg))
	r.Patch("/skills/{name}", patchSkillHandler(cfg))
	r.Delete("/skills/{name}", deleteSkillHandler(cfg))
	r.Post("/skills/{name}/run", runSkillHandler(state, cfg))
}

// ── skill record ──────────────────────────────────────────────────────────────

type skillRecord struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Scope       string `json:"scope"`  // "global" | "workspace"
	FilePath    string `json:"filePath"`
}

// ── list ──────────────────────────────────────────────────────────────────────

func listSkillsHandler(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)

		var skills []skillRecord
		skills = append(skills, scanSkillDir(filepath.Join(config.Home(), "skills"), "global")...)
		if wsPath != "" {
			skills = append(skills, scanSkillDir(filepath.Join(wsPath, ".agent", "skills"), "workspace")...)
		}
		if skills == nil {
			skills = []skillRecord{}
		}
		writeJSON(w, skills)
	}
}

// ── get ───────────────────────────────────────────────────────────────────────

func getSkillHandler(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)
		sk, err := findSkill(name, wsPath)
		if err != nil {
			writeErr(w, http.StatusNotFound, "skill not found")
			return
		}
		writeJSON(w, sk)
	}
}

// ── create ────────────────────────────────────────────────────────────────────

func createSkillHandler(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)

		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Body        string `json:"body"`
			Scope       string `json:"scope"` // "global" | "workspace"
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeErr(w, http.StatusBadRequest, "name is required")
			return
		}

		var dir string
		if body.Scope == "workspace" && wsPath != "" {
			dir = filepath.Join(wsPath, ".agent", "skills")
		} else {
			dir = filepath.Join(config.Home(), "skills")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		filename := sanitizeSkillName(body.Name) + ".md"
		if filename == ".md" {
			filename = "skill-" + fmt.Sprintf("%d", time.Now().UnixNano()) + ".md"
		}
		fp := filepath.Join(dir, filename)
		content := "---\nname: " + body.Name + "\ndescription: " + body.Description + "\nversion: 1.0\n---\n\n" + body.Body
		if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		scope := body.Scope
		if scope == "" {
			scope = "global"
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, skillRecord{
			Name: body.Name, Description: body.Description,
			Body: body.Body, Scope: scope, FilePath: fp,
		})
	}
}

// ── patch ─────────────────────────────────────────────────────────────────────

func patchSkillHandler(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)
		sk, err := findSkill(name, wsPath)
		if err != nil {
			writeErr(w, http.StatusNotFound, "skill not found")
			return
		}

		var patch struct {
			Description string `json:"description"`
			Body        string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if patch.Description != "" {
			sk.Description = patch.Description
		}
		if patch.Body != "" {
			sk.Body = patch.Body
		}

		content := "---\nname: " + sk.Name + "\ndescription: " + sk.Description + "\nversion: 1.0\n---\n\n" + sk.Body
		if err := os.WriteFile(sk.FilePath, []byte(content), 0o644); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, sk)
	}
}

// ── delete ────────────────────────────────────────────────────────────────────

func deleteSkillHandler(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, _ := resolveWorkspacePath(wsID)
		sk, err := findSkill(name, wsPath)
		if err != nil {
			writeErr(w, http.StatusNotFound, "skill not found")
			return
		}
		if err := os.Remove(sk.FilePath); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ── run (SSE) ─────────────────────────────────────────────────────────────────

func runSkillHandler(state *apiState, cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		wsID := r.URL.Query().Get("workspace_id")
		wsPath, ok := resolveWorkspacePath(wsID)
		if !ok {
			writeErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}

		sk, err := findSkill(name, wsPath)
		if err != nil {
			writeErr(w, http.StatusNotFound, "skill not found")
			return
		}

		var body struct {
			Message        string `json:"message"`
			ShellAutoAllow *bool  `json:"shellAutoAllow"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		message := body.Message
		if message == "" {
			message = "Run the " + sk.Name + " skill."
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		provider, model := apiFirstProvider(state.defaultProvider, state.defaultModel)
		sess, err := session.Create(wsPath, provider, model, "build",
			session.WithSkills([]string{sk.Name}),
		)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")

		var writeMu sync.Mutex
		emitSSE := func(event map[string]any) {
			data, _ := json.Marshal(event)
			writeMu.Lock()
			fmt.Fprintf(w, "data: %s\n\n", data) //nolint:errcheck
			flusher.Flush()
			writeMu.Unlock()
		}

		shellAutoAllow := cfg.Tools.ShellAllowed
		if body.ShellAutoAllow != nil {
			shellAutoAllow = *body.ShellAutoAllow
		}

		ctx, cancelFn := context.WithCancel(r.Context())
		done := make(chan struct{})
		state.register(sess.ID, cancelFn, done)

		session.ClearLiveEvents(sess.ID)
		session.Global.ClearBuffer(sess.ID)
		session.UpdateFields(wsPath, sess.ID, map[string]any{"status": "running"})
		session.RegisterSession(sess.ID, wsPath, "api")

		go func() {
			defer close(done)
			defer state.remove(sess.ID)
			defer session.UnregisterSession(sess.ID)

			agentCfg := agent.Config{
				SessionID:          sess.ID,
				Workspace:          wsPath,
				Message:            message,
				Mode:               "build",
				ShellAutoAllow:     shellAutoAllow,
				CustomInstructions: sk.Body,
				EmitFn: func(event map[string]any) {
					off, _ := session.AppendLiveEvent(sess.ID, event)
					event["_off"] = off
					session.Global.Publish(sess.ID, event)
					emitSSE(event)
				},
				RequestPerm: func(ctx context.Context, command string) (bool, bool) {
					event := map[string]any{
						"type": "pending_permission", "session_id": sess.ID, "command": command,
					}
					off, _ := session.AppendLiveEvent(sess.ID, event)
					event["_off"] = off
					session.Global.Publish(sess.ID, event)
					emitSSE(event)
					ch := state.permQueue(sess.ID)
					select {
					case resp := <-ch:
						return resp.Allow, resp.AllowAll
					case <-time.After(60 * time.Second):
						return false, false
					case <-ctx.Done():
						return false, false
					}
				},
			}
			agent.Execute(ctx, agentCfg)
		}()

		<-done
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func scanSkillDir(dir, scope string) []skillRecord {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []skillRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		fp := filepath.Join(dir, e.Name())
		name, desc, body := parseSkillFile(fp)
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".md")
		}
		out = append(out, skillRecord{
			Name: name, Description: desc, Body: body,
			Scope: scope, FilePath: fp,
		})
	}
	return out
}

func parseSkillFile(path string) (name, desc, body string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	inFM := false
	pastFM := false
	var bodyLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if !inFM && !pastFM && line == "---" {
			inFM = true
			continue
		}
		if inFM {
			if line == "---" {
				inFM = false
				pastFM = true
				continue
			}
			if kv := strings.SplitN(line, ":", 2); len(kv) == 2 {
				switch strings.TrimSpace(kv[0]) {
				case "name":
					name = strings.TrimSpace(kv[1])
				case "description":
					desc = strings.TrimSpace(kv[1])
				}
			}
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	body = strings.Join(bodyLines, "\n")
	return
}

func findSkill(name, wsPath string) (skillRecord, error) {
	dirs := []struct{ dir, scope string }{
		{filepath.Join(config.Home(), "skills"), "global"},
	}
	if wsPath != "" {
		dirs = append(dirs, struct{ dir, scope string }{
			filepath.Join(wsPath, ".agent", "skills"), "workspace",
		})
	}
	for _, d := range dirs {
		for _, sk := range scanSkillDir(d.dir, d.scope) {
			if strings.EqualFold(sk.Name, name) || sk.Name == name {
				return sk, nil
			}
		}
	}
	return skillRecord{}, fmt.Errorf("not found")
}

func sanitizeSkillName(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

package stdio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/config"
)

type agentRecord struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Color        string   `json:"color"`
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	SystemPrompt string   `json:"systemPrompt"`
	Photo        string   `json:"photo"`
	Gradient     string   `json:"gradient"`
	Skills       []string `json:"skills"`
	Permissions  []string `json:"permissions"`
	Status       string   `json:"status"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt,omitempty"`
	LastActive   *string  `json:"lastActive"`
	OwnerID      string   `json:"ownerId,omitempty"`
	TeamID       string   `json:"teamId,omitempty"`
}

func agentsFilePath() string {
	return filepath.Join(config.Home(), "agents.json")
}

func loadAgents() []agentRecord {
	data, err := os.ReadFile(agentsFilePath())
	if err != nil {
		return []agentRecord{}
	}
	var list []agentRecord
	if json.Unmarshal(data, &list) != nil {
		return []agentRecord{}
	}
	return list
}

func saveAgents(list []agentRecord) {
	data, _ := json.MarshalIndent(list, "", "  ")
	_ = os.WriteFile(agentsFilePath(), data, 0o644)
}

func agentToMap(a agentRecord) map[string]any {
	skills := a.Skills
	if skills == nil {
		skills = []string{}
	}
	perms := a.Permissions
	if perms == nil {
		perms = []string{}
	}
	return map[string]any{
		"id": a.ID, "name": a.Name, "role": a.Role, "color": a.Color,
		"provider": a.Provider, "model": a.Model, "systemPrompt": a.SystemPrompt,
		"photo": a.Photo, "gradient": a.Gradient,
		"skills": skills, "permissions": perms,
		"status": a.Status, "createdAt": a.CreatedAt, "lastActive": a.LastActive,
		"updatedAt": a.UpdatedAt,
		"ownerId":   a.OwnerID, "teamId": a.TeamID,
	}
}

func handleAgentsList(state *ServerState, cmd map[string]any) {
	list := loadAgents()
	out := make([]map[string]any, len(list))
	for i, a := range list {
		out[i] = agentToMap(a)
	}
	Emit(map[string]any{"type": "agents_list", "agents": out})
}

func handleAgentsCreate(state *ServerState, cmd map[string]any) {
	name, _ := cmd["name"].(string)
	skills := toStringSlice(cmd["skills"])
	perms := toStringSlice(cmd["permissions"])
	a := agentRecord{
		ID:           uuid.NewString(),
		Name:         name,
		Role:         strField(cmd, "role"),
		Color:        strField(cmd, "color"),
		Provider:     strField(cmd, "provider"),
		Model:        strField(cmd, "model"),
		SystemPrompt: strField(cmd, "systemPrompt"),
		Photo:        strField(cmd, "photo"),
		Gradient:     strField(cmd, "gradient"),
		Skills:       skills,
		Permissions:  perms,
		Status:       "idle",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		LastActive:   nil,
		OwnerID:      "local",
	}
	list := loadAgents()
	list = append(list, a)
	saveAgents(list)
	Emit(map[string]any{"type": "agent_created", "agent": agentToMap(a)})
}

func handleAgentsUpdate(state *ServerState, cmd map[string]any) {
	id, _ := cmd["id"].(string)
	list := loadAgents()
	var updated *agentRecord
	for i := range list {
		if list[i].ID != id {
			continue
		}
		if v, ok := cmd["name"].(string); ok && v != "" {
			list[i].Name = v
		}
		if v, ok := cmd["role"].(string); ok {
			list[i].Role = v
		}
		if v, ok := cmd["color"].(string); ok {
			list[i].Color = v
		}
		if v, ok := cmd["provider"].(string); ok {
			list[i].Provider = v
		}
		if v, ok := cmd["model"].(string); ok {
			list[i].Model = v
		}
		if v, ok := cmd["systemPrompt"].(string); ok {
			list[i].SystemPrompt = v
		}
		if v, ok := cmd["photo"].(string); ok {
			list[i].Photo = v
		}
		if v, ok := cmd["gradient"].(string); ok {
			list[i].Gradient = v
		}
		if v := toStringSlice(cmd["skills"]); v != nil {
			list[i].Skills = v
		}
		if v := toStringSlice(cmd["permissions"]); v != nil {
			list[i].Permissions = v
		}
		list[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		updated = &list[i]
		break
	}
	saveAgents(list)
	if updated != nil {
		Emit(map[string]any{"type": "agent_updated", "agent": agentToMap(*updated)})
	} else {
		Emit(map[string]any{"type": "agent_updated", "error": "agent not found"})
	}
}

func handleAgentsDelete(state *ServerState, cmd map[string]any) {
	id, _ := cmd["id"].(string)
	list := loadAgents()
	filtered := list[:0]
	for _, a := range list {
		if a.ID != id {
			filtered = append(filtered, a)
		}
	}
	saveAgents(filtered)
	Emit(map[string]any{"type": "agent_deleted", "ok": true, "id": id})
}

func handleAgentsDuplicate(state *ServerState, cmd map[string]any) {
	id, _ := cmd["id"].(string)
	list := loadAgents()
	var src *agentRecord
	for i := range list {
		if list[i].ID == id {
			src = &list[i]
			break
		}
	}
	if src == nil {
		Emit(map[string]any{"type": "agent_duplicated", "error": "agent not found"})
		return
	}
	skills := append([]string{}, src.Skills...)
	perms := append([]string{}, src.Permissions...)
	copy_ := agentRecord{
		ID: uuid.NewString(), Name: src.Name + " (copy)",
		Role: src.Role, Color: src.Color,
		Provider: src.Provider, Model: src.Model,
		SystemPrompt: src.SystemPrompt,
		Photo:        src.Photo,
		Gradient:     src.Gradient,
		Skills:       skills, Permissions: perms,
		Status:     "idle",
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		LastActive: nil,
	}
	list = append(list, copy_)
	saveAgents(list)
	Emit(map[string]any{"type": "agent_duplicated", "agent": agentToMap(copy_)})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func strField(cmd map[string]any, key string) string {
	v, _ := cmd[key].(string)
	return v
}

func toStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

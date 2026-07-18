package stdio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/config"
)

type teamRecord struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	Color          string   `json:"color"`
	Initial        string   `json:"initial"`
	LeadAgentID    string   `json:"leadAgentId"`
	MemberAgentIDs []string `json:"memberAgentIds"`
	CreatedAt      string   `json:"createdAt"`
}

func teamsFilePath() string {
	return filepath.Join(config.Home(), "teams.json")
}

func loadTeams() []teamRecord {
	data, err := os.ReadFile(teamsFilePath())
	if err != nil {
		return []teamRecord{}
	}
	var list []teamRecord
	if json.Unmarshal(data, &list) != nil {
		return []teamRecord{}
	}
	return list
}

func saveTeams(list []teamRecord) {
	data, _ := json.MarshalIndent(list, "", "  ")
	_ = os.WriteFile(teamsFilePath(), data, 0o644)
}

// findTeam looks up a team by exact ID or case-insensitive name prefix.
func findTeam(nameOrID string) *teamRecord {
	lower := lowerStr(nameOrID)
	for _, t := range loadTeams() {
		if t.ID == nameOrID {
			return &t
		}
		if lowerStr(t.Name) == lower {
			return &t
		}
	}
	// prefix match on name
	for _, t := range loadTeams() {
		if len(t.Name) >= len(nameOrID) && lowerStr(t.Name[:len(nameOrID)]) == lower {
			return &t
		}
	}
	return nil
}

func lowerStr(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// handleGetTeams returns all teams stored in teams.json.
func handleGetTeams(_ *ServerState, _ map[string]any) {
	teams := loadTeams()
	out := make([]map[string]any, 0, len(teams))
	for _, t := range teams {
		members := t.MemberAgentIDs
		if members == nil {
			members = []string{}
		}
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "description": t.Description,
			"color": t.Color, "initial": t.Initial,
			"leadAgentId": t.LeadAgentID, "memberAgentIds": members,
			"createdAt": t.CreatedAt,
		})
	}
	Emit(map[string]any{"type": "teams_list", "teams": out})
}

// handleSyncTeams replaces the local teams.json with the array sent by the desktop.
// Called on workspace load so the CLI always has an up-to-date snapshot.
func handleSyncTeams(_ *ServerState, cmd map[string]any) {
	raw, _ := cmd["teams"].([]any)
	var list []teamRecord
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		members := []string{}
		if rawM, ok := m["memberAgentIds"].([]any); ok {
			for _, x := range rawM {
				if s, ok := x.(string); ok && s != "" {
					members = append(members, s)
				}
			}
		}
		list = append(list, teamRecord{
			ID:             id,
			Name:           strVal(m, "name"),
			Description:    strVal(m, "description"),
			Color:          strVal(m, "color"),
			Initial:        strVal(m, "initial"),
			LeadAgentID:    strVal(m, "leadAgentId"),
			MemberAgentIDs: members,
			CreatedAt:      strVal(m, "createdAt"),
		})
	}
	if list == nil {
		list = []teamRecord{}
	}
	saveTeams(list)
	Emit(map[string]any{"type": "teams_synced", "count": len(list)})
}

// handleUpsertTeam creates or updates a single team.
func handleUpsertTeam(_ *ServerState, cmd map[string]any) {
	m, _ := cmd["team"].(map[string]any)
	if m == nil {
		Emit(map[string]any{"type": "team_saved", "ok": false, "error": "missing team"})
		return
	}
	id, _ := m["id"].(string)
	if id == "" {
		id = uuid.New().String()
	}
	members := []string{}
	if rawM, ok := m["memberAgentIds"].([]any); ok {
		for _, x := range rawM {
			if s, ok := x.(string); ok && s != "" {
				members = append(members, s)
			}
		}
	}
	rec := teamRecord{
		ID:             id,
		Name:           strVal(m, "name"),
		Description:    strVal(m, "description"),
		Color:          strVal(m, "color"),
		Initial:        strVal(m, "initial"),
		LeadAgentID:    strVal(m, "leadAgentId"),
		MemberAgentIDs: members,
		CreatedAt:      strVal(m, "createdAt"),
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	list := loadTeams()
	found := false
	for i, t := range list {
		if t.ID == id {
			list[i] = rec
			found = true
			break
		}
	}
	if !found {
		list = append([]teamRecord{rec}, list...)
	}
	saveTeams(list)
	Emit(map[string]any{"type": "team_saved", "ok": true, "id": id})
}

// handleDeleteTeam removes a team by ID.
func handleDeleteTeam(_ *ServerState, cmd map[string]any) {
	id, _ := cmd["id"].(string)
	list := loadTeams()
	filtered := list[:0]
	for _, t := range list {
		if t.ID != id {
			filtered = append(filtered, t)
		}
	}
	saveTeams(filtered)
	Emit(map[string]any{"type": "team_deleted", "ok": true, "id": id})
}

func strVal(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

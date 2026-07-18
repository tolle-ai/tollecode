package httpserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
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
	Skills       []string `json:"skills"`
	Permissions  []string `json:"permissions"`
	Status       string   `json:"status"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt,omitempty"`
	OwnerID      string   `json:"ownerId,omitempty"`
	TeamID       string   `json:"teamId,omitempty"`
}

func mountAgents(r chi.Router) {
	r.Get("/agents", listAgents)
	r.Post("/agents", createAgent)
	r.Get("/agents/{id}", getAgent)
	r.Patch("/agents/{id}", updateAgent)
	r.Delete("/agents/{id}", deleteAgent)
	r.Post("/agents/{id}/transfer", transferAgent)
}

// filterAgentsForUser returns agents visible to the given user:
//   - Legacy agents (ownerId == "") → global, visible to all
//   - Personal agents (ownerId == userID, teamId == "") → owner only
//   - Team agents (teamId != "") → visible to members of that team
func filterAgentsForUser(all []agentRecord, user *userRecord) []agentRecord {
	// system user and admins see everything
	if user.ID == "system" || user.Role == "admin" {
		return all
	}

	memberTeamIDs := getUserTeamIDs(user.ID)
	memberSet := make(map[string]bool, len(memberTeamIDs))
	for _, tid := range memberTeamIDs {
		memberSet[tid] = true
	}

	out := make([]agentRecord, 0, len(all))
	for _, a := range all {
		switch {
		case a.OwnerID == "":
			out = append(out, a) // global/legacy
		case a.TeamID != "" && memberSet[a.TeamID]:
			out = append(out, a) // team agent, user is a member
		case a.OwnerID == user.ID && a.TeamID == "":
			out = append(out, a) // personal agent
		}
	}
	return out
}

// canModifyAgent returns true if the user may edit or delete the agent.
func canModifyAgent(a agentRecord, user *userRecord) bool {
	if user.Role == "admin" || user.ID == "system" {
		return true
	}
	if a.OwnerID == user.ID {
		return true
	}
	// Team agents: only the team owner may modify them
	if a.TeamID != "" {
		teams := loadHumanTeams()
		for _, t := range teams {
			if t.ID == a.TeamID {
				return t.OwnerID == user.ID
			}
		}
	}
	return false
}

func listAgents(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	all := loadAgentRecords()
	visible := filterAgentsForUser(all, user)
	if visible == nil {
		visible = []agentRecord{}
	}
	writeJSON(w, visible)
}

func createAgent(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var a agentRecord
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if a.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	t := time.Now().UTC().Format(time.RFC3339)
	a.ID = uuid.NewString()
	a.CreatedAt = t
	a.UpdatedAt = t
	if a.Status == "" {
		a.Status = "active"
	}
	if a.Skills == nil {
		a.Skills = []string{}
	}
	if a.Permissions == nil {
		a.Permissions = []string{}
	}
	// Stamp ownership: system user creates global agents (ownerId=""),
	// all other users create personal agents by default.
	if a.OwnerID == "" && user.ID != "system" {
		a.OwnerID = user.ID
	}
	// If a teamId is specified, verify the caller is a member of that team.
	if a.TeamID != "" {
		teams := loadHumanTeams()
		allowed := false
		for _, team := range teams {
			if team.ID == a.TeamID {
				for _, mid := range team.MemberIDs {
					if mid == user.ID {
						allowed = true
						break
					}
				}
				break
			}
		}
		if !allowed && user.Role != "admin" && user.ID != "system" {
			writeErr(w, http.StatusForbidden, "not a member of the specified team")
			return
		}
	}

	agents := loadAgentRecords()
	agents = append(agents, a)
	saveAgentRecords(agents)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, a)
}

func getAgent(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := chi.URLParam(r, "id")
	all := loadAgentRecords()
	visible := filterAgentsForUser(all, user)
	for _, a := range visible {
		if a.ID == id {
			writeJSON(w, a)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func updateAgent(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := chi.URLParam(r, "id")
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	agents := loadAgentRecords()
	for i, a := range agents {
		if a.ID != id {
			continue
		}
		if !canModifyAgent(a, user) {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		if v, ok := patch["name"].(string); ok {
			agents[i].Name = v
		}
		if v, ok := patch["role"].(string); ok {
			agents[i].Role = v
		}
		if v, ok := patch["color"].(string); ok {
			agents[i].Color = v
		}
		if v, ok := patch["provider"].(string); ok {
			agents[i].Provider = v
		}
		if v, ok := patch["model"].(string); ok {
			agents[i].Model = v
		}
		if v, ok := patch["systemPrompt"].(string); ok {
			agents[i].SystemPrompt = v
		}
		if v, ok := patch["status"].(string); ok {
			agents[i].Status = v
		}
		if rawSkills, ok := patch["skills"].([]any); ok {
			skills := make([]string, 0, len(rawSkills))
			for _, s := range rawSkills {
				if str, ok := s.(string); ok {
					skills = append(skills, str)
				}
			}
			agents[i].Skills = skills
		}
		if rawPerms, ok := patch["permissions"].([]any); ok {
			perms := make([]string, 0, len(rawPerms))
			for _, p := range rawPerms {
				if str, ok := p.(string); ok {
					perms = append(perms, str)
				}
			}
			agents[i].Permissions = perms
		}
		agents[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		saveAgentRecords(agents)
		writeJSON(w, agents[i])
		return
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func deleteAgent(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := chi.URLParam(r, "id")
	agents := loadAgentRecords()
	filtered := agents[:0]
	found := false
	for _, a := range agents {
		if a.ID == id {
			found = true
			if !canModifyAgent(a, user) {
				writeErr(w, http.StatusForbidden, "forbidden")
				return
			}
			continue
		}
		filtered = append(filtered, a)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	saveAgentRecords(filtered)
	w.WriteHeader(http.StatusNoContent)
}

// transferAgent moves a personal agent into a team's agent pool.
func transferAgent(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := chi.URLParam(r, "id")

	var body struct {
		TeamID string `json:"teamId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.TeamID == "" {
		writeErr(w, http.StatusBadRequest, "teamId is required")
		return
	}

	// Verify caller is a member of the target team
	teams := loadHumanTeams()
	teamFound := false
	for _, t := range teams {
		if t.ID == body.TeamID {
			teamFound = true
			if user.Role != "admin" {
				isMember := false
				for _, mid := range t.MemberIDs {
					if mid == user.ID {
						isMember = true
						break
					}
				}
				if !isMember {
					writeErr(w, http.StatusForbidden, "not a member of the specified team")
					return
				}
			}
			break
		}
	}
	if !teamFound {
		writeErr(w, http.StatusNotFound, "team not found")
		return
	}

	agents := loadAgentRecords()
	for i, a := range agents {
		if a.ID != id {
			continue
		}
		if !canModifyAgent(a, user) {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		agents[i].TeamID = body.TeamID
		agents[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		saveAgentRecords(agents)
		writeJSON(w, agents[i])
		return
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func agentsFilePath() string {
	return filepath.Join(config.Home(), "agents.json")
}

func loadAgentRecords() []agentRecord {
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

func saveAgentRecords(list []agentRecord) {
	data, _ := json.MarshalIndent(list, "", "  ")
	_ = os.WriteFile(agentsFilePath(), data, 0o644)
}

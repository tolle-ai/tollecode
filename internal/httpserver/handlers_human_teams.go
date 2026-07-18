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

type humanTeamRecord struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Color       string   `json:"color"`
	OwnerID     string   `json:"ownerId"`
	MemberIDs   []string `json:"memberIds"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt,omitempty"`
}

func humanTeamsFilePath() string {
	return filepath.Join(config.Home(), "human-teams.json")
}

func loadHumanTeams() []humanTeamRecord {
	data, err := os.ReadFile(humanTeamsFilePath())
	if err != nil {
		return []humanTeamRecord{}
	}
	var list []humanTeamRecord
	if json.Unmarshal(data, &list) != nil {
		return []humanTeamRecord{}
	}
	return list
}

func saveHumanTeams(list []humanTeamRecord) {
	data, _ := json.MarshalIndent(list, "", "  ")
	_ = os.WriteFile(humanTeamsFilePath(), data, 0o644)
}

// getUserTeamIDs returns the IDs of all human teams where userID is a member.
func getUserTeamIDs(userID string) []string {
	if userID == "" || userID == "system" {
		return nil
	}
	var ids []string
	for _, t := range loadHumanTeams() {
		for _, mid := range t.MemberIDs {
			if mid == userID {
				ids = append(ids, t.ID)
				break
			}
		}
	}
	return ids
}

func mountHumanTeams(r chi.Router) {
	r.Get("/human-teams", listHumanTeamsHandler)
	r.Post("/human-teams", createHumanTeamHandler)
	r.Get("/human-teams/{id}", getHumanTeamHandler)
	r.Patch("/human-teams/{id}", updateHumanTeamHandler)
	r.Delete("/human-teams/{id}", deleteHumanTeamHandler)
	r.Post("/human-teams/{id}/members", addTeamMemberHandler)
	r.Delete("/human-teams/{id}/members/{userId}", removeTeamMemberHandler)
}

func listHumanTeamsHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	teams := loadHumanTeams()

	// Admins and system user see all teams
	if u.Role == "admin" || u.ID == "system" {
		if teams == nil {
			teams = []humanTeamRecord{}
		}
		writeJSON(w, teams)
		return
	}

	out := make([]humanTeamRecord, 0)
	for _, t := range teams {
		for _, mid := range t.MemberIDs {
			if mid == u.ID {
				out = append(out, t)
				break
			}
		}
	}
	writeJSON(w, out)
}

func createHumanTeamHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var body humanTeamRecord
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	body.ID = "team_" + uuid.NewString()
	body.OwnerID = u.ID
	body.CreatedAt = now
	body.UpdatedAt = ""

	// Ensure owner is in the members list
	if body.MemberIDs == nil {
		body.MemberIDs = []string{}
	}
	if u.ID != "system" {
		ownerPresent := false
		for _, mid := range body.MemberIDs {
			if mid == u.ID {
				ownerPresent = true
				break
			}
		}
		if !ownerPresent {
			body.MemberIDs = append([]string{u.ID}, body.MemberIDs...)
		}
	}

	teams := loadHumanTeams()
	teams = append(teams, body)
	saveHumanTeams(teams)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, body)
}

func getHumanTeamHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	for _, t := range loadHumanTeams() {
		if t.ID == id {
			writeJSON(w, t)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func updateHumanTeamHandler(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r)
	id := chi.URLParam(r, "id")

	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	teams := loadHumanTeams()
	for i, t := range teams {
		if t.ID != id {
			continue
		}
		if caller.Role != "admin" && caller.ID != t.OwnerID {
			writeErr(w, http.StatusForbidden, "only team owner or admin may update")
			return
		}
		if v, ok := patch["name"].(string); ok {
			teams[i].Name = v
		}
		if v, ok := patch["description"].(string); ok {
			teams[i].Description = v
		}
		if v, ok := patch["color"].(string); ok {
			teams[i].Color = v
		}
		teams[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		saveHumanTeams(teams)
		writeJSON(w, teams[i])
		return
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func deleteHumanTeamHandler(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r)
	id := chi.URLParam(r, "id")

	teams := loadHumanTeams()
	filtered := teams[:0]
	found := false
	for _, t := range teams {
		if t.ID == id {
			found = true
			if caller.Role != "admin" && caller.ID != t.OwnerID {
				writeErr(w, http.StatusForbidden, "only team owner or admin may delete")
				return
			}
			continue
		}
		filtered = append(filtered, t)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	saveHumanTeams(filtered)
	w.WriteHeader(http.StatusNoContent)
}

func addTeamMemberHandler(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r)
	id := chi.URLParam(r, "id")

	var body struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.UserID == "" {
		writeErr(w, http.StatusBadRequest, "userId is required")
		return
	}

	teams := loadHumanTeams()
	for i, t := range teams {
		if t.ID != id {
			continue
		}
		if caller.Role != "admin" && caller.ID != t.OwnerID {
			writeErr(w, http.StatusForbidden, "only team owner or admin may add members")
			return
		}
		for _, mid := range t.MemberIDs {
			if mid == body.UserID {
				// Already a member — idempotent
				writeJSON(w, teams[i])
				return
			}
		}
		teams[i].MemberIDs = append(teams[i].MemberIDs, body.UserID)
		teams[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		saveHumanTeams(teams)
		writeJSON(w, teams[i])
		return
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func removeTeamMemberHandler(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r)
	id := chi.URLParam(r, "id")
	targetUserID := chi.URLParam(r, "userId")

	teams := loadHumanTeams()
	for i, t := range teams {
		if t.ID != id {
			continue
		}
		// Owner, admin, or the member themselves may remove
		if caller.Role != "admin" && caller.ID != t.OwnerID && caller.ID != targetUserID {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		// Cannot remove the owner
		if targetUserID == t.OwnerID {
			writeErr(w, http.StatusBadRequest, "cannot remove team owner")
			return
		}
		filtered := make([]string, 0, len(t.MemberIDs))
		for _, mid := range t.MemberIDs {
			if mid != targetUserID {
				filtered = append(filtered, mid)
			}
		}
		teams[i].MemberIDs = filtered
		teams[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		saveHumanTeams(teams)
		writeJSON(w, teams[i])
		return
	}
	writeErr(w, http.StatusNotFound, "not found")
}

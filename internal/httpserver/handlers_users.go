package httpserver

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func mountUsers(r chi.Router) {
	r.Get("/users", listUsers)
	r.Post("/users", createUser)
	r.Get("/users/me", getMe)
	r.Get("/users/{id}", getUser)
	r.Patch("/users/{id}", updateUser)
	r.Delete("/users/{id}", deleteUser)
}

func listUsers(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r)
	if caller.Role != "admin" {
		writeErr(w, http.StatusForbidden, "admin only")
		return
	}
	users := loadUsers()
	out := make([]map[string]any, len(users))
	for i, u := range users {
		out[i] = userPublic(u)
	}
	writeJSON(w, out)
}

func createUser(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r)
	existing := loadUsers()

	// First POST always succeeds (bootstrap). After that, only admins may create users.
	if len(existing) > 0 && caller.Role != "admin" {
		writeErr(w, http.StatusForbidden, "admin only")
		return
	}

	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}

	role := body.Role
	if role == "" {
		if len(existing) == 0 {
			role = "admin" // first user is always admin
		} else {
			role = "member"
		}
	}
	// Non-admins cannot elevate to admin
	if role == "admin" && len(existing) > 0 && caller.Role != "admin" {
		role = "member"
	}

	newUser := userRecord{
		ID:        "user_" + uuid.NewString(),
		Name:      body.Name,
		Email:     body.Email,
		APIKey:    uuid.NewString(),
		Role:      role,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	existing = append(existing, newUser)
	saveUsers(existing)

	// Return full record including API key — only time it's visible.
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, newUser)
}

func getMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	writeJSON(w, userPublic(*u))
}

func getUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	for _, u := range loadUsers() {
		if u.ID == id {
			writeJSON(w, userPublic(u))
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func updateUser(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r)
	id := chi.URLParam(r, "id")

	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	users := loadUsers()
	for i, u := range users {
		if u.ID != id {
			continue
		}
		// Admin can update anyone; users can update themselves (but not role)
		if caller.Role != "admin" && caller.ID != id {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		if v, ok := patch["name"].(string); ok {
			users[i].Name = v
		}
		if v, ok := patch["email"].(string); ok {
			users[i].Email = v
		}
		if v, ok := patch["role"].(string); ok && caller.Role == "admin" {
			users[i].Role = v
		}
		saveUsers(users)
		writeJSON(w, userPublic(users[i]))
		return
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func deleteUser(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r)
	if caller.Role != "admin" {
		writeErr(w, http.StatusForbidden, "admin only")
		return
	}
	id := chi.URLParam(r, "id")
	users := loadUsers()
	filtered := users[:0]
	found := false
	for _, u := range users {
		if u.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, u)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	saveUsers(filtered)
	w.WriteHeader(http.StatusNoContent)
}

// userPublic returns a map without the API key — safe to send in list/get responses.
func userPublic(u userRecord) map[string]any {
	return map[string]any{
		"id": u.ID, "name": u.Name, "email": u.Email,
		"role": u.Role, "createdAt": u.CreatedAt,
	}
}

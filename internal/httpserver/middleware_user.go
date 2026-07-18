package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tolle-ai/tollecode/internal/config"
)

type userRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	APIKey    string `json:"apiKey"`
	Role      string `json:"role"` // "admin" | "member"
	CreatedAt string `json:"createdAt"`
}

type contextKey string

const ctxKeyUser contextKey = "user"

// sysUser is returned when no users.json exists or the Bearer key doesn't match any user.
var sysUser = &userRecord{ID: "system", Name: "System", Role: "admin"}

func usersFilePath() string {
	return filepath.Join(config.Home(), "users.json")
}

func loadUsers() []userRecord {
	data, err := os.ReadFile(usersFilePath())
	if err != nil {
		return []userRecord{}
	}
	var list []userRecord
	if json.Unmarshal(data, &list) != nil {
		return []userRecord{}
	}
	return list
}

func saveUsers(list []userRecord) {
	data, _ := json.MarshalIndent(list, "", "  ")
	_ = os.WriteFile(usersFilePath(), data, 0o644)
}

func resolveUserFromKey(key string) *userRecord {
	if key == "" {
		return sysUser
	}
	users := loadUsers()
	for i := range users {
		if users[i].APIKey == key {
			return &users[i]
		}
	}
	return sysUser
}

// userContextMiddleware resolves the caller's userRecord from the Bearer token
// and stores it in the request context. Must run after authMiddleware.
func userContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			key = strings.TrimPrefix(auth, "Bearer ")
		}
		u := resolveUserFromKey(key)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyUser, u)))
	})
}

func currentUser(r *http.Request) *userRecord {
	u, _ := r.Context().Value(ctxKeyUser).(*userRecord)
	if u == nil {
		return sysUser
	}
	return u
}

package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
)

// authMiddleware returns a middleware that enforces Bearer token auth on all
// requests. If apiKeys is empty, all requests are allowed through (no-auth mode).
func authMiddleware(apiKeys []string) func(http.Handler) http.Handler {
	set := make(map[string]struct{}, len(apiKeys))
	for _, k := range apiKeys {
		if k != "" {
			set[k] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(set) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			auth := r.Header.Get("Authorization")
			key := ""
			if strings.HasPrefix(auth, "Bearer ") {
				key = strings.TrimPrefix(auth, "Bearer ")
			}

			if _, ok := set[key]; !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"}) //nolint:errcheck
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

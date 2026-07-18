package webmode

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/tolle-ai/tollecode/internal/liteaccess"
	"github.com/tolle-ai/tollecode/internal/liteauth"
	"github.com/tolle-ai/tollecode/internal/webauth"
)

// Web-mode auth over HTTP. The TOTP handshake and the access-key door used to run
// over the /ws/cmd WebSocket, but a WebSocket message can't set an httpOnly
// cookie — so anything that establishes a secret (unlock, verify) is an HTTP
// endpoint here. The two secrets (the access-key grant and the TOTP session
// token) are handed back ONLY as httpOnly, Secure, SameSite=Strict cookies: they
// never touch localStorage or a URL, so JavaScript can't read them, XSS can't
// exfiltrate them, and the reverse proxy never logs them. The browser then
// proves itself on every request (including the WS upgrade) via those cookies.

// cookieMaxAge is the lifetime of both auth cookies (30 days), matching the
// liteauth session and the liteaccess grant TTLs.
const cookieMaxAge = 30 * 24 * 60 * 60

// mountAuthRoutes wires the HTTP auth surface onto r. The access-key endpoints
// are open (they ARE the door opener); the TOTP endpoints sit behind the door.
func mountAuthRoutes(r chi.Router) {
	// Door (access key). Reachable without a grant — this is how you get one.
	r.Get("/web/access/status", handleAccessStatus)
	r.Post("/web/access/unlock", handleAccessUnlock)

	// TOTP (the lock). Behind the door: a configured access key gates these too.
	r.Group(func(pr chi.Router) {
		pr.Use(requireDoor)
		pr.Post("/web/auth/begin-login", handleBeginLogin)
		pr.Post("/web/auth/register", handleRegister)
		pr.Post("/web/auth/verify-registration", handleVerifyRegistration)
		pr.Post("/web/auth/verify-login", handleVerifyLogin)
		pr.Get("/web/auth/session", handleSession)
		pr.Get("/web/auth/local-user", handleLocalUser)
		pr.Post("/web/auth/signout", handleSignout)
	})
}

// requireDoor rejects a request that lacks a valid access-key grant when the door
// is enabled. Open when no key is configured.
func requireDoor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !liteaccess.GrantOK(webauth.AccessGrantFromRequest(r)) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "access key required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Access key (door) ─────────────────────────────────────────────────────────

func handleAccessStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"required": liteaccess.Required(),
		"ok":       liteaccess.GrantOK(webauth.AccessGrantFromRequest(r)),
	})
}

func handleAccessUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	grant, err := liteaccess.Unlock(body.Key)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid access key"})
		return
	}
	// grant == "" means no key is required; there's nothing to store, but the
	// caller still gets ok:true so it proceeds to the login screen.
	if grant != "" {
		webauth.SetAuthCookie(w, r, webauth.AccessCookie, grant, cookieMaxAge)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── TOTP (lock) ───────────────────────────────────────────────────────────────

func handleBeginLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	exists, name := liteauth.BeginLogin(body.Email)
	writeJSON(w, http.StatusOK, map[string]any{"exists": exists, "name": name})
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	userID, qrURI, backupCodes, err := liteauth.Register(body.Name, body.Email)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":      userID,
		"qr_uri":       qrURI,
		"backup_codes": backupCodes,
	})
}

func handleVerifyRegistration(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID int    `json:"userId"`
		Code   string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	token, _, user, err := liteauth.VerifyRegistration(body.UserID, body.Code)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}
	webauth.SetAuthCookie(w, r, webauth.SessionCookie, token, cookieMaxAge)
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func handleVerifyLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	token, _, user, err := liteauth.VerifyLogin(body.Email, body.Code)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}
	webauth.SetAuthCookie(w, r, webauth.SessionCookie, token, cookieMaxAge)
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func handleSession(w http.ResponseWriter, r *http.Request) {
	valid, user := liteauth.ValidateSession(webauth.SessionTokenFromRequest(r))
	resp := map[string]any{"valid": valid}
	if user != nil {
		resp["user"] = user
	}
	writeJSON(w, http.StatusOK, resp)
}

func handleLocalUser(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": liteauth.LocalUser()})
}

func handleSignout(w http.ResponseWriter, r *http.Request) {
	liteauth.SignOut(webauth.SessionTokenFromRequest(r))
	webauth.ClearAuthCookie(w, r, webauth.SessionCookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

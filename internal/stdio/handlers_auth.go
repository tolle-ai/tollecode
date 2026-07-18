package stdio

import (
	"strconv"

	"github.com/tolle-ai/tollecode/internal/liteauth"
)

// Lite 2FA over the command bus. These mirror the shapes the frontend used with
// the desktop Rust auth commands, so a single AuthService talks to the sidecar
// the same way in desktop (stdio) and web (/ws/cmd) — one account, one secret.
//
// Convention: every response carries the command's own type. Failures include
// an "error" string; the frontend throws on it (drives the shake/error UI).
//
// Response building is split into pure compute* functions (no Emit) so web mode
// can deliver an auth response to the ONE requesting /ws/cmd connection instead
// of broadcasting it — a login/registration response can carry a session token
// or TOTP secret, which must never reach another connected socket. The desktop
// stdio path keeps broadcasting via the handleAuth* wrappers (its only client is
// its own parent process over stdout).

// authCommandTypes is the set of auth bootstrap commands a /ws/cmd connection may
// send before it holds a valid session. Every other command requires auth.
var authCommandTypes = map[string]bool{
	"auth_begin_login":         true,
	"auth_register":            true,
	"auth_verify_registration": true,
	"auth_verify_login":        true,
	"auth_validate_session":    true,
	"auth_get_local_user":      true,
	"auth_sign_out":            true,
}

// IsAuthCommand reports whether typ is an auth bootstrap command.
func IsAuthCommand(typ string) bool { return authCommandTypes[typ] }

// ComputeAuth handles an auth_* command without emitting, returning the response
// map to send back to the requesting connection and whether the command left the
// caller authenticated (a successful login/registration, or a valid session
// check). The response is intended for connection-local delivery, never a
// broadcast.
func ComputeAuth(cmd map[string]any) (resp map[string]any, authenticated bool) {
	typ, _ := cmd["type"].(string)
	switch typ {
	case "auth_begin_login":
		return computeAuthBeginLogin(cmd), false
	case "auth_register":
		return computeAuthRegister(cmd), false
	case "auth_verify_registration":
		r := computeAuthVerifyRegistration(cmd)
		return r, !hasError(r)
	case "auth_verify_login":
		r := computeAuthVerifyLogin(cmd)
		return r, !hasError(r)
	case "auth_validate_session":
		r := computeAuthValidateSession(cmd)
		valid, _ := r["valid"].(bool)
		return r, valid
	case "auth_get_local_user":
		return computeAuthGetLocalUser(cmd), false
	case "auth_sign_out":
		return computeAuthSignOut(cmd), false
	}
	return map[string]any{"type": typ, "error": "unknown auth command"}, false
}

func hasError(m map[string]any) bool { _, ok := m["error"]; return ok }

// ── Desktop stdio wrappers (Emit the computed response) ──────────────────────

func handleAuthBeginLogin(state *ServerState, cmd map[string]any) { Emit(computeAuthBeginLogin(cmd)) }
func handleAuthRegister(state *ServerState, cmd map[string]any)   { Emit(computeAuthRegister(cmd)) }
func handleAuthVerifyRegistration(state *ServerState, cmd map[string]any) {
	Emit(computeAuthVerifyRegistration(cmd))
}
func handleAuthVerifyLogin(state *ServerState, cmd map[string]any) { Emit(computeAuthVerifyLogin(cmd)) }
func handleAuthValidateSession(state *ServerState, cmd map[string]any) {
	Emit(computeAuthValidateSession(cmd))
}
func handleAuthGetLocalUser(state *ServerState, cmd map[string]any) {
	Emit(computeAuthGetLocalUser(cmd))
}
func handleAuthSignOut(state *ServerState, cmd map[string]any) { Emit(computeAuthSignOut(cmd)) }

// ── Pure response builders ───────────────────────────────────────────────────

func computeAuthBeginLogin(cmd map[string]any) map[string]any {
	email, _ := cmd["email"].(string)
	exists, name := liteauth.BeginLogin(email)
	return map[string]any{"type": "auth_begin_login", "exists": exists, "name": name}
}

func computeAuthRegister(cmd map[string]any) map[string]any {
	name, _ := cmd["name"].(string)
	email, _ := cmd["email"].(string)
	userID, qrURI, backupCodes, err := liteauth.Register(name, email)
	if err != nil {
		return map[string]any{"type": "auth_register", "error": err.Error()}
	}
	return map[string]any{
		"type":         "auth_register",
		"user_id":      userID,
		"qr_uri":       qrURI,
		"backup_codes": backupCodes,
	}
}

func computeAuthVerifyRegistration(cmd map[string]any) map[string]any {
	userID := toInt(cmd["userId"])
	code, _ := cmd["code"].(string)
	token, expiresAt, user, err := liteauth.VerifyRegistration(userID, code)
	if err != nil {
		return map[string]any{"type": "auth_verify_registration", "error": err.Error()}
	}
	return map[string]any{
		"type":       "auth_verify_registration",
		"token":      token,
		"expires_at": expiresAt,
		"user":       user,
	}
}

func computeAuthVerifyLogin(cmd map[string]any) map[string]any {
	email, _ := cmd["email"].(string)
	code, _ := cmd["code"].(string)
	token, expiresAt, user, err := liteauth.VerifyLogin(email, code)
	if err != nil {
		return map[string]any{"type": "auth_verify_login", "error": err.Error()}
	}
	return map[string]any{
		"type":       "auth_verify_login",
		"token":      token,
		"expires_at": expiresAt,
		"user":       user,
	}
}

func computeAuthValidateSession(cmd map[string]any) map[string]any {
	token, _ := cmd["token"].(string)
	valid, user := liteauth.ValidateSession(token)
	resp := map[string]any{"type": "auth_validate_session", "valid": valid}
	if user != nil {
		resp["user"] = user
	}
	return resp
}

func computeAuthGetLocalUser(cmd map[string]any) map[string]any {
	return map[string]any{"type": "auth_get_local_user", "user": liteauth.LocalUser()}
}

func computeAuthSignOut(cmd map[string]any) map[string]any {
	token, _ := cmd["token"].(string)
	liteauth.SignOut(token)
	return map[string]any{"type": "auth_sign_out", "ok": true}
}

// toInt coerces a JSON number/string into an int (JSON unmarshals numbers as
// float64; the frontend may also send a string id).
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

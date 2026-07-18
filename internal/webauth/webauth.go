// Package webauth holds the HTTP-layer security shared by `tollecode web`
// (browser Lite): the Origin policy, a session-token gate backed by liteauth, and
// a CORS policy that reflects the request Origin.
//
// It exists as its own package so both the web router (internal/webmode) and the
// command bridge (internal/stdio) can enforce the same rules without an import
// cycle. None of this touches desktop stdio mode — that transport is trusted
// local IPC on a random loopback port and never routes through here.
//
// Access model. Web mode is meant to be served over the network (behind a
// reverse proxy / custom domain), so the Origin is NOT restricted — any origin
// is accepted. Access is gated instead by:
//   - the liteauth session token (TOTP): every non-login command on /ws/cmd and
//     every RequireSession route needs a valid token, and a browser on another
//     origin cannot read this origin's token, so it can drive nothing; and
//   - the optional server access key (liteaccess), the recommended door for a
//     public deployment — without it a client can't even reach the login.
package webauth

import (
	"net/http"
	"strings"

	"github.com/tolle-ai/tollecode/internal/liteaccess"
	"github.com/tolle-ai/tollecode/internal/liteauth"
)

// OriginAllowed reports whether a request may proceed based on its Origin.
//
// Web mode accepts ALL origins: it is designed to be served over the network, so
// pinning the Origin to loopback would 403 every real-domain deployment. The
// session token and access key are the actual gates (see the package doc). Kept
// as a function (rather than inlined) so the Origin policy has one definition and
// callers/tests keep a stable seam.
func OriginAllowed(_ *http.Request, _ int) bool {
	return true
}

// TokenFromRequest extracts a Lite session token from `?token=` (used by browser
// Cookie names for the two secrets. Both are set httpOnly so JavaScript can
// never read them (not visible in localStorage, un-stealable by XSS), Secure so
// they only travel over HTTPS, and SameSite=Strict so a cross-site page can't
// make the browser send them — which also restores CSRF protection.
const (
	SessionCookie = "tc_session" // the liteauth TOTP session token (the lock)
	AccessCookie  = "tc_access"  // the liteaccess grant (the door)
)

// SessionTokenFromRequest reads the session token from the tc_session cookie.
// Browsers attach it automatically to same-origin requests — including the
// WebSocket upgrade — so nothing needs to put it in a URL or header.
func SessionTokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(SessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// AccessGrantFromRequest reads the access-key grant from the tc_access cookie.
func AccessGrantFromRequest(r *http.Request) string {
	if c, err := r.Cookie(AccessCookie); err == nil {
		return c.Value
	}
	return ""
}

// isSecureRequest reports whether the client reached us over HTTPS, honoring the
// reverse proxy's X-Forwarded-Proto (the sidecar itself speaks plain HTTP behind
// the proxy). Used to decide the cookie Secure attribute: on a real HTTPS
// deployment cookies are Secure; on a bare http://localhost dev run they aren't,
// so login still works without TLS.
func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SetAuthCookie writes one of the auth cookies (httpOnly, SameSite=Strict, and
// Secure on HTTPS). maxAgeSeconds sets its lifetime.
func SetAuthCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAgeSeconds int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAgeSeconds,
	})
}

// ClearAuthCookie expires one of the auth cookies.
func ClearAuthCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// RequireSession is chi middleware that enforces, in order, the Origin policy,
// the optional server access key (the door, via the tc_access grant cookie), and
// a valid liteauth session (the lock, via the tc_session cookie) on the routes it
// wraps. Used in web mode for the session/channel WebSockets and the
// LSP/completion endpoints; NOT used by desktop stdio mode.
func RequireSession(port int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !OriginAllowed(r, port) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			// The door: when a server access key is configured, no request gets
			// through without a valid grant — even one carrying a session cookie.
			if !liteaccess.GrantOK(AccessGrantFromRequest(r)) {
				http.Error(w, "access key required", http.StatusUnauthorized)
				return
			}
			if ok, _ := liteauth.ValidateSession(SessionTokenFromRequest(r)); !ok {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORS is chi middleware that reflects the request Origin (never a wildcard, so
// credentialed cookie requests work) with Allow-Credentials, and answers
// preflight OPTIONS. Requests with no Origin pass through unchanged.
func CORS(port int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if origin := r.Header.Get("Origin"); origin != "" && OriginAllowed(r, port) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

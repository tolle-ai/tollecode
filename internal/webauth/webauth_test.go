package webauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tolle-ai/tollecode/internal/liteaccess"
)

func reqWithOrigin(origin string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// Web mode accepts every origin (it's served over the network); the session
// token + access key are the real gates. This asserts that policy for a
// representative spread — loopback, a real domain, and an arbitrary site.
func TestOriginAllowed(t *testing.T) {
	const port = 5180
	origins := []string{
		"",                          // absent Origin — non-browser / same-origin GET
		"http://127.0.0.1:5180",     // the served origin
		"http://localhost:5180",     // loopback by name
		"https://code.example.com",  // a real reverse-proxied domain
		"https://another.site:8443", // any other origin
		"null",                      // sandboxed iframe origin
	}
	for _, o := range origins {
		if !OriginAllowed(reqWithOrigin(o), port) {
			t.Errorf("OriginAllowed(%q) = false, want true (web mode allows any origin)", o)
		}
	}
}

func TestCookieReaders(t *testing.T) {
	t.Run("access grant", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: AccessCookie, Value: "grant123"})
		if got := AccessGrantFromRequest(r); got != "grant123" {
			t.Errorf("got %q, want grant123", got)
		}
	})
	t.Run("session token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "sess456"})
		if got := SessionTokenFromRequest(r); got != "sess456" {
			t.Errorf("got %q, want sess456", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if AccessGrantFromRequest(r) != "" || SessionTokenFromRequest(r) != "" {
			t.Error("expected empty when cookies absent")
		}
	})
}

// TestRequireSessionEnforcesDoor proves the access-key grant is checked before
// the session: with the door on, a request carrying no grant cookie is refused as
// "access key required" before any session validation happens.
func TestRequireSessionEnforcesDoor(t *testing.T) {
	const port = 5180
	t.Setenv("TOLLECODE_HOME", t.TempDir())

	reached := false
	h := RequireSession(port)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	// Door off (default): fails on the session check (no cookie), not the door.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ws/session/x", nil))
	if reached || rr.Code != http.StatusUnauthorized {
		t.Fatalf("door-off: reached=%v code=%d (want false, 401)", reached, rr.Code)
	}

	// Turn the door on and mint a grant. A request with no grant cookie must be
	// refused specifically at the door.
	key, err := liteaccess.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ws/session/x", nil))
	if reached || rr.Body.String() != "access key required\n" {
		t.Fatalf("door-on/no-grant: reached=%v body=%q (want the access-key error)", reached, rr.Body.String())
	}

	// A request WITH a valid grant cookie gets past the door (now fails only the
	// session check): the body flips away from the access-key message.
	grant, err := liteaccess.Unlock(key)
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/ws/session/x", nil)
	req.AddCookie(&http.Cookie{Name: AccessCookie, Value: grant})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if reached {
		t.Fatal("handler reached with a valid grant but no session")
	}
	if rr.Body.String() == "access key required\n" {
		t.Fatal("valid grant was still rejected at the door")
	}
}

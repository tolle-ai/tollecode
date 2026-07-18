package webmode

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tolle-ai/tollecode/internal/liteaccess"
)

// serveForTest starts a web-mode server on a free port and waits until it serves
// HTTP, returning the port and a cancel func.
func serveForTest(t *testing.T) (int, context.CancelFunc) {
	t.Helper()
	t.Setenv("TOLLECODE_HOME", t.TempDir())
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = Run(ctx, port, false) }()

	deadline := time.Now().Add(8 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port)); err == nil {
			resp.Body.Close()
			return port, cancel
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	t.Fatal("web server did not come up")
	return 0, nil
}

// readEvent reads one JSON event with a deadline.
func readEvent(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m map[string]any
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

// readUntil reads events until one has the given type (skipping others), or fails.
func readUntil(t *testing.T, c *websocket.Conn, typ string) map[string]any {
	t.Helper()
	for i := 0; i < 10; i++ {
		m := readEvent(t, c)
		if m["type"] == typ {
			return m
		}
	}
	t.Fatalf("did not receive %q", typ)
	return nil
}

// Web mode accepts a handshake from ANY origin (it's meant to be served behind a
// reverse proxy / custom domain). The connection still opens unauthenticated, so
// the session-token gate — not the Origin — is what stops a foreign origin from
// actually driving the sidecar (see TestWebModeCmdBridgeAuthGate).
func TestWebModeAllowsAnyOriginHandshake(t *testing.T) {
	port, cancel := serveForTest(t)
	defer cancel()

	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws/cmd", port)
	c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {"https://code.example.com"}})
	if err != nil {
		t.Fatalf("a cross-origin handshake should now connect: %v", err)
	}
	defer c.Close()
	readUntil(t, c, "server_started")

	// Still gated: an unauthenticated non-auth command is refused.
	if err := c.WriteJSON(map[string]any{"type": "kv_get_all"}); err != nil {
		t.Fatal(err)
	}
	if resp := readUntil(t, c, "kv_get_all"); resp["error"] != "authentication required" {
		t.Fatalf("cross-origin socket must stay token-gated, got %v", resp)
	}
}

// TestWebModeAuthFlowHTTP exercises the cookie-based login: register + verify
// over the HTTP endpoints (which set the httpOnly tc_session cookie), then a
// /ws/cmd handshake carrying that cookie dispatches commands, while a handshake
// without it stays unauthenticated. No secret is ever sent in a URL or read by JS.
func TestWebModeAuthFlowHTTP(t *testing.T) {
	port, cancel := serveForTest(t)
	defer cancel()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	jar := newJar(t)
	client := &http.Client{Jar: jar}

	// Register + verify over HTTP.
	reg := postJSON(t, client, base+"/web/auth/register", map[string]any{"name": "A", "email": "a@example.com"})
	secret := secretFromOtpauth(t, reg["qr_uri"].(string))
	ver := postJSON(t, client, base+"/web/auth/verify-registration", map[string]any{"userId": 1, "code": totpNow(t, secret)})
	if ver["error"] != nil || ver["user"] == nil {
		t.Fatalf("verify-registration should succeed, got %v", ver)
	}
	u, _ := url.Parse(base)
	if cookieValue(jar, u, "tc_session") == "" {
		t.Fatal("verify-registration did not set the tc_session cookie")
	}

	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws/cmd", port)
	origin := http.Header{"Origin": {base}}

	// Handshake WITHOUT the cookie → unauthenticated: commands are refused.
	c, _, err := websocket.DefaultDialer.Dial(wsURL, origin)
	if err != nil {
		t.Fatalf("handshake should connect: %v", err)
	}
	defer c.Close()
	readUntil(t, c, "server_started")
	if err := c.WriteJSON(map[string]any{"type": "kv_get_all"}); err != nil {
		t.Fatal(err)
	}
	if resp := readUntil(t, c, "kv_get_all"); resp["error"] != "authentication required" {
		t.Fatalf("cookieless socket should be unauthenticated, got %v", resp)
	}

	// Handshake WITH the session cookie → authenticated: the command dispatches.
	h := origin.Clone()
	h.Set("Cookie", cookieHeader(jar, u))
	c2, _, err := websocket.DefaultDialer.Dial(wsURL, h)
	if err != nil {
		t.Fatalf("keyed handshake should connect: %v", err)
	}
	defer c2.Close()
	readUntil(t, c2, "server_started")
	if err := c2.WriteJSON(map[string]any{"type": "kv_get_all"}); err != nil {
		t.Fatal(err)
	}
	if kv := readUntil(t, c2, "kv_all"); kv["error"] != nil {
		t.Fatalf("cookie-authenticated command should dispatch, got %v", kv)
	}
}

// TestWebModeAccessKeyDoor exercises the access-key door over HTTP: with a key
// configured, /web/access/status reports it's required, the auth endpoints are
// refused until the browser unlocks (exchanging the raw key for a grant cookie),
// and only the right key unlocks.
func TestWebModeAccessKeyDoor(t *testing.T) {
	port, cancel := serveForTest(t)
	defer cancel()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Turn the door on (isolated TOLLECODE_HOME).
	key, err := liteaccess.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	jar := newJar(t)
	client := &http.Client{Jar: jar}

	// status: required, and not yet satisfied (no grant cookie).
	st := getJSON(t, client, base+"/web/access/status")
	if st["required"] != true || st["ok"] != false {
		t.Fatalf("status should be required=true ok=false, got %v", st)
	}

	// The TOTP endpoints are behind the door → refused without a grant.
	if resp := postJSON(t, client, base+"/web/auth/register", map[string]any{"name": "A", "email": "a@x.com"}); resp["error"] != "access key required" {
		t.Fatalf("auth behind the door should be refused, got %v", resp)
	}

	// A wrong key does not unlock.
	if resp := postJSON(t, client, base+"/web/access/unlock", map[string]any{"key": "nope"}); resp["error"] == nil {
		t.Fatalf("wrong key should fail to unlock, got %v", resp)
	}

	// The right key unlocks → sets the tc_access grant cookie.
	if resp := postJSON(t, client, base+"/web/access/unlock", map[string]any{"key": key}); resp["ok"] != true {
		t.Fatalf("correct key should unlock, got %v", resp)
	}
	u, _ := url.Parse(base)
	if cookieValue(jar, u, "tc_access") == "" {
		t.Fatal("unlock did not set the tc_access grant cookie")
	}

	// status now satisfied, and the door-gated endpoint runs.
	if st := getJSON(t, client, base+"/web/access/status"); st["ok"] != true {
		t.Fatalf("status should be ok=true after unlock, got %v", st)
	}
	if resp := postJSON(t, client, base+"/web/auth/register", map[string]any{"name": "A", "email": "a@x.com"}); resp["error"] == "access key required" {
		t.Fatalf("with a valid grant the door should be open, got %v", resp)
	}
}

// ── HTTP test helpers ─────────────────────────────────────────────────────────

func newJar(t *testing.T) *cookiejar.Jar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return jar
}

func postJSON(t *testing.T, c *http.Client, url string, body map[string]any) map[string]any {
	t.Helper()
	buf, _ := json.Marshal(body)
	resp, err := c.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func getJSON(t *testing.T, c *http.Client, url string) map[string]any {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func cookieValue(jar *cookiejar.Jar, u *url.URL, name string) string {
	for _, ck := range jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func cookieHeader(jar *cookiejar.Jar, u *url.URL) string {
	var parts []string
	for _, ck := range jar.Cookies(u) {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

// secretFromOtpauth pulls the base32 TOTP secret out of an otpauth:// URI.
func secretFromOtpauth(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse otpauth uri: %v", err)
	}
	s := u.Query().Get("secret")
	if s == "" {
		t.Fatalf("no secret in %q", uri)
	}
	return s
}

// totpNow computes the current RFC 6238 code (SHA1, 6 digits, 30s), matching
// internal/liteauth, so the test can complete a real registration.
func totpNow(t *testing.T, secret string) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(time.Now().Unix()/30))
	h := hmac.New(sha1.New, key)
	h.Write(msg[:])
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off]&0x7f) << 24) | (uint32(sum[off+1]) << 16) | (uint32(sum[off+2]) << 8) | uint32(sum[off+3])
	return fmt.Sprintf("%06d", bin%1_000_000)
}

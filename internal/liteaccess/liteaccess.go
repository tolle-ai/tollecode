// Package liteaccess implements the optional "server access key" — the DOOR that
// gates browser access to Tollecode Lite web mode. It is a pre-shared key,
// generated on and obtained FROM the machine running `tollecode web`, that a
// browser must present before it can reach the command bus or even begin the
// TOTP login. TOTP (see internal/liteauth) remains the separate LOCK that
// unlocks the app once you're through the door and lets a user step away.
//
// The key is optional and off by default: on a personal machine you leave it
// disabled and web mode behaves exactly as before (loopback Origin + TOTP only).
// When enabled, a browser without the key cannot reach the login handshake — so
// "no server access, no key, no entry" holds even if the loopback port is
// forwarded onto a network. The key lives in ~/.tollecode/lite-access.json
// (0600), the same private directory as the TOTP secret and session tokens.
//
// Desktop stdio mode never routes through here: that transport is trusted local
// IPC on a random loopback port and ignores the access key entirely, which is
// why a personal-PC install "doesn't need the server key".
package liteaccess

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tolle-ai/tollecode/internal/config"
)

type state struct {
	Enabled   bool   `json:"enabled"`
	Key       string `json:"key"` // hex; empty until Generate is called
	CreatedAt string `json:"created_at"`
	// Grants are opaque tokens issued after a browser presents the correct key,
	// stored server-side so the raw key never has to live in the browser. The
	// browser holds only the grant (in an httpOnly cookie). Rotating or disabling
	// the key clears every grant. token -> RFC3339 expiry.
	Grants map[string]string `json:"grants,omitempty"`
}

// grantTTL is how long an access grant stays valid before the browser must
// re-enter the key. Matches the session lifetime so the door and the lock expire
// on the same cadence.
const grantTTL = 30 * 24 * time.Hour

// ErrInvalidKey is returned by Unlock when the presented key is wrong.
var ErrInvalidKey = errors.New("invalid access key")

var mu sync.Mutex

func storePath() string {
	return filepath.Join(config.Home(), "lite-access.json")
}

func load() *state {
	s := &state{}
	data, err := os.ReadFile(storePath())
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, s)
	return s
}

func save(s *state) error {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the file holds the access key, a live credential.
	return os.WriteFile(storePath(), data, 0o600)
}

// Required reports whether a valid access key must be presented to use web mode:
// the feature is enabled AND a key has actually been generated.
func Required() bool {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	return s.Enabled && s.Key != ""
}

// Allow reports whether a request presenting candidate may pass the door. When
// no key is required (the personal-PC default) everything is allowed; otherwise
// candidate must equal the stored key, compared in constant time.
func Allow(candidate string) bool {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if !s.Enabled || s.Key == "" {
		return true
	}
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s.Key), []byte(candidate)) == 1
}

// Generate turns the door on with a fresh 256-bit random key and returns the
// plaintext so the owning machine can display it. Calling it again ROTATES the
// key, immediately invalidating every browser still holding the previous one.
func Generate() (string, error) {
	mu.Lock()
	defer mu.Unlock()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	key := hex.EncodeToString(buf)
	s := load()
	s.Enabled = true
	s.Key = key
	s.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	s.Grants = nil // rotating the key invalidates every outstanding grant
	if err := save(s); err != nil {
		return "", err
	}
	return key, nil
}

// Disable turns the door off and forgets the key; web mode reverts to TOTP-only.
func Disable() error {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	s.Enabled = false
	s.Key = ""
	s.CreatedAt = ""
	s.Grants = nil
	return save(s)
}

// Unlock exchanges a correct raw key for an opaque grant token, which the caller
// stores in the browser (httpOnly cookie) so the raw key never persists there.
// Returns ("", nil) when no key is required (the door is open) — the caller then
// needs no grant. Returns ErrInvalidKey when the key is wrong.
func Unlock(candidate string) (grant string, err error) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if !s.Enabled || s.Key == "" {
		return "", nil // door open — no grant needed
	}
	if candidate == "" || subtle.ConstantTimeCompare([]byte(s.Key), []byte(candidate)) != 1 {
		return "", ErrInvalidKey
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	grant = hex.EncodeToString(buf)
	if s.Grants == nil {
		s.Grants = map[string]string{}
	}
	pruneGrants(s)
	s.Grants[grant] = time.Now().Add(grantTTL).UTC().Format(time.RFC3339)
	if err := save(s); err != nil {
		return "", err
	}
	return grant, nil
}

// ValidateGrant reports whether grant is a live (unexpired) access grant.
func ValidateGrant(grant string) bool {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if grant == "" || s.Grants == nil {
		return false
	}
	exp, ok := s.Grants[grant]
	if !ok {
		return false
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil || time.Now().After(t) {
		delete(s.Grants, grant)
		_ = save(s)
		return false
	}
	return true
}

// GrantOK is the door check for an incoming request: allowed when no key is
// required, or when grant is a live grant. This is what the WS handshake and the
// RequireSession middleware call, with the grant read from the tc_access cookie.
func GrantOK(grant string) bool {
	if !Required() {
		return true
	}
	return ValidateGrant(grant)
}

// RevokeGrant drops a single grant (used on sign-out / explicit lock).
func RevokeGrant(grant string) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if s.Grants == nil {
		return
	}
	if _, ok := s.Grants[grant]; ok {
		delete(s.Grants, grant)
		_ = save(s)
	}
}

// pruneGrants drops expired grants. Caller holds mu.
func pruneGrants(s *state) {
	now := time.Now()
	for g, exp := range s.Grants {
		if t, err := time.Parse(time.RFC3339, exp); err != nil || now.After(t) {
			delete(s.Grants, g)
		}
	}
}

// Key returns the current plaintext access key for display on the machine that
// owns it, or "" when the door is off. Only ever handed to an already-authorized
// caller (the desktop shell, or an authenticated web session in Settings).
func Key() string {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if !s.Enabled {
		return ""
	}
	return s.Key
}

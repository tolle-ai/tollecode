// Package liteauth is the single source of truth for Tollecode Lite's local
// 2FA. It backs BOTH the desktop app (via the stdio command bus) and web mode
// (via /ws/cmd): because the same sidecar process and the same on-disk store
// (~/.tollecode/lite-auth.json) serve both, a user registers their authenticator
// once and can unlock either surface with the same 6-digit code.
//
// The scheme is standard TOTP (RFC 6238): SHA1, 6 digits, 30-second period —
// compatible with Google Authenticator, 1Password, Authy, etc. Everything here
// is stdlib (crypto/hmac, crypto/sha1, encoding/base32, crypto/rand).
package liteauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tolle-ai/tollecode/internal/config"
)

const (
	issuer          = "Tollecode"
	totpDigits      = 6
	totpPeriod      = 30
	totpSkew        = 1 // accept the code from ±1 window for clock drift
	sessionDays     = 30
	backupCodeCount = 10
)

// PublicUser is the account shape returned to the frontend (never the secret).
type PublicUser struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type account struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	Email        string   `json:"email"`
	TOTPSecret   string   `json:"totp_secret"`   // base32, no padding
	BackupHashes []string `json:"backup_hashes"` // sha256 hex of each unused backup code
	CreatedAt    string   `json:"created_at"`
}

func (a *account) public() PublicUser {
	return PublicUser{ID: a.ID, Name: a.Name, Email: a.Email}
}

// store is the whole persisted state.
type store struct {
	User     *account          `json:"user"`     // the registered account, nil until verified
	Pending  *account          `json:"pending"`  // mid-registration, before the first code is verified
	Sessions map[string]string `json:"sessions"` // token -> RFC3339 expiry
}

var mu sync.Mutex

func storePath() string {
	return filepath.Join(config.Home(), "lite-auth.json")
}

func load() *store {
	s := &store{Sessions: map[string]string{}}
	data, err := os.ReadFile(storePath())
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, s)
	if s.Sessions == nil {
		s.Sessions = map[string]string{}
	}
	return s
}

func save(s *store) error {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the file holds the TOTP secret and live session tokens.
	return os.WriteFile(storePath(), data, 0o600)
}

// ── Public API ────────────────────────────────────────────────────────────────

// BeginLogin reports whether an account exists for email (case-insensitive).
func BeginLogin(email string) (exists bool, name string) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if s.User != nil && strings.EqualFold(s.User.Email, strings.TrimSpace(email)) {
		return true, s.User.Name
	}
	return false, ""
}

// LocalUser returns the registered account (without secrets), or nil.
func LocalUser() *PublicUser {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if s.User == nil {
		return nil
	}
	u := s.User.public()
	return &u
}

// Register starts registration: it mints a fresh TOTP secret + backup codes for
// a not-yet-verified pending account and returns the otpauth URI (for the QR)
// and the plaintext backup codes (shown once). Registration only completes when
// VerifyRegistration succeeds with a valid code.
func Register(name, email string) (userID int, qrURI string, backupCodes []string, err error) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if s.User != nil {
		return 0, "", nil, fmt.Errorf("an account already exists")
	}

	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	if name == "" || email == "" {
		return 0, "", nil, fmt.Errorf("name and email are required")
	}

	secret, err := generateSecret()
	if err != nil {
		return 0, "", nil, err
	}
	plain, hashes := generateBackupCodes(backupCodeCount)

	s.Pending = &account{
		ID:           1,
		Name:         name,
		Email:        email,
		TOTPSecret:   secret,
		BackupHashes: hashes,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := save(s); err != nil {
		return 0, "", nil, err
	}
	return s.Pending.ID, otpauthURI(email, secret), plain, nil
}

// VerifyRegistration checks the first code against the pending account; on
// success it promotes pending → active and returns a session.
func VerifyRegistration(userID int, code string) (token, expiresAt string, user PublicUser, err error) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if s.Pending == nil {
		return "", "", PublicUser{}, fmt.Errorf("no registration in progress")
	}
	if !verifyTOTP(s.Pending.TOTPSecret, code) {
		return "", "", PublicUser{}, fmt.Errorf("invalid code")
	}
	s.User = s.Pending
	s.Pending = nil
	tok, exp := s.newSession()
	if err := save(s); err != nil {
		return "", "", PublicUser{}, err
	}
	return tok, exp, s.User.public(), nil
}

// VerifyLogin checks a TOTP code (or an unused backup code) for the registered
// account and, on success, returns a session. A consumed backup code is removed.
func VerifyLogin(email, code string) (token, expiresAt string, user PublicUser, err error) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if s.User == nil {
		return "", "", PublicUser{}, fmt.Errorf("no account")
	}
	if !strings.EqualFold(s.User.Email, strings.TrimSpace(email)) {
		return "", "", PublicUser{}, fmt.Errorf("unknown account")
	}

	ok := verifyTOTP(s.User.TOTPSecret, code)
	if !ok {
		if idx := matchBackup(s.User.BackupHashes, code); idx >= 0 {
			// Consume the used backup code.
			s.User.BackupHashes = append(s.User.BackupHashes[:idx], s.User.BackupHashes[idx+1:]...)
			ok = true
		}
	}
	if !ok {
		return "", "", PublicUser{}, fmt.Errorf("invalid code")
	}

	tok, exp := s.newSession()
	if err := save(s); err != nil {
		return "", "", PublicUser{}, err
	}
	return tok, exp, s.User.public(), nil
}

// ValidateSession reports whether token maps to a live (unexpired) session.
func ValidateSession(token string) (bool, *PublicUser) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if s.User == nil || token == "" {
		return false, nil
	}
	exp, ok := s.Sessions[token]
	if !ok {
		return false, nil
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil || time.Now().After(t) {
		delete(s.Sessions, token)
		_ = save(s)
		return false, nil
	}
	u := s.User.public()
	return true, &u
}

// StorePath returns the absolute path of the local auth store, so callers can
// show the user exactly which file (under which data dir) they are operating on.
func StorePath() string { return storePath() }

// Reset deletes the entire local auth store (see StorePath): the registered
// account, its TOTP secret, backup codes, and every live session. The next login
// starts fresh from registration. removed reports whether a file was actually
// deleted — false means there was nothing to reset (safe, not an error).
func Reset() (removed bool, err error) {
	mu.Lock()
	defer mu.Unlock()
	if err := os.Remove(storePath()); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SignOut invalidates a specific session token (best effort).
func SignOut(token string) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if _, ok := s.Sessions[token]; ok {
		delete(s.Sessions, token)
		_ = save(s)
	}
}

// newSession mints a token, prunes expired ones, and records it. Caller holds mu
// and is responsible for save().
func (s *store) newSession() (token, expiresAt string) {
	now := time.Now()
	for t, exp := range s.Sessions {
		if pt, err := time.Parse(time.RFC3339, exp); err == nil && now.After(pt) {
			delete(s.Sessions, t)
		}
	}
	token = randomHex(32)
	exp := now.Add(sessionDays * 24 * time.Hour).UTC().Format(time.RFC3339)
	s.Sessions[token] = exp
	return token, exp
}

// ── TOTP (RFC 6238) ────────────────────────────────────────────────────────────

func generateSecret() (string, error) {
	buf := make([]byte, 20) // 160-bit secret
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

func otpauthURI(email, secret string) string {
	label := url.PathEscape(issuer + ":" + email)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", totpDigits))
	q.Set("period", fmt.Sprintf("%d", totpPeriod))
	return fmt.Sprintf("otpauth://totp/%s?%s", label, q.Encode())
}

func totpAt(secret string, counter int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	h := hmac.New(sha1.New, key)
	h.Write(msg[:])
	sum := h.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	code := bin % 1_000_000
	return fmt.Sprintf("%06d", code), nil
}

// verifyTOTP checks a submitted code against the current window and ±totpSkew,
// using a constant-time compare to avoid leaking timing information.
func verifyTOTP(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}
	counter := time.Now().Unix() / totpPeriod
	for w := int64(-totpSkew); w <= totpSkew; w++ {
		want, err := totpAt(secret, counter+w)
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// ── Backup codes ────────────────────────────────────────────────────────────

func generateBackupCodes(n int) (plain []string, hashes []string) {
	for i := 0; i < n; i++ {
		raw := strings.ToUpper(randomHex(4)) // 8 hex chars
		code := raw[:4] + "-" + raw[4:]      // e.g. "1A2B-3C4D"
		plain = append(plain, code)
		hashes = append(hashes, hashBackup(code))
	}
	return plain, hashes
}

func matchBackup(hashes []string, code string) int {
	want := hashBackup(code)
	for i, h := range hashes {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			return i
		}
	}
	return -1
}

// hashBackup normalizes (strip dashes/spaces, uppercase) then sha256s the code.
func hashBackup(code string) string {
	norm := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

func randomHex(nBytes int) string {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is catastrophic; fall back to a time-seeded value
		// so we never emit an empty token (extremely unlikely path).
		binary.BigEndian.PutUint64(buf, uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(buf)
}

package liteaccess

import "testing"

// isolate points liteaccess at a throwaway data dir so the test never touches a
// real ~/.tollecode/lite-access.json.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("TOLLECODE_HOME", t.TempDir())
}

func TestDisabledByDefault(t *testing.T) {
	isolate(t)
	if Required() {
		t.Fatal("Required() = true on a fresh install; the door should default off")
	}
	// With no key configured, every candidate (including empty) passes.
	if !Allow("") || !Allow("anything") {
		t.Fatal("Allow should let everything through when no key is required")
	}
	if Key() != "" {
		t.Fatalf("Key() = %q, want empty when disabled", Key())
	}
}

func TestGenerateEnablesAndGates(t *testing.T) {
	isolate(t)
	key, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(key) != 64 { // 32 bytes hex-encoded
		t.Fatalf("key length = %d, want 64", len(key))
	}
	if !Required() {
		t.Fatal("Required() = false after Generate")
	}
	if Key() != key {
		t.Fatalf("Key() = %q, want %q", Key(), key)
	}
	if !Allow(key) {
		t.Fatal("Allow rejected the correct key")
	}
	if Allow("") || Allow(key+"x") || Allow("wrong") {
		t.Fatal("Allow accepted an incorrect / empty key while the door was on")
	}
}

func TestRotateInvalidatesOldKey(t *testing.T) {
	isolate(t)
	old, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	next, err := Generate()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if old == next {
		t.Fatal("rotation produced the same key")
	}
	if Allow(old) {
		t.Fatal("old key still accepted after rotation")
	}
	if !Allow(next) {
		t.Fatal("new key not accepted after rotation")
	}
}

func TestDisableReopensDoor(t *testing.T) {
	isolate(t)
	key, _ := Generate()
	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if Required() {
		t.Fatal("Required() = true after Disable")
	}
	// Door open again: the old key value is now meaningless, everything passes.
	if !Allow("") || !Allow(key) {
		t.Fatal("Allow should let everything through after Disable")
	}
	if Key() != "" {
		t.Fatal("Key() should be empty after Disable")
	}
}

func TestKeyPersistsAcrossReload(t *testing.T) {
	isolate(t)
	key, _ := Generate()
	// A fresh load() (no in-memory cache) must still recognise the key — proves
	// it was written to disk, which is what a sidecar restart relies on.
	if !Allow(key) {
		t.Fatal("key not honored after (implicit) reload")
	}
}

func TestGrantExchange(t *testing.T) {
	isolate(t)

	// No door → GrantOK is always true; Unlock returns no grant (none needed).
	if !GrantOK("") {
		t.Fatal("GrantOK should be true when no key is required")
	}
	if g, err := Unlock(""); err != nil || g != "" {
		t.Fatalf("Unlock with no door: grant=%q err=%v (want empty, nil)", g, err)
	}

	// Turn the door on. A wrong key can't unlock; the right key mints a grant.
	key, _ := Generate()
	if _, err := Unlock("wrong"); err != ErrInvalidKey {
		t.Fatalf("Unlock(wrong) err=%v, want ErrInvalidKey", err)
	}
	grant, err := Unlock(key)
	if err != nil || grant == "" {
		t.Fatalf("Unlock(key): grant=%q err=%v", grant, err)
	}

	// The grant passes the door; the raw key does NOT (grants are the credential now).
	if !GrantOK(grant) {
		t.Fatal("valid grant rejected by GrantOK")
	}
	if GrantOK("") || GrantOK("bogus") || GrantOK(key) {
		t.Fatal("GrantOK accepted a non-grant while the door is on")
	}

	// Revoking the grant (sign-out) invalidates it.
	RevokeGrant(grant)
	if GrantOK(grant) {
		t.Fatal("revoked grant still passes the door")
	}
}

func TestRotateInvalidatesGrants(t *testing.T) {
	isolate(t)
	key1, _ := Generate()
	grant, _ := Unlock(key1)
	if !GrantOK(grant) {
		t.Fatal("fresh grant should pass")
	}
	// Rotating the key clears every outstanding grant.
	if _, err := Generate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if GrantOK(grant) {
		t.Fatal("grant survived a key rotation")
	}
}

func TestDisableClearsGrants(t *testing.T) {
	isolate(t)
	key, _ := Generate()
	grant, _ := Unlock(key)
	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	// Door off → everything passes (the old grant is moot, not "valid").
	if !GrantOK(grant) || !GrantOK("") {
		t.Fatal("GrantOK should be open after Disable")
	}
	if ValidateGrant(grant) {
		t.Fatal("grant should not validate after Disable cleared it")
	}
}

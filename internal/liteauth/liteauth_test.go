package liteauth

import (
	"net/url"
	"testing"
	"time"
)

// setTempHome points the store at a throwaway dir so tests never touch a real
// ~/.tollecode account.
func setTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("TOLLECODE_HOME", t.TempDir())
}

func TestRegisterVerifyLoginCycle(t *testing.T) {
	setTempHome(t)

	if u := LocalUser(); u != nil {
		t.Fatalf("expected no account, got %+v", u)
	}

	uid, qr, backups, err := Register("Ada", "ada@example.com")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if uid != 1 || len(backups) != backupCodeCount {
		t.Fatalf("unexpected register result uid=%d backups=%d", uid, len(backups))
	}

	// Extract the secret from the otpauth URI and compute the current code.
	secret := secretFromURI(t, qr)
	code, err := totpAt(secret, time.Now().Unix()/totpPeriod)
	if err != nil {
		t.Fatalf("totpAt: %v", err)
	}

	// A wrong code must be rejected.
	if _, _, _, err := VerifyRegistration(1, "000000"); err == nil {
		t.Fatalf("expected wrong code to fail registration")
	}

	token, exp, user, err := VerifyRegistration(1, code)
	if err != nil {
		t.Fatalf("VerifyRegistration: %v", err)
	}
	if token == "" || user.Email != "ada@example.com" {
		t.Fatalf("bad verify result token=%q user=%+v", token, user)
	}
	if pt, err := time.Parse(time.RFC3339, exp); err != nil || time.Until(pt) < 24*time.Hour {
		t.Fatalf("session expiry looks wrong: %q", exp)
	}

	// Session must validate.
	if ok, u := ValidateSession(token); !ok || u == nil {
		t.Fatalf("ValidateSession failed for fresh token")
	}

	// begin_login should now report the account exists.
	if exists, name := BeginLogin("ADA@example.com"); !exists || name != "Ada" {
		t.Fatalf("BeginLogin exists=%v name=%q", exists, name)
	}

	// Login with a fresh TOTP code.
	code2, _ := totpAt(secret, time.Now().Unix()/totpPeriod)
	tok2, _, _, err := VerifyLogin("ada@example.com", code2)
	if err != nil || tok2 == "" {
		t.Fatalf("VerifyLogin(totp): %v", err)
	}

	// Login with a backup code, then confirm it's consumed (single-use).
	if _, _, _, err := VerifyLogin("ada@example.com", backups[0]); err != nil {
		t.Fatalf("VerifyLogin(backup): %v", err)
	}
	if _, _, _, err := VerifyLogin("ada@example.com", backups[0]); err == nil {
		t.Fatalf("backup code should be single-use")
	}

	// Sign out invalidates the session.
	SignOut(token)
	if ok, _ := ValidateSession(token); ok {
		t.Fatalf("session still valid after SignOut")
	}
}

func TestReset(t *testing.T) {
	setTempHome(t)

	// Reset on a fresh install (no file yet) is a no-op, not an error.
	if removed, err := Reset(); err != nil || removed {
		t.Fatalf("Reset with no account: removed=%v err=%v (want false, nil)", removed, err)
	}

	// Register + verify an account, then reset it away.
	_, qr, _, err := Register("Ada", "ada@example.com")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	code, _ := totpAt(secretFromURI(t, qr), time.Now().Unix()/totpPeriod)
	token, _, _, err := VerifyRegistration(1, code)
	if err != nil {
		t.Fatalf("VerifyRegistration: %v", err)
	}
	if LocalUser() == nil {
		t.Fatal("expected an account before reset")
	}

	if removed, err := Reset(); err != nil || !removed {
		t.Fatalf("Reset: removed=%v err=%v (want true, nil)", removed, err)
	}

	// Account, session, and login-existence are all gone.
	if LocalUser() != nil {
		t.Fatal("account survived Reset")
	}
	if ok, _ := ValidateSession(token); ok {
		t.Fatal("session survived Reset")
	}
	if exists, _ := BeginLogin("ada@example.com"); exists {
		t.Fatal("BeginLogin still reports the account after Reset")
	}
}

func TestRegisterRejectsSecondAccount(t *testing.T) {
	setTempHome(t)
	if _, qr, _, err := Register("A", "a@x.com"); err != nil {
		t.Fatalf("first register: %v", err)
	} else {
		code, _ := totpAt(secretFromURI(t, qr), time.Now().Unix()/totpPeriod)
		if _, _, _, err := VerifyRegistration(1, code); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}
	if _, _, _, err := Register("B", "b@x.com"); err == nil {
		t.Fatalf("expected second Register to fail once an account exists")
	}
}

func secretFromURI(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse otpauth uri: %v", err)
	}
	s := u.Query().Get("secret")
	if s == "" {
		t.Fatalf("no secret in uri %q", uri)
	}
	return s
}

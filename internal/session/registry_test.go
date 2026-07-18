package session

import "testing"

// TestIsRunning covers the signal the session WS sends to a connecting client so
// a second browser streams an in-flight turn instead of waiting for a
// session_reset it already missed.
func TestIsRunning(t *testing.T) {
	t.Setenv("TOLLECODE_HOME", t.TempDir())

	if IsRunning("s1") {
		t.Fatal("IsRunning = true before any turn registered")
	}

	RegisterSession("s1", "/ws", "desktop")
	if !IsRunning("s1") {
		t.Fatal("IsRunning = false while a turn is registered")
	}
	// A different session is unaffected.
	if IsRunning("s2") {
		t.Fatal("IsRunning(s2) = true; only s1 is registered")
	}

	UnregisterSession("s1")
	if IsRunning("s1") {
		t.Fatal("IsRunning = true after the turn finished (unregistered)")
	}
}

package config

import "testing"

func resetLiteKV() {
	liteKVMu.Lock()
	liteKVData = nil
	liteKVLoaded = false
	liteKVMu.Unlock()
}

func TestLiteKVSetGetRemovePersists(t *testing.T) {
	t.Setenv("TOLLECODE_HOME", t.TempDir())
	resetLiteKV()
	defer resetLiteKV()

	if got := LiteKVGetAll(); len(got) != 0 {
		t.Fatalf("expected empty KV, got %v", got)
	}
	if err := LiteKVSet("lite_teams", `[{"id":"t1"}]`); err != nil {
		t.Fatal(err)
	}
	if err := LiteKVSet("k2", "v2"); err != nil {
		t.Fatal(err)
	}

	// Drop the in-memory cache to prove the values round-tripped to disk.
	resetLiteKV()
	all := LiteKVGetAll()
	if all["lite_teams"] != `[{"id":"t1"}]` || all["k2"] != "v2" {
		t.Fatalf("persistence failed: %v", all)
	}

	if err := LiteKVRemove("k2"); err != nil {
		t.Fatal(err)
	}
	resetLiteKV()
	if _, ok := LiteKVGetAll()["k2"]; ok {
		t.Fatal("k2 should have been removed")
	}
}

func TestSeedDenylistAndEmptyGuard(t *testing.T) {
	t.Setenv("TOLLECODE_HOME", t.TempDir())
	resetLiteKV()
	defer resetLiteKV()

	// On machines without the desktop DB this is a no-op; either way the
	// denylisted keys must never end up in the shared store.
	SeedLiteKVFromDesktop()
	for k := range map[string]bool{"lite_connection_mode": true, "lite_session_token": true, "lite_session_expires": true} {
		if _, ok := LiteKVGetAll()[k]; ok {
			t.Errorf("denylisted key %q was imported", k)
		}
	}

	// Empty-guard: once the store has any data, a re-seed must not touch it.
	resetLiteKV()
	if err := LiteKVSet("sentinel", "1"); err != nil {
		t.Fatal(err)
	}
	SeedLiteKVFromDesktop()
	if LiteKVGetAll()["sentinel"] != "1" {
		t.Error("seed clobbered existing data (empty-guard failed)")
	}
}

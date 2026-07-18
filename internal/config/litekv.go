package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// The Lite frontend (Angular) persists all of its client-side settings —
// providers, agents, teams, workspaces, todos, UI prefs — as a flat string→string
// KV store. On the Tauri desktop app that store lives in a native SQLite DB; in
// `tollecode web` it lives in the browser's localStorage. Those two stores never
// shared, so the web UI came up blank even though the desktop had everything set.
//
// This file gives the sidecar its OWN copy of that KV, persisted to
// ~/.tollecode/lite_kv.json (shared by every process that resolves the same
// config.Home()). The web UI reads/writes it over the command protocol
// (kv_get_all / kv_set / kv_remove), and a one-time import seeds it from the
// desktop app's SQLite so an existing desktop config shows up in web with no
// desktop rebuild required.

var (
	liteKVMu     sync.Mutex
	liteKVData   map[string]string
	liteKVLoaded bool
)

func liteKVPath() string { return filepath.Join(Home(), "lite_kv.json") }

// loadLiteKVLocked lazily loads lite_kv.json. Caller holds liteKVMu.
func loadLiteKVLocked() {
	if liteKVLoaded {
		return
	}
	liteKVData = map[string]string{}
	if b, err := os.ReadFile(liteKVPath()); err == nil {
		_ = json.Unmarshal(b, &liteKVData)
	}
	liteKVLoaded = true
}

// saveLiteKVLocked writes lite_kv.json atomically-ish. Caller holds liteKVMu.
func saveLiteKVLocked() error {
	if err := os.MkdirAll(Home(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(liteKVData, "", "  ")
	if err != nil {
		return err
	}
	tmp := liteKVPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, liteKVPath())
}

// LiteKVGetAll returns a copy of the whole shared Lite KV map.
func LiteKVGetAll() map[string]string {
	liteKVMu.Lock()
	defer liteKVMu.Unlock()
	loadLiteKVLocked()
	out := make(map[string]string, len(liteKVData))
	for k, v := range liteKVData {
		out[k] = v
	}
	return out
}

// LiteKVSet sets one key and persists.
func LiteKVSet(key, value string) error {
	if key == "" {
		return nil
	}
	liteKVMu.Lock()
	defer liteKVMu.Unlock()
	loadLiteKVLocked()
	liteKVData[key] = value
	return saveLiteKVLocked()
}

// LiteKVRemove deletes one key and persists.
func LiteKVRemove(key string) error {
	liteKVMu.Lock()
	defer liteKVMu.Unlock()
	loadLiteKVLocked()
	if _, ok := liteKVData[key]; !ok {
		return nil
	}
	delete(liteKVData, key)
	return saveLiteKVLocked()
}

// seedDenylist holds keys that must NOT be imported from the desktop store.
// Connection mode + auth tokens are excluded so `tollecode web` always defaults
// to talking to its own LOCAL sidecar (direct mode) rather than silently
// adopting the desktop's remote-server session — and so we never copy a
// credential into a new plaintext file.
var seedDenylist = map[string]bool{
	"lite_connection_mode": true,
	"lite_session_token":   true,
	"lite_session_expires": true,
}

// SeedLiteKVFromDesktop imports the Tauri desktop app's KV settings into the
// shared store, but ONLY when the shared store is still empty — so it never
// clobbers data the web UI has since written. Best-effort: any failure (no
// sqlite3 CLI, no desktop DB, parse error) is silently ignored.
//
// The desktop persists its KV in a SQLite `kv_store(key,value)` table under the
// Tauri app-config dir. We read it via the `sqlite3` CLI (present by default on
// macOS) rather than adding a SQLite driver dependency to the sidecar.
func SeedLiteKVFromDesktop() {
	liteKVMu.Lock()
	loadLiteKVLocked()
	empty := len(liteKVData) == 0
	liteKVMu.Unlock()
	if !empty {
		return
	}

	db := desktopKVDBPath()
	if db == "" {
		return
	}
	if _, err := os.Stat(db); err != nil {
		return
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return
	}

	out, err := exec.Command(sqlite, db, "SELECT json_group_object(key, value) FROM kv_store").Output()
	if err != nil {
		return
	}
	imported := map[string]string{}
	if err := json.Unmarshal(out, &imported); err != nil || len(imported) == 0 {
		return
	}

	liteKVMu.Lock()
	defer liteKVMu.Unlock()
	loadLiteKVLocked()
	if len(liteKVData) != 0 { // re-check under lock — someone may have written first
		return
	}
	for k, v := range imported {
		if seedDenylist[k] {
			continue
		}
		liteKVData[k] = v
	}
	_ = saveLiteKVLocked()
}

// ReconcileKVArrayWithFile keeps a Lite KV array (kvKey inside lite_kv.json,
// read by the Lite desktop/web UI) and a JSON-array file at path (read by the
// CLI/agent runtime) in sync, so array configs like teams cut across every
// surface.
//
// It is ADD-ONLY, matched by each element's "id": an element in one store but
// missing from the other is copied over; existing elements are never modified.
// That makes it safe to run on every startup — it can't clobber data a surface
// already has, and it writes only when something is actually added.
func ReconcileKVArrayWithFile(kvKey, path string) {
	idOf := func(m map[string]any) string { s, _ := m["id"].(string); return s }

	var kvArr []map[string]any
	if raw := LiteKVGetAll()[kvKey]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &kvArr)
	}
	var fileArr []map[string]any
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &fileArr)
	}

	inKV := make(map[string]bool, len(kvArr))
	for _, e := range kvArr {
		if id := idOf(e); id != "" {
			inKV[id] = true
		}
	}
	inFile := make(map[string]bool, len(fileArr))
	for _, e := range fileArr {
		if id := idOf(e); id != "" {
			inFile[id] = true
		}
	}

	// KV → file: elements the UI has but the CLI's file lacks.
	fileChanged := false
	for _, e := range kvArr {
		id := idOf(e)
		if id == "" || inFile[id] {
			continue
		}
		fileArr = append(fileArr, e)
		inFile[id] = true
		fileChanged = true
	}
	if fileChanged {
		if b, err := json.MarshalIndent(fileArr, "", "  "); err == nil {
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			_ = os.WriteFile(path, b, 0o644)
		}
	}

	// file → KV: elements the CLI has but the UI's KV lacks. (fileArr now also
	// holds the KV-added elements, but those are already inKV, so skipped.)
	kvChanged := false
	for _, e := range fileArr {
		id := idOf(e)
		if id == "" || inKV[id] {
			continue
		}
		kvArr = append(kvArr, e)
		inKV[id] = true
		kvChanged = true
	}
	if kvChanged {
		if b, err := json.Marshal(kvArr); err == nil {
			_ = LiteKVSet(kvKey, string(b))
		}
	}
}

// desktopKVDBPath returns the Tauri desktop app's SQLite KV path, mirroring the
// Rust side's dirs::config_dir()/com.tollecode.lite/tollecode.db.
func desktopKVDBPath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	var configDir string
	switch runtime.GOOS {
	case "darwin":
		configDir = filepath.Join(home, "Library", "Application Support")
	case "windows":
		if ad := os.Getenv("APPDATA"); ad != "" {
			configDir = ad
		} else {
			configDir = filepath.Join(home, "AppData", "Roaming")
		}
	default: // linux and the rest — XDG
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			configDir = xdg
		} else {
			configDir = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(configDir, "com.tollecode.lite", "tollecode.db")
}

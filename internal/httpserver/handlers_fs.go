package httpserver

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

// mountFS registers the filesystem browsing and mkdir endpoints.
// All paths are validated against cfg.AllowedRoots / cfg.AllowRootFS — if the
// list is empty and allow_root_fs is false, only the user's home directory is
// permitted. When allow_root_fs is true, any path is allowed.
func mountFS(r chi.Router, cfg ServerConfig) {
	r.Get("/fs/home", fsHome)
	r.Get("/fs/roots", fsRoots(cfg))
	r.Get("/fs/browse", fsBrowse(cfg))
	r.Get("/fs/file", fsFile(cfg))
	r.Post("/fs/mkdir", fsMkdir(cfg))
	r.Get("/fs/config", fsConfig(cfg))
}

// ── GET /v1/fs/home ────────────────────────────────────────────────────────

func fsHome(w http.ResponseWriter, r *http.Request) {
	home, err := resolveHomeDir()
	if err != nil {
		log.Printf("[fsHome] could not resolve home directory: %v", err)
		writeJSON(w, map[string]string{"home": "/", "warning": "home directory not found"})
		return
	}
	writeJSON(w, map[string]string{"home": home})
}

// ── GET /v1/fs/roots ────────────────────────────────────────────────────────
// Returns root directories the user can actually access. This is useful when
// the home directory is unknown or inaccessible (e.g. containers, restricted
// environments). Each root is validated as both existing and readable.

func fsRoots(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		home, _ := resolveHomeDir()

		// Build candidate roots: explicit allowed_roots first, then home,
		// then workspace paths, then common defaults.
		var candidates []string

		if cfg.AllowRootFS {
			// When root FS is allowed, just return / (or C:\ on Windows)
			if runtime.GOOS == "windows" {
				candidates = []string{"C:\\"}
			} else {
				candidates = []string{"/"}
			}
		} else {
			candidates = append(candidates, cfg.AllowedRoots...)
			if home != "" && home != "/" {
				candidates = append(candidates, home)
			}
			// Include local workspace paths as browsable roots
			globalRegistry.mu.RLock()
			for _, ws := range globalRegistry.localWorkspaces {
				if ws.Path != "" {
					candidates = append(candidates, ws.Path)
				}
			}
			globalRegistry.mu.RUnlock()
			if len(candidates) == 0 {
				// No explicit roots, no home, no workspaces — try common accessible paths
				if runtime.GOOS == "windows" {
					candidates = []string{"C:\\Users", "C:\\"}
				} else {
					candidates = []string{"/home", "/Users", "/tmp", "/"}
				}
			}
		}

		// Deduplicate
		seen := map[string]bool{}
		unique := candidates[:0]
		for _, c := range candidates {
			c = filepath.Clean(c)
			if !seen[c] {
				seen[c] = true
				unique = append(unique, c)
			}
		}

		// Verify each root is accessible
		type rootInfo struct {
			Path    string `json:"path"`
			Home    bool   `json:"home"`
			Exists  bool   `json:"exists"`
			Readable bool  `json:"readable"`
		}

		var roots []rootInfo
		for _, p := range unique {
			info := rootInfo{Path: p, Home: p == home}
			if st, err := os.Stat(p); err == nil {
				info.Exists = true
				info.Readable = st.IsDir() && isReadableDir(p)
			}
			roots = append(roots, info)
		}

		writeJSON(w, map[string]any{
			"home":  home,
			"roots": roots,
		})
	}
}

// ── GET /v1/fs/config ──────────────────────────────────────────────────────

func fsConfig(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Use firstBrowsableRoot so the returned defaultHome is always a path
		// the client can actually browse (i.e. within allowed_roots if configured).
		defaultHome := firstBrowsableRoot(cfg)

		// Collect local workspace paths so the client knows what directories are available
		globalRegistry.mu.RLock()
		wsPaths := make([]string, 0, len(globalRegistry.localWorkspaces))
		for _, ws := range globalRegistry.localWorkspaces {
			if ws.Path != "" {
				wsPaths = append(wsPaths, ws.Path)
			}
		}
		globalRegistry.mu.RUnlock()

		writeJSON(w, map[string]any{
			"allowRootFS":    cfg.AllowRootFS,
			"allowedRoots":   cfg.AllowedRoots,
			"defaultHome":    defaultHome,
			"workspacePaths": wsPaths,
		})
	}
}

// ── GET /v1/fs/browse?path= ─────────────────────────────────────────────────

type fsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

func fsBrowse(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qPath := r.URL.Query().Get("path")

		// Default to the best browsable root when the client asks for "" or "/".
		// This handles servers where $HOME is unset (containers, systemd services)
		// or where "/" itself is outside the configured allowed_roots.
		if qPath == "" || qPath == "/" {
			qPath = firstBrowsableRoot(cfg)
		}

		cleanPath := filepath.Clean(qPath)

		if !isAllowed(cleanPath, cfg.AllowedRoots, cfg.AllowRootFS) {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("path %s is outside allowed roots", cleanPath))
			return
		}

		info, err := os.Stat(cleanPath)
		if err != nil {
			// Directory doesn't exist — return empty listing instead of 404
			// so the frontend can still show the path and allow "mkdir"
			writeJSON(w, map[string]any{
				"path":    cleanPath,
				"entries": []fsEntry{},
				"exists":  false,
			})
			return
		}
		if !info.IsDir() {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("path is not a directory: %s", cleanPath))
			return
		}

		entries, err := os.ReadDir(cleanPath)
		if err != nil {
			// Permission denied or similar — return empty listing with a warning
			// instead of 403, so the frontend can still navigate up or show the path
			log.Printf("[fsBrowse] cannot read directory %s: %v", cleanPath, err)
			writeJSON(w, map[string]any{
				"path":    cleanPath,
				"entries": []fsEntry{},
				"warning": fmt.Sprintf("cannot read directory: %v", err),
			})
			return
		}

		var dirs []fsEntry
		var files []fsEntry

		for _, e := range entries {
			// Skip hidden files/directories (starting with ".")
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}

			fullPath := filepath.Join(cleanPath, e.Name())
			isDir := e.IsDir()

			entry := fsEntry{
				Name:  e.Name(),
				Path:  fullPath,
				IsDir: isDir,
			}

			if isDir {
				// Only include directories the user can actually enter
				if isReadableDir(fullPath) {
					dirs = append(dirs, entry)
				}
				// Skip dirs we can't read — don't show them as navigable
			} else {
				files = append(files, entry)
			}
		}

		// Sort both slices alphabetically
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

		// Dirs first, then files
		result := append(dirs, files...)

		writeJSON(w, map[string]any{
			"path":    cleanPath,
			"entries": result,
			"exists":  true,
		})
	}
}

// ── GET /v1/fs/file?path= ────────────────────────────────────────────────────
// Returns the content of a single file (text only, max 2 MB).

func fsFile(cfg ServerConfig) http.HandlerFunc {
	const maxFileSize = 2 * 1024 * 1024 // 2 MB

	return func(w http.ResponseWriter, r *http.Request) {
		qPath := r.URL.Query().Get("path")
		if qPath == "" {
			writeErr(w, http.StatusBadRequest, "path is required")
			return
		}

		cleanPath := filepath.Clean(qPath)

		if !isAllowed(cleanPath, cfg.AllowedRoots, cfg.AllowRootFS) {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("path %s is outside allowed roots", cleanPath))
			return
		}

		info, err := os.Stat(cleanPath)
		if err != nil {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("file not found: %s", cleanPath))
			return
		}
		if info.IsDir() {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("path is a directory, not a file: %s", cleanPath))
			return
		}
		if info.Size() > maxFileSize {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file too large (%d bytes, max %d)", info.Size(), maxFileSize))
			return
		}

		data, err := os.ReadFile(cleanPath)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("cannot read file: %v", err))
			return
		}

		writeJSON(w, map[string]any{
			"path":    cleanPath,
			"content": string(data),
		})
	}
}

// ── POST /v1/fs/mkdir ───────────────────────────────────────────────────────

func fsMkdir(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
			writeErr(w, http.StatusBadRequest, "path is required")
			return
		}

		cleanPath := filepath.Clean(body.Path)

		if !isAllowed(cleanPath, cfg.AllowedRoots, cfg.AllowRootFS) {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("path %s is outside allowed roots", cleanPath))
			return
		}

		// Check if directory already exists
		if info, err := os.Stat(cleanPath); err == nil && info.IsDir() {
			writeJSON(w, map[string]any{
				"path":    cleanPath,
				"created": false,
				"message": "directory already exists",
			})
			return
		}

		if err := os.MkdirAll(cleanPath, 0o755); err != nil {
			log.Printf("[fsMkdir] MkdirAll(%s) failed: %v", cleanPath, err)
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("could not create directory: %v", err))
			return
		}

		writeJSON(w, map[string]any{
			"path":    cleanPath,
			"created": true,
		})
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolveHomeDir attempts to find the user's home directory using multiple
// strategies. It handles cases where:
//   - $HOME is unset (some containers, systemd services, Windows)
//   - os.UserHomeDir() fails (no /etc/passwd entry, etc.)
//   - The home directory doesn't exist or isn't accessible
//
// Returns "/" as a last resort on Unix, or "C:\" on Windows.
func resolveHomeDir() (string, error) {
	// 1. Try $HOME (most Unix systems, containers with proper env)
	if home := os.Getenv("HOME"); home != "" {
		if p, err := filepath.Abs(home); err == nil {
			p = filepath.Clean(p)
			if isReadableDir(p) {
				return p, nil
			}
			// Home exists but isn't readable — still return it as the
			// canonical path (the user might want to create subdirs).
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	// 2. Try $USERPROFILE (Windows)
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		if p, err := filepath.Abs(userProfile); err == nil {
			p = filepath.Clean(p)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	// 3. Try os.UserHomeDir (uses getpwuid on Unix, USERPROFILE on Windows)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		p := filepath.Clean(home)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	// 4. Try constructing from $USER on Unix
	if runtime.GOOS != "windows" {
		if user := os.Getenv("USER"); user != "" {
			for _, candidate := range []string{
				filepath.Join("/home", user),
				filepath.Join("/Users", user),
			} {
				if isReadableDir(candidate) {
					return candidate, nil
				}
			}
		}
	}

	// 5. Last resort: root on Unix, C:\ on Windows
	if runtime.GOOS == "windows" {
		return "C:\\", fmt.Errorf("could not determine home directory, falling back to C:\\")
	}
	return "/", fmt.Errorf("could not determine home directory, falling back to /")
}

// firstBrowsableRoot returns the best default path for directory browsing:
// the home directory when it is accessible and within allowed roots, otherwise
// the first accessible allowed root, otherwise the first configured allowed root,
// otherwise "/". This prevents "outside allowed roots" errors on servers where
// $HOME is unset (containers, systemd services) or where "/" is not permitted.
func firstBrowsableRoot(cfg ServerConfig) string {
	if cfg.AllowRootFS {
		if runtime.GOOS == "windows" {
			return "C:\\"
		}
		return "/"
	}

	// Try the real home directory first.
	if home, err := resolveHomeDir(); err == nil && home != "" && home != "/" {
		clean := filepath.Clean(home)
		if isAllowed(clean, cfg.AllowedRoots, cfg.AllowRootFS) {
			return clean
		}
	}

	// Walk explicit allowed_roots and return the first one we can read.
	for _, root := range cfg.AllowedRoots {
		clean := filepath.Clean(root)
		if isReadableDir(clean) {
			return clean
		}
	}

	// Fall back to any configured allowed root, readable or not.
	if len(cfg.AllowedRoots) > 0 {
		return filepath.Clean(cfg.AllowedRoots[0])
	}

	// Try registered workspace paths.
	globalRegistry.mu.RLock()
	for _, ws := range globalRegistry.localWorkspaces {
		if ws.Path != "" {
			globalRegistry.mu.RUnlock()
			return ws.Path
		}
	}
	globalRegistry.mu.RUnlock()

	// Absolute last resort.
	if runtime.GOOS == "windows" {
		return "C:\\"
	}
	return "/"
}

// isReadableDir returns true if the path is a directory that can be opened
// (i.e. the current process has read permission).
func isReadableDir(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// isAllowed returns true if p is within one of the allowed root directories.
// If allowRootFS is true, any path is allowed (root access toggle).
// Otherwise, if allowedRoots is non-empty, only those roots are allowed.
// If allowedRoots is empty, we fall back to: home directory → workspace paths → "/".
//
// Cross-platform: normalizes path separators before comparison so that
// Windows paths (backslashes) are handled correctly.
func isAllowed(p string, allowedRoots []string, allowRootFS bool) bool {
	if allowRootFS {
		return true
	}

	// Normalize the path for comparison
	p = filepath.Clean(p)

	roots := allowedRoots
	if len(roots) == 0 {
		// Fall back to home directory
		if home, err := resolveHomeDir(); err == nil && home != "" {
			roots = append(roots, home)
		}
		// Also allow local workspace paths — they were explicitly configured or created
		globalRegistry.mu.RLock()
		for _, ws := range globalRegistry.localWorkspaces {
			if ws.Path != "" {
				roots = append(roots, ws.Path)
			}
		}
		globalRegistry.mu.RUnlock()
	}

	// If we still have no roots, allow "/" so the user can at least browse
	// and discover the filesystem. This is important in containers where
	// home dir resolution fails and no allowed_roots are configured.
	if len(roots) == 0 {
		return true
	}

	for _, root := range roots {
		r := filepath.Clean(root)
		if p == r {
			return true
		}
		// Check if p is under r, using the OS-appropriate separator
		prefix := r + string(os.PathSeparator)
		if strings.HasPrefix(p, prefix) {
			return true
		}
		// Also check with / separator for cross-platform compatibility
		// (handles cases where paths come in from HTTP with forward slashes)
		prefixSlash := r + "/"
		if strings.HasPrefix(p, prefixSlash) {
			return true
		}
	}
	return false
}
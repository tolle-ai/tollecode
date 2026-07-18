package stdio

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// handleFSBrowse lists directory entries at an absolute path so the web-mode
// folder picker can walk the sidecar machine's filesystem (in web mode the
// browser can't open a native OS folder dialog, and the File System Access API
// yields opaque handles rather than the real paths the sidecar needs).
//
// Request:  {type:"fs_browse", path:"/abs/dir"}   // empty path → home directory
// Response: {type:"fs_browse", path, parent, entries:[{name, path, isDir}]}
//
// `path` is echoed for request/response correlation (see requestMatching).
func handleFSBrowse(state *ServerState, cmd map[string]any) {
	path, _ := cmd["path"].(string)
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		} else {
			path = string(filepath.Separator)
		}
	}
	path = filepath.Clean(path)

	parent := filepath.Dir(path)
	if parent == path {
		parent = "" // at the filesystem root — no further up
	}

	entries := []map[string]any{}
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		// Unreadable path (permissions, doesn't exist): return an empty listing
		// rather than an error so the picker can still navigate elsewhere.
		Emit(map[string]any{"type": "fs_browse", "path": path, "parent": parent, "entries": entries})
		return
	}

	sort.Slice(dirEntries, func(i, j int) bool {
		a, b := dirEntries[i], dirEntries[j]
		if a.IsDir() != b.IsDir() {
			return a.IsDir()
		}
		return strings.ToLower(a.Name()) < strings.ToLower(b.Name())
	})

	for _, de := range dirEntries {
		name := de.Name()
		// Skip dotfiles and noise — folder pickers rarely target them.
		if strings.HasPrefix(name, ".") {
			continue
		}
		entries = append(entries, map[string]any{
			"name":  name,
			"path":  filepath.Join(path, name),
			"isDir": de.IsDir(),
		})
	}

	Emit(map[string]any{"type": "fs_browse", "path": path, "parent": parent, "entries": entries})
}

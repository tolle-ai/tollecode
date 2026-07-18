package etl

import (
	"os"
	"path/filepath"
	"strings"
)

// textExtensions is the set of file extensions treated as plain text.
var textExtensions = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true, ".jsx": true, ".tsx": true,
	".html": true, ".htm": true, ".css": true, ".scss": true, ".less": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".xml": true,
	".md": true, ".txt": true, ".rst": true, ".sh": true, ".bash": true, ".zsh": true,
	".sql": true, ".graphql": true, ".proto": true, ".swift": true, ".kt": true,
	".java": true, ".c": true, ".cpp": true, ".h": true, ".hpp": true, ".cs": true,
	".rb": true, ".rs": true, ".php": true, ".lua": true, ".r": true, ".tf": true,
	".csv": true, ".tsv": true, ".env": true, ".mod": true, ".sum": true,
}

// Extract returns the text content of a file. Returns ("", nil) for unsupported
// types so callers can silently skip them.
func Extract(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	// No extension: try to read it as text (e.g. Makefile, Dockerfile)
	if ext != "" && !textExtensions[ext] {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Skip files that look binary (have null bytes in the first 8 KB).
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	for _, b := range sample {
		if b == 0 {
			return "", nil
		}
	}
	return string(data), nil
}

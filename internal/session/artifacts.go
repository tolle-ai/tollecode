package session

import (
	"os"
	"path/filepath"
	"time"
)

// Artifact represents a file produced during a session that the frontend can display or download.
type Artifact struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListArtifacts returns files stored in .agent/artifacts/{sessionID}/ within the workspace.
// Returns nil (not an error) if the directory doesn't exist yet.
func ListArtifacts(wsPath, sessionID string) []Artifact {
	dir := filepath.Join(wsPath, ".agent", "artifacts", sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]Artifact, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Artifact{
			Name:      e.Name(),
			Path:      filepath.Join(dir, e.Name()),
			Size:      info.Size(),
			CreatedAt: info.ModTime(),
		})
	}
	return out
}

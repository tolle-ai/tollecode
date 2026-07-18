package agent

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// snapshotManifest records which files were new at snapshot time so they can
// be deleted on restore, and which files were deleted so they can be recreated.
type snapshotManifest struct {
	// Created lists workspace-relative paths that did not exist before this turn.
	// On restore these files must be deleted.
	Created []string `json:"created,omitempty"`
	// Deleted lists workspace-relative paths that were deleted during this turn.
	// Their content is stored in the snapshot dir and must be recreated on restore.
	Deleted []string `json:"deleted,omitempty"`
}

// snapshotRoot returns the directory that holds all file snapshots for the
// sidecar. It uses the OS user-cache dir so it never touches the workspace's
// git history.
func snapshotRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "tollecode", "snapshots"), nil
}

// SnapshotDir returns the directory for a specific (session, message) pair.
func SnapshotDir(sessionID, messageID string) (string, error) {
	root, err := snapshotRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, sessionID, messageID), nil
}

// TryGitSnapshot copies the current working-tree changes for workspace into a
// per-(session,message) cache directory. No git commits are created.
// Silently no-ops when workspace is empty.
func TryGitSnapshot(workspace, sessionID, messageID string) {
	if workspace == "" || sessionID == "" || messageID == "" {
		return
	}

	dir, err := SnapshotDir(sessionID, messageID)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	isGit := exec.Command("git", "-C", workspace, "rev-parse", "--git-dir").Run() == nil

	var manifest snapshotManifest

	if isGit {
		// git status --porcelain gives us exactly which files changed.
		out, err := exec.Command("git", "-C", workspace, "status", "--porcelain", "-uall").Output()
		if err != nil {
			return
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if len(line) < 4 {
				continue
			}
			xy := line[:2]
			path := strings.TrimSpace(line[3:])
			// Handle renames: "R old -> new" format
			if idx := strings.Index(path, " -> "); idx != -1 {
				path = path[idx+4:]
			}

			switch {
			case xy == "??":
				// Untracked — new file created this session; record as Created.
				// No prior content to save.
				manifest.Created = append(manifest.Created, path)
			case strings.Contains(xy, "D"):
				// Deleted — save nothing (file is gone), but record path so restore
				// can bring it back from the snapshot copy we saved last time.
				manifest.Deleted = append(manifest.Deleted, path)
			default:
				// Modified — copy the current (pre-turn) version.
				copyToSnapshot(workspace, dir, path)
			}
		}
	}

	// Write the manifest even if empty, so the snapshot dir is always valid.
	writeManifest(dir, manifest)
}

// RestoreSnapshot restores the workspace to the state captured in the snapshot
// for the given (session, message) pair.
func RestoreSnapshot(workspace, sessionID, messageID string) error {
	dir, err := SnapshotDir(sessionID, messageID)
	if err != nil {
		return err
	}

	manifest, err := readManifest(dir)
	if err != nil {
		return err
	}

	// Delete files that were newly created after the snapshot point.
	for _, rel := range manifest.Created {
		os.Remove(filepath.Join(workspace, rel)) //nolint:errcheck
	}

	// Walk the snapshot dir and restore every saved file.
	err = filepath.Walk(dir, func(src string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, src)
		if err != nil || rel == "manifest.json" {
			return err
		}
		dst := filepath.Join(workspace, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(src, dst)
	})
	return err
}

// ListSnapshots returns all message IDs that have a snapshot for sessionID.
func ListSnapshots(sessionID string) ([]string, error) {
	root, err := snapshotRoot()
	if err != nil {
		return nil, err
	}
	sessDir := filepath.Join(root, sessionID)
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

func copyToSnapshot(workspace, snapshotDir, relPath string) {
	src := filepath.Join(workspace, relPath)
	dst := filepath.Join(snapshotDir, relPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	copyFile(src, dst) //nolint:errcheck
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func writeManifest(dir string, m snapshotManifest) {
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644) //nolint:errcheck
}

func readManifest(dir string) (snapshotManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return snapshotManifest{}, err
	}
	var m snapshotManifest
	return m, json.Unmarshal(data, &m)
}

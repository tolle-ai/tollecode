package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuildSystem_InjectsWorkspaceGuide verifies AGENTS.md is injected into the
// system prompt with authoritative "read first" framing so it governs the
// workspace before any user message.
func TestBuildSystem_InjectsWorkspaceGuide(t *testing.T) {
	ws := t.TempDir()
	marker := "ALWAYS run the linter before committing in this repo."
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("# House rules\n"+marker), 0o644); err != nil {
		t.Fatal(err)
	}

	sys := buildSystem(ws, "build", false, false, "", "", false, nil, "")

	assert.Contains(t, sys, marker, "AGENTS.md content must be injected")
	assert.Contains(t, sys, "READ FIRST", "AGENTS.md must be framed as read-first")
	// The guide and its framing must precede the per-turn mode section.
	guideIdx := strings.Index(sys, "Workspace instructions (AGENTS.md)")
	modeIdx := strings.Index(sys, "## Current mode")
	assert.GreaterOrEqual(t, guideIdx, 0, "expected the workspace instructions heading")
	assert.Greater(t, modeIdx, guideIdx, "workspace instructions should appear before the mode section")
}

// TestReadWorkspaceGuide_PrefersAgentsMdAndFallsBack covers the filename
// resolution: AGENTS.md wins, AGENT.md is accepted as a fallback, and a missing
// guide yields no content.
func TestReadWorkspaceGuide_PrefersAgentsMdAndFallsBack(t *testing.T) {
	// No guide → empty.
	assert.Empty(t, readWorkspaceGuide(t.TempDir()))

	// Singular AGENT.md is accepted.
	single := t.TempDir()
	os.WriteFile(filepath.Join(single, "AGENT.md"), []byte("singular guide"), 0o644)
	assert.Contains(t, readWorkspaceGuide(single), "singular guide")

	// AGENTS.md takes precedence over AGENT.md.
	both := t.TempDir()
	os.WriteFile(filepath.Join(both, "AGENTS.md"), []byte("plural wins"), 0o644)
	os.WriteFile(filepath.Join(both, "AGENT.md"), []byte("singular guide"), 0o644)
	got := readWorkspaceGuide(both)
	assert.Contains(t, got, "plural wins")
	assert.NotContains(t, got, "singular guide")

	// Oversized guide is truncated.
	big := t.TempDir()
	os.WriteFile(filepath.Join(big, "AGENTS.md"), []byte(strings.Repeat("x", 9000)), 0o644)
	assert.Contains(t, readWorkspaceGuide(big), "truncated")
}

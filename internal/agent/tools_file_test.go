package agent

import (
	"strings"
	"testing"
)

// TestSafeJoinAgentBoundary locks in the rule that the generic file tools may
// only reach .agent/plans and .agent/memory — every other path under .agent
// (credentials, tokens, skills, config) is walled off.
func TestSafeJoinAgentBoundary(t *testing.T) {
	ws := t.TempDir()

	allowed := []string{
		"src/main.go",             // ordinary project file
		".agentrc",                // not the .agent dir (prefix guard must not overreach)
		".agent/plans",            // permitted subtree root
		".agent/plans/feature.md", // inside permitted subtree
		".agent/memory",
		".agent/memory/config.json",
	}
	for _, rel := range allowed {
		if _, err := safeJoin(ws, rel); err != nil {
			t.Errorf("safeJoin(%q) should be allowed, got error: %v", rel, err)
		}
	}

	blocked := []string{
		".agent",                     // the internal dir itself — no enumeration
		".agent/email_config.json",   // SMTP credentials
		".agent/calendar_token.json", // OAuth token
		".agent/skills",              // skills dir
		".agent/skills/foo/skill.md",
		".agent/plans-not-really",    // sibling that only shares the "plans" prefix
	}
	for _, rel := range blocked {
		if _, err := safeJoin(ws, rel); err == nil {
			t.Errorf("safeJoin(%q) should be blocked, but it was allowed", rel)
		} else if !strings.Contains(err.Error(), ".agent") {
			t.Errorf("safeJoin(%q) blocked with an unexpected error: %v", rel, err)
		}
	}

	// Path traversal is still rejected (unchanged behaviour).
	if _, err := safeJoin(ws, "../../etc/passwd"); err == nil {
		t.Error("safeJoin should still block path traversal")
	}
}

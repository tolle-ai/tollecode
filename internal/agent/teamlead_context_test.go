package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildTeamLeadContext_SkilledMemberNotGeneralAssistant is the regression for
// the reported bug: a member whose role/skills are visible in the UI must never be
// classified as a "general assistant" in the lead's orchestration context.
func TestBuildTeamLeadContext_SkilledMemberNotGeneralAssistant(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOLLECODE_HOME", dir)

	agents := `[
	  {"id":"qa","name":"Richard","role":"Testing and QA","skills":["Autonomous Browser-Driven QA Agent"]},
	  {"id":"skilled","name":"Skye","role":"","skills":["Angular Frontend & UI Engineering Agent"]},
	  {"id":"bare","name":"Nomad","role":"","skills":[]}
	]`
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(agents), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := BuildTeamLeadContext([]string{"qa", "skilled", "bare"})

	// Richard has an explicit role — it must appear verbatim, and his line must not
	// be reduced to a generalist.
	if !strings.Contains(ctx, "Role: Testing and QA") {
		t.Errorf("expected Richard's role in context, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "Autonomous Browser-Driven QA Agent") {
		t.Errorf("expected Richard's skill listed in context, got:\n%s", ctx)
	}

	// Skye has skills but no explicit role → described as a specialist, never a
	// "general assistant".
	if !strings.Contains(ctx, "specialist — Angular Frontend & UI Engineering Agent") {
		t.Errorf("expected skilled-but-roleless member described as specialist, got:\n%s", ctx)
	}

	// Only the truly bare member (no role, no skills) may be classified as a general
	// assistant. Match the member-line form ("Role: general assistant") so the rule
	// text in the header (which also mentions the phrase) doesn't count.
	if strings.Count(ctx, "Role: general assistant") != 1 {
		t.Errorf("expected exactly one member classified 'general assistant' (the bare member), got:\n%s", ctx)
	}
}

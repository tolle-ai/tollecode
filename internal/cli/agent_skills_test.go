package cli

import (
	"testing"

	"github.com/google/uuid"
)

// TestAgentSkillsRoundTrip proves the skills selected when creating an agent
// survive the save → reload → resolve chain that createAgent and the agent
// picker use. This isolates whether a "skill not attached" report is in the
// persistence/resolution layers (this test) or the interactive picker.
func TestAgentSkillsRoundTrip(t *testing.T) {
	t.Setenv("TOLLECODE_HOME", t.TempDir())

	// Save an agent exactly the way createAgent does — skills stored as a []any
	// of skill names under "skills".
	agent := map[string]any{
		"id":     uuid.NewString(),
		"name":   "Researcher",
		"role":   "research",
		"skills": toAnySlice([]string{"web-research", "summarize"}),
		"status": "active",
	}
	if err := saveJSONList(agentsPath(), []map[string]any{agent}); err != nil {
		t.Fatalf("saveJSONList: %v", err)
	}

	// 1. Reloaded as a raw map (the /agents table read path).
	loaded := loadJSONList(agentsPath())
	if len(loaded) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(loaded))
	}
	if got := mapStrSlice(loaded[0], "skills"); len(got) != 2 || got[0] != "web-research" || got[1] != "summarize" {
		t.Fatalf("skills not persisted on the agent record: %v", got)
	}

	// 2. Through the agent picker → resolveAgentExec (the run path).
	entries := agentPickerCollect()
	var found *pickerAgentEntry
	for i := range entries {
		if entries[i].name == "Researcher" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("agent picker did not surface the created agent")
	}
	res := pickerEntryToResult(*found)
	_, _, skills, _, _ := res.resolveAgentExec()
	if len(skills) != 2 || skills[0] != "web-research" || skills[1] != "summarize" {
		t.Fatalf("resolveAgentExec dropped the agent's skills: %v", skills)
	}
}

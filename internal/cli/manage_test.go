package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAgentRoundTripPreservesUnknownFields proves the map-based load/save keeps
// fields the CLI does not model (e.g. a desktop-set photo) instead of dropping them
// on edit — the reason agents/teams are round-tripped as maps, not typed structs.
func TestAgentRoundTripPreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOLLECODE_HOME", dir)

	seed := `[{"id":"a1","name":"Rita","role":"Backend","photo":"data:image/png;base64,AAAA","gradient":"g1","ownerId":"remote","skills":["x"]}]`
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	agents := loadJSONList(agentsPath())
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	// Simulate an edit that only touches role.
	agents[0]["role"] = "Backend Engineer"
	if err := saveJSONList(agentsPath(), agents); err != nil {
		t.Fatal(err)
	}

	reloaded := loadJSONList(agentsPath())[0]
	if reloaded["photo"] != "data:image/png;base64,AAAA" {
		t.Errorf("photo was dropped on edit: %v", reloaded["photo"])
	}
	if reloaded["gradient"] != "g1" || reloaded["ownerId"] != "remote" {
		t.Errorf("unmodeled fields dropped: %+v", reloaded)
	}
	if reloaded["role"] != "Backend Engineer" {
		t.Errorf("role edit not saved: %v", reloaded["role"])
	}
}

// TestWriteSkillFileIsLoadable confirms a CLI-created skill is parseable by the same
// loader the runtime and desktop use, and is found for deletion by its name.
func TestWriteSkillFileIsLoadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOLLECODE_HOME", home)
	ws := t.TempDir()

	if err := writeSkillFile(ws, "global", "QA Bot", "runs regression tests", "Do QA work."); err != nil {
		t.Fatal(err)
	}

	skills := loadSkills(ws)
	var found *skillDef
	for i := range skills {
		if skills[i].Name == "QA Bot" {
			found = &skills[i]
		}
	}
	if found == nil {
		t.Fatalf("created skill not loaded back; got %d skills", len(skills))
	}
	if found.Description != "runs regression tests" {
		t.Errorf("description mismatch: %q", found.Description)
	}
	if found.Source != "global" {
		t.Errorf("expected global source, got %q", found.Source)
	}

	// The delete path must locate the file by frontmatter name.
	if p := findSkillFileByName(filepath.Join(home, "skills"), "QA Bot"); p == "" {
		t.Error("findSkillFileByName could not locate the created skill")
	}
}

// TestToAnySliceRoundTrip guards the []string→[]any conversion used when writing
// skills/members back into the JSON maps.
func TestToAnySliceRoundTrip(t *testing.T) {
	in := []string{"a", "b", "c"}
	out := toAnySlice(in)
	if len(out) != 3 || out[0] != "a" || out[2] != "c" {
		t.Fatalf("unexpected conversion: %+v", out)
	}
	m := map[string]any{"skills": out}
	if got := mapStrSlice(m, "skills"); len(got) != 3 || got[1] != "b" {
		t.Fatalf("mapStrSlice round-trip failed: %+v", got)
	}
}

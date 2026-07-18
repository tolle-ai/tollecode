package stdio

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/config"
	"github.com/tolle-ai/tollecode/internal/session"
)

// handleSkillsList scans global (~/.tollecode/skills/) and workspace (.agent/skills/) directories
// for .md skill files and returns the combined list with source labels.
func handleSkillsList(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	var skills []map[string]any

	// Global skills
	globalDir := filepath.Join(config.Home(), "skills")
	skills = append(skills, scanSkillDir(globalDir, "global")...)

	// Workspace skills
	if ws != "" {
		wsDir := filepath.Join(ws, ".agent", "skills")
		skills = append(skills, scanSkillDir(wsDir, "workspace")...)
	}

	if skills == nil {
		skills = []map[string]any{}
	}
	Emit(map[string]any{"type": "skills_list", "skills": skills})
}

// scanSkillDir reads all .md files in dir, parses their frontmatter, and returns skill maps.
func scanSkillDir(dir, source string) []map[string]any {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var skills []map[string]any
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		name, desc, body := parseSkillFile(path)
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".md")
		}
		skills = append(skills, map[string]any{
			"name":        name,
			"description": desc,
			"source":      source,
			"scope":       source,
			"body":        body,
			"active":      false,
		})
	}
	return skills
}

// parseSkillFile extracts name, description, and body from a YAML-frontmatter .md file.
func parseSkillFile(path string) (name, desc, body string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inFrontmatter := false
	pastFrontmatter := false
	var bodyLines []string

	for scanner.Scan() {
		line := scanner.Text()
		if !inFrontmatter && !pastFrontmatter && line == "---" {
			inFrontmatter = true
			continue
		}
		if inFrontmatter {
			if line == "---" {
				inFrontmatter = false
				pastFrontmatter = true
				continue
			}
			kv := strings.SplitN(line, ":", 2)
			if len(kv) != 2 {
				continue
			}
			key := strings.TrimSpace(kv[0])
			val := strings.TrimSpace(kv[1])
			switch key {
			case "name":
				name = val
			case "description":
				desc = val
			}
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	body = strings.Join(bodyLines, "\n")
	return
}

func handleSkillsCreate(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	name, _ := cmd["name"].(string)
	desc, _ := cmd["description"].(string)
	body_, _ := cmd["body"].(string)
	scope, _ := cmd["scope"].(string)

	var dir string
	if scope == "workspace" && ws != "" {
		dir = filepath.Join(ws, ".agent", "skills")
	} else {
		dir = filepath.Join(config.Home(), "skills")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		Emit(map[string]any{"type": "skill_created", "ok": false, "error": err.Error()})
		return
	}

	filename := sanitizeID(name) + ".md"
	if filename == ".md" {
		filename = "skill-" + time.Now().Format("20060102150405") + ".md"
	}
	filePath := filepath.Join(dir, filename)

	content := "---\nname: " + name + "\ndescription: " + desc + "\nversion: 1.0\n---\n\n" + body_
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		Emit(map[string]any{"type": "skill_created", "ok": false, "error": err.Error()})
		return
	}

	Emit(map[string]any{"type": "skill_created", "ok": true, "filePath": filePath})
}

func handleSkillsDelete(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	name, _ := cmd["name"].(string)
	scope, _ := cmd["scope"].(string)
	if name == "" {
		Emit(map[string]any{"type": "skill_deleted", "ok": false, "error": "name required"})
		return
	}

	var dirs []string
	if scope == "workspace" && ws != "" {
		dirs = []string{filepath.Join(ws, ".agent", "skills")}
	} else if scope == "global" {
		dirs = []string{filepath.Join(config.Home(), "skills")}
	} else {
		dirs = []string{filepath.Join(config.Home(), "skills")}
		if ws != "" {
			dirs = append(dirs, filepath.Join(ws, ".agent", "skills"))
		}
	}

	for _, dir := range dirs {
		if p := findSkillFile(dir, name); p != "" {
			if err := os.Remove(p); err == nil {
				Emit(map[string]any{"type": "skill_deleted", "ok": true})
				return
			}
		}
	}
	Emit(map[string]any{"type": "skill_deleted", "ok": false, "error": "skill not found"})
}

// findSkillFile scans dir for a .md file whose frontmatter name matches skillName.
// Falls back to checking sanitized filename so skills created by this app are always found.
func findSkillFile(dir, skillName string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	sanitized := sanitizeID(skillName) + ".md"
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fmName, _, _ := parseSkillFile(p)
		if strings.EqualFold(fmName, skillName) || e.Name() == sanitized {
			return p
		}
	}
	return ""
}

// ── Session skill management ──────────────────────────────────────────────────

func handleSkillsSessionGet(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["sessionId"].(string)
	ws := workspaceFromCmd(state, cmd)
	s, err := session.Load(ws, sessionID)
	active := []string{}
	if err == nil && s.ActiveSkills != nil {
		active = s.ActiveSkills
	}
	Emit(map[string]any{"type": "session_skills", "sessionId": sessionID, "activeSkills": active})
}

func handleSkillsSessionActivate(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["sessionId"].(string)
	skillName, _ := cmd["skillName"].(string)
	ws := workspaceFromCmd(state, cmd)
	s, _ := session.Load(ws, sessionID)
	active := []string{}
	if s != nil && s.ActiveSkills != nil {
		active = s.ActiveSkills
	}
	found := false
	for _, sk := range active {
		if sk == skillName {
			found = true
			break
		}
	}
	if !found {
		active = append(active, skillName)
		_, _ = session.UpdateFields(ws, sessionID, map[string]any{"activeSkills": active})
	}
	Emit(map[string]any{"type": "session_skills_updated", "sessionId": sessionID, "activeSkills": active})
}

func handleSkillsSessionDeactivate(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["sessionId"].(string)
	skillName, _ := cmd["skillName"].(string)
	ws := workspaceFromCmd(state, cmd)
	s, _ := session.Load(ws, sessionID)
	var active []string
	if s != nil {
		for _, sk := range s.ActiveSkills {
			if sk != skillName {
				active = append(active, sk)
			}
		}
	}
	if active == nil {
		active = []string{}
	}
	_, _ = session.UpdateFields(ws, sessionID, map[string]any{"activeSkills": active})
	Emit(map[string]any{"type": "session_skills_updated", "sessionId": sessionID, "activeSkills": active})
}

func handleSkillsSessionClear(state *ServerState, cmd map[string]any) {
	sessionID, _ := cmd["sessionId"].(string)
	ws := workspaceFromCmd(state, cmd)
	_, _ = session.UpdateFields(ws, sessionID, map[string]any{"activeSkills": []string{}})
	Emit(map[string]any{"type": "session_skills_updated", "sessionId": sessionID, "activeSkills": []string{}})
}

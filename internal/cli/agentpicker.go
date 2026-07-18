package cli

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/tolle-ai/tollecode/internal/agent"
	"github.com/tolle-ai/tollecode/internal/config"
	"golang.org/x/term"
)

// AgentPickerResult holds the selection from RunAgentPicker.
type AgentPickerResult struct {
	Kind          string // "agent" or "team"
	ID            string
	Name          string
	Role          string // agent role (used as an identity fallback when SystemPrompt is empty)
	SystemPrompt  string
	Model         string   // override model from agent config (may be empty)
	Skills        []string // agent skills to activate on the session
	TeamMemberIDs []string // populated when Kind == "team"
	LeadAgentID   string   // team lead agent ID (populated when Kind == "team")
}

// pickerAgentEntry is a combined agent/team entry shown in the picker.
type pickerAgentEntry struct {
	kind          string // "agent" or "team"
	id            string
	name          string
	subtitle      string // role for agents, member count for teams
	role          string
	systemPrompt  string
	model         string
	skills        []string
	teamMemberIDs []string
	leadAgentID   string
}

// RunAgentPicker shows an interactive fuzzy picker for agents and teams.
// initialQuery pre-filters on open. Returns nil if the user cancels.
func RunAgentPicker(initialQuery string) *AgentPickerResult {
	entries := agentPickerCollect()
	if len(entries) == 0 {
		return nil
	}

	query := initialQuery
	filtered := agentPickerFilter(entries, query)
	if len(filtered) == 0 && len(entries) > 0 {
		query = ""
		filtered = agentPickerFilter(entries, "")
	}
	cursor := 0

	// Reserve space for the picker UI.
	total := pickerTotalRows()
	for i := 0; i < total; i++ {
		fmt.Println()
	}
	fmt.Printf("\033[%dA\r", total)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		agentPickerClear()
		return nil
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	agentPickerDraw(query, filtered, cursor)

	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			agentPickerClear()
			return nil
		}
		b := buf[:n]

		switch {
		case n == 1 && b[0] == 27: // Esc
			agentPickerClear()
			return nil
		case n == 1 && b[0] == 3: // Ctrl+C
			agentPickerClear()
			return nil
		case n == 1 && (b[0] == 13 || b[0] == 10): // Enter
			agentPickerClear()
			if cursor < len(filtered) {
				return pickerEntryToResult(filtered[cursor])
			}
			return nil
		case n == 1 && b[0] == 9: // Tab — accept
			agentPickerClear()
			if cursor < len(filtered) {
				return pickerEntryToResult(filtered[cursor])
			}
			return nil
		case n == 1 && b[0] == 127: // Backspace
			if len(query) > 0 {
				rr := []rune(query)
				query = string(rr[:len(rr)-1])
				filtered = agentPickerFilter(entries, query)
				cursor = 0
			}
		case n == 3 && b[0] == 27 && b[1] == '[' && b[2] == 'A': // Up
			if cursor > 0 {
				cursor--
			}
		case n == 3 && b[0] == 27 && b[1] == '[' && b[2] == 'B': // Down
			if cursor < len(filtered)-1 {
				cursor++
			}
		case n == 1 && b[0] >= 32 && b[0] < 127: // Printable
			query += string(rune(b[0]))
			filtered = agentPickerFilter(entries, query)
			cursor = 0
		}

		agentPickerDraw(query, filtered, cursor)
	}
}

// pickerEntryToResult converts a selected picker entry into an AgentPickerResult,
// carrying the full agent/team profile (role, skills, team lead) through to the caller.
func pickerEntryToResult(e pickerAgentEntry) *AgentPickerResult {
	return &AgentPickerResult{
		Kind:          e.kind,
		ID:            e.id,
		Name:          e.name,
		Role:          e.role,
		SystemPrompt:  e.systemPrompt,
		Model:         e.model,
		Skills:        e.skills,
		TeamMemberIDs: e.teamMemberIDs,
		LeadAgentID:   e.leadAgentID,
	}
}

func agentPickerDraw(query string, filtered []pickerAgentEntry, cursor int) {
	w, _ := terminalSize()
	visible := pickerVisibleRows()
	lineW := w - 12
	if lineW < 20 {
		lineW = 20
	}
	ruleW := w - 4
	if ruleW < 4 {
		ruleW = 4
	}

	// Header
	fmt.Printf("\r\033[2K  %s%s◎  Agent / Team%s  %s%s\n",
		colorPrimary, ansiBold, ansiReset,
		query, ansiReset)

	// Divider
	fmt.Printf("\r\033[2K  %s%s%s\n",
		ansiDim, strings.Repeat("─", ruleW), ansiReset)

	// Entries
	start := 0
	if cursor >= visible {
		start = cursor - visible + 1
	}
	for i := 0; i < visible; i++ {
		idx := start + i
		if idx < len(filtered) {
			e := filtered[idx]

			// Agent vs. team is already legible from the "[A]"/"[T]" text itself,
			// so both tags share the same (dim, neutral) styling.
			kindTag := ansiDim + "[A]"
			if e.kind == "team" {
				kindTag = ansiDim + "[T]"
			}

			name := e.name
			sub := e.subtitle
			rr := []rune(name)
			if len(rr) > lineW-6 {
				name = string(rr[:lineW-7]) + "…"
			}

			if idx == cursor {
				// Full-brightness (vs. the dimmed unselected rows below) is enough
				// contrast to read as "selected" without a dedicated color.
				fmt.Printf("\r\033[2K  %s%s▸ %s%s %s%s%s  %s%s%s\n",
					colorPrimary+ansiBold, ansiReset,
					kindTag, ansiReset, name, ansiReset,
					ansiDim, sub, ansiReset, "")
			} else {
				fmt.Printf("\r\033[2K    %s%s%s %s%s%s  %s%s%s\n",
					kindTag, ansiDim, ansiReset+ansiDim,
					name, ansiReset,
					ansiDim, sub, ansiReset, "")
			}
		} else {
			fmt.Print("\r\033[2K\n")
		}
	}

	// Footer
	fmt.Printf("\r\033[2K  %s%d entries  ↑↓ navigate  enter select  ⌫ clear  esc cancel%s",
		ansiDim, len(filtered), ansiReset)

	fmt.Printf("\033[%dA\r", visible+2)
}

func agentPickerClear() {
	total := pickerTotalRows()
	for i := 0; i < total; i++ {
		fmt.Print("\r\033[2K\n")
	}
	fmt.Printf("\033[%dA\r", total)
}

// agentPickerCollect loads teams (first) then agents from disk.
func agentPickerCollect() []pickerAgentEntry {
	home := config.Home()
	var entries []pickerAgentEntry

	// Teams
	if data, err := os.ReadFile(filepath.Join(home, "teams.json")); err == nil {
		var teams []struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			LeadAgentID    string   `json:"leadAgentId"`
			MemberAgentIDs []string `json:"memberAgentIds"`
		}
		if json.Unmarshal(data, &teams) == nil {
			for _, t := range teams {
				sub := fmt.Sprintf("%d members", len(t.MemberAgentIDs))
				entries = append(entries, pickerAgentEntry{
					kind:          "team",
					id:            t.ID,
					name:          t.Name,
					subtitle:      sub,
					teamMemberIDs: t.MemberAgentIDs,
					leadAgentID:   t.LeadAgentID,
				})
			}
		}
	}

	// Agents
	if data, err := os.ReadFile(filepath.Join(home, "agents.json")); err == nil {
		var agents []struct {
			ID           string   `json:"id"`
			Name         string   `json:"name"`
			Role         string   `json:"role"`
			Model        string   `json:"model"`
			SystemPrompt string   `json:"systemPrompt"`
			Skills       []string `json:"skills"`
			Status       string   `json:"status"`
		}
		if json.Unmarshal(data, &agents) == nil {
			for _, a := range agents {
				if a.Status == "inactive" {
					continue
				}
				sub := a.Role
				entries = append(entries, pickerAgentEntry{
					kind:         "agent",
					id:           a.ID,
					name:         a.Name,
					subtitle:     sub,
					role:         a.Role,
					systemPrompt: a.SystemPrompt,
					model:        a.Model,
					skills:       a.Skills,
				})
			}
		}
	}

	return entries
}

// agentPickerFilter returns entries matching q (case-insensitive).
// Ranking: exact name → prefix → contains.
func agentPickerFilter(entries []pickerAgentEntry, q string) []pickerAgentEntry {
	if q == "" {
		return entries
	}
	ql := strings.ToLower(q)
	var exact, prefix, contains []pickerAgentEntry
	for _, e := range entries {
		low := strings.ToLower(e.name)
		if low == ql {
			exact = append(exact, e)
		} else if strings.HasPrefix(low, ql) {
			prefix = append(prefix, e)
		} else if strings.Contains(low, ql) || strings.Contains(strings.ToLower(e.subtitle), ql) {
			contains = append(contains, e)
		}
	}
	return append(exact, append(prefix, contains...)...)
}

// resolveAgentArg resolves --agent / --team flag values to a picker result.
// teamArg takes precedence over agentArg when both are set.
// Returns nil when neither flag was provided or no match is found.
func resolveAgentArg(agentArg, teamArg string) *AgentPickerResult {
	entries := agentPickerCollect()

	if teamArg != "" {
		for _, e := range entries {
			if e.kind == "team" {
				matched := agentPickerFilter([]pickerAgentEntry{e}, teamArg)
				if len(matched) > 0 {
					return pickerEntryToResult(e)
				}
			}
		}
		return nil
	}

	if agentArg != "" {
		for _, e := range entries {
			if e.kind == "agent" {
				matched := agentPickerFilter([]pickerAgentEntry{e}, agentArg)
				if len(matched) > 0 {
					return pickerEntryToResult(e)
				}
			}
		}
		return nil
	}

	return nil
}

// resolveAgentExec turns a picker selection into the fields the agent executor needs.
//
// Single agent: returns the agent's display name (identity anchor), persona
// (systemPrompt, falling back to role), and skills to activate on the session.
//
// Team: returns the LEAD agent's identity + persona and skills, plus the full
// orchestration roster built server-side from agents.json — including every member's
// role AND skills — so the lead knows exactly who its specialists are and never
// mislabels one as a "general assistant". TeamMemberIDs activates the team-mode tools
// (delegate_task / wait_for_team) in the executor.
func (res *AgentPickerResult) resolveAgentExec() (agentName, customInstructions string, skills, teamMemberIDs []string, model string) {
	if res == nil {
		return
	}
	model = res.Model

	if res.Kind == "team" {
		teamMemberIDs = res.TeamMemberIDs
		if lead := agent.LookupAgentCfg(res.LeadAgentID); lead != nil {
			agentName = lead.Name
			if lead.SystemPrompt != "" {
				customInstructions = lead.SystemPrompt
			} else if lead.Role != "" {
				customInstructions = lead.Role
			}
			skills = lead.Skills
			if model == "" {
				model = lead.Model
			}
		}
		if roster := agent.BuildTeamLeadContext(teamMemberIDs); roster != "" {
			if customInstructions != "" {
				customInstructions += "\n\n" + roster
			} else {
				customInstructions = roster
			}
		}
		return
	}

	// Single agent.
	agentName = res.Name
	if res.SystemPrompt != "" {
		customInstructions = res.SystemPrompt
	} else if res.Role != "" {
		customInstructions = res.Role
	}
	skills = res.Skills
	return
}

// MaterializeCloudAgent writes a cloud-provided agent definition into the local
// agents.json so the existing agent picker / --agent selection flow can apply it.
// Any embedded skills are written as ~/.tollecode/skills/<name>.md files (the same
// location the runtime skill loader scans), so the existing skill-injection path
// applies them unchanged.
//
// configJSON is the agent's stored config (AgentVersion.ConfigJson from the cloud,
// with skills resolved to full definitions by the cloud API). Returns the agent name
// to pass as --agent and the list of active skill names to activate on the session.
// Returns name "" (with nil error) when there is no usable systemPrompt.
func MaterializeCloudAgent(configJSON string) (name string, skillNames []string, err error) {
	var cfg struct {
		Name         string `json:"name"`
		Role         string `json:"role"`
		Model        string `json:"model"`
		SystemPrompt string `json:"systemPrompt"`
		Skills       []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Body        string `json:"body"`
		} `json:"skills"`
	}
	if err = json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(cfg.SystemPrompt) == "" {
		return "", nil, nil
	}

	name = cfg.Name
	if name == "" {
		name = "cloud-agent"
	}

	home := config.Home()
	if err = os.MkdirAll(home, 0o755); err != nil {
		return "", nil, err
	}

	// Write each embedded skill as a frontmatter .md file in the global skills dir.
	if len(cfg.Skills) > 0 {
		skillsDir := filepath.Join(home, "skills")
		if err = os.MkdirAll(skillsDir, 0o755); err != nil {
			return "", nil, err
		}
		for _, sk := range cfg.Skills {
			if strings.TrimSpace(sk.Name) == "" {
				continue
			}
			md := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", sk.Name, sk.Description, sk.Body)
			if werr := os.WriteFile(filepath.Join(skillsDir, sk.Name+".md"), []byte(md), 0o600); werr != nil {
				log.Printf("[cloud] could not write skill %q: %v", sk.Name, werr)
				continue
			}
			skillNames = append(skillNames, sk.Name)
		}
	}

	entry := []map[string]any{{
		"id":           "cloud-agent",
		"name":         name,
		"role":         cfg.Role,
		"model":        cfg.Model,
		"systemPrompt": cfg.SystemPrompt,
		"skills":       skillNames,
		"status":       "active",
	}}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err = os.WriteFile(filepath.Join(home, "agents.json"), data, 0o600); err != nil {
		return "", nil, err
	}
	return name, skillNames, nil
}

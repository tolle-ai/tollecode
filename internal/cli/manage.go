package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tolle-ai/tollecode/internal/config"
)

// ── Raw JSON round-tripping ─────────────────────────────────────────────────────
//
// Agents and teams are loaded and saved as []map[string]any (not typed structs) so
// that fields written by the desktop/lite app we don't model here — photo data URIs,
// gradients, ownerId, lastActive — survive an edit untouched. We only ever mutate the
// specific keys the CLI manages.

func agentsPath() string { return filepath.Join(config.Home(), "agents.json") }
func teamsPath() string  { return filepath.Join(config.Home(), "teams.json") }

func loadJSONList(path string) []map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var list []map[string]any
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	return list
}

func saveJSONList(path string, list []map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// agentColorPalette supplies accent colours for CLI-created agents, matching the
// desktop's hex-colour convention so cards render consistently across both UIs.
var agentColorPalette = []string{
	"#F97066", "#7C5CFC", "#12B76A", "#2E90FA",
	"#F79009", "#EE46BC", "#06AED4", "#EF4444",
}

func mapStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func mapStrSlice(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ── Input helpers ───────────────────────────────────────────────────────────────

func readLine(reader *bufio.Reader) string {
	s, _ := reader.ReadString('\n')
	return strings.TrimSpace(s)
}

// readLineDefault prompts, echoing a default in brackets, and returns the default
// when the user just presses Enter.
func promptLine(reader *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("  %s%s%s [%s%s%s]: ", ansiBold, label, ansiReset, ansiDim, def, ansiReset)
	} else {
		fmt.Printf("  %s%s%s: ", ansiBold, label, ansiReset)
	}
	v := readLine(reader)
	if v == "" {
		return def
	}
	return v
}

// promptMultiline reads lines until a lone "." or EOF. Returns the joined body.
func promptMultiline(reader *bufio.Reader, label, def string) string {
	fmt.Printf("  %s%s%s %s(end with a single '.' on its own line; blank keeps current)%s\n",
		ansiBold, label, ansiReset, ansiDim, ansiReset)
	var lines []string
	for {
		fmt.Printf("  %s│%s ", ansiDim, ansiReset)
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(trimmed) == "." {
			break
		}
		if err != nil {
			if trimmed != "" {
				lines = append(lines, trimmed)
			}
			break
		}
		lines = append(lines, trimmed)
	}
	body := strings.Join(lines, "\n")
	if strings.TrimSpace(body) == "" {
		return def
	}
	return body
}

func confirmYesNo(reader *bufio.Reader, prompt string) bool {
	fmt.Printf("  %s%s [y/N]: ", ansiBold, prompt)
	fmt.Print(ansiReset)
	ans := strings.ToLower(readLine(reader))
	return ans == "y" || ans == "yes"
}

// skillPickItems builds selection rows from all available skills.
func skillPickItems(workspace string) []pickItem {
	skills := loadSkills(workspace)
	items := make([]pickItem, 0, len(skills))
	for _, sk := range skills {
		desc := sk.Description
		if len(desc) > 48 {
			desc = desc[:47] + "…"
		}
		items = append(items, pickItem{id: sk.Name, label: sk.Name, sublabel: desc})
	}
	return items
}

// agentPickItems builds selection rows from all agents in agents.json.
func agentPickItems() []pickItem {
	var items []pickItem
	for _, a := range loadJSONList(agentsPath()) {
		items = append(items, pickItem{
			id:       mapStr(a, "id"),
			label:    mapStr(a, "name"),
			sublabel: mapStr(a, "role"),
		})
	}
	return items
}

// ── /agents manager ─────────────────────────────────────────────────────────────

func (r *TolleREPL) manageAgents() {
	reader := bufio.NewReader(os.Stdin)
	for {
		agents := loadJSONList(agentsPath())
		printAgentTable(agents)
		fmt.Printf("\n  %s[N]ew   [E]dit   [S]kills   [D]elete   [Q]uit%s\n", ansiDim, ansiReset)
		fmt.Printf("  %s%s❯%s ", colorPrimary, ansiBold, ansiReset)
		switch strings.ToLower(readLine(reader)) {
		case "n", "new":
			r.createAgent(reader)
		case "e", "edit":
			r.editAgent(reader, agents)
		case "s", "skills":
			r.assignAgentSkills(reader, agents)
		case "d", "delete":
			r.deleteAgent(reader, agents)
		case "q", "done", "quit", "":
			fmt.Printf("  %sDone.%s\n\n", ansiDim, ansiReset)
			return
		}
	}
}

func printAgentTable(agents []map[string]any) {
	fmt.Println()
	fmt.Printf("  %s%s◎  Agents%s\n", colorPrimary, ansiBold, ansiReset)
	fmt.Println(drawRule())
	if len(agents) == 0 {
		fmt.Printf("  %sNo agents yet. Press N to create one.%s\n", ansiDim, ansiReset)
		return
	}
	fmt.Printf("\n  %s%s%-3s %-20s  %-24s  %s%s\n",
		ansiBold, colorPrimary, "#", "Name", "Role", "Skills", ansiReset)
	for i, a := range agents {
		name := mapStr(a, "name")
		role := mapStr(a, "role")
		roleStyle := ""
		if role == "" {
			role = "—"
			roleStyle = ansiDim
		}
		skills := mapStrSlice(a, "skills")
		skillStr := ansiDim + "none" + ansiReset
		if len(skills) > 0 {
			skillStr = strings.Join(skills, ", ")
			if len(skillStr) > 40 {
				skillStr = skillStr[:39] + "…"
			}
		}
		// role is padded before it's colored (not after) — padding a
		// pre-colored placeholder counts the invisible escape bytes as width
		// and under-pads the row.
		fmt.Printf("  %s%-3d%s %-20s  %s%-24s%s  %s%s%s\n",
			ansiDim, i+1, ansiReset,
			name,
			roleStyle, role, ansiReset,
			ansiDim, skillStr, ansiReset)
	}
}

func (r *TolleREPL) createAgent(reader *bufio.Reader) {
	fmt.Printf("\n  %s%s◈  New agent%s\n", colorPrimary, ansiBold, ansiReset)
	name := promptLine(reader, "Name", "")
	if name == "" {
		fmt.Printf("  %sName is required — cancelled.%s\n", colorRed, ansiReset)
		return
	}
	role := promptLine(reader, "Role", "")
	model := promptLine(reader, "Model (blank = inherit session)", "")

	// Skill assignment via the interactive checklist.
	skills := r.pickSkills(nil)

	sysPrompt := ""
	if confirmYesNo(reader, "Add a custom system prompt?") {
		sysPrompt = promptMultiline(reader, "System prompt", "")
	}

	agents := loadJSONList(agentsPath())
	agent := map[string]any{
		"id":           uuid.NewString(),
		"name":         name,
		"role":         role,
		"color":        agentColorPalette[len(agents)%len(agentColorPalette)],
		"provider":     "",
		"model":        model,
		"systemPrompt": sysPrompt,
		"photo":        "",
		"gradient":     "",
		"skills":       toAnySlice(skills),
		"permissions":  []any{},
		"status":       "active",
		"createdAt":    time.Now().UTC().Format(time.RFC3339),
		"ownerId":      "local",
	}
	agents = append(agents, agent)
	if err := saveJSONList(agentsPath(), agents); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	fmt.Printf("  %s%s✓  Agent '%s' created.%s\n", ansiBold, colorGreen, name, ansiReset)
}

func (r *TolleREPL) editAgent(reader *bufio.Reader, agents []map[string]any) {
	idx := pickAgentIndex(agents)
	if idx < 0 {
		return
	}
	a := agents[idx]
	fmt.Printf("\n  %s%s◈  Edit %s%s\n", colorPrimary, ansiBold, mapStr(a, "name"), ansiReset)
	a["name"] = promptLine(reader, "Name", mapStr(a, "name"))
	a["role"] = promptLine(reader, "Role", mapStr(a, "role"))
	a["model"] = promptLine(reader, "Model", mapStr(a, "model"))
	if confirmYesNo(reader, "Edit the system prompt?") {
		a["systemPrompt"] = promptMultiline(reader, "System prompt", mapStr(a, "systemPrompt"))
	}
	a["updatedAt"] = time.Now().UTC().Format(time.RFC3339)
	agents[idx] = a
	if err := saveJSONList(agentsPath(), agents); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	fmt.Printf("  %s%s✓  Saved.%s\n", ansiBold, colorGreen, ansiReset)
}

// assignAgentSkills opens the skill checklist for a chosen agent and persists it.
func (r *TolleREPL) assignAgentSkills(reader *bufio.Reader, agents []map[string]any) {
	idx := pickAgentIndex(agents)
	if idx < 0 {
		return
	}
	a := agents[idx]
	current := mapStrSlice(a, "skills")
	picked := r.pickSkills(current)
	a["skills"] = toAnySlice(picked)
	a["updatedAt"] = time.Now().UTC().Format(time.RFC3339)
	agents[idx] = a
	if err := saveJSONList(agentsPath(), agents); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	if len(picked) == 0 {
		fmt.Printf("  %sCleared all skills for '%s'.%s\n", ansiDim, mapStr(a, "name"), ansiReset)
	} else {
		fmt.Printf("  %s%s✓  %s → %s%s\n", ansiBold, colorGreen, mapStr(a, "name"),
			strings.Join(picked, ", "), ansiReset)
	}
}

func (r *TolleREPL) deleteAgent(reader *bufio.Reader, agents []map[string]any) {
	idx := pickAgentIndex(agents)
	if idx < 0 {
		return
	}
	name := mapStr(agents[idx], "name")
	if !confirmYesNo(reader, fmt.Sprintf("Delete agent '%s'?", name)) {
		return
	}
	agents = append(agents[:idx], agents[idx+1:]...)
	if err := saveJSONList(agentsPath(), agents); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	fmt.Printf("  %sDeleted '%s'.%s\n", ansiDim, name, ansiReset)
}

// pickSkills opens the reusable checklist, seeded with preselected skill names,
// and returns the chosen names. On cancel it keeps the preselected set.
func (r *TolleREPL) pickSkills(preselected []string) []string {
	items := skillPickItems(r.workspace)
	if len(items) == 0 {
		fmt.Printf("  %sNo skills exist yet — create one with /skills.%s\n", ansiDim, ansiReset)
		return preselected
	}
	picked, ok := RunMultiSelect("Assign skills", items, preselected)
	if !ok {
		return preselected
	}
	return picked
}

// pickAgentIndex shows a single-select agent picker and returns the index into
// agents, or -1 on cancel.
func pickAgentIndex(agents []map[string]any) int {
	if len(agents) == 0 {
		fmt.Printf("  %sNo agents to choose from.%s\n", ansiDim, ansiReset)
		return -1
	}
	items := make([]pickItem, len(agents))
	for i, a := range agents {
		items[i] = pickItem{id: mapStr(a, "id"), label: mapStr(a, "name"), sublabel: mapStr(a, "role")}
	}
	id, ok := RunSingleSelect("Select agent", items, false)
	if !ok || id == "" {
		return -1
	}
	for i, a := range agents {
		if mapStr(a, "id") == id {
			return i
		}
	}
	return -1
}

// ── /teams manager ──────────────────────────────────────────────────────────────

func (r *TolleREPL) manageTeams() {
	reader := bufio.NewReader(os.Stdin)
	for {
		teams := loadJSONList(teamsPath())
		printTeamTable(teams)
		fmt.Printf("\n  %s[N]ew   [E]dit   [D]elete   [Q]uit%s\n", ansiDim, ansiReset)
		fmt.Printf("  %s%s❯%s ", colorPrimary, ansiBold, ansiReset)
		switch strings.ToLower(readLine(reader)) {
		case "n", "new":
			r.createTeam(reader)
		case "e", "edit":
			r.editTeam(reader, teams)
		case "d", "delete":
			r.deleteTeam(reader, teams)
		case "q", "done", "quit", "":
			fmt.Printf("  %sDone.%s\n\n", ansiDim, ansiReset)
			return
		}
	}
}

func printTeamTable(teams []map[string]any) {
	fmt.Println()
	fmt.Printf("  %s%s⬡  Teams%s\n", colorPrimary, ansiBold, ansiReset)
	fmt.Println(drawRule())
	if len(teams) == 0 {
		fmt.Printf("  %sNo teams yet. Press N to create one.%s\n", ansiDim, ansiReset)
		return
	}
	names := agentNamesByID()
	fmt.Printf("\n  %s%s%-3s %-20s  %-16s  %s%s\n",
		ansiBold, colorPrimary, "#", "Name", "Lead", "Members", ansiReset)
	for i, t := range teams {
		lead := names[mapStr(t, "leadAgentId")]
		leadStyle := ""
		if lead == "" {
			lead = "—"
			leadStyle = ansiDim
		}
		var memberNames []string
		for _, id := range mapStrSlice(t, "memberAgentIds") {
			if n := names[id]; n != "" {
				memberNames = append(memberNames, n)
			}
		}
		mstr := ansiDim + "none" + ansiReset
		if len(memberNames) > 0 {
			mstr = strings.Join(memberNames, ", ")
			if len(mstr) > 44 {
				mstr = mstr[:43] + "…"
			}
		}
		// lead is padded before it's colored (not after) — same reasoning as
		// the agent table's role column above.
		fmt.Printf("  %s%-3d%s %-20s  %s%-16s%s  %s%s%s\n",
			ansiDim, i+1, ansiReset,
			mapStr(t, "name"),
			leadStyle, lead, ansiReset,
			ansiDim, mstr, ansiReset)
	}
}

func (r *TolleREPL) createTeam(reader *bufio.Reader) {
	if len(loadJSONList(agentsPath())) == 0 {
		fmt.Printf("  %sCreate at least one agent first (/agents).%s\n", ansiDim, ansiReset)
		return
	}
	fmt.Printf("\n  %s%s◈  New team%s\n", colorPrimary, ansiBold, ansiReset)
	name := promptLine(reader, "Name", "")
	if name == "" {
		fmt.Printf("  %sName is required — cancelled.%s\n", colorRed, ansiReset)
		return
	}
	desc := promptLine(reader, "Description", "")

	leadID, members := r.pickTeamLeadAndMembers("", nil)

	teams := loadJSONList(teamsPath())
	initial := strings.ToUpper(string([]rune(name)[0]))
	team := map[string]any{
		"id":             uuid.NewString(),
		"name":           name,
		"description":    desc,
		"color":          agentColorPalette[len(teams)%len(agentColorPalette)],
		"initial":        initial,
		"leadAgentId":    leadID,
		"memberAgentIds": toAnySlice(members),
		"createdAt":      time.Now().UTC().Format(time.RFC3339),
	}
	teams = append(teams, team)
	if err := saveJSONList(teamsPath(), teams); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	fmt.Printf("  %s%s✓  Team '%s' created (%d members).%s\n",
		ansiBold, colorGreen, name, len(members), ansiReset)
}

func (r *TolleREPL) editTeam(reader *bufio.Reader, teams []map[string]any) {
	idx := pickTeamIndex(teams)
	if idx < 0 {
		return
	}
	t := teams[idx]
	fmt.Printf("\n  %s%s◈  Edit %s%s\n", colorPrimary, ansiBold, mapStr(t, "name"), ansiReset)
	t["name"] = promptLine(reader, "Name", mapStr(t, "name"))
	t["description"] = promptLine(reader, "Description", mapStr(t, "description"))
	leadID, members := r.pickTeamLeadAndMembers(mapStr(t, "leadAgentId"), mapStrSlice(t, "memberAgentIds"))
	t["leadAgentId"] = leadID
	t["memberAgentIds"] = toAnySlice(members)
	teams[idx] = t
	if err := saveJSONList(teamsPath(), teams); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	fmt.Printf("  %s%s✓  Saved.%s\n", ansiBold, colorGreen, ansiReset)
}

func (r *TolleREPL) deleteTeam(reader *bufio.Reader, teams []map[string]any) {
	idx := pickTeamIndex(teams)
	if idx < 0 {
		return
	}
	name := mapStr(teams[idx], "name")
	if !confirmYesNo(reader, fmt.Sprintf("Delete team '%s'?", name)) {
		return
	}
	teams = append(teams[:idx], teams[idx+1:]...)
	if err := saveJSONList(teamsPath(), teams); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	fmt.Printf("  %sDeleted '%s'.%s\n", ansiDim, name, ansiReset)
}

// pickTeamLeadAndMembers runs the lead single-select then the members checklist.
func (r *TolleREPL) pickTeamLeadAndMembers(curLead string, curMembers []string) (leadID string, members []string) {
	agentItems := agentPickItems()

	if id, ok := RunSingleSelect("Select team lead", agentItems, false); ok {
		leadID = id
	} else {
		leadID = curLead
	}
	if picked, ok := RunMultiSelect("Select members", agentItems, curMembers); ok {
		members = picked
	} else {
		members = curMembers
	}
	return leadID, members
}

func pickTeamIndex(teams []map[string]any) int {
	if len(teams) == 0 {
		fmt.Printf("  %sNo teams to choose from.%s\n", ansiDim, ansiReset)
		return -1
	}
	items := make([]pickItem, len(teams))
	for i, t := range teams {
		items[i] = pickItem{id: mapStr(t, "id"), label: mapStr(t, "name"), sublabel: mapStr(t, "description")}
	}
	id, ok := RunSingleSelect("Select team", items, false)
	if !ok || id == "" {
		return -1
	}
	for i, t := range teams {
		if mapStr(t, "id") == id {
			return i
		}
	}
	return -1
}

func agentNamesByID() map[string]string {
	m := map[string]string{}
	for _, a := range loadJSONList(agentsPath()) {
		m[mapStr(a, "id")] = mapStr(a, "name")
	}
	return m
}

// ── /skills manager ─────────────────────────────────────────────────────────────

func (r *TolleREPL) manageSkills() {
	reader := bufio.NewReader(os.Stdin)
	for {
		skills := loadSkills(r.workspace)
		printSkillTable(skills)
		fmt.Printf("\n  %s[N]ew   [E]dit   [D]elete   [Q]uit%s\n", ansiDim, ansiReset)
		fmt.Printf("  %s%s❯%s ", colorPrimary, ansiBold, ansiReset)
		switch strings.ToLower(readLine(reader)) {
		case "n", "new":
			r.createSkill(reader)
		case "e", "edit":
			r.editSkill(reader, skills)
		case "d", "delete":
			r.deleteSkill(reader, skills)
		case "q", "done", "quit", "":
			fmt.Printf("  %sDone.%s\n\n", ansiDim, ansiReset)
			return
		}
	}
}

func printSkillTable(skills []skillDef) {
	fmt.Println()
	fmt.Printf("  %s%s◈  Skills%s\n", colorPrimary, ansiBold, ansiReset)
	fmt.Println(drawRule())
	if len(skills) == 0 {
		fmt.Printf("  %sNo skills yet. Press N to create one.%s\n", ansiDim, ansiReset)
		return
	}
	fmt.Printf("\n  %s%s%-3s %-26s  %-10s  %s%s\n",
		ansiBold, colorPrimary, "#", "Name", "Scope", "Description", ansiReset)
	for i, sk := range skills {
		desc := sk.Description
		if len(desc) > 40 {
			desc = desc[:39] + "…"
		}
		fmt.Printf("  %s%-3d%s %-26s  %s%-10s%s  %s%s%s\n",
			ansiDim, i+1, ansiReset,
			sk.Name,
			ansiDim, sk.Source, ansiReset,
			ansiDim, desc, ansiReset)
	}
}

func (r *TolleREPL) createSkill(reader *bufio.Reader) {
	fmt.Printf("\n  %s%s◈  New skill%s\n", colorPrimary, ansiBold, ansiReset)
	name := promptLine(reader, "Name", "")
	if name == "" {
		fmt.Printf("  %sName is required — cancelled.%s\n", colorRed, ansiReset)
		return
	}
	desc := promptLine(reader, "Description", "")
	scope := "global"
	if r.workspace != "" && confirmYesNo(reader, "Scope to THIS workspace only? (default global)") {
		scope = "workspace"
	}
	body := promptMultiline(reader, "Skill instructions", "")
	if strings.TrimSpace(body) == "" {
		fmt.Printf("  %sEmpty body — cancelled.%s\n", colorRed, ansiReset)
		return
	}
	if err := writeSkillFile(r.workspace, scope, name, desc, body); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	fmt.Printf("  %s%s✓  Skill '%s' created (%s).%s\n", ansiBold, colorGreen, name, scope, ansiReset)
}

func (r *TolleREPL) editSkill(reader *bufio.Reader, skills []skillDef) {
	idx := pickSkillIndex(skills)
	if idx < 0 {
		return
	}
	sk := skills[idx]
	fmt.Printf("\n  %s%s◈  Edit %s%s\n", colorPrimary, ansiBold, sk.Name, ansiReset)
	desc := promptLine(reader, "Description", sk.Description)
	fmt.Printf("  %sCurrent body (%d chars). Enter new body or '.' alone to keep.%s\n",
		ansiDim, len(sk.Body), ansiReset)
	body := promptMultiline(reader, "Skill instructions", sk.Body)
	scope := "global"
	if sk.Source == "workspace" {
		scope = "workspace"
	}
	if err := writeSkillFile(r.workspace, scope, sk.Name, desc, body); err != nil {
		fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
		return
	}
	fmt.Printf("  %s%s✓  Saved.%s\n", ansiBold, colorGreen, ansiReset)
}

func (r *TolleREPL) deleteSkill(reader *bufio.Reader, skills []skillDef) {
	idx := pickSkillIndex(skills)
	if idx < 0 {
		return
	}
	sk := skills[idx]
	if !confirmYesNo(reader, fmt.Sprintf("Delete skill '%s'?", sk.Name)) {
		return
	}
	dir := filepath.Join(config.Home(), "skills")
	if sk.Source == "workspace" && r.workspace != "" {
		dir = filepath.Join(r.workspace, ".agent", "skills")
	}
	if p := findSkillFileByName(dir, sk.Name); p != "" {
		if err := os.Remove(p); err != nil {
			fmt.Printf("  %s%s✗  %s%s\n", ansiBold, colorRed, err.Error(), ansiReset)
			return
		}
	}
	fmt.Printf("  %sDeleted '%s'.%s\n", ansiDim, sk.Name, ansiReset)
}

func pickSkillIndex(skills []skillDef) int {
	if len(skills) == 0 {
		fmt.Printf("  %sNo skills to choose from.%s\n", ansiDim, ansiReset)
		return -1
	}
	items := make([]pickItem, len(skills))
	for i, sk := range skills {
		items[i] = pickItem{id: fmt.Sprintf("%d", i), label: sk.Name, sublabel: sk.Source}
	}
	id, ok := RunSingleSelect("Select skill", items, false)
	if !ok || id == "" {
		return -1
	}
	var n int
	if _, err := fmt.Sscanf(id, "%d", &n); err == nil && n >= 0 && n < len(skills) {
		return n
	}
	return -1
}

// writeSkillFile writes a frontmatter .md skill to the global or workspace skills
// dir, matching the format the desktop and runtime skill loader expect.
func writeSkillFile(workspace, scope, name, desc, body string) error {
	dir := filepath.Join(config.Home(), "skills")
	if scope == "workspace" && workspace != "" {
		dir = filepath.Join(workspace, ".agent", "skills")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	filename := sanitizeID(name)
	if filename == "" {
		filename = "skill-" + time.Now().Format("20060102150405")
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\nversion: 1.0\n---\n\n" + body + "\n"
	return os.WriteFile(filepath.Join(dir, filename+".md"), []byte(content), 0o644)
}

// findSkillFileByName scans dir for a .md whose frontmatter name matches, falling
// back to the sanitized filename used when the CLI/desktop created it.
func findSkillFileByName(dir, skillName string) string {
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

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

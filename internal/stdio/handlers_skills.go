package stdio

func handleGetSkills(state *ServerState, cmd map[string]any) {
	state.mu.Lock()
	active := append([]string{}, state.ActiveSkills...)
	state.mu.Unlock()
	// TODO Phase 6: discover global + workspace skills
	Emit(map[string]any{"type": "skills", "data": []any{}, "active": active})
}

func handleSetSkill(state *ServerState, cmd map[string]any) {
	skillName, _ := cmd["skill_name"].(string)
	enabled, _ := cmd["enabled"].(bool)

	state.mu.Lock()
	if enabled {
		found := false
		for _, s := range state.ActiveSkills {
			if s == skillName {
				found = true
				break
			}
		}
		if !found {
			state.ActiveSkills = append(state.ActiveSkills, skillName)
		}
	} else {
		filtered := state.ActiveSkills[:0]
		for _, s := range state.ActiveSkills {
			if s != skillName {
				filtered = append(filtered, s)
			}
		}
		state.ActiveSkills = filtered
	}
	active := append([]string{}, state.ActiveSkills...)
	state.mu.Unlock()

	Emit(map[string]any{"type": "skills_updated", "active": active})
}

func handleGetUsage(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	data := aggregateUsage(ws)
	Emit(map[string]any{"type": "usage", "data": data})
}

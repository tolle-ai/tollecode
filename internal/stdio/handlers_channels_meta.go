package stdio

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// channelMeta is stored in <workspace>/.agent/channels-meta.json as a JSON array.
type channelMeta struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Section       string `json:"section"`
	AgentID       string `json:"agentId,omitempty"`
	AgentColor    string `json:"agentColor,omitempty"`
	AgentInitial  string `json:"agentInitial,omitempty"`
	AgentPhoto    string `json:"agentPhoto,omitempty"`
	AgentGradient string `json:"agentGradient,omitempty"`
	AgentRole     string `json:"agentRole,omitempty"`
	Description   string `json:"description,omitempty"`
}

func channelMetaPath(workspace string) string {
	return filepath.Join(workspace, ".agent", "channels-meta.json")
}

func loadChannelsMeta(workspace string) []channelMeta {
	data, err := os.ReadFile(channelMetaPath(workspace))
	if err != nil {
		return []channelMeta{}
	}
	var list []channelMeta
	if json.Unmarshal(data, &list) != nil {
		return []channelMeta{}
	}
	return list
}

func saveChannelsMeta(workspace string, list []channelMeta) {
	path := channelMetaPath(workspace)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(list)
	_ = os.WriteFile(path, data, 0o644)
}

func metaToMap(c channelMeta) map[string]any {
	return map[string]any{
		"id":            c.ID,
		"name":          c.Name,
		"section":       c.Section,
		"agentId":       c.AgentID,
		"agentColor":    c.AgentColor,
		"agentInitial":  c.AgentInitial,
		"agentPhoto":    c.AgentPhoto,
		"agentGradient": c.AgentGradient,
		"agentRole":     c.AgentRole,
		"description":   c.Description,
	}
}

func handleChannelsList(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	list := loadChannelsMeta(ws)
	channels := make([]map[string]any, len(list))
	for i, c := range list {
		channels[i] = metaToMap(c)
	}
	Emit(map[string]any{"type": "channels_list", "channels": channels})
}

func handleChannelsCreate(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	name, _ := cmd["name"].(string)
	section, _ := cmd["section"].(string)
	if section == "" {
		section = "projects"
	}
	description, _ := cmd["description"].(string)

	meta := channelMeta{
		ID:          uuid.NewString(),
		Name:        name,
		Section:     section,
		Description: description,
	}

	list := loadChannelsMeta(ws)
	list = append(list, meta)
	saveChannelsMeta(ws, list)

	Emit(map[string]any{"type": "channel_created", "channel": metaToMap(meta)})
}

func handleChannelsPatch(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	channelID, _ := cmd["channelId"].(string)

	list := loadChannelsMeta(ws)
	var patched *channelMeta
	for i := range list {
		if list[i].ID != channelID {
			continue
		}
		if v, ok := cmd["agentId"].(string); ok {
			list[i].AgentID = v
		}
		if v, ok := cmd["agentColor"].(string); ok {
			list[i].AgentColor = v
		}
		if v, ok := cmd["agentInitial"].(string); ok {
			list[i].AgentInitial = v
		}
		if v, ok := cmd["agentPhoto"].(string); ok {
			list[i].AgentPhoto = v
		}
		if v, ok := cmd["agentGradient"].(string); ok {
			list[i].AgentGradient = v
		}
		if v, ok := cmd["agentRole"].(string); ok {
			list[i].AgentRole = v
		}
		if v, ok := cmd["section"].(string); ok && v != "" {
			list[i].Section = v
		}
		if v, ok := cmd["name"].(string); ok && v != "" {
			list[i].Name = v
		}
		patched = &list[i]
		break
	}
	saveChannelsMeta(ws, list)

	var ch map[string]any
	if patched != nil {
		ch = metaToMap(*patched)
	} else {
		ch = map[string]any{"id": channelID}
	}
	Emit(map[string]any{"type": "channel_patched", "channel": ch})
}

// removeChannelMeta removes a channel from the metadata list (called on delete).
func removeChannelMeta(workspace, channelID string) {
	list := loadChannelsMeta(workspace)
	filtered := list[:0]
	for _, c := range list {
		if c.ID != channelID {
			filtered = append(filtered, c)
		}
	}
	saveChannelsMeta(workspace, filtered)
}

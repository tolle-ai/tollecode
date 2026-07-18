package stdio

import "github.com/tolle-ai/tollecode/internal/config"

// The Lite frontend's StoreService speaks these three commands to persist its
// client-side settings in the sidecar's shared KV (~/.tollecode/lite_kv.json).
// Desktop mirrors its native store here (write-through) and web mode uses this
// as its primary store, so both shells see the same providers/agents/teams/
// workspaces. See internal/config/litekv.go.

// handleKVGetAll returns the entire shared KV as an array of [key, value] pairs,
// matching the desktop Tauri command's [string, string][] shape.
func handleKVGetAll(state *ServerState, cmd map[string]any) {
	kv := config.LiteKVGetAll()
	entries := make([][2]string, 0, len(kv))
	for k, v := range kv {
		entries = append(entries, [2]string{k, v})
	}
	Emit(map[string]any{"type": "kv_all", "entries": entries})
}

func handleKVSet(state *ServerState, cmd map[string]any) {
	key, _ := cmd["key"].(string)
	if key == "" {
		return
	}
	value, _ := cmd["value"].(string)
	_ = config.LiteKVSet(key, value)
}

func handleKVRemove(state *ServerState, cmd map[string]any) {
	key, _ := cmd["key"].(string)
	if key == "" {
		return
	}
	_ = config.LiteKVRemove(key)
}

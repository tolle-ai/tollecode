package stdio

import (
	"context"

	"github.com/tolle-ai/tollecode/internal/mcp"
)

// ── MCP server management ─────────────────────────────────────────────────────

// handleMCPList returns the configured MCP servers for the workspace.
// cmd: { type: "mcp_list" }
func handleMCPList(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	cfgs, err := mcp.LoadConfig(ws)
	if err != nil {
		Emit(map[string]any{"type": "mcp_error", "message": err.Error()})
		return
	}
	out := make([]map[string]any, len(cfgs))
	for i, c := range cfgs {
		out[i] = serverConfigToMap(c)
	}
	Emit(map[string]any{"type": "mcp_list", "servers": out})
}

// handleMCPAdd adds or replaces a server config and optionally connects.
// cmd: { type: "mcp_add", name, transport, command?, args?, env?, url?, enabled? }
func handleMCPAdd(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "mcp_error", "message": "no workspace set"})
		return
	}
	cfg, err := mapToServerConfig(cmd)
	if err != nil {
		Emit(map[string]any{"type": "mcp_error", "message": err.Error()})
		return
	}

	cfgs, _ := mcp.LoadConfig(ws)
	replaced := false
	for i, c := range cfgs {
		if c.Name == cfg.Name {
			cfgs[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		cfgs = append(cfgs, cfg)
	}
	if err := mcp.SaveConfig(ws, cfgs); err != nil {
		Emit(map[string]any{"type": "mcp_error", "message": err.Error()})
		return
	}

	// Reload registry so the new server is immediately available.
	go func() {
		ctx := context.Background()
		_ = mcp.Global.Get(ws).Reload(ctx)
		Emit(map[string]any{"type": "mcp_added", "server": serverConfigToMap(cfg)})
	}()
}

// handleMCPRemove removes a server and disconnects it.
// cmd: { type: "mcp_remove", name: string }
func handleMCPRemove(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	name, _ := cmd["name"].(string)
	if name == "" {
		Emit(map[string]any{"type": "mcp_error", "message": "'name' is required"})
		return
	}

	cfgs, _ := mcp.LoadConfig(ws)
	filtered := cfgs[:0]
	for _, c := range cfgs {
		if c.Name != name {
			filtered = append(filtered, c)
		}
	}
	if err := mcp.SaveConfig(ws, filtered); err != nil {
		Emit(map[string]any{"type": "mcp_error", "message": err.Error()})
		return
	}

	go func() {
		ctx := context.Background()
		_ = mcp.Global.Get(ws).Reload(ctx)
		Emit(map[string]any{"type": "mcp_removed", "name": name})
	}()
}

// handleMCPListTools fetches tools from all connected MCP servers.
// cmd: { type: "mcp_list_tools" }
func handleMCPListTools(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "mcp_tools", "tools": []any{}})
		return
	}
	ctx := context.Background()
	tools := mcp.Global.Get(ws).Tools(ctx)
	out := make([]map[string]any, len(tools))
	for i, t := range tools {
		out[i] = map[string]any{
			"name":        t.Name,
			"description": t.Description,
		}
	}
	Emit(map[string]any{"type": "mcp_tools", "tools": out})
}

// handleMCPReload reconnects all enabled MCP servers for the workspace.
// cmd: { type: "mcp_reload" }
func handleMCPReload(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "mcp_error", "message": "no workspace set"})
		return
	}
	go func() {
		ctx := context.Background()
		if err := mcp.Global.Get(ws).Reload(ctx); err != nil {
			Emit(map[string]any{"type": "mcp_error", "message": err.Error()})
			return
		}
		Emit(map[string]any{"type": "mcp_reloaded"})
	}()
}

// ── Custom tool management ────────────────────────────────────────────────────

// handleCustomToolsList lists custom workspace tools.
// cmd: { type: "custom_tools_list" }
func handleCustomToolsList(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	tools, err := mcp.LoadCustomTools(ws)
	if err != nil {
		Emit(map[string]any{"type": "custom_tools_error", "message": err.Error()})
		return
	}
	out := make([]map[string]any, len(tools))
	for i, t := range tools {
		out[i] = map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"command":     t.Command,
			"enabled":     t.Enabled,
		}
	}
	Emit(map[string]any{"type": "custom_tools_list", "tools": out})
}

// handleCustomToolsSave creates or replaces a custom tool definition.
// cmd: { type: "custom_tools_save", name, description, inputSchema, command, env?, enabled? }
func handleCustomToolsSave(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "custom_tools_error", "message": "no workspace set"})
		return
	}
	name, _ := cmd["name"].(string)
	if name == "" {
		Emit(map[string]any{"type": "custom_tools_error", "message": "'name' is required"})
		return
	}
	command, _ := cmd["command"].(string)
	if command == "" {
		Emit(map[string]any{"type": "custom_tools_error", "message": "'command' is required"})
		return
	}
	description, _ := cmd["description"].(string)
	enabled := true
	if v, ok := cmd["enabled"].(bool); ok {
		enabled = v
	}
	schema, _ := cmd["inputSchema"].(map[string]any)
	envRaw, _ := cmd["env"].(map[string]any)
	env := make(map[string]string, len(envRaw))
	for k, v := range envRaw {
		if s, ok := v.(string); ok {
			env[k] = s
		}
	}

	tool := mcp.CustomTool{
		Name: name, Description: description,
		InputSchema: schema, Command: command,
		Env: env, Enabled: enabled,
	}

	tools, _ := mcp.LoadCustomTools(ws)
	replaced := false
	for i, t := range tools {
		if t.Name == name {
			tools[i] = tool
			replaced = true
			break
		}
	}
	if !replaced {
		tools = append(tools, tool)
	}
	if err := mcp.SaveCustomTools(ws, tools); err != nil {
		Emit(map[string]any{"type": "custom_tools_error", "message": err.Error()})
		return
	}
	Emit(map[string]any{"type": "custom_tools_saved", "name": name})
}

// handleCustomToolsDelete removes a custom tool by name.
// cmd: { type: "custom_tools_delete", name: string }
func handleCustomToolsDelete(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	name, _ := cmd["name"].(string)
	if name == "" {
		Emit(map[string]any{"type": "custom_tools_error", "message": "'name' is required"})
		return
	}
	tools, _ := mcp.LoadCustomTools(ws)
	filtered := tools[:0]
	for _, t := range tools {
		if t.Name != name {
			filtered = append(filtered, t)
		}
	}
	if err := mcp.SaveCustomTools(ws, filtered); err != nil {
		Emit(map[string]any{"type": "custom_tools_error", "message": err.Error()})
		return
	}
	Emit(map[string]any{"type": "custom_tools_deleted", "name": name})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func serverConfigToMap(c mcp.ServerConfig) map[string]any {
	return map[string]any{
		"name":    c.Name,
		"type":    c.Type,
		"command": c.Command,
		"args":    c.Args,
		"url":     c.URL,
		"enabled": c.Enabled,
	}
}

func mapToServerConfig(cmd map[string]any) (mcp.ServerConfig, error) {
	name, _ := cmd["name"].(string)
	if name == "" {
		return mcp.ServerConfig{}, errMissing("name")
	}
	transport, _ := cmd["type"].(string)
	if transport == "" {
		transport, _ = cmd["transport"].(string)
	}
	if transport == "" {
		return mcp.ServerConfig{}, errMissing("type")
	}
	enabled := true
	if v, ok := cmd["enabled"].(bool); ok {
		enabled = v
	}
	cfg := mcp.ServerConfig{
		Name:    name,
		Type:    transport,
		Enabled: enabled,
		URL:     strField(cmd, "url"),
		Command: strField(cmd, "command"),
	}
	if rawArgs, ok := cmd["args"].([]any); ok {
		for _, a := range rawArgs {
			if s, ok := a.(string); ok {
				cfg.Args = append(cfg.Args, s)
			}
		}
	}
	if rawEnv, ok := cmd["env"].(map[string]any); ok {
		cfg.Env = make(map[string]string, len(rawEnv))
		for k, v := range rawEnv {
			if s, ok := v.(string); ok {
				cfg.Env[k] = s
			}
		}
	}
	return cfg, nil
}

func errMissing(field string) error {
	return &missingFieldError{field}
}

type missingFieldError struct{ field string }

func (e *missingFieldError) Error() string { return "'" + e.field + "' is required" }

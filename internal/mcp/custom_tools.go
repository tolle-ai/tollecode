package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// CustomTool defines a workspace-local tool backed by a shell command.
// Template variables {{param_name}} in Command are replaced with argument values.
type CustomTool struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	InputSchema map[string]any    `json:"inputSchema"`
	Command     string            `json:"command"`
	Env         map[string]string `json:"env,omitempty"`
	Enabled     bool              `json:"enabled"`
}

func customToolsPath(workspace string) string {
	return filepath.Join(workspace, ".agent", "tools.json")
}

// LoadCustomTools reads custom tool definitions from <workspace>/.agent/tools.json.
func LoadCustomTools(workspace string) ([]CustomTool, error) {
	data, err := os.ReadFile(customToolsPath(workspace))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tools []CustomTool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, err
	}
	return tools, nil
}

// SaveCustomTools writes custom tool definitions to disk.
func SaveCustomTools(workspace string, tools []CustomTool) error {
	dir := filepath.Join(workspace, ".agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(customToolsPath(workspace), data, 0o644)
}

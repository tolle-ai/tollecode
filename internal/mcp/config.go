package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func configPath(workspace string) string {
	return filepath.Join(workspace, ".agent", "mcp.json")
}

// LoadConfig reads the MCP server list from <workspace>/.agent/mcp.json.
// Returns an empty slice (not an error) when the file doesn't exist yet.
func LoadConfig(workspace string) ([]ServerConfig, error) {
	data, err := os.ReadFile(configPath(workspace))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfgs []ServerConfig
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return nil, err
	}
	return cfgs, nil
}

// SaveConfig writes the MCP server list to disk.
func SaveConfig(workspace string, cfgs []ServerConfig) error {
	dir := filepath.Join(workspace, ".agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(workspace), data, 0o644)
}

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tolle-ai/tollecode/internal/mcp"
	"github.com/tolle-ai/tollecode/internal/shellenv"
)

// toolRunCustom executes a custom workspace tool by name.
// Arguments are passed as environment variables (TOLL_<UPPER_KEY>=value) so
// they cannot break out of the shell command via injection. Templates using
// {{param_name}} syntax automatically use the env-var form.
func toolRunCustom(ctx context.Context, cfg *Config, name string, inp map[string]any) (string, bool) {
	tools, err := mcp.LoadCustomTools(cfg.Workspace)
	if err != nil {
		return "Error loading custom tools: " + err.Error(), true
	}

	var ct *mcp.CustomTool
	for i := range tools {
		if tools[i].Name == name && tools[i].Enabled {
			ct = &tools[i]
			break
		}
	}
	if ct == nil {
		return fmt.Sprintf("Custom tool %q not found or not enabled.", name), true
	}

	// Build the command template: replace {{param}} with $TOLL_PARAM so values
	// are expanded safely via the environment, never interpolated directly.
	cmd := ct.Command
	env := os.Environ()
	for k, v := range inp {
		envKey := "TOLL_" + strings.ToUpper(k)
		cmd = strings.ReplaceAll(cmd, "{{"+k+"}}", "$"+envKey)
		env = append(env, envKey+"="+fmt.Sprintf("%v", v))
	}
	for k, v := range ct.Env {
		env = append(env, k+"="+v)
	}

	shell, err := shellenv.Lookup("sh")
	if err != nil {
		return "Failed to run custom tool: " + err.Error(), true
	}
	sh := exec.CommandContext(ctx, shell.Path, append(shell.Args(false), cmd)...)
	sh.Dir = cfg.Workspace
	sh.Env = env

	var out bytes.Buffer
	sh.Stdout = &out
	sh.Stderr = &out

	if err := sh.Run(); err != nil {
		return out.String() + "\nError: " + err.Error(), true
	}
	result := out.String()
	const maxLen = 8000
	if len(result) > maxLen {
		result = result[:maxLen] + "\n[output truncated]"
	}
	return result, false
}

package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxReadLines = 2000

// safeJoin resolves rel relative to workspace and rejects path traversal. It also
// walls off the internal .agent directory: through the generic file tools an agent
// (or sub-agent) may only reach .agent/plans and .agent/memory. Everything else
// under .agent — credentials in email_config.json / calendar_token.json, OAuth
// tokens, skills, config — is off-limits. The dedicated plan/memory/skill tools
// write those paths directly (not via safeJoin) so they are unaffected.
func safeJoin(workspace, rel string) (string, error) {
	base, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Join(base, rel))
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs+string(filepath.Separator), base+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal blocked: %s", rel)
	}
	if restrictedAgentPath(base, abs) {
		return "", fmt.Errorf(".agent is internal to tollecode — only .agent/plans and .agent/memory are accessible")
	}
	return abs, nil
}

// restrictedAgentPath reports whether abs (already confirmed inside the workspace)
// lands in the internal .agent directory but OUTSIDE the two subtrees the file
// tools are permitted to touch: .agent/plans and .agent/memory.
func restrictedAgentPath(workspaceAbs, abs string) bool {
	rel, err := filepath.Rel(workspaceAbs, abs)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel != ".agent" && !strings.HasPrefix(rel, ".agent/") {
		return false // not under .agent at all
	}
	for _, allowed := range []string{".agent/plans", ".agent/memory"} {
		if rel == allowed || strings.HasPrefix(rel, allowed+"/") {
			return false
		}
	}
	return true
}

func toolReadFile(workspace string, inp map[string]any) string {
	rel, _ := inp["path"].(string)
	if rel == "" {
		return "Error: 'path' is required."
	}
	path, err := safeJoin(workspace, rel)
	if err != nil {
		return "Error: " + err.Error()
	}

	startLine := toInt(inp["start_line"])
	endLine := toInt(inp["end_line"])

	if startLine > 0 || endLine > 0 {
		return readLineRange(path, startLine, endLine)
	}

	// No range specified — count total lines first so we can warn if capped.
	totalLines, countErr := countLines(path)

	if countErr != nil {
		return "Error reading file: " + countErr.Error()
	}

	end := totalLines
	if end > maxReadLines {
		end = maxReadLines
	}
	result := readLineRange(path, 1, end)
	if totalLines > maxReadLines {
		result += fmt.Sprintf("\n\n[File has %d lines total. Only lines 1–%d shown. Use start_line/end_line to read further sections.]", totalLines, maxReadLines)
	}
	return result
}

// readLineRange returns lines [startLine, endLine] (1-indexed, inclusive) of the
// file, each prefixed with its line number in cat -n style. endLine <= 0 reads
// to end of file. Implemented in pure Go so it works identically on every OS
// (no dependency on a Unix sed binary).
func readLineRange(path string, startLine, endLine int) string {
	if startLine <= 0 {
		startLine = 1
	}

	f, err := os.Open(path)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	defer f.Close()

	// Prefix each line with its line number (cat -n style).
	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo < startLine {
			continue
		}
		if endLine > 0 && lineNo > endLine {
			break
		}
		fmt.Fprintf(&sb, "%d\t%s\n", lineNo, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "Error reading file: " + err.Error()
	}
	return sb.String()
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	n := 0
	for scanner.Scan() {
		n++
	}
	return n, scanner.Err()
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case int64:
		return int(x)
	}
	return 0
}

func toolWriteFile(ctx context.Context, cfg *Config, inp map[string]any) string {
	if cfg.Mode == "plan" {
		return "[Plan mode] write_file skipped — recorded in plan."
	}
	rel, _ := inp["path"].(string)
	content, _ := inp["content"].(string)
	if rel == "" {
		return "Error: 'path' is required."
	}
	switch cfg.checkPermission(ctx, "file", "write_file: "+rel) {
	case permUnavailable:
		return "File write permission is not available in this context. Do not retry or try alternative approaches (e.g., using run_shell to write files). Inform the user if this capability is needed."
	case permDenied:
		if cfg.EmitEvent != nil {
			cfg.EmitEvent(map[string]any{"type": "permission_denied", "tool": "write_file", "detail": rel})
		}
		return "Permission denied by the user. Do NOT retry this operation, do not try alternative approaches (e.g., using run_shell to write files), and do not ask for permission again. Move on to tasks that don't require this permission, or inform the user what you need."
	}
	path, err := safeJoin(cfg.Workspace, rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	// Capture the prior content before overwriting so the CLI can show a diff of
	// exactly what changed (empty + created=true when the file is brand new).
	oldData, readErr := os.ReadFile(path)
	created := readErr != nil
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "Error creating directory: " + err.Error()
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "Error writing file: " + err.Error()
	}
	emitFileDiff(cfg, rel, string(oldData), content, created)
	return "Written: " + rel
}

func toolEditFile(ctx context.Context, cfg *Config, inp map[string]any) string {
	if cfg.Mode == "plan" {
		return "[Plan mode] edit_file skipped — recorded in plan."
	}
	rel, _ := inp["path"].(string)
	oldStr, _ := inp["old_string"].(string)
	newStr, _ := inp["new_string"].(string)
	replaceAll, _ := inp["replace_all"].(bool)
	if rel == "" {
		return "Error: 'path' is required."
	}
	if oldStr == "" {
		return "Error: 'old_string' is required for edit_file."
	}
	switch cfg.checkPermission(ctx, "file", "edit_file: "+rel) {
	case permUnavailable:
		return "File write permission is not available in this context. Do not retry or try alternative approaches. Inform the user if this capability is needed."
	case permDenied:
		if cfg.EmitEvent != nil {
			cfg.EmitEvent(map[string]any{"type": "permission_denied", "tool": "edit_file", "detail": rel})
		}
		return "Permission denied by the user. Do NOT retry this operation, do not try alternative approaches, and do not ask for permission again. Move on to tasks that don't require this permission, or inform the user what you need."
	}
	path, err := safeJoin(cfg.Workspace, rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "Error: file not found — " + rel + ". Use write_file to create it first."
		}
		return "Error reading file: " + err.Error()
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "Error: old_string not found in " + rel + ". " +
			"Make sure the string matches exactly (including whitespace and indentation). " +
			"Use read_file to verify the exact content before calling edit_file."
	}
	if count > 1 && !replaceAll {
		return fmt.Sprintf(
			"Error: old_string appears %d times in %s. "+
				"Provide more surrounding context to make it unique, "+
				"or set replace_all=true to replace every occurrence.", count, rel)
	}
	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		updated = strings.Replace(content, oldStr, newStr, 1)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "Error writing file: " + err.Error()
	}
	emitFileDiff(cfg, rel, content, updated, false)
	if replaceAll && count > 1 {
		return fmt.Sprintf("Edited: %s (all %d occurrences replaced)", rel, count)
	}
	return "Edited: " + rel + " (1 occurrence replaced)"
}

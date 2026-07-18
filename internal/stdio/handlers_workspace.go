package stdio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/tolle-ai/tollecode/internal/ai"
)

var ignoreSet = map[string]bool{
	".git": true, ".agent": true, "__pycache__": true, "node_modules": true,
	".pnpm": true, ".yarn": true, ".venv": true, "venv": true, "env": true,
	".env": true, ".next": true, ".nuxt": true, ".svelte-kit": true,
	".output": true, ".turbo": true, ".angular": true, ".cache": true,
	"dist": true, "build": true, "out": true, "target": true,
	"coverage": true, "storybook-static": true, "tmp": true, "temp": true, "vendor": true,
}

func handleWorkspaceFiles(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	subPath, _ := cmd["path"].(string)
	includeFolders, _ := cmd["includeFolders"].(bool)

	root, err := filepath.Abs(wsPath)
	if err != nil || !isDir(root) {
		Emit(map[string]any{"type": "workspace_files", "files": []any{}, "folders": []any{}, "truncated": false})
		return
	}

	base := root
	if subPath != "" {
		base = resolveWorkspacePath(root, subPath)
	}

	var files, folders []string
	truncated := false

	filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && ignoreSet[d.Name()] {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if includeFolders && path != base {
				folders = append(folders, path)
			}
			return nil
		}
		if d.Name() == ".DS_Store" {
			return nil
		}
		files = append(files, path)
		if len(files)+len(folders) >= 5000 {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})

	sort.Strings(files)
	sort.Strings(folders)
	Emit(map[string]any{
		"type":      "workspace_files",
		"files":     stringsToAny(files),
		"folders":   stringsToAny(folders),
		"truncated": truncated,
	})
}

func handleWorkspaceLs(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	subPath, _ := cmd["path"].(string)

	root, _ := filepath.Abs(wsPath)
	target := root
	if subPath != "" {
		target = resolveWorkspacePath(root, subPath)
	}

	entries := []map[string]any{}
	if !isDir(target) {
		Emit(map[string]any{"type": "workspace_ls", "entries": entries})
		return
	}

	dirEntries, _ := os.ReadDir(target)
	sort.Slice(dirEntries, func(i, j int) bool {
		a, b := dirEntries[i], dirEntries[j]
		if a.IsDir() != b.IsDir() {
			return a.IsDir()
		}
		return strings.ToLower(a.Name()) < strings.ToLower(b.Name())
	})

	for _, de := range dirEntries {
		if ignoreSet[de.Name()] || de.Name() == ".DS_Store" {
			continue
		}
		absPath := filepath.Join(target, de.Name())
		entries = append(entries, map[string]any{
			"name":  de.Name(),
			"path":  absPath,
			"isDir": de.IsDir(),
		})
	}
	Emit(map[string]any{"type": "workspace_ls", "entries": entries})
}

func handleWorkspaceSearch(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	q, _ := cmd["q"].(string)
	dirScope, _ := cmd["dir"].(string)

	root, _ := filepath.Abs(wsPath)
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" || !isDir(root) {
		Emit(map[string]any{"type": "workspace_search", "entries": []any{}})
		return
	}

	searchRoot := root
	if dirScope != "" {
		searchRoot = filepath.Join(root, dirScope)
	}

	const maxResults, maxWalk = 50, 15000
	walked := 0
	var matches []map[string]any

	filepath.WalkDir(searchRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || walked >= maxWalk {
			return filepath.SkipAll
		}
		if d.IsDir() && ignoreSet[d.Name()] {
			return filepath.SkipDir
		}
		walked++
		if d.Name() == ".DS_Store" {
			return nil
		}
		if strings.Contains(strings.ToLower(d.Name()), q) {
			matches = append(matches, map[string]any{
				"name":  d.Name(),
				"path":  path,
				"isDir": d.IsDir(),
			})
		}
		return nil
	})

	sort.Slice(matches, func(i, j int) bool {
		ni, nj := strings.ToLower(matches[i]["name"].(string)), strings.ToLower(matches[j]["name"].(string))
		if ni == q && nj != q {
			return true
		}
		if strings.HasPrefix(ni, q) && !strings.HasPrefix(nj, q) {
			return true
		}
		return ni < nj
	})
	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}

	out := make([]any, len(matches))
	for i, m := range matches {
		out[i] = m
	}
	Emit(map[string]any{"type": "workspace_search", "entries": out})
}

func handleWorkspaceFile(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	path, _ := cmd["path"].(string)

	root, _ := filepath.Abs(wsPath)
	target := resolveWorkspacePath(root, path)
	if !strings.HasPrefix(target, root) {
		Emit(map[string]any{"type": "workspace_file", "path": path, "content": "", "error": "path outside workspace"})
		return
	}
	data, err := os.ReadFile(target)
	if err != nil {
		Emit(map[string]any{"type": "workspace_file", "path": path, "content": "", "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "workspace_file", "path": path, "content": string(data)})
}

func handleWorkspaceWriteFile(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	path, _ := cmd["path"].(string)
	content, _ := cmd["content"].(string)

	root, _ := filepath.Abs(wsPath)
	target := resolveWorkspacePath(root, path)
	if !strings.HasPrefix(target, root) {
		Emit(map[string]any{"type": "workspace_write_file", "path": path, "ok": false, "error": "path outside workspace"})
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		Emit(map[string]any{"type": "workspace_write_file", "path": path, "ok": false, "error": err.Error()})
		return
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		Emit(map[string]any{"type": "workspace_write_file", "path": path, "ok": false, "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "workspace_write_file", "path": path, "ok": true})
}

// handleWorkspaceCreate creates a new empty file or directory.
func handleWorkspaceCreate(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	path, _ := cmd["path"].(string)
	isDirReq, _ := cmd["isDir"].(bool)

	root, _ := filepath.Abs(wsPath)
	target := resolveWorkspacePath(root, path)
	if !strings.HasPrefix(target, root) || target == root {
		Emit(map[string]any{"type": "workspace_create", "ok": false, "error": "path outside workspace"})
		return
	}
	if _, err := os.Stat(target); err == nil {
		Emit(map[string]any{"type": "workspace_create", "ok": false, "error": "a file or folder with this name already exists"})
		return
	}
	if isDirReq {
		if err := os.MkdirAll(target, 0o755); err != nil {
			Emit(map[string]any{"type": "workspace_create", "ok": false, "error": err.Error()})
			return
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			Emit(map[string]any{"type": "workspace_create", "ok": false, "error": err.Error()})
			return
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			Emit(map[string]any{"type": "workspace_create", "ok": false, "error": err.Error()})
			return
		}
		f.Close()
	}
	Emit(map[string]any{"type": "workspace_create", "ok": true, "path": target, "isDir": isDirReq})
}

// handleWorkspaceDelete removes a file or directory (recursively).
func handleWorkspaceDelete(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	path, _ := cmd["path"].(string)

	root, _ := filepath.Abs(wsPath)
	target := resolveWorkspacePath(root, path)
	if !strings.HasPrefix(target, root) || target == root {
		Emit(map[string]any{"type": "workspace_delete", "ok": false, "error": "invalid path"})
		return
	}
	if err := os.RemoveAll(target); err != nil {
		Emit(map[string]any{"type": "workspace_delete", "ok": false, "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "workspace_delete", "ok": true, "path": target})
}

// handleWorkspaceRename renames or moves a file/directory. Also used for
// cut-and-paste and drag-and-drop moves in the explorer.
func handleWorkspaceRename(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	from, _ := cmd["from"].(string)
	to, _ := cmd["to"].(string)

	root, _ := filepath.Abs(wsPath)
	src := resolveWorkspacePath(root, from)
	dst := resolveWorkspacePath(root, to)
	if !strings.HasPrefix(src, root) || !strings.HasPrefix(dst, root) || src == root {
		Emit(map[string]any{"type": "workspace_rename", "ok": false, "error": "path outside workspace"})
		return
	}
	if src == dst {
		Emit(map[string]any{"type": "workspace_rename", "ok": true, "from": src, "to": dst})
		return
	}
	if _, err := os.Stat(dst); err == nil {
		Emit(map[string]any{"type": "workspace_rename", "ok": false, "error": "a file or folder with this name already exists"})
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		Emit(map[string]any{"type": "workspace_rename", "ok": false, "error": err.Error()})
		return
	}
	if err := os.Rename(src, dst); err != nil {
		Emit(map[string]any{"type": "workspace_rename", "ok": false, "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "workspace_rename", "ok": true, "from": src, "to": dst})
}

// handleWorkspaceCopy copies a file/directory (recursively). Used for
// copy-and-paste and duplicate in the explorer.
func handleWorkspaceCopy(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	from, _ := cmd["from"].(string)
	to, _ := cmd["to"].(string)

	root, _ := filepath.Abs(wsPath)
	src := resolveWorkspacePath(root, from)
	dst := resolveWorkspacePath(root, to)
	if !strings.HasPrefix(src, root) || !strings.HasPrefix(dst, root) || src == root {
		Emit(map[string]any{"type": "workspace_copy", "ok": false, "error": "path outside workspace"})
		return
	}
	if _, err := os.Stat(dst); err == nil {
		Emit(map[string]any{"type": "workspace_copy", "ok": false, "error": "a file or folder with this name already exists"})
		return
	}
	// Refuse to copy a directory into itself.
	if strings.HasPrefix(dst, src+string(os.PathSeparator)) {
		Emit(map[string]any{"type": "workspace_copy", "ok": false, "error": "cannot copy a folder into itself"})
		return
	}
	if err := copyPath(src, dst); err != nil {
		Emit(map[string]any{"type": "workspace_copy", "ok": false, "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "workspace_copy", "ok": true, "from": src, "to": dst})
}

// handleWorkspaceReveal opens the OS file manager with the path selected.
func handleWorkspaceReveal(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	path, _ := cmd["path"].(string)

	root, _ := filepath.Abs(wsPath)
	target := resolveWorkspacePath(root, path)
	if !strings.HasPrefix(target, root) {
		Emit(map[string]any{"type": "workspace_reveal", "ok": false, "error": "path outside workspace"})
		return
	}

	var err error
	switch runtime.GOOS {
	case "darwin":
		err = exec.Command("open", "-R", target).Start()
	case "windows":
		err = exec.Command("explorer", "/select,", target).Start()
	default:
		// Linux: open the containing directory (xdg-open has no reliable "select")
		dir := target
		if !isDir(target) {
			dir = filepath.Dir(target)
		}
		err = exec.Command("xdg-open", dir).Start()
	}
	if err != nil {
		Emit(map[string]any{"type": "workspace_reveal", "ok": false, "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "workspace_reveal", "ok": true})
}

// copyPath recursively copies src to dst, preserving file modes.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode())
}

func handleWorkspaceGitInfo(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	filePath, _ := cmd["path"].(string)

	if wsPath == "" {
		Emit(map[string]any{"type": "workspace_git_info", "branch": "", "status": "", "diff": ""})
		return
	}

	root, _ := filepath.Abs(wsPath)

	branchOut, _ := exec.Command("git", "-C", root, "rev-parse", "--abbrev-ref", "HEAD").Output()
	branch := strings.TrimSpace(string(branchOut))

	statusOut, _ := exec.Command("git", "-C", root, "status", "--short").Output()

	// Count commits ahead of remote tracking branch (0 if no upstream)
	ahead := 0
	aheadOut, err := exec.Command("git", "-C", root, "rev-list", "--count", "@{u}..HEAD").Output()
	if err == nil {
		fmt.Sscanf(strings.TrimSpace(string(aheadOut)), "%d", &ahead)
	}

	diff := ""
	if filePath != "" {
		absFile := filepath.Join(root, filePath)
		diffOut, _ := exec.Command("git", "-C", root, "diff", "HEAD", "--", absFile).Output()
		diff = string(diffOut)
	}

	Emit(map[string]any{
		"type":   "workspace_git_info",
		"branch": branch,
		"status": string(statusOut),
		"ahead":  ahead,
		"diff":   diff,
	})
}

// handleWorkspaceGitBranches lists local and remote branches.
func handleWorkspaceGitBranches(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	if wsPath == "" {
		Emit(map[string]any{"type": "workspace_git_branches", "local": []string{}, "remote": []string{}})
		return
	}
	root, _ := filepath.Abs(wsPath)

	parseLines := func(out []byte) []string {
		var result []string
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			l = strings.TrimSpace(l)
			if l != "" && !strings.Contains(l, "->") {
				result = append(result, l)
			}
		}
		if result == nil {
			return []string{}
		}
		return result
	}

	localOut, _  := exec.Command("git", "-C", root, "branch", "--format=%(refname:short)").Output()
	remoteOut, _ := exec.Command("git", "-C", root, "branch", "-r", "--format=%(refname:short)").Output()

	Emit(map[string]any{
		"type":   "workspace_git_branches",
		"local":  parseLines(localOut),
		"remote": parseLines(remoteOut),
	})
}

// handleWorkspaceGitCheckout switches to an existing branch.
func handleWorkspaceGitCheckout(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	branch, _ := cmd["branch"].(string)
	if wsPath == "" || branch == "" {
		Emit(map[string]any{"type": "workspace_git_checkout", "ok": false, "error": "missing params"})
		return
	}
	root, _ := filepath.Abs(wsPath)
	out, err := exec.Command("git", "-C", root, "checkout", branch).CombinedOutput()
	if err != nil {
		Emit(map[string]any{"type": "workspace_git_checkout", "ok": false, "error": string(out)})
		return
	}
	Emit(map[string]any{"type": "workspace_git_checkout", "ok": true})
}

// handleWorkspaceGitCreateBranch creates and checks out a new branch.
func handleWorkspaceGitCreateBranch(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	name, _ := cmd["name"].(string)
	if wsPath == "" || name == "" {
		Emit(map[string]any{"type": "workspace_git_create_branch", "ok": false, "error": "missing params"})
		return
	}
	root, _ := filepath.Abs(wsPath)
	out, err := exec.Command("git", "-C", root, "checkout", "-b", name).CombinedOutput()
	if err != nil {
		Emit(map[string]any{"type": "workspace_git_create_branch", "ok": false, "error": string(out)})
		return
	}
	Emit(map[string]any{"type": "workspace_git_create_branch", "ok": true})
}

// handleWorkspaceGitPull runs git pull.
func handleWorkspaceGitPull(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	if wsPath == "" {
		Emit(map[string]any{"type": "workspace_git_pull", "ok": false, "error": "missing workspacePath"})
		return
	}
	root, _ := filepath.Abs(wsPath)
	out, err := exec.Command("git", "-C", root, "pull").CombinedOutput()
	if err != nil {
		Emit(map[string]any{"type": "workspace_git_pull", "ok": false, "error": string(out)})
		return
	}
	Emit(map[string]any{"type": "workspace_git_pull", "ok": true, "output": string(out)})
}

// handleWorkspaceGitPush runs git push, falling back to --set-upstream for new branches.
func handleWorkspaceGitPush(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	if wsPath == "" {
		Emit(map[string]any{"type": "workspace_git_push", "ok": false, "error": "missing workspacePath"})
		return
	}
	root, _ := filepath.Abs(wsPath)

	out, err := exec.Command("git", "-C", root, "push").CombinedOutput()
	if err != nil {
		// Try --set-upstream for branches with no remote tracking yet
		branchOut, _ := exec.Command("git", "-C", root, "rev-parse", "--abbrev-ref", "HEAD").Output()
		branch := strings.TrimSpace(string(branchOut))
		out2, err2 := exec.Command("git", "-C", root, "push", "--set-upstream", "origin", branch).CombinedOutput()
		if err2 != nil {
			Emit(map[string]any{"type": "workspace_git_push", "ok": false, "error": string(out2)})
			return
		}
		out = out2
	}
	Emit(map[string]any{"type": "workspace_git_push", "ok": true, "output": string(out)})
}

// apiError extracts a human-readable error from an LLM API response so a real,
// actionable failure (usage limit, missing model, bad key) is surfaced instead
// of being masked as an empty completion. It covers the Ollama shape
// ({"error":"…"}) and the OpenAI shape ({"error":{"message":"…"}}), and falls
// back to the HTTP status when the body carries no error field but the call
// failed.
func apiError(status int, result map[string]any) string {
	switch e := result["error"].(type) {
	case string:
		if e != "" {
			return e
		}
	case map[string]any:
		if m, _ := e["message"].(string); m != "" {
			return m
		}
	}
	if status >= 400 {
		return fmt.Sprintf("provider returned HTTP %d", status)
	}
	return ""
}

// handleWorkspaceGenCommitMsg generates a git commit message via Ollama.
func handleWorkspaceGenCommitMsg(state *ServerState, cmd map[string]any) {
	wsPath, _       := cmd["workspacePath"].(string)
	model, _        := cmd["model"].(string)
	endpoint, _     := cmd["endpoint"].(string)
	providerType, _ := cmd["providerType"].(string)
	apiKey, _       := cmd["apiKey"].(string)

	// Resolve the API key (and any unspecified fields) server-side from the stored
	// provider so the browser never has to hold or send it — the frontend only
	// passes a providerId. Falls back to whatever fields the caller did send.
	if providerId, _ := cmd["providerId"].(string); providerId != "" {
		if cfg, ok := ai.Global.Config(providerId); ok {
			apiKey = cfg.APIKey
			if endpoint == "" {
				endpoint = cfg.Endpoint
			}
			if providerType == "" {
				providerType = cfg.Type
			}
			if model == "" {
				model = cfg.DefaultModel
			}
		}
	}

	if wsPath == "" {
		Emit(map[string]any{"type": "workspace_generate_commit_msg", "ok": false, "error": "missing workspacePath"})
		return
	}
	if model == "" {
		Emit(map[string]any{"type": "workspace_generate_commit_msg", "ok": false, "error": "no model configured — set a commit message model in Settings → Editor"})
		return
	}
	if endpoint == "" { endpoint = "http://localhost:11434" }
	if providerType == "" { providerType = "ollama" }

	root, _ := filepath.Abs(wsPath)

	statOut, _ := exec.Command("git", "-C", root, "diff", "HEAD", "--stat").Output()
	diffOut, _ := exec.Command("git", "-C", root, "diff", "HEAD").Output()
	if strings.TrimSpace(string(diffOut)) == "" {
		diffOut, _ = exec.Command("git", "-C", root, "diff", "--cached").Output()
	}
	if strings.TrimSpace(string(diffOut)) == "" {
		Emit(map[string]any{"type": "workspace_generate_commit_msg", "ok": false, "error": "no changes to summarise"})
		return
	}

	diff := strings.TrimSpace(string(statOut)) + "\n\n" + strings.TrimSpace(string(diffOut))
	if len(diff) > 8000 {
		diff = diff[:8000] + "\n... (diff truncated)"
	}

	systemPrompt := `Write a git commit message for the provided diff. Use imperative mood (e.g. "Add feature" not "Added feature"). Subject line under 72 chars. No surrounding quotes. Reply with only the commit message, nothing else.`
	userPrompt   := diff

	var msg string
	var callErr error

	switch providerType {
	case "ollama":
		// Native Ollama /api/generate
		prompt := fmt.Sprintf("%s\n\n%s", systemPrompt, userPrompt)
		body, _ := json.Marshal(map[string]any{"model": model, "prompt": prompt, "stream": false})
		resp, err := http.Post(strings.TrimRight(endpoint, "/")+"/api/generate", "application/json", bytes.NewReader(body))
		if err != nil {
			callErr = err
			break
		}
		defer resp.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			callErr = fmt.Errorf("failed to parse response")
			break
		}
		if apiErr := apiError(resp.StatusCode, result); apiErr != "" {
			callErr = fmt.Errorf("%s", apiErr)
			break
		}
		msg, _ = result["response"].(string)

	default:
		// OpenAI-compatible /v1/chat/completions (ollama-cloud, openai, anthropic, custom, …)
		base := strings.TrimRight(endpoint, "/")
		// Ollama Cloud exposes /v1; other OpenAI bases already include /v1
		if !strings.HasSuffix(base, "/v1") {
			base = base + "/v1"
		}
		messages := []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		}
		// Anthropic uses max_tokens; OpenAI accepts it too — safe to include.
		reqBody, _ := json.Marshal(map[string]any{
			"model":      model,
			"messages":   messages,
			"max_tokens": 256,
			"stream":     false,
		})
		req, _ := http.NewRequest("POST", base+"/chat/completions", bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			callErr = err
			break
		}
		defer resp.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			callErr = fmt.Errorf("failed to parse response")
			break
		}
		if apiErr := apiError(resp.StatusCode, result); apiErr != "" {
			callErr = fmt.Errorf("%s", apiErr)
			break
		}
		choices, _ := result["choices"].([]any)
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			msgObj, _ := choice["message"].(map[string]any)
			msg, _ = msgObj["content"].(string)
		}
		if msg == "" {
			// Anthropic shape: { content: [{type:"text", text:"..."}] }
			content, _ := result["content"].([]any)
			if len(content) > 0 {
				block, _ := content[0].(map[string]any)
				msg, _ = block["text"].(string)
			}
		}
	}

	if callErr != nil {
		Emit(map[string]any{"type": "workspace_generate_commit_msg", "ok": false, "error": callErr.Error()})
		return
	}

	msg = strings.Trim(strings.TrimSpace(msg), `"'`)
	if msg == "" {
		Emit(map[string]any{"type": "workspace_generate_commit_msg", "ok": false,
			"error": "model returned an empty message — if it is a reasoning model it may have spent its output on thinking; try a non-reasoning model in Settings → Editor"})
		return
	}
	Emit(map[string]any{"type": "workspace_generate_commit_msg", "ok": true, "message": msg})
}

// handleWorkspaceGitCommit stages all changes and creates a commit.
func handleWorkspaceGitCommit(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	message, _ := cmd["message"].(string)

	if wsPath == "" || message == "" {
		Emit(map[string]any{"type": "workspace_git_commit", "ok": false, "error": "missing params"})
		return
	}

	root, _ := filepath.Abs(wsPath)

	if addOut, err := exec.Command("git", "-C", root, "add", "-A").CombinedOutput(); err != nil {
		Emit(map[string]any{"type": "workspace_git_commit", "ok": false, "error": string(addOut)})
		return
	}

	commitOut, err := exec.Command("git", "-C", root, "commit", "-m", message).CombinedOutput()
	if err != nil {
		Emit(map[string]any{"type": "workspace_git_commit", "ok": false, "error": string(commitOut)})
		return
	}

	Emit(map[string]any{"type": "workspace_git_commit", "ok": true, "output": string(commitOut)})
}

// handleWorkspaceGitOriginal returns the file content at HEAD (git show HEAD:<path>).
// For untracked / new files it returns an empty string with no error.
func handleWorkspaceGitOriginal(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	filePath, _ := cmd["path"].(string)

	if wsPath == "" || filePath == "" {
		Emit(map[string]any{"type": "workspace_git_original", "content": "", "error": "missing params"})
		return
	}

	root, _ := filepath.Abs(wsPath)
	// Git expects a path relative to the repo root; convert if absolute was supplied.
	gitPath := filePath
	if filepath.IsAbs(filePath) {
		if rel, err := filepath.Rel(root, filePath); err == nil {
			gitPath = rel
		}
	}
	out, err := exec.Command("git", "-C", root, "show", "HEAD:"+gitPath).Output()
	if err != nil {
		// Untracked / new file — empty original is correct
		Emit(map[string]any{"type": "workspace_git_original", "content": ""})
		return
	}
	Emit(map[string]any{"type": "workspace_git_original", "content": string(out)})
}

// handleWorkspaceGrep does a content search (grep) across the workspace.
func handleWorkspaceGrep(state *ServerState, cmd map[string]any) {
	wsPath, _ := cmd["workspacePath"].(string)
	q, _ := cmd["q"].(string)

	root, _ := filepath.Abs(wsPath)
	q = strings.TrimSpace(q)
	if q == "" || !isDir(root) {
		Emit(map[string]any{"type": "workspace_grep", "results": []any{}})
		return
	}

	const maxResults, maxWalk = 200, 10000
	walked := 0
	var results []map[string]any

	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || walked >= maxWalk || len(results) >= maxResults {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if ignoreSet[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		walked++
		// Skip binary-likely files
		ext := strings.ToLower(filepath.Ext(d.Name()))
		skip := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
			".webp": true, ".ico": true, ".svg": true, ".pdf": true,
			".zip": true, ".tar": true, ".gz": true, ".exe": true, ".bin": true}
		if skip[ext] {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		qLow := strings.ToLower(q)
		for lineNum, line := range lines {
			if strings.Contains(strings.ToLower(line), qLow) {
				preview := strings.TrimSpace(line)
				if len(preview) > 120 {
					preview = preview[:120]
				}
				results = append(results, map[string]any{
					"path":    path,
					"name":    d.Name(),
					"line":    lineNum + 1,
					"preview": preview,
				})
				if len(results) >= maxResults {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})

	out := make([]any, len(results))
	for i, r := range results {
		out[i] = r
	}
	Emit(map[string]any{"type": "workspace_grep", "results": out})
}

// helpers

// resolveWorkspacePath resolves a path within root, accepting both absolute and
// relative inputs. Always returns an absolute, clean path within root.
// If path is already absolute it is used directly (after Clean); otherwise it is
// joined with root. If the result would escape root, root itself is returned.
func resolveWorkspacePath(root, path string) string {
	var target string
	if filepath.IsAbs(path) {
		target = filepath.Clean(path)
	} else {
		target = filepath.Clean(filepath.Join(root, path))
	}
	// Security: keep target inside root
	if !strings.HasPrefix(target, root) {
		return root
	}
	return target
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

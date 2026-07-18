package lsp

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// ── Binary resolution ─────────────────────────────────────────────────────────

type lspCommand struct {
	bin  string
	args []string
	// env holds extra KEY=value entries appended to the process environment,
	// e.g. DOTNET_ROOT for OmniSharp. Empty for most servers.
	env []string
}

// Detect returns lang → resolved binary path for every installed LSP server.
// Called by the /lsp/check diagnostic endpoint.
func Detect() map[string]string {
	result := make(map[string]string)
	for _, pkg := range Registry {
		for _, lang := range pkg.Languages {
			if _, ok := result[lang]; ok {
				continue // already have a server for this lang
			}
			if fn, ok := resolvers[pkg.Resolver]; ok {
				if cmd, err := fn(pkg, ""); err == nil {
					result[lang] = cmd.bin
				}
			} else if path, err := findBin(pkg.Bin, ""); err == nil {
				result[lang] = path
			}
		}
	}
	return result
}

// ── WebSocket handler ─────────────────────────────────────────────────────────

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  65536,
	WriteBufferSize: 65536,
}

// HandleWS upgrades the HTTP connection to a WebSocket and proxies LSP JSON-RPC
// between the browser and the language server process.
//
// URL param:   lang    — Monaco language ID (go, rust, python, html, …)
// Query param: rootUri — workspace root, e.g. file:///path/to/project
func HandleWS(w http.ResponseWriter, r *http.Request, lang string) {
	rootURI := r.URL.Query().Get("rootUri")

	pkg, ok := FindPackageForLang(lang)
	if !ok {
		http.Error(w,
			fmt.Sprintf("no language server registered for %q — install one from the Language Servers marketplace", lang),
			http.StatusServiceUnavailable)
		return
	}

	cmd, err := resolveCmd(pkg, rootURI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[lsp] %s: %v\n", lang, err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	proxyWSToProcess(w, r, cmd)
}

// resolveCmd picks the right resolver for the package.
func resolveCmd(pkg LspPackage, rootURI string) (*lspCommand, error) {
	if fn, ok := resolvers[pkg.Resolver]; ok {
		return fn(pkg, rootURI)
	}
	return resolveStandard(pkg, rootURI)
}

// ── Process ↔ WebSocket proxy ─────────────────────────────────────────────────

func proxyWSToProcess(w http.ResponseWriter, r *http.Request, lsCmd *lspCommand) {
	proc := exec.Command(lsCmd.bin, lsCmd.args...)
	proc.Env = append(envWithShellPATH(), lsCmd.env...)
	// Own process group so the whole subtree is killed when the WS closes,
	// even if the sidecar itself exits uncleanly (e.g. SIGTERM).
	setProcGroup(proc)

	stdin, err := proc.StdinPipe()
	if err != nil {
		http.Error(w, "failed to create LSP stdin pipe", http.StatusInternalServerError)
		return
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		http.Error(w, "failed to create LSP stdout pipe", http.StatusInternalServerError)
		return
	}
	proc.Stderr = os.Stderr

	if err := proc.Start(); err != nil {
		http.Error(w, "failed to start language server: "+err.Error(), http.StatusInternalServerError)
		return
	}
	pid := proc.Process.Pid
	fmt.Fprintf(os.Stderr, "[lsp] started %s (pid %d)\n", lsCmd.bin, pid)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		killProcGroup(pid)
		proc.Wait()
		return
	}

	defer func() {
		conn.Close()
		stdin.Close()
		// Kill the entire process group so OmniSharp (and any children it
		// spawned) are reaped even if the sidecar exits mid-session.
		killProcGroup(pid)
		proc.Wait()
	}()

	var writeMu sync.Mutex
	writeWS := func(msg []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, msg)
	}

	// LSP stdout → WebSocket: strip Content-Length framing, forward raw JSON.
	go func() {
		reader := bufio.NewReaderSize(stdout, 65536)
		for {
			var contentLength int
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSpace(line)
				if line == "" {
					break
				}
				if strings.HasPrefix(line, "Content-Length:") {
					if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))); err == nil {
						contentLength = v
					}
				}
			}
			if contentLength == 0 {
				continue
			}
			body := make([]byte, contentLength)
			if _, err := io.ReadFull(reader, body); err != nil {
				return
			}
			if writeWS(body) != nil {
				return
			}
		}
	}()

	// WebSocket → LSP stdin: add Content-Length framing.
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		frame := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(msg))
		if _, err := io.WriteString(stdin, frame); err != nil {
			return
		}
		if _, err := stdin.Write(msg); err != nil {
			return
		}
	}
}

// ── Binary resolution ─────────────────────────────────────────────────────────
//
// Strategy: ask the user's interactive login shell to run `which <bin>`.
// We cache results so the shell is only spawned once per binary per sidecar lifetime.

var (
	binCacheMu sync.RWMutex
	binCache   = map[string]string{}
)

// findBin resolves a binary to an absolute path, trying (in order):
//  1. The process PATH
//  2. shell `which` — interactive login shell covers .zshrc / .bashrc
//  3. VSCode ext dirs — for servers bundled inside extensions
//  4. fallback binary — alternative name (e.g. pylsp instead of pyright-langserver)
func findBin(bin, fallback string) (string, error) {
	binCacheMu.RLock()
	if p, ok := binCache[bin]; ok {
		binCacheMu.RUnlock()
		return p, nil
	}
	binCacheMu.RUnlock()

	path, err := resolveBinUncached(bin)
	if err != nil && fallback != "" {
		path, err = resolveBinUncached(fallback)
	}
	if err != nil {
		return "", fmt.Errorf("%s not found", bin)
	}

	binCacheMu.Lock()
	binCache[bin] = path
	binCacheMu.Unlock()
	fmt.Fprintf(os.Stderr, "[lsp] resolved %s → %s\n", bin, path)
	return path, nil
}

func resolveBinUncached(bin string) (string, error) {
	if p, err := exec.LookPath(bin); err == nil {
		return p, nil
	}
	if p, err := shellWhich(bin); err == nil {
		return p, nil
	}
	if p, err := lookInTollecodeBin(bin); err == nil {
		return p, nil
	}
	return lookInVSCodeExtensions(bin)
}

// lookInTollecodeBin checks ~/.tollecode/bin — the marketplace's own install dir.
// If the entry is a symlink pointing to a directory (a common mis-install),
// it walks one level inside looking for a binary with the same base name.
func lookInTollecodeBin(bin string) (string, error) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".tollecode", "bin", bin)
	if isExecutable(p) {
		return p, nil
	}
	// Symlink may point at the install directory rather than the binary itself.
	// Resolve and check <dir>/<bin> and <dir>/<Bin> (capitalized).
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%s not in ~/.tollecode/bin", bin)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s not in ~/.tollecode/bin", bin)
	}
	for _, name := range []string{bin, strings.ToUpper(bin[:1]) + bin[1:]} {
		candidate := filepath.Join(resolved, name)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not in ~/.tollecode/bin", bin)
}

// TollecodeBinDir returns (and creates) ~/.tollecode/bin.
func TollecodeBinDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".tollecode", "bin")
	os.MkdirAll(dir, 0o755)
	return dir
}

// shellWhich asks the user's interactive login shell to locate a binary.
func shellWhich(bin string) (string, error) {
	shell := userShell()

	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "where "+bin).Output()
		if err != nil {
			return "", fmt.Errorf("where %s: %w", bin, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && isExecutable(line) {
				return line, nil
			}
		}
		return "", fmt.Errorf("%s not found via where", bin)
	}

	whichCmd := "command -v " + bin
	out, err := exec.Command(shell, "-i", "-l", "-c", whichCmd).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		out, err = exec.Command(shell, "-l", "-c", whichCmd).Output()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			return "", fmt.Errorf("%s not found via %s", bin, shell)
		}
	}

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if filepath.IsAbs(line) && isExecutable(line) {
			return line, nil
		}
	}
	return "", fmt.Errorf("%s: shell returned no usable path", bin)
}

var (
	userShellOnce   sync.Once
	cachedUserShell string
)

func userShell() string {
	userShellOnce.Do(func() {
		candidates := []string{os.Getenv("SHELL")}
		if runtime.GOOS == "darwin" {
			candidates = append(candidates, "/bin/zsh")
		}
		candidates = append(candidates, "/bin/bash", "/bin/sh")
		for _, s := range candidates {
			if s != "" {
				if _, err := os.Stat(s); err == nil {
					cachedUserShell = s
					return
				}
			}
		}
		cachedUserShell = "/bin/sh"
	})
	return cachedUserShell
}

// lookInVSCodeExtensions checks ~/.vscode/extensions for binaries bundled
// inside extensions and never placed in PATH (e.g. rust-analyzer, clangd).
func lookInVSCodeExtensions(bin string) (string, error) {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".vscode", "extensions")

	patterns := map[string][]string{
		"rust-analyzer": {
			"rust-lang.rust-analyzer-*/server/rust-analyzer",
			"rust-lang.rust-analyzer-*/dist/rust-analyzer",
		},
		"clangd": {
			"llvm-vs-code-extensions.vscode-clangd-*/bin/clangd",
			"ms-vscode.cpptools-*/LLVM/bin/clangd",
		},
	}

	globs, ok := patterns[bin]
	if !ok {
		return "", fmt.Errorf("no VSCode extension pattern for %s", bin)
	}

	var matches []string
	for _, p := range globs {
		m, _ := filepath.Glob(filepath.Join(root, p))
		matches = append(matches, m...)
	}
	for i := len(matches) - 1; i >= 0; i-- {
		if isExecutable(matches[i]) {
			return matches[i], nil
		}
	}
	return "", fmt.Errorf("%s not found in VSCode extensions", bin)
}

// ShellUsed returns the shell binary being used for binary resolution.
// Exposed for the /lsp/check diagnostic endpoint.
func ShellUsed() string { return userShell() }

// envWithShellPATH returns the sidecar's environment with PATH augmented by the
// shell-derived PATH. Language servers need the full PATH to find their runtime
// dependencies (gopls needs `go`, rust-analyzer needs `rustup`, etc.)
func envWithShellPATH() []string {
	shellPATH := shellDerivedPATH()
	env := os.Environ()
	if shellPATH == "" {
		return env
	}
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			current := strings.TrimPrefix(e, "PATH=")
			env[i] = "PATH=" + shellPATH + ":" + current
			return env
		}
	}
	return append(env, "PATH="+shellPATH)
}

var (
	shellPATHOnce   sync.Once
	cachedShellPATH string
)

func shellDerivedPATH() string {
	shellPATHOnce.Do(func() {
		shell := userShell()
		out, err := exec.Command(shell, "-i", "-l", "-c", "printenv PATH").Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			out, err = exec.Command(shell, "-l", "-c", "printenv PATH").Output()
		}
		if err == nil {
			cachedShellPATH = strings.TrimSpace(string(out))
			fmt.Fprintf(os.Stderr, "[lsp] shell PATH for child processes: %s\n", cachedShellPATH)
		}
	})
	return cachedShellPATH
}

// ── .NET runtime resolution (for OmniSharp) ─────────────────────────────────────
//
// OmniSharp ships as a framework-dependent net6.0 build. Its native apphost must
// locate a shared .NET runtime, but:
//   - GUI-launched apps (Tauri) don't inherit the user's shell environment, so
//     DOTNET_ROOT set in .zshrc/.bashrc is invisible to the spawned process.
//   - The net6.0-preview apphost mis-resolves the default macOS install location
//     on Apple Silicon (it looks in /usr/local/share/dotnet/x64).
// Either way the launch fails with "libhostfxr.dylib could not be found". We fix
// it by handing OmniSharp an explicit DOTNET_ROOT.

var (
	dotnetRootMu     sync.RWMutex
	cachedDotnetRoot string
)

// findDotnetRoot returns a directory suitable for DOTNET_ROOT, or "" if no .NET
// runtime can be located. A successful result is cached for the sidecar's
// lifetime; a failure is deliberately NOT cached, so a user who installs the
// runtime and reconnects gets picked up without restarting the app.
func findDotnetRoot() string {
	dotnetRootMu.RLock()
	cached := cachedDotnetRoot
	dotnetRootMu.RUnlock()
	if cached != "" {
		return cached
	}

	root := resolveDotnetRoot()
	if root != "" {
		dotnetRootMu.Lock()
		cachedDotnetRoot = root
		dotnetRootMu.Unlock()
		fmt.Fprintf(os.Stderr, "[lsp] DOTNET_ROOT for OmniSharp: %s\n", root)
	} else {
		fmt.Fprintf(os.Stderr, "[lsp] warning: no .NET runtime found — OmniSharp may fail to start\n")
	}
	return root
}

func resolveDotnetRoot() string {
	// 1. Already in the sidecar's environment (true when launched from a terminal).
	if root := os.Getenv("DOTNET_ROOT"); validDotnetRoot(root) {
		return root
	}
	// 2. The user's login shell may export it (.zshrc / .bashrc).
	if root := shellEnv("DOTNET_ROOT"); validDotnetRoot(root) {
		return root
	}
	// 3. Well-known install locations.
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/usr/local/share/dotnet",              // official macOS installer (arm64/x64)
		"/opt/homebrew/opt/dotnet/libexec",     // homebrew `dotnet`
		"/opt/homebrew/opt/dotnet@8/libexec",   // homebrew `dotnet@8`
		"/usr/lib/dotnet", "/usr/share/dotnet", // linux
		filepath.Join(home, ".dotnet"),
	}
	for _, c := range candidates {
		if validDotnetRoot(c) {
			return c
		}
	}
	// 4. Derive from the `dotnet` binary on PATH (resolve symlinks to the real root).
	if bin, err := findBin("dotnet", ""); err == nil {
		if real, err := filepath.EvalSymlinks(bin); err == nil {
			if dir := filepath.Dir(real); validDotnetRoot(dir) {
				return dir
			}
		}
	}
	return ""
}

// validDotnetRoot reports whether dir contains a shared .NET runtime OmniSharp
// (tfm net6.0, rollForward LatestMajor) can load.
func validDotnetRoot(dir string) bool {
	if dir == "" {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(dir, "shared", "Microsoft.NETCore.App"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
	}
	return false
}

// shellEnv asks the user's interactive login shell for the value of an env var.
// Used to recover variables (DOTNET_ROOT) that GUI-launched apps don't inherit.
func shellEnv(name string) string {
	if runtime.GOOS == "windows" {
		return os.Getenv(name)
	}
	shell := userShell()
	out, err := exec.Command(shell, "-i", "-l", "-c", "printenv "+name).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		out, _ = exec.Command(shell, "-l", "-c", "printenv "+name).Output()
	}
	return strings.TrimSpace(string(out))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

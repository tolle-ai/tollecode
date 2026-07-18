// Package shellenv resolves the shell interpreter used to run agent and
// workflow shell commands, so run_shell works on every platform.
//
// The whole system emits POSIX commands (ls, grep, cat, rm, pipes, &&, $VAR)
// and the run_shell safety denylist is POSIX-shaped, so a POSIX shell is always
// preferred. On Linux/macOS that shell is always present (/bin/sh). On Windows
// we discover a bash (Git for Windows, MSYS2, or an explicit override) so agents
// behave identically across platforms. When no POSIX shell exists on a Windows
// host we fall back to PowerShell so run_shell still works rather than failing
// outright — behaviour is best-effort there, but the tool remains usable.
package shellenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// EnvOverride is the environment variable that pins the shell interpreter,
// taking precedence over discovery. Point it at a bash.exe/sh.exe on Windows.
const EnvOverride = "TOLLECODE_SHELL"

// Kind identifies the interpreter family so callers build the right argv.
type Kind int

const (
	POSIX      Kind = iota // sh/bash: command runs after "-c" (or "-lc" for login)
	PowerShell             // pwsh/powershell.exe: command runs after "-Command"
)

// Shell is a resolved interpreter and its family.
type Shell struct {
	Path string
	Kind Kind
}

// Args returns the argument list that must precede the command string for this
// shell. login requests a login shell (POSIX only; ignored for PowerShell).
//
//	sh, _ := shellenv.Lookup("sh")
//	cmd := exec.CommandContext(ctx, sh.Path, append(sh.Args(false), command)...)
func (s Shell) Args(login bool) []string {
	if s.Kind == PowerShell {
		return []string{"-NoProfile", "-NonInteractive", "-Command"}
	}
	if login {
		return []string{"-lc"}
	}
	return []string{"-c"}
}

var (
	winShellOnce sync.Once
	winShell     Shell
	winShellErr  error
)

// Lookup returns the interpreter to run a command with. On Linux/macOS it is the
// unixDefault ("sh" or "bash") as a POSIX shell, preserving each call site's
// historical choice. On Windows it discovers a POSIX bash, falling back to
// PowerShell, and caches the result.
func Lookup(unixDefault string) (Shell, error) {
	if runtime.GOOS != "windows" {
		return Shell{Path: unixDefault, Kind: POSIX}, nil
	}
	winShellOnce.Do(func() {
		winShell, winShellErr = discoverWindowsShell()
	})
	return winShell, winShellErr
}

// discoverWindowsShell locates a shell on Windows, in priority order:
//  1. TOLLECODE_SHELL override (assumed POSIX)
//  2. SHELL environment variable (if it points at an existing bash/sh)
//  3. bash / sh on PATH (via `where`)
//  4. Git for Windows / MSYS2 well-known install locations
//  5. PowerShell (pwsh, then the built-in Windows PowerShell) as a last resort
func discoverWindowsShell() (Shell, error) {
	if p := strings.TrimSpace(os.Getenv(EnvOverride)); p != "" {
		if isExecutableFile(p) {
			return Shell{Path: p, Kind: POSIX}, nil
		}
		return Shell{}, fmt.Errorf("%s=%q does not point to an executable file", EnvOverride, p)
	}

	if p := strings.TrimSpace(os.Getenv("SHELL")); p != "" && isExecutableFile(p) {
		return Shell{Path: p, Kind: POSIX}, nil
	}

	for _, bin := range []string{"bash", "sh"} {
		if p := lookPathWhere(bin); p != "" {
			return Shell{Path: p, Kind: POSIX}, nil
		}
	}

	for _, p := range gitForWindowsCandidates() {
		if isExecutableFile(p) {
			return Shell{Path: p, Kind: POSIX}, nil
		}
	}

	// No POSIX shell — fall back to PowerShell so run_shell still works.
	for _, p := range powerShellCandidates() {
		if p != "" {
			return Shell{Path: p, Kind: PowerShell}, nil
		}
	}

	return Shell{}, fmt.Errorf(
		"no shell found on Windows. Install Git for Windows "+
			"(https://git-scm.com/download/win) or set %s to a bash.exe/sh.exe path",
		EnvOverride)
}

// gitForWindowsCandidates enumerates the standard bash locations shipped by Git
// for Windows and MSYS2 under the common Program Files roots.
func gitForWindowsCandidates() []string {
	roots := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("ProgramW6432"),
		os.Getenv("LOCALAPPDATA"), // per-user Git install
		`C:\`,
	}
	// bin\bash.exe is the plain shell; usr\bin\bash.exe is the full MSYS2 bash.
	rels := []string{
		filepath.Join("Git", "bin", "bash.exe"),
		filepath.Join("Git", "usr", "bin", "bash.exe"),
		filepath.Join("Programs", "Git", "bin", "bash.exe"),
	}
	var out []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, rel := range rels {
			out = append(out, filepath.Join(root, rel))
		}
	}
	return out
}

// powerShellCandidates returns usable PowerShell interpreters, preferring
// PowerShell 7+ (pwsh, which supports && and ||) over the built-in Windows
// PowerShell 5.1 at its fixed System32 path.
func powerShellCandidates() []string {
	out := []string{
		lookPathWhere("pwsh"),
		lookPathWhere("powershell"),
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		builtin := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if isExecutableFile(builtin) {
			out = append(out, builtin)
		}
	}
	return out
}

// lookPathWhere resolves a binary via Go's PATH lookup, falling back to the
// Windows `where` command (which also searches App Paths).
func lookPathWhere(bin string) string {
	if p, err := exec.LookPath(bin); err == nil && isExecutableFile(p) {
		return p
	}
	out, err := exec.Command("cmd", "/c", "where "+bin).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && isExecutableFile(line) {
			return line
		}
	}
	return ""
}

func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

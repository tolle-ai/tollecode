package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tolle-ai/tollecode/internal/shellenv"
)

// shellLineWriter splits a byte stream into newline-delimited lines and hands
// each to emit. It is written to only by exec's copy goroutine (Stdout and
// Stderr point at the same writer, which exec serialises), so it needs no lock.
type shellLineWriter struct {
	emit func(string)
	buf  []byte
}

func (w *shellLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSuffix(w.buf[:i], []byte("\r"))
		w.emit(string(line))
		w.buf = w.buf[i+1:]
	}
	// A single line that never ends would grow buf without bound — flush it once
	// it exceeds the output cap so memory stays bounded.
	if len(w.buf) > maxShellOutput {
		w.emit(string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// flush emits any buffered trailing text that had no terminating newline.
func (w *shellLineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = w.buf[:0]
	}
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07`)

// ── Catastrophic-command safety floor ─────────────────────────────────────────
// These guard the indirect-prompt-injection -> run_shell chain: attacker-
// controlled content (a fetched web page, a repo file, an AGENTS.md) that reaches
// the model must not be able to drive host-destroying commands autonomously.
//
// Tier 1 patterns are blocked UNCONDITIONALLY — they have no legitimate use in
// normal development, so blocking them never breaks a real workflow.
// Tier 2 patterns are blocked only when human approval is bypassed
// (cfg.ShellAutoAllow — set by chat channels, cron, and webhook runners). In
// interactive use the user approves such commands themselves, so capability is
// preserved; only the unattended autonomous path is fenced off.
var (
	// rm with recursive AND force flags (any spelling: -rf, -fr, -r -f, --recursive --force)
	rmRecursive = regexp.MustCompile(`\brm\b[^|;&\n]*(\s-[a-zA-Z]*r[a-zA-Z]*\b|\s--recursive\b)`)
	rmForce     = regexp.MustCompile(`\brm\b[^|;&\n]*(\s-[a-zA-Z]*f[a-zA-Z]*\b|\s--force\b)`)
	// ...targeting a root-ish path: bare /, /*, ~, ~/, $HOME, or --no-preserve-root
	rmCatastrophicTarget = regexp.MustCompile(`(--no-preserve-root|(^|\s)/($|\s|\*)|(^|\s)~/?($|\s)|(^|\s)\$\{?HOME\}?($|\s|/))`)
	// fork bomb :(){ :|:& };:
	forkBomb = regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`)
	// raw-disk write / filesystem creation on a device
	diskDestroy = regexp.MustCompile(`\bdd\b[^|;&\n]*\bof=/dev/(sd|nvme|hd|disk|mmcblk|vd)|\bmkfs(\.\w+)?\b[^|;&\n]*/dev/|>\s*/dev/(sd|nvme|hd|disk)`)
	// Tier 2: remote download piped straight into a shell interpreter
	pipeRemoteToShell = regexp.MustCompile(`\b(curl|wget|fetch)\b[^|]*\|\s*(sudo\s+)?(sh|bash|zsh|dash|ksh|fish)\b`)

	// ── PowerShell equivalents (case-insensitive) ─────────────────────────────
	// On a Windows host with no POSIX shell, run_shell falls back to PowerShell,
	// so the same safety floor must recognise PowerShell-native syntax. The
	// recursive-delete verb, -Recurse, and -Force are matched separately (like the
	// POSIX rm patterns) and only escalate to a block when combined with a
	// root/home target.
	//
	// Remove-Item and its aliases (rm, ri, del, erase, rd, rmdir). "rm" is listed
	// here too because the POSIX rmForce pattern only matches a lowercase -f flag,
	// so PowerShell's "rm -Recurse -Force" needs these case-insensitive patterns.
	psRemove  = regexp.MustCompile(`(?i)\b(remove-item|rmdir|erase|del|rd|ri|rm)\b`)
	psRecurse = regexp.MustCompile(`(?i)(-recurse\b|-r\b)`)
	psForce   = regexp.MustCompile(`(?i)(-force\b|-fo\b)`)
	// Windows root/home targets: a bare drive root (C:\, C:\*), a bare backslash
	// root, ~, and the PowerShell environment variables that point at user/system
	// roots — each only when it is the whole target, not a subdirectory beneath it.
	winCatastrophicTarget = regexp.MustCompile(`(?i)([a-z]:\\?(\s|$|\*|"|')|(^|\s)\\(\s|$|\*)|\$env:(userprofile|homepath|home|systemroot|windir|systemdrive|programfiles(\(x86\))?|programdata|appdata|localappdata)(\s|$|\*|"|'|\\(\s|$|\*))|\$home(\s|$|\*|"|')|(^|\s)~(\s|$))`)
	// Disk/partition wipe cmdlets and diskpart.
	winDiskDestroy = regexp.MustCompile(`(?i)\b(format-volume|clear-disk|initialize-disk|remove-partition|diskpart)\b`)
	// Tier 2: remote download piped/passed into PowerShell's expression evaluator.
	psRemoteToShell = regexp.MustCompile(`(?i)\b(invoke-webrequest|invoke-restmethod|iwr|irm|curl|wget)\b[^|]*\|\s*(iex|invoke-expression)\b|(iex|invoke-expression)\b[^\n]*\b(downloadstring|invoke-webrequest|invoke-restmethod|iwr|irm)\b`)
)

// blockedShellCommand reports whether a command must be refused outright. The
// returned reason is surfaced to the model so it stops rather than retrying.
func blockedShellCommand(cfg *Config, command string) (reason string, blocked bool) {
	// A root/home target expressed in either POSIX or Windows form.
	catastrophicTarget := rmCatastrophicTarget.MatchString(command) || winCatastrophicTarget.MatchString(command)
	switch {
	case forkBomb.MatchString(command):
		return "a fork bomb", true
	case diskDestroy.MatchString(command) || winDiskDestroy.MatchString(command):
		return "writing to or formatting a raw disk device", true
	case rmRecursive.MatchString(command) && rmForce.MatchString(command) && catastrophicTarget:
		return "a recursive force-delete of a root or home directory", true
	case psRemove.MatchString(command) && psRecurse.MatchString(command) && psForce.MatchString(command) && catastrophicTarget:
		return "a recursive force-delete of a root or home directory", true
	}
	// Tier 2: only when a human is not in the approval loop.
	if cfg.ShellAutoAllow && (pipeRemoteToShell.MatchString(command) || psRemoteToShell.MatchString(command)) {
		return "piping a remote download directly into a shell interpreter (blocked on unattended/auto-approved surfaces)", true
	}
	return "", false
}

// defaultShellTimeout mirrors Claude Code's 2-minute default.
const defaultShellTimeout = 2 * time.Minute

// maxShellTimeout is the ceiling for the caller-supplied "timeout" parameter.
const maxShellTimeout = 10 * time.Minute

// maxShellOutput is the soft cap on captured output returned to the LLM.
// Lines are still streamed to the UI beyond this limit; only the in-memory
// buffer (returned as the tool result) is capped.
const maxShellOutput = 30_000

func toolRunShell(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	command, _ := inp["command"].(string)
	if command == "" {
		return "Error: 'command' is required.", true
	}

	// Catastrophic-command safety floor — applies before any auto-allow bypass so
	// injected content cannot drive host destruction on unattended surfaces.
	if reason, blocked := blockedShellCommand(cfg, command); blocked {
		if cfg.EmitEvent != nil {
			cfg.EmitEvent(map[string]any{"type": "permission_denied", "tool": "run_shell", "detail": command, "reason": reason})
		}
		return fmt.Sprintf("Blocked: this command was refused by a safety policy (%s). Do NOT retry it or attempt a workaround. Inform the user if this action is genuinely required so they can run it manually.", reason), false
	}

	if cfg.Mode == "plan" {
		return "Error: run_shell is not available in PLAN mode.", true
	}

	switch cfg.checkPermission(ctx, "shell", command) {
	case permUnavailable:
		return "Shell execution is not available in this context. Do not retry or try alternative approaches to run commands. Inform the user if this capability is needed.", true
	case permDenied:
		if cfg.EmitEvent != nil {
			cfg.EmitEvent(map[string]any{"type": "permission_denied", "tool": "run_shell", "detail": command})
		}
		return "Permission denied by the user. Do NOT retry this operation, do not try alternative approaches (e.g., using run_shell to write files), and do not ask for permission again. Move on to tasks that don't require this permission, or inform the user what you need.", false
	}

	// Resolve caller-supplied timeout (default 120s, max 600s).
	timeout := defaultShellTimeout
	if s, ok := inp["timeout"].(float64); ok && s > 0 {
		t := time.Duration(s) * time.Second
		if t > maxShellTimeout {
			t = maxShellTimeout
		}
		timeout = t
	}

	background, _ := inp["background"].(bool)

	if background {
		return runShellBackground(ctx, cfg, command)
	}
	return runShellForeground(ctx, cfg, command, timeout)
}

// runShellForeground runs the command bounded by timeout, streaming output to
// the client as it arrives. The collected output (capped at maxShellOutput) is
// returned to the LLM when the process exits or the deadline is reached.
func runShellForeground(ctx context.Context, cfg *Config, command string, timeout time.Duration) (string, bool) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, err := shellenv.Lookup("sh")
	if err != nil {
		return "Failed to run command: " + err.Error(), true
	}
	cmd := exec.CommandContext(runCtx, shell.Path, append(shell.Args(false), command)...)
	cmd.Dir = cfg.Workspace
	setProcGroup(cmd)
	// On cancel/timeout, kill the whole process group — not just the shell.
	// Children that inherited the stdout/stderr pipes (dev servers, watchers,
	// anything the command spawns) otherwise keep those pipes open and the
	// command can't be stopped. Overriding Cancel replaces exec's default
	// single-process kill.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			killProcGroup(cmd.Process.Pid)
		}
		return nil
	}
	// Bound how long Wait() blocks on output after the process exits or is
	// killed. If an orphaned child still holds a pipe, exec force-closes the I/O
	// after this delay so the call returns instead of hanging forever. This is
	// effective because exec (not us) owns the copy goroutines — hence cmd.Stdout
	// below rather than StdoutPipe.
	cmd.WaitDelay = 2 * time.Second

	var (
		buf       strings.Builder
		mu        sync.Mutex
		truncated bool
	)

	emitLine := func(line string) {
		clean := ansiEscape.ReplaceAllString(line, "")
		if cfg.EmitEvent != nil {
			cfg.EmitEvent(map[string]any{"type": "shell_output", "line": clean})
		}
		mu.Lock()
		defer mu.Unlock()
		if truncated {
			return
		}
		need := len(clean) + 1 // +1 for newline
		if buf.Len()+need <= maxShellOutput {
			buf.WriteString(clean)
			buf.WriteByte('\n')
		} else {
			remaining := maxShellOutput - buf.Len()
			if remaining > 0 {
				buf.WriteString(clean[:remaining])
			}
			buf.WriteString("\n[output truncated]")
			truncated = true
		}
	}

	// One writer for both streams (merged, as before). exec serialises Write
	// calls when Stdout and Stderr are the same value, so the splitter needs no
	// locking. Letting exec own the copiers is what makes WaitDelay work.
	lw := &shellLineWriter{emit: emitLine}
	cmd.Stdout = lw
	cmd.Stderr = lw

	cmdErr := cmd.Run()
	lw.flush() // emit any trailing line that had no newline

	mu.Lock()
	result := buf.String()
	mu.Unlock()

	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Command timed out after %.0f seconds.\n%s", timeout.Seconds(), result), true
	}
	if ctx.Err() != nil {
		return fmt.Sprintf("Command cancelled.\n%s", result), true
	}
	if cmdErr != nil {
		if result == "" {
			return fmt.Sprintf("Command failed: %v", cmdErr), false
		}
		return fmt.Sprintf("Exit error: %v\n%s", cmdErr, result), false
	}
	if result == "" {
		return "(no output)", false
	}
	return result, false
}

// runShellBackground starts the command without a timeout and streams its
// output for up to 10 seconds (startup window). After the window the process
// is left running and the agent receives the collected startup output.
// Use this for dev servers, watchers, and other long-lived daemons.
func runShellBackground(ctx context.Context, cfg *Config, command string) (string, bool) {
	// Deliberately not using exec.CommandContext so the process outlives this
	// agent turn. The process becomes a child of the sidecar and persists until
	// the user kills it or the sidecar exits.
	shell, err := shellenv.Lookup("sh")
	if err != nil {
		return "Failed to run command: " + err.Error(), true
	}
	cmd := exec.Command(shell.Path, append(shell.Args(false), command)...)
	cmd.Dir = cfg.Workspace
	// Own process group so a cancel can take down the daemon AND its children.
	setProcGroup(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Sprintf("Failed to open stdout pipe: %v", err), true
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Sprintf("Failed to open stderr pipe: %v", err), true
	}

	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("Failed to start: %v", err), true
	}

	var (
		buf       strings.Builder
		mu        sync.Mutex
		truncated bool
	)

	emitLine := func(line string) {
		clean := ansiEscape.ReplaceAllString(line, "")
		if cfg.EmitEvent != nil {
			cfg.EmitEvent(map[string]any{"type": "shell_output", "line": clean})
		}
		mu.Lock()
		defer mu.Unlock()
		if !truncated && buf.Len()+len(clean)+1 <= maxShellOutput {
			buf.WriteString(clean)
			buf.WriteByte('\n')
		} else if !truncated {
			buf.WriteString("\n[output truncated]")
			truncated = true
		}
	}

	var wg sync.WaitGroup
	drain := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			emitLine(sc.Text())
		}
	}
	wg.Add(2)
	go drain(stdoutPipe)
	go drain(stderrPipe)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	const startupWindow = 10 * time.Second
	select {
	case <-done:
		// Process exited before the startup window — wasn't a daemon.
		cmd.Wait()
		mu.Lock()
		result := buf.String()
		mu.Unlock()
		if result == "" {
			return "(no output)", false
		}
		return result, false

	case <-time.After(startupWindow):
		// Startup window elapsed; leave the process running.
		mu.Lock()
		result := buf.String()
		mu.Unlock()
		if result == "" {
			result = "(no output in startup window)"
		}
		return result + "\n[Process detached — running in background]", false

	case <-ctx.Done():
		// Agent turn was cancelled; kill the whole group so children release the
		// pipes and the drain goroutines (<-done) actually finish.
		killProcGroup(cmd.Process.Pid)
		cmd.Process.Kill() // covers platforms where killProcGroup is a no-op
		<-done
		cmd.Wait()
		mu.Lock()
		result := buf.String()
		mu.Unlock()
		return fmt.Sprintf("Cancelled.\n%s", result), true
	}
}

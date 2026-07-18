package stdio

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
)

// ─── PTY interactive terminal ────────────────────────────────────────────────

type ptySession struct {
	id  string
	ptm *os.File // PTY master
	cmd *exec.Cmd
	mu  sync.Mutex
}

var (
	ptyMu       sync.Mutex
	ptySessions = map[string]*ptySession{}
)

func handleTerminalPTYCreate(state *ServerState, cmd map[string]any) {
	cwd, _ := cmd["cwd"].(string)
	cols, _ := cmd["cols"].(float64)
	rows, _ := cmd["rows"].(float64)
	if cols < 10 {
		cols = 80
	}
	if rows < 4 {
		rows = 24
	}

	// Expand cwd
	expanded := os.ExpandEnv(cwd)
	if h, err := os.UserHomeDir(); err == nil {
		if expanded == "" || expanded == "~" || expanded == "$HOME" {
			expanded = h
		}
	}
	if info, err := os.Stat(expanded); err != nil || !info.IsDir() {
		if h, err2 := os.UserHomeDir(); err2 == nil {
			expanded = h
		}
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}

	id := uuid.NewString()
	c := exec.Command(shell, "-l")
	c.Dir = expanded
	c.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)

	winSize := &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}
	ptm, err := pty.StartWithSize(c, winSize)
	if err != nil {
		Emit(map[string]any{"type": "terminal_pty_created", "terminal_id": "", "error": err.Error()})
		return
	}

	sess := &ptySession{id: id, ptm: ptm, cmd: c}
	ptyMu.Lock()
	ptySessions[id] = sess
	ptyMu.Unlock()

	Emit(map[string]any{"type": "terminal_pty_created", "terminal_id": id})

	go readPTYOutput(id, sess, ptm)
}

func readPTYOutput(id string, sess *ptySession, ptm *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := ptm.Read(buf)
		if n > 0 {
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			Emit(map[string]any{"type": "terminal_pty_data", "terminal_id": id, "data": encoded})
		}
		if err != nil {
			break
		}
	}
	code := 0
	if err := sess.cmd.Wait(); err != nil {
		if ex, ok := err.(*exec.ExitError); ok {
			code = ex.ExitCode()
		}
	}
	ptyMu.Lock()
	delete(ptySessions, id)
	ptyMu.Unlock()
	Emit(map[string]any{"type": "terminal_pty_exit", "terminal_id": id, "code": code})
}

func handleTerminalPTYInput(state *ServerState, cmd map[string]any) {
	id, _ := cmd["terminal_id"].(string)
	data, _ := cmd["data"].(string) // base64-encoded bytes
	ptyMu.Lock()
	sess, ok := ptySessions[id]
	ptyMu.Unlock()
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return
	}
	sess.mu.Lock()
	_, _ = sess.ptm.Write(raw)
	sess.mu.Unlock()
}

func handleTerminalPTYResize(state *ServerState, cmd map[string]any) {
	id, _ := cmd["terminal_id"].(string)
	cols, _ := cmd["cols"].(float64)
	rows, _ := cmd["rows"].(float64)
	if cols < 2 {
		cols = 80
	}
	if rows < 2 {
		rows = 24
	}
	ptyMu.Lock()
	sess, ok := ptySessions[id]
	ptyMu.Unlock()
	if !ok {
		return
	}
	_ = pty.Setsize(sess.ptm, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func handleTerminalPTYClose(state *ServerState, cmd map[string]any) {
	id, _ := cmd["terminal_id"].(string)
	ptyMu.Lock()
	sess, ok := ptySessions[id]
	if ok {
		delete(ptySessions, id)
	}
	ptyMu.Unlock()
	if ok {
		_ = sess.cmd.Process.Kill()
		_ = sess.ptm.Close()
	}
	Emit(map[string]any{"type": "terminal_pty_closed", "terminal_id": id})
}

// ─── Command-runner terminal (kept for agent use) ────────────────────────────

type termProcess struct {
	id        string
	cmd       string
	cwd       string
	startedAt float64
	proc      *exec.Cmd
	stdin     io.WriteCloser
	exitCode  *int
	mu        sync.Mutex
	buf       []byte
}

var (
	termMu    sync.Mutex
	termProcs = map[string]*termProcess{}
)

const maxTermBuf = 2 * 1024 * 1024

func handleTerminalSpawn(state *ServerState, cmd map[string]any) {
	termCmd, _ := cmd["cmd"].(string)
	cwd, _ := cmd["cwd"].(string)
	if cwd == "" {
		cwd = "~"
	}
	expanded := os.ExpandEnv(cwd)
	if h, err := os.UserHomeDir(); err == nil {
		if expanded == "~" || expanded == "$HOME" {
			expanded = h
		}
	}
	if info, err := os.Stat(expanded); err != nil || !info.IsDir() {
		if h, err2 := os.UserHomeDir(); err2 == nil {
			expanded = h
		}
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}

	id := uuid.NewString()
	c := exec.Command(shell, "-l", "-c", termCmd)
	c.Dir = expanded
	c.Env = append(os.Environ(), "TERM=dumb", "NO_COLOR=1")

	stdinPipe, err := c.StdinPipe()
	if err != nil {
		Emit(map[string]any{"type": "terminal_spawned", "process_id": "", "error": err.Error()})
		return
	}
	c.Stderr = nil

	stdoutPipe, err := c.StdoutPipe()
	if err != nil {
		Emit(map[string]any{"type": "terminal_spawned", "process_id": "", "error": err.Error()})
		return
	}

	if err := c.Start(); err != nil {
		Emit(map[string]any{"type": "terminal_spawned", "process_id": "", "error": err.Error()})
		return
	}

	tp := &termProcess{
		id:        id,
		cmd:       termCmd,
		cwd:       expanded,
		startedAt: float64(time.Now().UnixMilli()) / 1000,
		proc:      c,
		stdin:     stdinPipe,
	}
	termMu.Lock()
	termProcs[id] = tp
	termMu.Unlock()

	Emit(map[string]any{
		"type":       "terminal_spawned",
		"process_id": id,
		"cmd":        termCmd,
		"cwd":        expanded,
	})

	go readTermOutput(id, tp, stdoutPipe)
}

func readTermOutput(id string, tp *termProcess, r io.ReadCloser) {
	buf := make([]byte, 512)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			tp.mu.Lock()
			if len(tp.buf) < maxTermBuf {
				tp.buf = append(tp.buf, buf[:n]...)
			}
			tp.mu.Unlock()
			Emit(map[string]any{"type": "terminal_output", "process_id": id, "data": chunk})
		}
		if err != nil {
			break
		}
	}
	code := 0
	if err := tp.proc.Wait(); err != nil {
		if ex, ok := err.(*exec.ExitError); ok {
			code = ex.ExitCode()
		}
	}
	tp.mu.Lock()
	tp.exitCode = &code
	tp.mu.Unlock()
	Emit(map[string]any{"type": "terminal_exit", "process_id": id, "code": code})
}

func handleTerminalStatus(state *ServerState, cmd map[string]any) {
	id, _ := cmd["process_id"].(string)
	termMu.Lock()
	tp, ok := termProcs[id]
	termMu.Unlock()
	if !ok {
		Emit(map[string]any{"type": "terminal_status", "process_id": id, "error": "not found"})
		return
	}
	tp.mu.Lock()
	running := tp.exitCode == nil
	code := 0
	if tp.exitCode != nil {
		code = *tp.exitCode
	}
	output := string(tp.buf)
	tp.mu.Unlock()
	Emit(map[string]any{
		"type":       "terminal_status",
		"process_id": id,
		"cmd":        tp.cmd,
		"cwd":        tp.cwd,
		"running":    running,
		"exit_code":  code,
		"output":     output,
		"started_at": tp.startedAt,
	})
}

func handleTerminalKill(state *ServerState, cmd map[string]any) {
	id, _ := cmd["process_id"].(string)
	termMu.Lock()
	tp, ok := termProcs[id]
	if ok {
		delete(termProcs, id)
	}
	termMu.Unlock()
	if !ok {
		Emit(map[string]any{"type": "terminal_killed", "process_id": id, "ok": false})
		return
	}
	_ = tp.proc.Process.Kill()
	Emit(map[string]any{"type": "terminal_killed", "process_id": id, "ok": true})
}

func handleTerminalInput(state *ServerState, cmd map[string]any) {
	id, _ := cmd["process_id"].(string)
	text, _ := cmd["text"].(string)
	termMu.Lock()
	tp, ok := termProcs[id]
	termMu.Unlock()
	if !ok {
		Emit(map[string]any{"type": "terminal_input_result", "process_id": id, "ok": false})
		return
	}
	tp.mu.Lock()
	running := tp.exitCode == nil
	tp.mu.Unlock()
	if !running {
		Emit(map[string]any{"type": "terminal_input_result", "process_id": id, "ok": false})
		return
	}
	_, err := fmt.Fprintln(tp.stdin, text)
	if err != nil {
		Emit(map[string]any{"type": "terminal_input_result", "process_id": id, "ok": false, "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "terminal_input_result", "process_id": id, "ok": true})
}

func handleTerminalList(state *ServerState, cmd map[string]any) {
	termMu.Lock()
	list := make([]any, 0, len(termProcs))
	for _, tp := range termProcs {
		tp.mu.Lock()
		running := tp.exitCode == nil
		code := 0
		if tp.exitCode != nil {
			code = *tp.exitCode
		}
		tp.mu.Unlock()
		list = append(list, map[string]any{
			"process_id": tp.id,
			"cmd":        tp.cmd,
			"cwd":        tp.cwd,
			"running":    running,
			"exit_code":  code,
			"started_at": tp.startedAt,
		})
	}
	termMu.Unlock()
	Emit(map[string]any{"type": "terminal_list", "processes": list})
}

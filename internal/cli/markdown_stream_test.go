package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns what was
// written. The loader is left dormant (StartLoader is never called) so no
// cursor-positioning bytes interfere with the capture.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	fn()
	pw.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, pr)
	return buf.String()
}

// TestStreamRendererBuffersAndFlushes exercises the real display wiring: tokens
// buffer, then flush as a formatted "●" block at each segment boundary
// (tool_call, done), interleaved with the tool-call line.
func TestStreamRendererBuffersAndFlushes(t *testing.T) {
	r := NewStreamRenderer()
	out := captureStdout(t, func() {
		r.HandleEvent(map[string]any{"type": "token", "content": "Here is a **plan**:\n\n"})
		r.HandleEvent(map[string]any{"type": "token", "content": "- step one\n- step two"})
		r.HandleEvent(map[string]any{"type": "tool_call", "tool": "run_shell",
			"toolInput": map[string]any{"command": "ls -la"}})
		// Production always follows "tool_call" (fired while the model is still
		// streaming) with "tool_use_start" (fired right before dispatch, in true
		// execution order) — the header itself prints on the latter.
		r.HandleEvent(map[string]any{"type": "tool_use_start", "tool": "run_shell",
			"toolInput": map[string]any{"command": "ls -la"}})
		r.HandleEvent(map[string]any{"type": "token", "content": "All **done** now."})
		r.HandleEvent(map[string]any{"type": "done"})
	})

	plain := reANSI.ReplaceAllString(out, "")

	// First text segment rendered as a bullet block with a list.
	if !strings.Contains(plain, "● Here is a plan:") {
		t.Errorf("missing first bullet block; got:\n%s", plain)
	}
	if !strings.Contains(plain, "– step one") || !strings.Contains(plain, "– step two") {
		t.Errorf("list not rendered; got:\n%s", plain)
	}
	// Tool call appears between the two text segments.
	if !strings.Contains(plain, "Run") || !strings.Contains(plain, "ls -la") {
		t.Errorf("tool call line missing; got:\n%s", plain)
	}
	// Second text segment (before done) also flushed as a bullet block.
	if !strings.Contains(plain, "● All done now.") {
		t.Errorf("second bullet block missing; got:\n%s", plain)
	}
	// Ordering: first block, then tool, then second block.
	iBlock1 := strings.Index(plain, "● Here is a plan:")
	iTool := strings.Index(plain, "ls -la")
	iBlock2 := strings.Index(plain, "● All done now.")
	if !(iBlock1 < iTool && iTool < iBlock2) {
		t.Errorf("segments out of order: block1=%d tool=%d block2=%d", iBlock1, iTool, iBlock2)
	}
}

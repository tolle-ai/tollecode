package cli

import (
	"bytes"
	"io"
	"testing"
)

// chunkedReader hands out its data in fixed-size chunks so tests can exercise
// markers that straddle Read boundaries.
type chunkedReader struct {
	data []byte
	size int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := c.size
	if n <= 0 || n > len(c.data) {
		n = len(c.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// drain reads the wrapper to EOF and returns (bytes readline would see, wrapper).
func drain(t *testing.T, in []byte, chunk int) (string, *bracketedPasteReader) {
	t.Helper()
	r := newBracketedPasteReader(&chunkedReader{data: in, size: chunk})
	var out bytes.Buffer
	buf := make([]byte, 8)
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	return out.String(), r
}

func TestBracketedPasteInline(t *testing.T) {
	// Single-line pastes and non-paste input pass straight through, unchanged,
	// across chunk boundaries.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single-line paste stays inline", pasteBegin + "hello world" + pasteEnd, "hello world"},
		{"markers stripped, surrounding text kept", "a" + pasteBegin + "b" + pasteEnd + "c", "abc"},
		{"typed newline passes through as Enter", "hello\r", "hello\r"},
		{"arrow-key sequence passes through", "a\x1b[Ab", "a\x1b[Ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{0, 1, 2, 3, 5, 7} {
				got, _ := drain(t, []byte(tc.in), chunk)
				if got != tc.want {
					t.Fatalf("chunk=%d got %q want %q", chunk, got, tc.want)
				}
			}
		})
	}
}

func TestBracketedPasteMultilineChip(t *testing.T) {
	content := "line one\nline two\nline three"
	for _, chunk := range []int{0, 1, 2, 3, 5, 7, 13} {
		got, r := drain(t, []byte(pasteBegin+content+pasteEnd), chunk)

		// readline only ever sees a short single-line chip — no newlines, no
		// giant blob that would trigger the redraw "glitch".
		wantChip := "[Pasted 3 lines #1]"
		if got != wantChip {
			t.Fatalf("chunk=%d chip=%q want %q", chunk, got, wantChip)
		}
		// On submit the chip expands back to the exact original content.
		if expanded := r.expand(got); expanded != content {
			t.Fatalf("chunk=%d expand=%q want %q", chunk, expanded, content)
		}
		// Stash is cleared after expand.
		if expanded := r.expand(wantChip); expanded != wantChip {
			t.Fatalf("expand should be a no-op after clear, got %q", expanded)
		}
	}
}

func TestBracketedPasteCRLFNormalized(t *testing.T) {
	got, r := drain(t, []byte(pasteBegin+"a\r\nb\rc"+pasteEnd), 3)
	if got != "[Pasted 3 lines #1]" {
		t.Fatalf("chip=%q", got)
	}
	if expanded := r.expand(got); expanded != "a\nb\nc" {
		t.Fatalf("expand=%q want %q", expanded, "a\nb\nc")
	}
}

func TestBracketedPasteWithSurroundingText(t *testing.T) {
	// User types, pastes a block, then types more, then submits.
	got, r := drain(t, []byte("see: "+pasteBegin+"x\ny"+pasteEnd), 4)
	if got != "see: [Pasted 2 lines #1]" {
		t.Fatalf("got %q", got)
	}
	line := r.expand(got + " thanks")
	if line != "see: x\ny thanks" {
		t.Fatalf("expand=%q", line)
	}
}

func TestBracketedPasteTwoBlocksDistinctChips(t *testing.T) {
	raw := pasteBegin + "a\nb" + pasteEnd + " and " + pasteBegin + "c\nd" + pasteEnd
	got, r := drain(t, []byte(raw), 5)
	want := "[Pasted 2 lines #1] and [Pasted 2 lines #2]"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if expanded := r.expand(got); expanded != "a\nb and c\nd" {
		t.Fatalf("expand=%q", expanded)
	}
}

func TestResolveIdleEscape(t *testing.T) {
	// A lone ESC with no follow-up byte is a standalone Escape → becomes Ctrl-C,
	// which the REPL treats like Ctrl-C and exits.
	t.Run("lone esc becomes interrupt", func(t *testing.T) {
		b := newBracketedPasteReader(&chunkedReader{})
		b.pend = []byte{0x1b}
		b.resolveIdleEscape()
		if got := b.out.String(); got != string([]byte{charInterrupt}) {
			t.Fatalf("got %q want Ctrl-C (0x03)", got)
		}
		if len(b.pend) != 0 {
			t.Fatalf("pend not cleared: %q", b.pend)
		}
	})

	// A stalled partial escape sequence is not a lone Esc: pass it through
	// literally so readline can finish parsing it.
	t.Run("partial sequence passes through literally", func(t *testing.T) {
		b := newBracketedPasteReader(&chunkedReader{})
		b.pend = []byte("\x1b[2")
		b.resolveIdleEscape()
		if got := b.out.String(); got != "\x1b[2" {
			t.Fatalf("got %q want literal ESC[2", got)
		}
	})
}

func TestBracketedPasteEndToEnd(t *testing.T) {
	// Mirror the REPL path: translate raw stream, then expand on submit.
	code := "func main() {\n\tprintln(\"hi\")\n}"
	got, r := drain(t, []byte(pasteBegin+code+pasteEnd), 4)
	if got != "[Pasted 3 lines #1]" {
		t.Fatalf("chip=%q", got)
	}
	if expanded := r.expand(got); expanded != code {
		t.Fatalf("expand=%q want %q", expanded, code)
	}
}

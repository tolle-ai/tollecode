package cli

import (
	"fmt"
	"testing"
)

// TestPasteReaderPassesArrowKeysThrough: the bracketed-paste stdin wrapper sits
// between the terminal and readline, and readline can only do cursor movement
// if arrow sequences reach it byte-for-byte. The wrapper holds ESC-led bytes
// back while it decides whether they open a paste marker (ESC[200~), so a
// regression here would silently turn arrow keys into literal text.
//
// Each input is replayed at several chunk sizes: a terminal may hand over a
// sequence whole or split it across reads, and size 1 is the worst case —
// every byte arrives separately.
func TestPasteReaderPassesArrowKeysThrough(t *testing.T) {
	inputs := []struct {
		name string
		seq  string
	}{
		{"left arrow", "\x1b[D"},
		{"right arrow", "\x1b[C"},
		{"all four arrows", "\x1b[A\x1b[B\x1b[C\x1b[D"},
		{"text then arrow", "abc\x1b[D"},
		{"arrow then text", "\x1b[Dabc"},
		{"home and end", "\x1b[H\x1b[F"},
		{"word-wise (ctrl+arrow)", "\x1b[1;5D\x1b[1;5C"},
		{"SS3 arrows (application mode)", "\x1bOA\x1bOD"},
	}
	for _, in := range inputs {
		for _, size := range []int{0, 1, 2, 3} { // 0 = deliver everything at once
			t.Run(fmt.Sprintf("%s/chunk=%d", in.name, size), func(t *testing.T) {
				b := newBracketedPasteReader(&chunkedReader{data: []byte(in.seq), size: size})
				var got []byte
				buf := make([]byte, 64)
				for {
					n, err := b.Read(buf)
					got = append(got, buf[:n]...)
					if err != nil {
						break
					}
				}
				if string(got) != in.seq {
					t.Errorf("reader altered the sequence:\n got %q\nwant %q", got, in.seq)
				}
			})
		}
	}
}

package cli

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
)

// runChoiceOnPTY drives runChoicePrompt over a real pseudo-terminal (so raw mode
// and arrow-key parsing are exercised), writing keys with a small delay between
// them, and returns the selected index. Fails if it doesn't finish in time.
func runChoiceOnPTY(t *testing.T, options []string, keys [][]byte) int {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80})

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = tty, tty
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	go io.Copy(io.Discard, ptmx) // drain rendered frames so writes never block

	res := make(chan int, 1)
	go func() { res <- runChoicePrompt("Proceed?", options, 0) }()

	time.Sleep(60 * time.Millisecond) // let the read loop start + draw
	for _, k := range keys {
		ptmx.Write(k)
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case got := <-res:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("runChoicePrompt did not return")
		return -99
	}
}

func TestRunChoicePromptArrowKeys(t *testing.T) {
	opts := []string{"Yes", "Always allow", "No"}
	down := []byte{27, '[', 'B'}
	up := []byte{27, '[', 'A'}
	enter := []byte{13}
	esc := []byte{27}

	cases := []struct {
		name string
		keys [][]byte
		want int
	}{
		{"enter picks initial", [][]byte{enter}, 0},
		{"down then enter", [][]byte{down, enter}, 1},
		{"down down enter", [][]byte{down, down, enter}, 2},
		{"down past end clamps", [][]byte{down, down, down, down, enter}, 2},
		{"down up enter", [][]byte{down, up, enter}, 0},
		{"digit selects", [][]byte{{'2'}}, 1},
		{"esc cancels", [][]byte{esc}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runChoiceOnPTY(t, opts, tc.keys); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

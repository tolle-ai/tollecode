package cli

import (
	"io"
	"os"
	"testing"
)

// withStdio runs fn with os.Stdin fed by `input` and os.Stdout discarded, so the
// cooked-mode fallback of runChoicePrompt can be exercised without a TTY.
func withStdio(t *testing.T, input string, fn func()) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { io.WriteString(inW, input); inW.Close() }()
	go func() { io.Copy(io.Discard, outR) }()

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		outW.Close()
	}()
	fn()
}

func TestRunChoicePromptCookedFallback(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{"picks second", "2\n", 1},
		{"picks first", "1\n", 0},
		{"picks third", "3\n", 2},
		{"empty cancels", "\n", -1},
		{"invalid then valid", "9\n2\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			withStdio(t, tc.input, func() {
				got = runChoicePrompt("Do you want to proceed?", permissionChoices, 0)
			})
			if got != tc.want {
				t.Errorf("input %q: got %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

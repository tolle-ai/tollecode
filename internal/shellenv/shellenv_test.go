package shellenv

import (
	"runtime"
	"testing"
)

func TestLookupUnixDefaultUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix default path not exercised on Windows")
	}
	sh, err := Lookup("sh")
	if err != nil {
		t.Fatalf("Lookup returned error on %s: %v", runtime.GOOS, err)
	}
	if sh.Path != "sh" || sh.Kind != POSIX {
		t.Fatalf("Lookup(sh) = %+v, want {sh POSIX}", sh)
	}
	if sh, _ := Lookup("bash"); sh.Path != "bash" {
		t.Fatalf("Lookup(bash).Path = %q, want bash", sh.Path)
	}
}

func TestArgs(t *testing.T) {
	posix := Shell{Path: "sh", Kind: POSIX}
	if got := posix.Args(false); len(got) != 1 || got[0] != "-c" {
		t.Fatalf("POSIX Args(false) = %v, want [-c]", got)
	}
	if got := posix.Args(true); len(got) != 1 || got[0] != "-lc" {
		t.Fatalf("POSIX Args(true) = %v, want [-lc]", got)
	}
	ps := Shell{Path: "powershell", Kind: PowerShell}
	got := ps.Args(false)
	if len(got) == 0 || got[len(got)-1] != "-Command" {
		t.Fatalf("PowerShell Args = %v, want to end with -Command", got)
	}
}

// discoverWindowsShell is OS-agnostic in its helpers, so its override handling
// can be exercised on any platform.
func TestDiscoverOverride(t *testing.T) {
	// A real executable on the test host; sh exists on the CI/dev machines.
	t.Setenv(EnvOverride, "/bin/sh")
	sh, err := discoverWindowsShell()
	if err != nil {
		t.Fatalf("override discovery failed: %v", err)
	}
	if sh.Path != "/bin/sh" || sh.Kind != POSIX {
		t.Fatalf("override = %+v, want {/bin/sh POSIX}", sh)
	}
}

func TestDiscoverOverrideMissingErrors(t *testing.T) {
	t.Setenv(EnvOverride, "/no/such/shell-xyz")
	if _, err := discoverWindowsShell(); err == nil {
		t.Fatal("expected error for non-executable override, got nil")
	}
}

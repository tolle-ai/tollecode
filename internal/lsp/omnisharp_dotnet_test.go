package lsp

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidDotnetRoot(t *testing.T) {
	if validDotnetRoot("") {
		t.Error("empty path should be invalid")
	}
	if validDotnetRoot("/nonexistent/dotnet") {
		t.Error("missing path should be invalid")
	}

	// A dir without shared/Microsoft.NETCore.App is not a valid root.
	bare := t.TempDir()
	if validDotnetRoot(bare) {
		t.Error("dir without shared runtime should be invalid")
	}

	// A dir with a runtime version subdir is valid.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "shared", "Microsoft.NETCore.App", "8.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !validDotnetRoot(root) {
		t.Error("dir with a shared runtime version should be valid")
	}

	// shared/Microsoft.NETCore.App present but empty (no version dir) → invalid.
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "shared", "Microsoft.NETCore.App"), 0o755); err != nil {
		t.Fatal(err)
	}
	if validDotnetRoot(empty) {
		t.Error("empty shared runtime dir should be invalid")
	}
}

// TestOmniSharpResolverAttachesDotnetRoot verifies that when OmniSharp is
// installed locally, the resolver attaches a DOTNET_ROOT so the apphost can find
// the .NET runtime even when launched without an inherited shell environment.
func TestOmniSharpResolverAttachesDotnetRoot(t *testing.T) {
	pkg, ok := FindPackageByID("omnisharp")
	if !ok {
		t.Fatal("omnisharp not in registry")
	}
	cmd, err := resolveOmniSharpPkg(pkg, "")
	if err != nil {
		t.Skipf("OmniSharp not installed locally: %v", err)
	}
	if findDotnetRoot() == "" {
		t.Skip("no .NET runtime available in this environment")
	}
	for _, e := range cmd.env {
		if strings.HasPrefix(e, "DOTNET_ROOT=") {
			return
		}
	}
	t.Fatalf("resolver attached no DOTNET_ROOT; env=%v", cmd.env)
}

func TestRuntimePrereq(t *testing.T) {
	// A server with no special runtime is always ready.
	gopls, _ := FindPackageByID("gopls")
	if ready, _, _ := runtimePrereq(gopls); !ready {
		t.Error("gopls should not require a special runtime")
	}

	// OmniSharp readiness tracks the presence of a .NET runtime, and an
	// unready result must carry both a message and a download URL to act on.
	omni, _ := FindPackageByID("omnisharp")
	ready, msg, url := runtimePrereq(omni)
	if ready != (findDotnetRoot() != "") {
		t.Errorf("omnisharp ready=%v but dotnet root present=%v", ready, findDotnetRoot() != "")
	}
	if !ready && (msg == "" || url == "") {
		t.Errorf("unready omnisharp must include message+url, got msg=%q url=%q", msg, url)
	}
}

func TestHandleRuntimeCheck(t *testing.T) {
	decode := func(lang string) (ready bool, message, downloadURL string) {
		rec := httptest.NewRecorder()
		HandleRuntimeCheck(rec, httptest.NewRequest("GET", "/lsp/runtime/"+lang, nil), lang)
		var resp struct {
			Ready       bool   `json:"ready"`
			Message     string `json:"message"`
			DownloadURL string `json:"downloadUrl"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode %s: %v", lang, err)
		}
		return resp.Ready, resp.Message, resp.DownloadURL
	}

	// Unknown language → trivially ready.
	if ready, _, _ := decode("cobol"); !ready {
		t.Error("unknown language should report ready")
	}

	// C# (OmniSharp): when not ready, the payload must guide the user to a download.
	if ready, msg, url := decode("csharp"); !ready && (msg == "" || url == "") {
		t.Errorf("unready csharp response must include message+downloadUrl, got msg=%q url=%q", msg, url)
	}
}

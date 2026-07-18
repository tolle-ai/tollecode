package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// ── Registry endpoint ─────────────────────────────────────────────────────────

type packageStatus struct {
	LspPackage
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
}

// HandleRegistry returns the full package list with live installed/path fields.
func HandleRegistry(w http.ResponseWriter, _ *http.Request) {
	result := make([]packageStatus, 0, len(Registry))
	for _, pkg := range Registry {
		ps := packageStatus{LspPackage: pkg}
		if pkg.Resolver != "" {
			if fn, ok := resolvers[pkg.Resolver]; ok {
				if cmd, err := fn(pkg, ""); err == nil {
					ps.Installed = true
					ps.Path = cmd.bin
				}
			}
		} else if path, err := findBin(pkg.Bin, ""); err == nil {
			ps.Installed = true
			ps.Path = path
		}
		result = append(result, ps)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ── Install endpoint ──────────────────────────────────────────────────────────

// HandleInstall streams installation output via Server-Sent Events.
//
// GET /lsp/install/{id}?workspacePath=/path/to/project
//
// Each SSE event is one of:
//
//	data: <log line>
//	data: {"done":true}
//	data: {"error":"<message>"}
func HandleInstall(w http.ResponseWriter, r *http.Request, id string) {
	pkg, ok := FindPackageByID(id)
	if !ok {
		http.Error(w, "unknown package: "+id, http.StatusNotFound)
		return
	}

	installCmd, ok := pkg.Install[runtime.GOOS]
	if !ok {
		http.Error(w, "no install command for "+runtime.GOOS, http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	emit := func(line string) {
		fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " "))
		flusher.Flush()
	}

	workspacePath := r.URL.Query().Get("workspacePath")

	cmd := exec.CommandContext(r.Context(), installCmd[0], installCmd[1:]...)
	cmd.Env = envWithShellPATH()
	if workspacePath != "" {
		cmd.Dir = workspacePath
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emit(jsonError(err.Error()))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		emit(jsonError(err.Error()))
		return
	}

	if err := cmd.Start(); err != nil {
		emit(jsonError("failed to start: " + err.Error()))
		return
	}
	emit(fmt.Sprintf("Running: %s", strings.Join(installCmd, " ")))

	var wg sync.WaitGroup
	stream := func(reader io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			emit(scanner.Text())
		}
	}
	wg.Add(2)
	go stream(stdout)
	go stream(stderr)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		emit(jsonError(err.Error()))
		return
	}

	// Bust the binary cache so the next WS connect re-detects the installed binary.
	binCacheMu.Lock()
	delete(binCache, pkg.Bin)
	binCacheMu.Unlock()

	// Surface runtime prerequisites now (in the install log) rather than as a
	// silent "Connecting…" hang when the user later opens a file.
	if note := verifyInstall(pkg); note != "" {
		emit(note)
	}

	emit(`{"done":true}`)
}

// ── Runtime prerequisite check ────────────────────────────────────────────────

// runtimePrereq reports whether pkg's external runtime prerequisites are met,
// plus a user-facing message and download URL when they are not. Most servers
// have no extra runtime, so this returns ready=true by default.
func runtimePrereq(pkg LspPackage) (ready bool, message, downloadURL string) {
	switch pkg.ID {
	case "omnisharp":
		if findDotnetRoot() == "" {
			return false,
				"OmniSharp (the C# language server) needs the .NET 6+ runtime to run, " +
					"but none was found on this machine. Install it, then reconnect.",
				"https://dotnet.microsoft.com/download/dotnet"
		}
	}
	return true, "", ""
}

// verifyInstall runs package-specific sanity checks after a successful install
// and returns a human-readable note if the server won't actually run yet.
// An empty string means the server is ready to use.
func verifyInstall(pkg LspPackage) string {
	if ready, msg, url := runtimePrereq(pkg); !ready {
		return "⚠ " + msg + " Download it from " + url
	}
	return ""
}

// HandleRuntimeCheck reports whether the language server for a given Monaco
// language has its runtime prerequisites satisfied.
//
//	GET /lsp/runtime/{lang}
//
// Response: {"ready":true} or
//
//	{"ready":false,"message":"…","downloadUrl":"…"}
func HandleRuntimeCheck(w http.ResponseWriter, _ *http.Request, lang string) {
	type resp struct {
		Ready       bool   `json:"ready"`
		Message     string `json:"message,omitempty"`
		DownloadURL string `json:"downloadUrl,omitempty"`
	}

	out := resp{Ready: true}
	if pkg, ok := FindPackageForLang(lang); ok {
		out.Ready, out.Message, out.DownloadURL = runtimePrereq(pkg)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func jsonError(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

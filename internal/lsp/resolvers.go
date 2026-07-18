package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolveFunc computes the final lspCommand for a package at connection time.
// It receives the package definition and the workspace rootUri from the query string.
type ResolveFunc func(pkg LspPackage, rootURI string) (*lspCommand, error)

// resolvers maps Resolver field values to their implementation.
var resolvers = map[string]ResolveFunc{
	"angular":   resolveAngularPkg,
	"omnisharp": resolveOmniSharpPkg,
}

// resolveStandard handles 99% of packages: locate the binary, pass static args.
func resolveStandard(pkg LspPackage, _ string) (*lspCommand, error) {
	bin, err := findBin(pkg.Bin, "")
	if err != nil {
		return nil, fmt.Errorf(
			"%s not found — open the Language Servers marketplace to install it", pkg.Name)
	}
	return &lspCommand{bin: bin, args: pkg.Args}, nil
}

// resolveAngularPkg delegates to the Angular-specific resolver.
func resolveAngularPkg(pkg LspPackage, rootURI string) (*lspCommand, error) {
	return resolveAngularCmd(rootURI)
}

// resolveOmniSharpPkg looks directly in ~/.tollecode/lsp/omnisharp/ for the
// OmniSharp binary. The standard symlink-based lookup fails when the install
// script creates the symlink pointing at the directory instead of the binary.
//
// It also appends -s <rootPath> when a rootURI is provided so OmniSharp
// loads the project from the workspace rather than the sidecar's cwd.
func resolveOmniSharpPkg(pkg LspPackage, rootURI string) (*lspCommand, error) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".tollecode", "lsp", "omnisharp")

	candidates := []string{"OmniSharp", "run", "omnisharp"}
	if runtime.GOOS == "windows" {
		candidates = []string{"OmniSharp.exe", "omnisharp.exe"}
	}

	var cmd *lspCommand
	for _, name := range candidates {
		p := filepath.Join(dir, name)
		if isExecutable(p) {
			args := append([]string{}, pkg.Args...)
			if root := strings.TrimPrefix(rootURI, "file://"); root != "" {
				args = append(args, "-s", root)
			}
			cmd = &lspCommand{bin: p, args: args}
			break
		}
	}

	if cmd == nil {
		// Fallback to PATH / ~/.tollecode/bin symlink in case the user installed manually.
		var err error
		if cmd, err = resolveStandard(pkg, ""); err != nil {
			return nil, err
		}
	}

	// OmniSharp's framework-dependent net6.0 apphost needs an explicit DOTNET_ROOT:
	// GUI-launched apps don't inherit the user's shell env, and the apphost
	// mis-resolves the runtime location on Apple Silicon. Without this it dies with
	// "libhostfxr.dylib could not be found" and the LSP never initializes.
	if root := findDotnetRoot(); root != "" {
		cmd.env = append(cmd.env, "DOTNET_ROOT="+root)
	}
	return cmd, nil
}

// ── Angular resolver ──────────────────────────────────────────────────────────

func resolveAngularCmd(rootURI string) (*lspCommand, error) {
	root := strings.TrimPrefix(rootURI, "file://")

	if root != "" {
		angularRoot, found := findAngularJSON(root, 3)
		if !found {
			return nil, fmt.Errorf(
				"not an Angular project (no angular.json in %s or its subdirectories)", root)
		}
		fmt.Fprintf(os.Stderr, "[lsp] angular project root: %s\n", angularRoot)
		root = angularRoot
	}

	nodePath, serverPath, err := findAngularLS(root)
	if err != nil {
		return nil, fmt.Errorf("Angular Language Server not found: %w", err)
	}

	// Resolve the node_modules that is a peer of @angular/language-server.
	// VSCode extension layout: <ext>/server/index.js → <ext>/node_modules  (2 up)
	// npm package layout:      node_modules/@angular/language-server/index.js → node_modules (3 up)
	var extNodeModules string
	if strings.Contains(serverPath, ".vscode"+string(filepath.Separator)+"extensions") {
		extNodeModules = filepath.Join(filepath.Dir(filepath.Dir(serverPath)), "node_modules")
	} else {
		extNodeModules = filepath.Dir(filepath.Dir(filepath.Dir(serverPath)))
	}

	probeDirs := extNodeModules
	if root != "" {
		if wsNM := filepath.Join(root, "node_modules"); fileExists(wsNM) {
			// Workspace-local wins — its versions match the project's Angular version.
			probeDirs = wsNM + "," + extNodeModules
		}
	}

	return &lspCommand{
		bin:  nodePath,
		args: []string{serverPath, "--stdio", "--tsProbeLocations", probeDirs, "--ngProbeLocations", probeDirs},
	}, nil
}

// findAngularJSON searches for angular.json up to maxDepth levels deep,
// skipping hidden dirs, node_modules, dist and vendor.
func findAngularJSON(root string, maxDepth int) (string, bool) {
	if fileExists(filepath.Join(root, "angular.json")) {
		return root, true
	}
	if maxDepth <= 0 {
		return "", false
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist" || name == "vendor" {
			continue
		}
		if found, ok := findAngularJSON(filepath.Join(root, name), maxDepth-1); ok {
			return found, ok
		}
	}
	return "", false
}

func findAngularLS(workspaceRoot string) (nodePath, serverPath string, err error) {
	nodePath, err = findBin("node", "")
	if err != nil {
		err = fmt.Errorf("node not found — required to run Angular Language Server")
		return
	}

	home, _ := os.UserHomeDir()
	vscodeExtRoot := filepath.Join(home, ".vscode", "extensions")

	// Priority (first match wins): workspace-local → VSCode extension → system-global.
	var candidates []string
	if workspaceRoot != "" {
		candidates = append(candidates,
			filepath.Join(workspaceRoot, "node_modules", "@angular", "language-server", "index.js"))
	}
	extMatches, _ := filepath.Glob(filepath.Join(vscodeExtRoot, "angular.ng-template-*", "server", "index.js"))
	candidates = append(candidates, extMatches...)
	for _, prefix := range []string{"/usr/local/lib", "/opt/homebrew/lib", filepath.Join(home, ".npm-global", "lib")} {
		candidates = append(candidates,
			filepath.Join(prefix, "node_modules", "@angular", "language-server", "index.js"))
	}

	for _, c := range candidates {
		if fileExists(c) {
			serverPath = c
			return
		}
	}
	err = fmt.Errorf("@angular/language-server not found — install it from the Language Servers marketplace")
	return
}

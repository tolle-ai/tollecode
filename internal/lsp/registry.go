package lsp

// LspPackage describes a language server that can be installed and proxied.
type LspPackage struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Languages   []string            `json:"languages"`
	Bin         string              `json:"bin"`
	Args        []string            `json:"args"`
	Install     map[string][]string `json:"install"` // runtime.GOOS → command+args
	Homepage    string              `json:"homepage"`
	// Resolver names a custom resolver function in resolvers.go.
	// Empty string means standard: findBin(Bin) + static Args.
	Resolver string `json:"resolver,omitempty"`
}

// Registry is the built-in catalogue of supported language servers.
// New entries only need Bin (or a named Resolver); the rest is metadata.
var Registry = []LspPackage{
	{
		ID:          "gopls",
		Name:        "Go Language Server",
		Description: "Official Go language server — completions, diagnostics, refactoring.",
		Languages:   []string{"go"},
		Bin:         "gopls",
		Args:        []string{},
		Install: map[string][]string{
			"darwin":  {"go", "install", "golang.org/x/tools/gopls@latest"},
			"linux":   {"go", "install", "golang.org/x/tools/gopls@latest"},
			"windows": {"go", "install", "golang.org/x/tools/gopls@latest"},
		},
		Homepage: "https://pkg.go.dev/golang.org/x/tools/gopls",
	},
	{
		ID:          "rust-analyzer",
		Name:        "Rust Analyzer",
		Description: "Compiler-based Rust language server — inlay hints, macro expansion.",
		Languages:   []string{"rust"},
		Bin:         "rust-analyzer",
		Args:        []string{},
		Install: map[string][]string{
			"darwin":  {"rustup", "component", "add", "rust-analyzer"},
			"linux":   {"rustup", "component", "add", "rust-analyzer"},
			"windows": {"rustup", "component", "add", "rust-analyzer"},
		},
		Homepage: "https://rust-analyzer.github.io",
	},
	{
		ID:          "pyright",
		Name:        "Pyright",
		Description: "Microsoft Python static type checker and language server.",
		Languages:   []string{"python"},
		Bin:         "pyright-langserver",
		Args:        []string{"--stdio"},
		Install: map[string][]string{
			"darwin":  {"npm", "install", "-g", "pyright"},
			"linux":   {"npm", "install", "-g", "pyright"},
			"windows": {"npm", "install", "-g", "pyright"},
		},
		Homepage: "https://github.com/microsoft/pyright",
	},
	{
		ID:          "clangd",
		Name:        "clangd",
		Description: "LLVM's C/C++ language server — code completion, cross-references.",
		Languages:   []string{"c", "cpp"},
		Bin:         "clangd",
		Args:        []string{},
		Install: map[string][]string{
			"darwin":  {"brew", "install", "llvm"},
			"linux":   {"apt-get", "install", "-y", "clangd"},
			"windows": {"winget", "install", "LLVM.LLVM"},
		},
		Homepage: "https://clangd.llvm.org",
	},
	{
		ID:          "angular-ls",
		Name:        "Angular Language Server",
		Description: "Official Angular language server — template completions, diagnostics.",
		Languages:   []string{"html"},
		Bin:         "node",
		Args:        []string{},
		Resolver:    "angular",
		Install: map[string][]string{
			"darwin":  {"npm", "install", "@angular/language-server", "@angular/language-service", "typescript"},
			"linux":   {"npm", "install", "@angular/language-server", "@angular/language-service", "typescript"},
			"windows": {"npm", "install", "@angular/language-server", "@angular/language-service", "typescript"},
		},
		Homepage: "https://github.com/angular/vscode-ng-language-service",
	},
	{
		ID:          "typescript-ls",
		Name:        "TypeScript Language Server",
		Description: "TypeScript and JavaScript language server.",
		Languages:   []string{"typescript", "javascript"},
		Bin:         "typescript-language-server",
		Args:        []string{"--stdio"},
		Install: map[string][]string{
			"darwin":  {"npm", "install", "-g", "typescript", "typescript-language-server"},
			"linux":   {"npm", "install", "-g", "typescript", "typescript-language-server"},
			"windows": {"npm", "install", "-g", "typescript", "typescript-language-server"},
		},
		Homepage: "https://github.com/typescript-language-server/typescript-language-server",
	},
	{
		ID:          "vscode-css-ls",
		Name:        "CSS Language Server",
		Description: "CSS, SCSS and Less language server from VS Code.",
		Languages:   []string{"css", "scss", "less"},
		Bin:         "vscode-css-language-server",
		Args:        []string{"--stdio"},
		Install: map[string][]string{
			"darwin":  {"npm", "install", "-g", "vscode-langservers-extracted"},
			"linux":   {"npm", "install", "-g", "vscode-langservers-extracted"},
			"windows": {"npm", "install", "-g", "vscode-langservers-extracted"},
		},
		Homepage: "https://github.com/hrsh7th/vscode-langservers-extracted",
	},
	{
		ID:          "vscode-json-ls",
		Name:        "JSON Language Server",
		Description: "JSON / JSONC language server with schema support from VS Code.",
		Languages:   []string{"json", "jsonc"},
		Bin:         "vscode-json-language-server",
		Args:        []string{"--stdio"},
		Install: map[string][]string{
			"darwin":  {"npm", "install", "-g", "vscode-langservers-extracted"},
			"linux":   {"npm", "install", "-g", "vscode-langservers-extracted"},
			"windows": {"npm", "install", "-g", "vscode-langservers-extracted"},
		},
		Homepage: "https://github.com/hrsh7th/vscode-langservers-extracted",
	},
	{
		ID:          "yaml-ls",
		Name:        "YAML Language Server",
		Description: "YAML language server with JSON Schema validation.",
		Languages:   []string{"yaml"},
		Bin:         "yaml-language-server",
		Args:        []string{"--stdio"},
		Install: map[string][]string{
			"darwin":  {"npm", "install", "-g", "yaml-language-server"},
			"linux":   {"npm", "install", "-g", "yaml-language-server"},
			"windows": {"npm", "install", "-g", "yaml-language-server"},
		},
		Homepage: "https://github.com/redhat-developer/yaml-language-server",
	},
	{
		ID:          "bash-ls",
		Name:        "Bash Language Server",
		Description: "Language server for shell scripts using shellcheck.",
		Languages:   []string{"shell", "shellscript"},
		Bin:         "bash-language-server",
		Args:        []string{"start"},
		Install: map[string][]string{
			"darwin":  {"npm", "install", "-g", "bash-language-server"},
			"linux":   {"npm", "install", "-g", "bash-language-server"},
			"windows": {"npm", "install", "-g", "bash-language-server"},
		},
		Homepage: "https://github.com/bash-lsp/bash-language-server",
	},
	{
		ID:          "lua-ls",
		Name:        "Lua Language Server",
		Description: "Full-featured Lua language server with type annotations.",
		Languages:   []string{"lua"},
		Bin:         "lua-language-server",
		Args:        []string{},
		Install: map[string][]string{
			"darwin":  {"brew", "install", "lua-language-server"},
			"linux":   {"snap", "install", "--classic", "lua-language-server"},
			"windows": {"winget", "install", "lua-language-server"},
		},
		Homepage: "https://github.com/LuaLS/lua-language-server",
	},
	{
		ID:          "terraform-ls",
		Name:        "Terraform Language Server",
		Description: "Official HashiCorp Terraform language server.",
		Languages:   []string{"terraform", "hcl"},
		Bin:         "terraform-ls",
		Args:        []string{"serve"},
		Install: map[string][]string{
			"darwin":  {"brew", "install", "hashicorp/tap/terraform-ls"},
			"linux":   {"apt-get", "install", "-y", "terraform-ls"},
			"windows": {"winget", "install", "Hashicorp.TerraformLS"},
		},
		Homepage: "https://github.com/hashicorp/terraform-ls",
	},
	{
		ID:          "marksman",
		Name:        "Marksman",
		Description: "Markdown language server with cross-file link completion.",
		Languages:   []string{"markdown"},
		Bin:         "marksman",
		Args:        []string{"server"},
		Install: map[string][]string{
			"darwin":  {"brew", "install", "marksman"},
			"linux":   {"brew", "install", "marksman"},
			"windows": {"winget", "install", "artempyanykh.marksman"},
		},
		Homepage: "https://github.com/artempyanykh/marksman",
	},
	{
		ID:          "omnisharp",
		Name:        "OmniSharp",
		Description: "Battle-tested C# language server built on Roslyn, backed by Microsoft.",
		Languages:   []string{"csharp"},
		Bin:         "omnisharp",
		Args:        []string{"--languageserver"},
		Resolver:    "omnisharp",
		Install: map[string][]string{
			"darwin": {"sh", "-c",
				`DEST="$HOME/.tollecode/lsp/omnisharp" && rm -rf "$DEST" && mkdir -p "$DEST" && ` +
					`ARCH=$(uname -m) && ` +
					`if [ "$ARCH" = "arm64" ]; then ASSET="omnisharp-osx-arm64-net6.0.tar.gz"; else ASSET="omnisharp-osx-x64-net6.0.tar.gz"; fi && ` +
					`echo "Downloading $ASSET..." && ` +
					`curl -sL "https://github.com/OmniSharp/omnisharp-roslyn/releases/latest/download/$ASSET" | tar xz -C "$DEST" && ` +
					`BIN=$(find "$DEST" -mindepth 1 -maxdepth 1 -type f \( -name "OmniSharp" -o -name "run" -o -name "omnisharp" \) | head -1) && ` +
					`if [ -z "$BIN" ]; then echo "ERROR: could not find OmniSharp binary in $DEST"; ls "$DEST"; exit 1; fi && ` +
					`chmod +x "$BIN" && ` +
					`echo "OmniSharp installed: $BIN"`,
			},
			"linux": {"sh", "-c",
				`DEST="$HOME/.tollecode/lsp/omnisharp" && rm -rf "$DEST" && mkdir -p "$DEST" && ` +
					`ARCH=$(uname -m) && ` +
					`if [ "$ARCH" = "aarch64" ]; then ASSET="omnisharp-linux-arm64-net6.0.tar.gz"; else ASSET="omnisharp-linux-x64-net6.0.tar.gz"; fi && ` +
					`echo "Downloading $ASSET..." && ` +
					`curl -sL "https://github.com/OmniSharp/omnisharp-roslyn/releases/latest/download/$ASSET" | tar xz -C "$DEST" && ` +
					`BIN=$(find "$DEST" -mindepth 1 -maxdepth 1 -type f \( -name "OmniSharp" -o -name "run" -o -name "omnisharp" \) | head -1) && ` +
					`if [ -z "$BIN" ]; then echo "ERROR: could not find OmniSharp binary in $DEST"; ls "$DEST"; exit 1; fi && ` +
					`chmod +x "$BIN" && ` +
					`echo "OmniSharp installed: $BIN"`,
			},
			"windows": {"powershell", "-Command",
				`$dest = "$env:USERPROFILE\.tollecode\lsp\omnisharp"; ` +
					`New-Item -ItemType Directory -Force -Path $dest | Out-Null; ` +
					`Invoke-WebRequest -Uri "https://github.com/OmniSharp/omnisharp-roslyn/releases/latest/download/omnisharp-win-x64-net6.0.zip" -OutFile "$env:TEMP\omnisharp.zip"; ` +
					`Expand-Archive -Path "$env:TEMP\omnisharp.zip" -DestinationPath $dest -Force; ` +
					`$bin = "$env:USERPROFILE\.tollecode\bin"; ` +
					`New-Item -ItemType Directory -Force -Path $bin | Out-Null; ` +
					`Copy-Item "$dest\OmniSharp.exe" "$bin\omnisharp.exe" -Force; ` +
					`Write-Host "OmniSharp installed to $bin\omnisharp.exe"`,
			},
		},
		Homepage: "https://github.com/OmniSharp/omnisharp-roslyn",
	},
	{
		ID:          "dart",
		Name:        "Dart & Flutter",
		Description: "Dart Analysis Server — completions, diagnostics, and refactoring for Dart and Flutter. Ships with the Flutter/Dart SDK.",
		Languages:   []string{"dart"},
		Bin:         "dart",
		Args:        []string{"language-server", "--protocol=lsp"},
		Install: map[string][]string{
			// Flutter bundles the Dart SDK (and `dart language-server`), so a
			// Flutter install covers both. Most Flutter devs already have it — the
			// resolver then just finds `dart` on the shell PATH.
			"darwin":  {"brew", "install", "--cask", "flutter"},
			"linux":   {"snap", "install", "flutter", "--classic"},
			"windows": {"choco", "install", "flutter", "-y"},
		},
		Homepage: "https://dart.dev/tools/dart-language-server",
	},
	{
		ID:          "dockerfile-ls",
		Name:        "Dockerfile Language Server",
		Description: "Language server for Dockerfiles.",
		Languages:   []string{"dockerfile"},
		Bin:         "docker-langserver",
		Args:        []string{"--stdio"},
		Install: map[string][]string{
			"darwin":  {"npm", "install", "-g", "dockerfile-language-server-nodejs"},
			"linux":   {"npm", "install", "-g", "dockerfile-language-server-nodejs"},
			"windows": {"npm", "install", "-g", "dockerfile-language-server-nodejs"},
		},
		Homepage: "https://github.com/rcjsuen/dockerfile-language-server",
	},
}

// FindPackageForLang returns the first registered package for a language ID.
// It does NOT check whether the binary is installed — that is the resolver's job.
func FindPackageForLang(lang string) (LspPackage, bool) {
	for _, pkg := range Registry {
		for _, l := range pkg.Languages {
			if l == lang {
				return pkg, true
			}
		}
	}
	return LspPackage{}, false
}

// FindPackageByID returns a package by its unique ID.
func FindPackageByID(id string) (LspPackage, bool) {
	for _, pkg := range Registry {
		if pkg.ID == id {
			return pkg, true
		}
	}
	return LspPackage{}, false
}

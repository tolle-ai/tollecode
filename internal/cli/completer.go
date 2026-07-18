package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// expandAtRefs finds @path references in msg, reads their content from workspace,
// and returns the augmented message along with the list of successfully resolved paths.
// Unresolved references are left unchanged in the message.
func expandAtRefs(workspace, msg string) (string, []string) {
	refs := parseAtRefs(msg)
	if len(refs) == 0 {
		return msg, nil
	}

	root, err := filepath.Abs(workspace)
	if err != nil {
		return msg, nil
	}

	var injected []string
	var resolved []string
	for _, ref := range refs {
		clean := strings.Trim(strings.TrimPrefix(ref, "@"), `"`)
		abs := filepath.Join(root, filepath.FromSlash(clean))

		info, err := os.Stat(abs)
		if err != nil {
			continue
		}

		resolved = append(resolved, clean)
		if info.IsDir() {
			listing := atDirListing(abs, root)
			if listing != "" {
				injected = append(injected, fmt.Sprintf("<directory path=%q>\n%s\n</directory>", clean, listing))
			}
		} else {
			data, err := os.ReadFile(abs)
			if err != nil {
				continue
			}
			content := string(data)
			if len(content) > 80_000 {
				content = content[:80_000] + "\n[file truncated — exceeds 80k chars]"
			}
			injected = append(injected, fmt.Sprintf("<file path=%q>\n%s\n</file>", clean, content))
		}
	}

	if len(injected) == 0 {
		return msg, nil
	}
	return strings.Join(injected, "\n\n") + "\n\n" + msg, resolved
}

// atRefRe matches @word, @word/word, @word.ext, etc.
var atRefRe = regexp.MustCompile(`@([\w./\-]+)`)

// atQuotedRefRe matches @"path with spaces" — the form the @ file picker
// pre-fills when the selected path contains a space.
var atQuotedRefRe = regexp.MustCompile(`@"([^"]+)"`)

func parseAtRefs(msg string) []string {
	// Quoted refs first; the bare-word regex can't match a `@"` prefix, so the
	// two passes never overlap.
	matches := append(atQuotedRefRe.FindAllString(msg, -1), atRefRe.FindAllString(msg, -1)...)
	seen := make(map[string]bool)
	var out []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func atDirListing(abs, root string) string {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return ""
	}
	rel, _ := filepath.Rel(root, abs)
	lines := []string{filepath.ToSlash(rel) + "/"}
	for _, e := range entries {
		if cliIgnore[e.Name()] {
			continue
		}
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		lines = append(lines, "  "+e.Name()+suffix)
	}
	return strings.Join(lines, "\n")
}

// cliIgnore mirrors the ignore list used by the sidecar workspace handlers.
var cliIgnore = map[string]bool{
	".git": true, ".agent": true, "__pycache__": true, "node_modules": true,
	".pnpm": true, ".yarn": true, ".venv": true, "venv": true, "env": true,
	".env": true, ".next": true, ".nuxt": true, ".svelte-kit": true,
	".output": true, ".turbo": true, ".angular": true, ".cache": true,
	"dist": true, "build": true, "out": true, "target": true,
	"coverage": true, "storybook-static": true, "tmp": true, "temp": true, "vendor": true,
}

package cli

import (
	"strings"
	"testing"
)

const sampleAssistantMsg = "Connected to Blender. This is the **default startup scene** — untouched.\n" +
	"\n" +
	"**Scene: \"Scene\" — 3 objects**\n" +
	"\n" +
	"| Object | Type | Location (X, Y, Z) |\n" +
	"|--------|------|--------------------|\n" +
	"| **Cube** | Mesh | (0, 0, 0) — at origin |\n" +
	"| **Light** | Light | (4.08, 1.01, 5.9) |\n" +
	"\n" +
	"## What the viewport shows\n" +
	"\n" +
	"- The **Cube** sits at the world origin, currently **selected** (orange outline).\n" +
	"- The **Camera** is off to the lower-left, angled back toward the cube — its lamp isn't distinctly visible in this framing.\n"

func TestRenderAssistantMarkdown(t *testing.T) {
	const width = 88
	out := RenderAssistantMarkdown(sampleAssistantMsg, width)
	if out == "" {
		t.Fatal("expected non-empty render")
	}

	// Leads with the "●" bullet.
	if !strings.HasPrefix(out, mdBullet+" ") {
		t.Errorf("output should start with the bullet marker")
	}
	// Bold is applied somewhere (default startup scene / Cube / …).
	if !strings.Contains(out, ansiBold) {
		t.Errorf("expected bold styling in output")
	}
	// Table became a box-drawing grid.
	for _, glyph := range []string{"┌", "┬", "┐", "├", "┼", "┤", "└", "┴", "┘", "│"} {
		if !strings.Contains(out, glyph) {
			t.Errorf("expected table glyph %q in output", glyph)
		}
	}
	// Unordered list uses the en-dash marker (check with styling removed).
	plain := reANSI.ReplaceAllString(out, "")
	if !strings.Contains(plain, "– The") {
		t.Errorf("expected en-dash list markers, got:\n%s", plain)
	}

	// Every rendered line stays within the terminal width, and continuation
	// lines are indented two columns beneath the bullet.
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := visibleWidth(line); w > width {
			t.Errorf("line exceeds width %d (%d): %q", width, w, line)
		}
		if line != "" && !strings.HasPrefix(line, mdBullet) && !strings.HasPrefix(line, "  ") {
			t.Errorf("non-bullet line not indented: %q", line)
		}
	}
}

func TestRenderAssistantMarkdownEmpty(t *testing.T) {
	if got := RenderAssistantMarkdown("   \n\n ", 80); got != "" {
		t.Errorf("blank input should render empty, got %q", got)
	}
}

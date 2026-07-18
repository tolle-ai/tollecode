package stdio

import "testing"

// TestIsSmartSafe verifies that Smart Authorize auto-approves routine workspace
// work (file writes/edits/plans and read-only shell) while still prompting for
// potentially destructive shell commands — i.e. it is meaningfully looser than
// Ask mode but does not blindly allow everything like Full Authorization.
func TestIsSmartSafe(t *testing.T) {
	cases := []struct {
		command string
		want    bool
	}{
		// File operations are always auto-approved (gated by the workspace sandbox).
		{"write_file: src/app.ts", true},
		{"edit_file: README.md", true},
		{"create_plan: refactor-auth", true},

		// Read-only shell stays auto-approved.
		{"ls -la", true},
		{"git status", true},
		{"cat package.json", true},

		// Risky shell still prompts the user.
		{"rm -rf build", false},
		{"curl https://example.com", false},
		{"git push origin main", false},
		{"npm install left-pad", false},
		{"echo hacked > /etc/hosts", false},
	}

	for _, c := range cases {
		if got := isSmartSafe(c.command); got != c.want {
			t.Errorf("isSmartSafe(%q) = %v, want %v", c.command, got, c.want)
		}
	}
}

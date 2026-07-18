package agent

import "strings"

// diffContextLines is how many unchanged lines surround each change in a hunk,
// matching the conventional unified-diff / Claude Code presentation.
const diffContextLines = 3

// lcsMaxCells caps the LCS DP table size. Divergent regions larger than this
// fall back to a plain delete-then-add block so a huge overwrite can't stall
// the turn on an O(n·m) table.
const lcsMaxCells = 4_000_000

// diffOp is a single line in the computed edit script.
type diffOp struct {
	kind  string // "context", "add", or "del"
	oldNo int    // 1-based line number in the old file (0 when kind == "add")
	newNo int    // 1-based line number in the new file (0 when kind == "del")
	text  string
}

// splitDiffLines splits content into lines, dropping the single empty element
// a trailing newline produces so a file ending in "\n" doesn't render a phantom
// blank final line in the diff.
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// computeLineDiff produces unified-diff hunks between oldContent and newContent.
// Each hunk is a JSON-friendly map ({oldStart, newStart, lines}) so it can travel
// through the event bus to the CLI renderer and the web UI unchanged. It returns
// no hunks when the two versions are line-for-line identical.
func computeLineDiff(oldContent, newContent string) (hunks []map[string]any, additions, removals int) {
	old := splitDiffLines(oldContent)
	neu := splitDiffLines(newContent)

	// Trim the common prefix and suffix so the (expensive) LCS only runs over the
	// region that actually differs. p and s never overlap.
	p := 0
	for p < len(old) && p < len(neu) && old[p] == neu[p] {
		p++
	}
	s := 0
	for s < len(old)-p && s < len(neu)-p && old[len(old)-1-s] == neu[len(neu)-1-s] {
		s++
	}

	oldMid := old[p : len(old)-s]
	newMid := neu[p : len(neu)-s]

	ops := make([]diffOp, 0, len(old)+len(neu))
	for k := 0; k < p; k++ {
		ops = append(ops, diffOp{kind: "context", text: old[k]})
	}
	ops = append(ops, lcsDiff(oldMid, newMid)...)
	for k := len(old) - s; k < len(old); k++ {
		ops = append(ops, diffOp{kind: "context", text: old[k]})
	}

	// Assign line numbers and tally the change counts.
	oldNo, newNo := 1, 1
	for i := range ops {
		switch ops[i].kind {
		case "context":
			ops[i].oldNo, ops[i].newNo = oldNo, newNo
			oldNo++
			newNo++
		case "del":
			ops[i].oldNo = oldNo
			oldNo++
			removals++
		case "add":
			ops[i].newNo = newNo
			newNo++
			additions++
		}
	}
	if additions == 0 && removals == 0 {
		return nil, 0, 0
	}

	// Keep only context lines within diffContextLines of a change; the rest are
	// collapsed into hunk boundaries.
	keep := make([]bool, len(ops))
	for i := range ops {
		if ops[i].kind == "context" {
			continue
		}
		keep[i] = true
		for d := 1; d <= diffContextLines; d++ {
			if i-d >= 0 {
				keep[i-d] = true
			}
			if i+d < len(ops) {
				keep[i+d] = true
			}
		}
	}

	// Group maximal runs of kept ops into hunks.
	for i := 0; i < len(ops); {
		if !keep[i] {
			i++
			continue
		}
		j := i
		lines := []map[string]any{}
		for j < len(ops) && keep[j] {
			lines = append(lines, map[string]any{
				"kind":  ops[j].kind,
				"oldNo": ops[j].oldNo,
				"newNo": ops[j].newNo,
				"text":  ops[j].text,
			})
			j++
		}
		hunks = append(hunks, map[string]any{
			"oldStart": ops[i].oldNo,
			"newStart": ops[i].newNo,
			"lines":    lines,
		})
		i = j
	}
	return hunks, additions, removals
}

// lcsDiff returns the line-level edit script between a and b using a longest
// common subsequence, so unchanged lines are preserved as context.
func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n*m > lcsMaxCells {
		ops := make([]diffOp, 0, n+m)
		for _, l := range a {
			ops = append(ops, diffOp{kind: "del", text: l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{kind: "add", text: l})
		}
		return ops
	}

	// dp[i][j] = LCS length of a[i:] and b[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{kind: "context", text: a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{kind: "del", text: a[i]})
			i++
		default:
			ops = append(ops, diffOp{kind: "add", text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: "del", text: a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: "add", text: b[j]})
	}
	return ops
}

// emitFileDiff computes the line diff between oldContent and newContent and, when
// anything changed, emits a "file_diff" event so the CLI renders a colored diff
// of what the agent changed (the web UI receives the same structured payload).
func emitFileDiff(cfg *Config, rel, oldContent, newContent string, created bool) {
	if cfg == nil || cfg.EmitEvent == nil {
		return
	}
	hunks, additions, removals := computeLineDiff(oldContent, newContent)
	if len(hunks) == 0 {
		return
	}
	cfg.EmitEvent(map[string]any{
		"type":      "file_diff",
		"path":      rel,
		"created":   created,
		"additions": additions,
		"removals":  removals,
		"hunks":     hunks,
	})
}

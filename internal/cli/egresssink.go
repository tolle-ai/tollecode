package cli

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tolle-ai/tollecode/internal/ai"
)

// egresssink.go replaces the ai package's default egress sink for the
// interactive CLI. The default writes straight to os.Stderr, which is wrong
// here on two counts:
//
//  1. It bypasses the loader, so its newline commits the half-drawn status line
//     to scrollback — the status word then appears to repeat once per request.
//     printAboveLoader restores the pause/print/resume discipline.
//  2. It reports on every request. scanRequest re-scans the whole replayed
//     history, so a finding in the system prompt or an early tool result is
//     re-reported every turn for the rest of the session. Below, a finding is
//     announced only when its type/count signature changes — the first hit and
//     any genuine escalation are shown, the identical repeats are not.
//
// Alarm fatigue is the real failure mode: the mode=log posture takes no action,
// so a warning nobody reads is the whole cost of the feature.
var (
	egressSeenMu sync.Mutex
	egressSeen   string // last announced signature; "" until the first finding
)

// installEgressSink routes guardrail findings through the loader-safe printer.
// Called once from Run, before any request can be issued.
func installEgressSink() {
	ai.EgressSink = func(findings []ai.EgressFinding, model string) {
		sig := egressSignature(findings)
		egressSeenMu.Lock()
		dup := sig == egressSeen
		if !dup {
			egressSeen = sig
		}
		egressSeenMu.Unlock()
		if dup {
			return
		}
		printAboveLoader(fmt.Sprintf("  %s⚠ egress: %s sent to %s (mode=%s)%s\n",
			ansiDim, egressSummary(findings), model, ai.CurrentEgressPolicy(), ansiReset))
	}
}

// egressSignature keys the dedup. findings arrives sorted by type from
// scanRequest, so joining is stable across requests.
func egressSignature(findings []ai.EgressFinding) string {
	parts := make([]string, len(findings))
	for i, f := range findings {
		parts[i] = fmt.Sprintf("%s=%d", f.Type, f.Count)
	}
	return strings.Join(parts, ",")
}

func egressSummary(findings []ai.EgressFinding) string {
	parts := make([]string, len(findings))
	for i, f := range findings {
		parts[i] = fmt.Sprintf("%s×%d", f.Type, f.Count)
	}
	return strings.Join(parts, ", ")
}

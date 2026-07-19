package cli

import (
	"strings"
	"testing"

	"github.com/tolle-ai/tollecode/internal/ai"
)

// TestEgressSinkDedupes: scanRequest re-scans the whole replayed history, so an
// unchanged finding arrives on every request of the session. The sink must
// announce the first occurrence and each genuine escalation, and stay silent
// for the identical repeats in between.
func TestEgressSinkDedupes(t *testing.T) {
	prev := ai.EgressSink
	t.Cleanup(func() {
		ai.EgressSink = prev
		egressSeenMu.Lock()
		egressSeen = ""
		egressSeenMu.Unlock()
	})
	egressSeenMu.Lock()
	egressSeen = ""
	egressSeenMu.Unlock()

	installEgressSink()

	two := []ai.EgressFinding{{Type: "email", Count: 2}}
	three := []ai.EgressFinding{{Type: "email", Count: 3}}

	got := captureStdout(t, func() {
		ai.EgressSink(two, "kimi-k2.7-code")   // first hit — announced
		ai.EgressSink(two, "kimi-k2.7-code")   // identical — silent
		ai.EgressSink(two, "kimi-k2.7-code")   // identical — silent
		ai.EgressSink(three, "kimi-k2.7-code") // escalation — announced
		ai.EgressSink(three, "kimi-k2.7-code") // identical — silent
	})

	printed := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(printed) != 2 {
		t.Fatalf("want 2 announcements, got %d: %q", len(printed), printed)
	}
	if !strings.Contains(printed[0], "email×2") {
		t.Errorf("first announcement missing email×2: %q", printed[0])
	}
	if !strings.Contains(printed[1], "email×3") {
		t.Errorf("second announcement missing email×3: %q", printed[1])
	}
	for _, p := range printed {
		if !strings.Contains(p, "kimi-k2.7-code") || !strings.Contains(p, "mode=") {
			t.Errorf("announcement missing model/mode: %q", p)
		}
	}
}

// TestEgressSignatureStable: the dedup key must not depend on findings order
// beyond what scanRequest guarantees, and must distinguish counts.
func TestEgressSignatureStable(t *testing.T) {
	a := []ai.EgressFinding{{Type: "email", Count: 2}, {Type: "jwt", Count: 1}}
	b := []ai.EgressFinding{{Type: "email", Count: 2}, {Type: "jwt", Count: 1}}
	if egressSignature(a) != egressSignature(b) {
		t.Errorf("identical findings produced different signatures")
	}
	c := []ai.EgressFinding{{Type: "email", Count: 3}, {Type: "jwt", Count: 1}}
	if egressSignature(a) == egressSignature(c) {
		t.Errorf("differing counts collapsed to one signature")
	}
}

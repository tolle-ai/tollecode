package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// TestPermGate_SubAgentInheritsAllowAll verifies that once any agent sharing a
// gate is granted "allow all", other agents (e.g. sub-agents) proceed without a
// second prompt.
func TestPermGate_SubAgentInheritsAllowAll(t *testing.T) {
	var prompts int32
	perm := func(_ context.Context, _ string) (bool, bool) {
		atomic.AddInt32(&prompts, 1)
		return true, true // allow, allowAll
	}
	g := &permGate{}
	parent := Config{gate: g, RequestPerm: perm}
	sub := Config{gate: g, RequestPerm: perm, IsSubAgent: true}

	if got := parent.checkPermission(context.Background(), "shell", "ls"); got != permAllowed {
		t.Fatalf("parent: want permAllowed, got %v", got)
	}
	// Sub-agent shares the gate — the parent's "allow all" should carry over.
	if got := sub.checkPermission(context.Background(), "shell", "cat x"); got != permAllowed {
		t.Fatalf("sub: want permAllowed, got %v", got)
	}
	if got := sub.checkPermission(context.Background(), "file", "write_file: y"); got != permAllowed {
		t.Fatalf("sub file: want permAllowed, got %v", got)
	}
	if n := atomic.LoadInt32(&prompts); n != 1 {
		t.Fatalf("expected exactly 1 prompt across parent+sub, got %d", n)
	}
}

// TestPermGate_DenialInherited verifies a denial by one agent auto-denies the
// rest of the shared tree without re-prompting.
func TestPermGate_DenialInherited(t *testing.T) {
	var prompts int32
	perm := func(_ context.Context, _ string) (bool, bool) {
		atomic.AddInt32(&prompts, 1)
		return false, false // deny
	}
	g := &permGate{}
	parent := Config{gate: g, RequestPerm: perm}
	sub := Config{gate: g, RequestPerm: perm}

	if got := parent.checkPermission(context.Background(), "shell", "rm -rf"); got != permDenied {
		t.Fatalf("parent: want permDenied, got %v", got)
	}
	if got := sub.checkPermission(context.Background(), "shell", "rm -rf again"); got != permDenied {
		t.Fatalf("sub: want permDenied, got %v", got)
	}
	if n := atomic.LoadInt32(&prompts); n != 1 {
		t.Fatalf("expected exactly 1 prompt (denial inherited), got %d", n)
	}
}

// TestPermGate_ConcurrentPromptsSerialized verifies that when many agents hit the
// gate at once, only a single prompt is shown — the rest inherit the grant. This
// is the property that stops stacked prompts and the terminal raw-mode race.
func TestPermGate_ConcurrentPromptsSerialized(t *testing.T) {
	var prompts int32
	perm := func(_ context.Context, _ string) (bool, bool) {
		atomic.AddInt32(&prompts, 1)
		return true, true
	}
	g := &permGate{}

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			c := Config{gate: g, RequestPerm: perm}
			if got := c.checkPermission(context.Background(), "shell", "cmd"); got != permAllowed {
				t.Errorf("want permAllowed, got %v", got)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Fatalf("expected exactly 1 prompt across %d concurrent agents, got %d", n, got)
	}
}

// TestPermGate_NoPrompterUnavailable verifies that with no RequestPerm wired up
// the gate reports the operation unavailable rather than silently allowing it.
func TestPermGate_NoPrompterUnavailable(t *testing.T) {
	c := Config{gate: &permGate{}}
	if got := c.checkPermission(context.Background(), "shell", "ls"); got != permUnavailable {
		t.Fatalf("want permUnavailable, got %v", got)
	}
}

// TestPermGate_ShellAutoAllowBypasses verifies ShellAutoAllow short-circuits the
// gate entirely (channels/cron/webhook contexts).
func TestPermGate_ShellAutoAllowBypasses(t *testing.T) {
	c := Config{ShellAutoAllow: true} // no gate, no prompter needed
	if got := c.checkPermission(context.Background(), "shell", "ls"); got != permAllowed {
		t.Fatalf("want permAllowed with ShellAutoAllow, got %v", got)
	}
}

// TestPermGate_SingleAllowDoesNotPersist verifies a plain "allow" (not "allow
// all") authorises just that one operation — the next check prompts again.
func TestPermGate_SingleAllowDoesNotPersist(t *testing.T) {
	var prompts int32
	perm := func(_ context.Context, _ string) (bool, bool) {
		atomic.AddInt32(&prompts, 1)
		return true, false // allow once, not all
	}
	c := Config{gate: &permGate{}, RequestPerm: perm}
	_ = c.checkPermission(context.Background(), "shell", "a")
	_ = c.checkPermission(context.Background(), "shell", "b")
	if n := atomic.LoadInt32(&prompts); n != 2 {
		t.Fatalf("single allow should prompt each time, got %d prompts", n)
	}
}

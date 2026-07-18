package workflow

import (
	"context"
	"sync"
	"testing"
	"time"
)

// collector captures node states and events from a run for assertions.
type collector struct {
	mu     sync.Mutex
	states map[string]NodeRunState
	events []map[string]any
}

func newCollector() *collector {
	return &collector{states: map[string]NodeRunState{}}
}

func (c *collector) deps() Deps {
	return Deps{
		Emit: func(ev map[string]any) {
			c.mu.Lock()
			c.events = append(c.events, ev)
			c.mu.Unlock()
		},
		PersistNode: func(ns NodeRunState) {
			c.mu.Lock()
			c.states[ns.NodeID] = ns
			c.mu.Unlock()
		},
		MaxConcurrency: 4,
	}
}

func (c *collector) status(nodeID string) NodeStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[nodeID].Status
}

func run(t *testing.T, wf *Workflow, c *collector) *WorkflowRun {
	t.Helper()
	run := &WorkflowRun{ID: "run1", WorkflowID: wf.ID, Status: RunPending, TriggerType: "manual"}
	Run(context.Background(), wf, run, c.deps(), RunOptions{})
	return run
}

func node(id, typ string, cfg WorkflowNodeConfig) WorkflowNode {
	return WorkflowNode{ID: id, Type: typ, Label: id, Config: cfg}
}

func edge(src, srcPort, tgt string) WorkflowEdge {
	return WorkflowEdge{ID: src + "->" + tgt, SourceNodeID: src, SourcePort: srcPort, TargetNodeID: tgt}
}

// Trigger → code(returns {n:5}) → condition(n>3) fires true; false branch skipped.
func TestConditionBranching(t *testing.T) {
	wf := &Workflow{
		ID: "wf1",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("c", TypeCodeTransform, WorkflowNodeConfig{Code: "{n: 5}", Language: "expr"}),
			node("if", TypeCondition, WorkflowNodeConfig{ConditionExpression: "json.n > 3"}),
			node("yes", TypeSetVariable, WorkflowNodeConfig{Variables: []WorkflowVar{{Name: "hit", Value: "true"}}}),
			node("no", TypeSetVariable, WorkflowNodeConfig{Variables: []WorkflowVar{{Name: "hit", Value: "false"}}}),
		},
		Edges: []WorkflowEdge{
			edge("t", "", "c"),
			edge("c", "", "if"),
			{ID: "if-true", SourceNodeID: "if", SourcePort: "true", TargetNodeID: "yes"},
			{ID: "if-false", SourceNodeID: "if", SourcePort: "false", TargetNodeID: "no"},
		},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunDone {
		t.Fatalf("run status = %s, want done (err=%s)", r.Status, r.Error)
	}
	if got := c.status("yes"); got != NodeDone {
		t.Errorf("true branch status = %s, want done", got)
	}
	if got := c.status("no"); got != NodeSkipped {
		t.Errorf("false branch status = %s, want skipped", got)
	}
}

// Two source nodes → merge; merge waits for both and outputs a combined object.
func TestMergeJoin(t *testing.T) {
	wf := &Workflow{
		ID: "wf2",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("a", TypeCodeTransform, WorkflowNodeConfig{Code: `{v: "A"}`}),
			node("b", TypeCodeTransform, WorkflowNodeConfig{Code: `{v: "B"}`}),
			node("m", TypeMerge, WorkflowNodeConfig{}),
		},
		Edges: []WorkflowEdge{
			edge("t", "", "a"),
			edge("t", "", "b"),
			{ID: "a-m", SourceNodeID: "a", TargetNodeID: "m", TargetPort: "input_0"},
			{ID: "b-m", SourceNodeID: "b", TargetNodeID: "m", TargetPort: "input_1"},
		},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunDone {
		t.Fatalf("run status = %s, want done (err=%s)", r.Status, r.Error)
	}
	out, ok := c.states["m"].Output.(map[string]any)
	if !ok {
		t.Fatalf("merge output type = %T, want map", c.states["m"].Output)
	}
	if len(out) != 2 {
		t.Errorf("merge output has %d slots, want 2: %v", len(out), out)
	}
}

// The web UI saves every default-output edge with sourcePort "output" (not the
// engine's internal ""). A linear chain drawn in the UI must still run end to
// end: each node fires "" and its outgoing "output" edge has to be matched as
// live, or the whole chain past the trigger is skipped. Mirrors the reported
// Start → Fetch → Refine → Email graph.
func TestDefaultOutputPortEdges(t *testing.T) {
	wf := &Workflow{
		ID: "wfports",
		Nodes: []WorkflowNode{
			node("start", TypeTriggerManual, WorkflowNodeConfig{}),
			node("fetch", TypeCodeTransform, WorkflowNodeConfig{Code: "{n: 5}"}),
			node("refine", TypeCodeTransform, WorkflowNodeConfig{Language: "js", Code: "return { doubled: $json.n * 2 };"}),
			node("email", TypeSetVariable, WorkflowNodeConfig{Variables: []WorkflowVar{{Name: "sent", Value: "true"}}}),
		},
		// Every edge uses the UI's "output" source port.
		Edges: []WorkflowEdge{
			edge("start", "output", "fetch"),
			edge("fetch", "output", "refine"),
			edge("refine", "output", "email"),
		},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunDone {
		t.Fatalf("run status = %s, want done (err=%s)", r.Status, r.Error)
	}
	for _, id := range []string{"start", "fetch", "refine", "email"} {
		if got := c.status(id); got != NodeDone {
			t.Errorf("node %q status = %s, want done (downstream should not be skipped)", id, got)
		}
	}
	// Data must flow across the "output" edges, not just control.
	out, ok := c.states["refine"].Output.(map[string]any)
	if !ok {
		t.Fatalf("refine output type = %T, want map", c.states["refine"].Output)
	}
	var num float64
	switch v := out["doubled"].(type) {
	case float64:
		num = v
	case int64:
		num = float64(v)
	case int:
		num = float64(v)
	}
	if num != 10 {
		t.Errorf("data did not propagate across output edges: doubled = %v, want 10", out["doubled"])
	}
}

// onError=continue lets the run finish and downstream execute despite a node error.
func TestOnErrorContinue(t *testing.T) {
	wf := &Workflow{
		ID: "wf3",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			// http to an empty URL errors; onError=continue keeps going.
			node("h", TypeHTTPRequest, WorkflowNodeConfig{URL: "", OnError: "continue"}),
			node("after", TypeSetVariable, WorkflowNodeConfig{Variables: []WorkflowVar{{Name: "x", Value: "1"}}}),
		},
		Edges: []WorkflowEdge{edge("t", "", "h"), edge("h", "", "after")},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunDone {
		t.Fatalf("run status = %s, want done (continue should not fail run)", r.Status)
	}
	if got := c.status("after"); got != NodeDone {
		t.Errorf("downstream status = %s, want done", got)
	}
	if got := c.status("h"); got != NodeError {
		t.Errorf("errored node status = %s, want error", got)
	}
}

// code_transform with language:"js" runs real JavaScript via goja.
func TestCodeNodeJS(t *testing.T) {
	wf := &Workflow{
		ID: "wfjs",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("seed", TypeCodeTransform, WorkflowNodeConfig{Code: "{n: 3}"}),
			node("js", TypeCodeTransform, WorkflowNodeConfig{Language: "js", Code: "return { doubled: $json.n * 2 };"}),
		},
		Edges: []WorkflowEdge{edge("t", "", "seed"), edge("seed", "", "js")},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunDone {
		t.Fatalf("run status = %s, want done (err=%s)", r.Status, r.Error)
	}
	out, ok := c.states["js"].Output.(map[string]any)
	if !ok {
		t.Fatalf("js output type = %T, want map", c.states["js"].Output)
	}
	var num float64
	switch v := out["doubled"].(type) {
	case float64:
		num = v
	case int64:
		num = float64(v)
	case int:
		num = float64(v)
	}
	if num != 6 {
		t.Errorf("js doubled = %v (%T), want 6", out["doubled"], out["doubled"])
	}
}

// A merge inside a loop body joins both branches (join-aware body scheduler).
func TestLoopBodyMerge(t *testing.T) {
	wf := &Workflow{
		ID: "wfloop",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("src", TypeCodeTransform, WorkflowNodeConfig{Code: "[1, 2]"}),
			node("loop", TypeLoop, WorkflowNodeConfig{IterateOver: "{{ $json }}", LoopVar: "item"}),
			node("a", TypeCodeTransform, WorkflowNodeConfig{Code: `{v: "A"}`}),
			node("b", TypeCodeTransform, WorkflowNodeConfig{Code: `{v: "B"}`}),
			node("m", TypeMerge, WorkflowNodeConfig{}),
			node("after", TypeSetVariable, WorkflowNodeConfig{Variables: []WorkflowVar{{Name: "x", Value: "1"}}}),
		},
		Edges: []WorkflowEdge{
			edge("t", "", "src"),
			edge("src", "", "loop"),
			{ID: "l-a", SourceNodeID: "loop", SourcePort: "each", TargetNodeID: "a"},
			{ID: "l-b", SourceNodeID: "loop", SourcePort: "each", TargetNodeID: "b"},
			{ID: "a-m", SourceNodeID: "a", TargetNodeID: "m", TargetPort: "input_0"},
			{ID: "b-m", SourceNodeID: "b", TargetNodeID: "m", TargetPort: "input_1"},
			{ID: "loop-done", SourceNodeID: "loop", SourcePort: "done", TargetNodeID: "after"},
		},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunDone {
		t.Fatalf("run status = %s, want done (err=%s)", r.Status, r.Error)
	}
	if got := c.status("m"); got != NodeDone {
		t.Errorf("merge-in-loop status = %s, want done", got)
	}
	out, ok := c.states["m"].Output.(map[string]any)
	if !ok || len(out) != 2 {
		t.Errorf("merge output = %v, want 2 slots", c.states["m"].Output)
	}
	if got := c.status("after"); got != NodeDone {
		t.Errorf("loop 'done' branch status = %s, want done", got)
	}
}

// A long wait suspends the run durably; resuming continues downstream.
func TestDurableWaitSuspendResume(t *testing.T) {
	wf := &Workflow{
		ID: "wfwait",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("seed", TypeCodeTransform, WorkflowNodeConfig{Code: "{ready: true}"}),
			node("w", TypeWait, WorkflowNodeConfig{DelayValue: 3600, DelayUnit: "seconds"}),
			node("after", TypeSetVariable, WorkflowNodeConfig{Variables: []WorkflowVar{{Name: "done", Value: "yes"}}}),
		},
		Edges: []WorkflowEdge{edge("t", "", "seed"), edge("seed", "", "w"), edge("w", "", "after")},
	}

	// First run: capture the suspend and verify the run parks.
	c := newCollector()
	var suspendedNode string
	var snap Snapshot
	deps := c.deps()
	deps.Suspend = func(nodeID string, _ time.Time, s Snapshot) { suspendedNode = nodeID; snap = s }
	run1 := &WorkflowRun{ID: "run-w", WorkflowID: wf.ID, Status: RunPending, TriggerType: "manual"}
	Run(context.Background(), wf, run1, deps, RunOptions{})

	if run1.Status != RunWaiting {
		t.Fatalf("run status = %s, want waiting", run1.Status)
	}
	if suspendedNode != "w" {
		t.Fatalf("suspended node = %q, want w", suspendedNode)
	}
	if c.status("after") == NodeDone {
		t.Fatalf("downstream ran before resume")
	}

	// Resume: seed the snapshot, start from the wait node — it passes through.
	c2 := newCollector()
	run2 := &WorkflowRun{ID: "run-w", WorkflowID: wf.ID, Status: RunRunning, TriggerType: "resume"}
	Run(context.Background(), wf, run2, c2.deps(), RunOptions{
		StartNodeID: "w", SeedOutputs: snap.Outputs, SeedVars: snap.Vars, Resume: true,
	})
	if run2.Status != RunDone {
		t.Fatalf("resumed run status = %s, want done", run2.Status)
	}
	if got := c2.status("after"); got != NodeDone {
		t.Errorf("downstream after resume = %s, want done", got)
	}
}

// A stop (default) error fails the run.
func TestOnErrorStopFailsRun(t *testing.T) {
	wf := &Workflow{
		ID: "wf4",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("h", TypeHTTPRequest, WorkflowNodeConfig{URL: ""}), // default onError=stop
		},
		Edges: []WorkflowEdge{edge("t", "", "h")},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunError {
		t.Fatalf("run status = %s, want error", r.Status)
	}
}

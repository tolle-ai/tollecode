package workflow

import (
	"context"
	"sync"
	"time"
)

// RunOptions controls how a run is seeded (manual, trigger, or partial execution).
type RunOptions struct {
	// TriggerNodeID, when set, seeds only that trigger node (the trigger that
	// started this run). Empty seeds all trigger/indegree-0 nodes.
	TriggerNodeID string
	// StartNodeID begins a partial ("run from here") execution at this node.
	StartNodeID string
	// SingleNode runs only StartNodeID and does not follow its output edges.
	SingleNode bool
	// SeedOutputs pre-populates upstream node outputs (partial execution) so the
	// start node's input can be assembled without re-running upstream nodes.
	SeedOutputs map[string]any
	// SeedVars pre-populates workflow variables (used when resuming a suspended run).
	SeedVars map[string]any
	// Resume marks this as the continuation of a suspended run; the wait node at
	// StartNodeID passes through instead of waiting again.
	Resume bool
	// Test honors the workflow's pinned data: a node with pinned output is not
	// executed; its pinned value is used instead.
	Test bool
}

// nodeOutput captures a node's produced value and which output ports fired.
type nodeOutput struct {
	value any
	fired map[string]bool
}

type runContext struct {
	wf   *Workflow
	run  *WorkflowRun
	deps Deps
	ctx  context.Context
	opts RunOptions

	nodeByID   map[string]WorkflowNode
	labelByID  map[string]string
	outEdges   map[string]map[string][]WorkflowEdge // nodeID -> sourcePort -> edges
	loopBody   map[string]bool                      // union of all loop-body nodes (skip main scheduler)
	loopBodies map[string]map[string]bool           // loopID -> its body node set

	mu           sync.Mutex
	outputs      map[string]nodeOutput
	states       map[string]*NodeRunState
	vars         map[string]any
	remaining    map[string]int         // inbound data edges not yet delivered (join gate)
	liveArrivals map[string]int         // inbound edges that delivered a live (non-skipped) value
	slotIn       map[string]map[string]any // nodeID -> targetPort -> delivered value
	scheduled    map[string]bool        // guards each node from running/skipping twice
	failed       bool
	suspended    bool
	firstErr     error

	sem chan struct{}
	wg  sync.WaitGroup
}

// Run executes wf, mutating run (status, timing) and emitting events via deps.
// It blocks until the run finishes or ctx is cancelled, returning the final status.
func Run(ctx context.Context, wf *Workflow, run *WorkflowRun, deps Deps, opts RunOptions) RunStatus {
	maxc := deps.MaxConcurrency
	if maxc <= 0 {
		maxc = 8
	}
	rc := &runContext{
		wf:           wf,
		run:          run,
		deps:         deps,
		ctx:          ctx,
		opts:         opts,
		nodeByID:     make(map[string]WorkflowNode, len(wf.Nodes)),
		labelByID:    make(map[string]string, len(wf.Nodes)),
		outEdges:     make(map[string]map[string][]WorkflowEdge),
		loopBody:     make(map[string]bool),
		loopBodies:   make(map[string]map[string]bool),
		outputs:      make(map[string]nodeOutput),
		states:       make(map[string]*NodeRunState),
		vars:         make(map[string]any),
		remaining:    make(map[string]int),
		liveArrivals: make(map[string]int),
		slotIn:       make(map[string]map[string]any),
		scheduled:    make(map[string]bool),
		sem:          make(chan struct{}, maxc),
	}
	rc.index()
	return rc.run_()
}

// index builds edge maps, join-gate counts, seeds variables, and loop body sets.
func (rc *runContext) index() {
	for _, n := range rc.wf.Nodes {
		rc.nodeByID[n.ID] = n
		rc.labelByID[n.ID] = n.Label
	}
	for _, v := range rc.wf.Variables {
		rc.vars[v.Name] = v.Value
	}
	// Loop bodies: nodes reachable from a loop's `each` port. Excluded from the
	// main scheduler because the loop node drives them (with join support) per item.
	for _, n := range rc.wf.Nodes {
		if n.Type == TypeLoop {
			body := rc.computeLoopBody(n.ID)
			rc.loopBodies[n.ID] = body
			for id := range body {
				rc.loopBody[id] = true
			}
		}
	}
	// Out-edges and inbound data-edge counts (join gate), skipping sub-config
	// ports and loop-body nodes. The source port is canonicalized so an edge the
	// UI drew from a node's default output ("output") matches the "" port that
	// standard nodes fire — otherwise every such edge would look non-live and its
	// target would be skipped (see canonicalPort).
	for _, e := range rc.wf.Edges {
		if rc.outEdges[e.SourceNodeID] == nil {
			rc.outEdges[e.SourceNodeID] = make(map[string][]WorkflowEdge)
		}
		port := canonicalPort(e.SourcePort)
		rc.outEdges[e.SourceNodeID][port] = append(rc.outEdges[e.SourceNodeID][port], e)
		if subPorts[e.TargetPort] || rc.loopBody[e.TargetNodeID] {
			continue
		}
		rc.remaining[e.TargetNodeID]++
	}
}

// computeLoopBody returns the set of nodes forward-reachable from a loop's `each`
// port (the loop body), stopping at the loop node itself.
func (rc *runContext) computeLoopBody(loopID string) map[string]bool {
	body := map[string]bool{}
	stack := []string{}
	for _, e := range rc.wf.Edges {
		if e.SourceNodeID == loopID && e.SourcePort == "each" {
			stack = append(stack, e.TargetNodeID)
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if body[id] || id == loopID {
			continue
		}
		body[id] = true
		for _, e := range rc.wf.Edges {
			if e.SourceNodeID == id {
				stack = append(stack, e.TargetNodeID)
			}
		}
	}
	return body
}

func (rc *runContext) run_() RunStatus {
	rc.emit(map[string]any{"type": "run_started", "runId": rc.run.ID,
		"workflowId": rc.run.WorkflowID, "triggerType": rc.run.TriggerType})

	// Seed the initial frontier.
	for _, id := range rc.seedNodes() {
		if n, ok := rc.nodeByID[id]; ok {
			rc.schedule(n, rc.seedInput(n))
		}
	}
	rc.wg.Wait()

	status := RunDone
	switch {
	case rc.ctx.Err() != nil:
		status = RunCanceled
		rc.emit(map[string]any{"type": "cancelled", "runId": rc.run.ID})
	case rc.failed:
		status = RunError
		msg := ""
		if rc.firstErr != nil {
			msg = rc.firstErr.Error()
		}
		rc.run.Error = msg
		rc.emit(map[string]any{"type": "run_failed", "runId": rc.run.ID, "status": "error", "error": msg})
	case rc.isSuspended():
		status = RunWaiting
		rc.emit(map[string]any{"type": "run_waiting", "runId": rc.run.ID, "status": "waiting"})
	default:
		rc.emit(map[string]any{"type": "run_done", "runId": rc.run.ID, "status": "done"})
	}
	rc.run.Status = status
	// A suspended (waiting) run is not complete; leave CompletedAt unset.
	if status != RunWaiting {
		now := time.Now().UTC()
		rc.run.CompletedAt = &now
	}
	return status
}

// seedNodes returns the node IDs that start the run.
func (rc *runContext) seedNodes() []string {
	if rc.opts.StartNodeID != "" {
		// Partial execution / resume: pre-load seeded upstream outputs + vars.
		for id, out := range rc.opts.SeedOutputs {
			rc.outputs[id] = nodeOutput{value: out, fired: map[string]bool{"": true}}
		}
		for k, v := range rc.opts.SeedVars {
			rc.vars[k] = v
		}
		return []string{rc.opts.StartNodeID}
	}
	var seeds []string
	for _, n := range rc.wf.Nodes {
		if rc.loopBody[n.ID] {
			continue
		}
		if rc.opts.TriggerNodeID != "" {
			if n.ID == rc.opts.TriggerNodeID {
				seeds = append(seeds, n.ID)
			}
			continue
		}
		if IsTriggerType(n.Type) || rc.remaining[n.ID] == 0 {
			seeds = append(seeds, n.ID)
		}
	}
	return seeds
}

func (rc *runContext) seedInput(n WorkflowNode) any {
	if IsTriggerType(n.Type) {
		return map[string]any{"trigger": map[string]any{"payload": rc.run.TriggerPayload}}
	}
	return rc.run.TriggerPayload
}

// schedule runs node concurrently once its gate is satisfied.
func (rc *runContext) schedule(node WorkflowNode, input any) {
	rc.mu.Lock()
	if rc.scheduled[node.ID] {
		rc.mu.Unlock()
		return
	}
	rc.scheduled[node.ID] = true
	rc.mu.Unlock()

	rc.wg.Add(1)
	go func() {
		defer rc.wg.Done()
		select {
		case rc.sem <- struct{}{}:
		case <-rc.ctx.Done():
			return
		}
		defer func() { <-rc.sem }()
		if rc.ctx.Err() != nil || rc.isFailed() {
			return
		}
		rc.runNode(node, input)
	}()
}

func (rc *runContext) runNode(node WorkflowNode, input any) {
	rc.startNode(node.ID, input)

	// Disabled nodes pass their input through unchanged.
	if node.Disabled {
		rc.finishNode(node, NodeResult{Output: input, FiredPorts: defaultPort})
		return
	}
	// Pinned data (test runs) short-circuits execution.
	if rc.opts.Test {
		if pinned, ok := rc.wf.PinnedData[node.ID]; ok {
			rc.finishNode(node, NodeResult{Output: pinned, FiredPorts: defaultPort})
			return
		}
	}

	res, err, fatal := rc.execNode(node, input)
	if err != nil {
		rc.errNode(node.ID, err, fatal, res.AgentSessionID)
		if fatal {
			rc.fail(err)
			return
		}
		// non-fatal (onError=continue): fall through and propagate res.
	} else if res.Suspend {
		// Long wait: the run is suspended and will be resumed durably. Do not
		// propagate downstream; mark the run as waiting on completion.
		rc.waitNode(node.ID)
		rc.mu.Lock()
		rc.suspended = true
		rc.mu.Unlock()
		return
	} else if res.AgentSessionID != "" {
		rc.setAgentSession(node.ID, res.AgentSessionID)
	}
	rc.finishNode(node, res)
}

// execNode dispatches the node body applying onError (stop/continue/retry).
// Returns (result, err, fatal). For onError=continue, err is non-nil but fatal
// is false so downstream still runs.
func (rc *runContext) execNode(node WorkflowNode, input any) (NodeResult, error, bool) {
	cfg := node.Config
	attempts := 0
	for {
		attempts++
		rc.setAttempts(node.ID, attempts)
		res, err := dispatch(rc.ctx, rc, node, input)
		if err == nil {
			return res, nil, false
		}
		if rc.ctx.Err() != nil {
			return NodeResult{}, rc.ctx.Err(), true
		}
		switch cfg.OnError {
		case "retry":
			if attempts <= cfg.RetryCount {
				delay := time.Duration(cfg.RetryDelayMs) * time.Millisecond
				select {
				case <-time.After(delay):
				case <-rc.ctx.Done():
					return NodeResult{}, rc.ctx.Err(), true
				}
				continue
			}
			return NodeResult{AgentSessionID: res.AgentSessionID}, err, true
		case "continue":
			return NodeResult{
				Output:         map[string]any{"error": err.Error()},
				FiredPorts:     defaultPort,
				AgentSessionID: res.AgentSessionID,
			}, err, false
		default: // stop
			return NodeResult{AgentSessionID: res.AgentSessionID}, err, true
		}
	}
}

// finishNode records output, marks the node done, and propagates to downstream
// nodes: edges on a fired port deliver a live value; all others propagate a skip.
func (rc *runContext) finishNode(node WorkflowNode, res NodeResult) {
	fired := map[string]bool{}
	for _, p := range res.FiredPorts {
		fired[canonicalPort(p)] = true
	}
	if len(fired) == 0 {
		fired[""] = true
	}

	// Bind outputVar so downstream {{$vars.x}} sees this node's output.
	if node.Config.OutputVar != "" {
		rc.setVar(node.Config.OutputVar, res.Output)
	}

	rc.mu.Lock()
	rc.outputs[node.ID] = nodeOutput{value: res.Output, fired: fired}
	rc.mu.Unlock()
	rc.doneNode(node.ID, res.Output)

	if rc.opts.SingleNode {
		return
	}
	// Propagate along every outgoing edge.
	for port, edges := range rc.outEdges[node.ID] {
		live := fired[port]
		for _, e := range edges {
			if rc.loopBody[e.TargetNodeID] {
				continue // loop bodies are driven by the loop node itself
			}
			rc.deliver(e, res.Output, live)
		}
	}
}

// deliver hands a value (or a skip) to a target's input slot and, when the
// target's gate is fully satisfied, schedules it (or skips it if nothing live).
func (rc *runContext) deliver(edge WorkflowEdge, value any, live bool) {
	tgt := edge.TargetNodeID
	rc.mu.Lock()
	if live {
		if rc.slotIn[tgt] == nil {
			rc.slotIn[tgt] = map[string]any{}
		}
		port := edge.TargetPort
		if port == "" {
			port = "input"
		}
		rc.slotIn[tgt][port] = value
		rc.liveArrivals[tgt]++
	}
	rc.remaining[tgt]--
	ready := rc.remaining[tgt] <= 0
	hasLive := rc.liveArrivals[tgt] > 0
	rc.mu.Unlock()

	if !ready {
		return
	}
	if !hasLive {
		rc.markSkipped(tgt)
		return
	}
	node, ok := rc.nodeByID[tgt]
	if !ok {
		return
	}
	rc.schedule(node, rc.assembleInput(node))
}

// assembleInput builds a node's input from delivered upstream values.
func (rc *runContext) assembleInput(node WorkflowNode) any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	slots := rc.slotIn[node.ID]
	if len(slots) == 0 {
		return nil
	}
	if node.Type == TypeMerge || len(slots) > 1 {
		out := make(map[string]any, len(slots))
		for k, v := range slots {
			out[k] = v
		}
		return out
	}
	for _, v := range slots {
		return v
	}
	return nil
}

// markSkipped records a node as skipped and propagates skip along its edges so
// downstream join gates still resolve.
func (rc *runContext) markSkipped(nodeID string) {
	rc.mu.Lock()
	if rc.scheduled[nodeID] {
		rc.mu.Unlock()
		return
	}
	rc.scheduled[nodeID] = true
	rc.mu.Unlock()

	rc.skipNode(nodeID)
	for _, edges := range rc.outEdges[nodeID] {
		for _, e := range edges {
			if rc.loopBody[e.TargetNodeID] {
				continue
			}
			rc.deliver(e, nil, false)
		}
	}
}

// runLoopBody executes a loop's body subgraph for one item using a scoped,
// join-aware scheduler: gate counts are computed over body-internal edges, so
// merges and converging branches inside a loop body resolve correctly. Execution
// is sequential within an iteration (parallelism is bounded to the outer run).
func (rc *runContext) runLoopBody(body map[string]bool, loopID string, item any) any {
	remaining := map[string]int{}
	live := map[string]int{}
	slotIn := map[string]map[string]any{}
	done := map[string]bool{}
	for _, e := range rc.wf.Edges {
		if body[e.TargetNodeID] && body[e.SourceNodeID] && !subPorts[e.TargetPort] {
			remaining[e.TargetNodeID]++
		}
	}

	var ready []string
	deliver := func(e WorkflowEdge, val any, liveEdge bool) {
		t := e.TargetNodeID
		if !body[t] {
			return
		}
		if liveEdge {
			if slotIn[t] == nil {
				slotIn[t] = map[string]any{}
			}
			port := e.TargetPort
			if port == "" {
				port = "input"
			}
			slotIn[t][port] = val
			live[t]++
		}
		remaining[t]--
		if remaining[t] <= 0 && !done[t] {
			ready = append(ready, t)
		}
	}

	// Seed entry nodes (targets of the loop's `each` port) with the item.
	for _, e := range rc.outEdges[loopID]["each"] {
		if !body[e.TargetNodeID] {
			continue
		}
		if slotIn[e.TargetNodeID] == nil {
			slotIn[e.TargetNodeID] = map[string]any{}
		}
		slotIn[e.TargetNodeID]["input"] = item
		live[e.TargetNodeID]++
		if remaining[e.TargetNodeID] <= 0 {
			ready = append(ready, e.TargetNodeID)
		}
	}

	var last any = item
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		if done[id] || rc.ctx.Err() != nil {
			continue
		}
		done[id] = true

		// A node whose gate resolved with no live inbound edge is skipped.
		if live[id] == 0 {
			rc.skipNode(id)
			for _, edges := range rc.outEdges[id] {
				for _, e := range edges {
					deliver(e, nil, false)
				}
			}
			continue
		}

		node := rc.nodeByID[id]
		input := assembleSlots(node, slotIn[id])
		rc.startNode(id, input)
		res, err, fatal := rc.execNode(node, input)
		if err != nil {
			rc.errNode(id, err, fatal, res.AgentSessionID)
			if fatal {
				rc.fail(err)
				return last
			}
		}
		fired := map[string]bool{}
		for _, p := range res.FiredPorts {
			fired[canonicalPort(p)] = true
		}
		if len(fired) == 0 {
			fired[""] = true
		}
		rc.mu.Lock()
		rc.outputs[id] = nodeOutput{value: res.Output, fired: fired}
		rc.mu.Unlock()
		rc.doneNode(id, res.Output)
		last = res.Output

		for port, edges := range rc.outEdges[id] {
			liveEdge := fired[port]
			for _, e := range edges {
				deliver(e, res.Output, liveEdge)
			}
		}
	}
	return last
}

// assembleSlots builds a node input from delivered slot values (merge → object).
func assembleSlots(node WorkflowNode, slots map[string]any) any {
	if len(slots) == 0 {
		return nil
	}
	if node.Type == TypeMerge || len(slots) > 1 {
		out := make(map[string]any, len(slots))
		for k, v := range slots {
			out[k] = v
		}
		return out
	}
	for _, v := range slots {
		return v
	}
	return nil
}

// runLoop resolves the iteration array and drives the `each` subgraph once per
// item, then fires `done` with the collected per-item terminal outputs.
func runLoop(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	arr := toSlice(interpolate(node.Config.IterateOver, rc.env(in)))
	loopVar := node.Config.LoopVar
	if loopVar == "" {
		loopVar = "item"
	}
	body := rc.loopBodies[node.ID]
	results := make([]any, 0, len(arr))
	for _, item := range arr {
		if ctx.Err() != nil {
			return NodeResult{}, ctx.Err()
		}
		rc.setVar(loopVar, item)
		results = append(results, rc.runLoopBody(body, node.ID, item))
	}
	return NodeResult{Output: results, FiredPorts: []string{"done"}}, nil
}

// ── Environment / variables ───────────────────────────────────────────────────

// env builds the expression environment for evaluating a node's config.
func (rc *runContext) env(input any) evalEnv {
	rc.mu.Lock()
	nodes := make(map[string]any, len(rc.outputs)*2)
	for id, out := range rc.outputs {
		entry := map[string]any{"json": out.value}
		nodes[id] = entry
		if label := rc.labelByID[id]; label != "" {
			nodes[label] = entry
		}
	}
	vars := make(map[string]any, len(rc.vars))
	for k, v := range rc.vars {
		vars[k] = v
	}
	rc.mu.Unlock()

	var trigger map[string]any
	if rc.run != nil {
		trigger = map[string]any{"payload": rc.run.TriggerPayload}
	}
	return evalEnv{input: input, vars: vars, nodes: nodes, trigger: trigger}
}

func (rc *runContext) setVar(name string, value any) {
	rc.mu.Lock()
	rc.vars[name] = value
	rc.mu.Unlock()
}

// ── Failure / status helpers ──────────────────────────────────────────────────

func (rc *runContext) fail(err error) {
	rc.mu.Lock()
	if !rc.failed {
		rc.failed = true
		rc.firstErr = err
	}
	rc.mu.Unlock()
}

func (rc *runContext) isFailed() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.failed
}

func (rc *runContext) isSuspended() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.suspended
}

// snapshot captures accumulated node outputs and variables for durable resume.
func (rc *runContext) snapshot() Snapshot {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	outputs := make(map[string]any, len(rc.outputs))
	for id, o := range rc.outputs {
		outputs[id] = o.value
	}
	vars := make(map[string]any, len(rc.vars))
	for k, v := range rc.vars {
		vars[k] = v
	}
	return Snapshot{Outputs: outputs, Vars: vars}
}

// ── Node state transitions (emit + persist) ───────────────────────────────────

func (rc *runContext) stateOf(nodeID string) *NodeRunState {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	s := rc.states[nodeID]
	if s == nil {
		s = &NodeRunState{RunID: rc.run.ID, NodeID: nodeID, Status: NodePending}
		rc.states[nodeID] = s
	}
	return s
}

func (rc *runContext) startNode(nodeID string, input any) {
	s := rc.stateOf(nodeID)
	now := time.Now().UTC()
	rc.mu.Lock()
	s.Status = NodeRunning
	s.Input = input
	s.StartedAt = &now
	snap := *s
	rc.mu.Unlock()
	rc.deps.PersistNode(snap)
	rc.emit(map[string]any{"type": "node_started", "runId": rc.run.ID, "nodeId": nodeID, "input": input})
}

func (rc *runContext) doneNode(nodeID string, output any) {
	s := rc.stateOf(nodeID)
	now := time.Now().UTC()
	rc.mu.Lock()
	if s.Status != NodeError {
		s.Status = NodeDone
	}
	s.Output = output
	s.CompletedAt = &now
	snap := *s
	rc.mu.Unlock()
	rc.deps.PersistNode(snap)
	rc.emit(map[string]any{"type": "node_done", "runId": rc.run.ID, "nodeId": nodeID, "output": output})
}

func (rc *runContext) errNode(nodeID string, err error, fatal bool, agentSessionID string) {
	s := rc.stateOf(nodeID)
	now := time.Now().UTC()
	rc.mu.Lock()
	s.Status = NodeError
	s.Error = err.Error()
	if agentSessionID != "" {
		s.AgentSessionID = agentSessionID
	}
	s.CompletedAt = &now
	snap := *s
	rc.mu.Unlock()
	rc.deps.PersistNode(snap)
	rc.emit(map[string]any{"type": "node_error", "runId": rc.run.ID, "nodeId": nodeID,
		"error": err.Error(), "fatal": fatal})
}

func (rc *runContext) waitNode(nodeID string) {
	s := rc.stateOf(nodeID)
	rc.mu.Lock()
	s.Status = NodeWaiting
	snap := *s
	rc.mu.Unlock()
	rc.deps.PersistNode(snap)
	rc.emit(map[string]any{"type": "node_waiting", "runId": rc.run.ID, "nodeId": nodeID})
}

func (rc *runContext) skipNode(nodeID string) {
	s := rc.stateOf(nodeID)
	rc.mu.Lock()
	s.Status = NodeSkipped
	snap := *s
	rc.mu.Unlock()
	rc.deps.PersistNode(snap)
	rc.emit(map[string]any{"type": "node_skipped", "runId": rc.run.ID, "nodeId": nodeID})
}

func (rc *runContext) setAttempts(nodeID string, n int) {
	s := rc.stateOf(nodeID)
	rc.mu.Lock()
	s.Attempts = n
	rc.mu.Unlock()
}

func (rc *runContext) setAgentSession(nodeID, sessionID string) {
	s := rc.stateOf(nodeID)
	rc.mu.Lock()
	s.AgentSessionID = sessionID
	rc.mu.Unlock()
}

func (rc *runContext) emit(ev map[string]any) {
	if rc.deps.Emit != nil {
		rc.deps.Emit(ev)
	}
}

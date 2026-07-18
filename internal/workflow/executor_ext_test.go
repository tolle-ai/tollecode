package workflow

import (
	"context"
	"testing"
)

// Trigger → code(array) → filter(score>0.5) keeps only the matching items.
func TestFilterNode(t *testing.T) {
	wf := &Workflow{
		ID: "wf",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("arr", TypeCodeTransform, WorkflowNodeConfig{
				Language: "expr",
				Code:     "[{score: 0.9, id: 1}, {score: 0.2, id: 2}, {score: 0.7, id: 3}]",
			}),
			node("f", TypeFilter, WorkflowNodeConfig{FilterExpression: "json.score > 0.5"}),
		},
		Edges: []WorkflowEdge{edge("t", "", "arr"), edge("arr", "", "f")},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunDone {
		t.Fatalf("status=%s err=%s", r.Status, r.Error)
	}
	out, ok := c.states["f"].Output.([]any)
	if !ok {
		t.Fatalf("filter output is %T, want []any", c.states["f"].Output)
	}
	if len(out) != 2 {
		t.Fatalf("filtered length = %d, want 2 (0.9 and 0.7)", len(out))
	}
}

// Trigger → code({name}) → template renders the string.
func TestTemplateNode(t *testing.T) {
	wf := &Workflow{
		ID: "wf",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("d", TypeCodeTransform, WorkflowNodeConfig{Language: "expr", Code: `{name: "Ada"}`}),
			node("tpl", TypeTemplate, WorkflowNodeConfig{Template: "Hi {{json.name}}!"}),
		},
		Edges: []WorkflowEdge{edge("t", "", "d"), edge("d", "", "tpl")},
	}
	c := newCollector()
	r := run(t, wf, c)
	if r.Status != RunDone {
		t.Fatalf("status=%s err=%s", r.Status, r.Error)
	}
	if got, _ := c.states["tpl"].Output.(string); got != "Hi Ada!" {
		t.Fatalf("template output = %q, want %q", got, "Hi Ada!")
	}
}

// Approval fires the approved/rejected port based on the human decision.
func TestApprovalBranch(t *testing.T) {
	cases := []struct {
		name             string
		approve          bool
		wantFire, wantSk string
	}{
		{"approved", true, "ok", "no"},
		{"rejected", false, "no", "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := &Workflow{
				ID: "wf",
				Nodes: []WorkflowNode{
					node("t", TypeTriggerManual, WorkflowNodeConfig{}),
					node("ap", TypeApproval, WorkflowNodeConfig{ApprovalTitle: "Deploy?"}),
					node("ok", TypeSetVariable, WorkflowNodeConfig{}),
					node("no", TypeSetVariable, WorkflowNodeConfig{}),
				},
				Edges: []WorkflowEdge{
					edge("t", "", "ap"),
					{ID: "a-app", SourceNodeID: "ap", SourcePort: "approved", TargetNodeID: "ok"},
					{ID: "a-rej", SourceNodeID: "ap", SourcePort: "rejected", TargetNodeID: "no"},
				},
			}
			c := newCollector()
			d := c.deps()
			d.RequestApproval = func(_ context.Context, _ ApprovalReq) (bool, error) { return tc.approve, nil }
			r := &WorkflowRun{ID: "r", WorkflowID: "wf", Status: RunPending, TriggerType: "manual"}
			Run(context.Background(), wf, r, d, RunOptions{})
			if c.status(tc.wantFire) != NodeDone {
				t.Errorf("%s should be done, got %s", tc.wantFire, c.status(tc.wantFire))
			}
			if c.status(tc.wantSk) != NodeSkipped {
				t.Errorf("%s should be skipped, got %s", tc.wantSk, c.status(tc.wantSk))
			}
		})
	}
}

// LLM prompt with JSON parsing turns the model reply into structured output.
func TestLLMPromptJSON(t *testing.T) {
	wf := &Workflow{
		ID: "wf",
		Nodes: []WorkflowNode{
			node("t", TypeTriggerManual, WorkflowNodeConfig{}),
			node("llm", TypeLLMPrompt, WorkflowNodeConfig{TaskPrompt: "classify", LLMJSON: true}),
		},
		Edges: []WorkflowEdge{edge("t", "", "llm")},
	}
	c := newCollector()
	d := c.deps()
	d.RunLLM = func(_ context.Context, _ LLMReq) (string, error) {
		return "```json\n{\"label\": \"urgent\"}\n```", nil
	}
	r := &WorkflowRun{ID: "r", WorkflowID: "wf", Status: RunPending, TriggerType: "manual"}
	Run(context.Background(), wf, r, d, RunOptions{})
	out, ok := c.states["llm"].Output.(map[string]any)
	if !ok || out["label"] != "urgent" {
		t.Fatalf("llm output = %#v, want map with label=urgent", c.states["llm"].Output)
	}
}

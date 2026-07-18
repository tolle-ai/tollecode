package workflow

// executor_ext.go — node bodies for the extended node set (channels, LLM, web,
// knowledge base, SQL, filter, template, approval, sub-workflow). Like the rest
// of the engine these stay pure: every external effect is reached through Deps.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ── send_channel ────────────────────────────────────────────────────────────────

func runSendChannel(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	if rc.deps.SendChannel == nil {
		return NodeResult{}, fmt.Errorf("send_channel: channel delivery not available")
	}
	if node.Config.ChannelID == "" {
		return NodeResult{}, fmt.Errorf("send_channel: no channel selected")
	}
	env := rc.env(in)
	target := interpolateStr(node.Config.ChannelTarget, env)
	text := interpolateStr(node.Config.Body, env)
	if strings.TrimSpace(text) == "" {
		return NodeResult{}, fmt.Errorf("send_channel: message is empty")
	}
	if err := rc.deps.SendChannel(ctx, ChannelReq{
		NodeID: node.ID, ChannelID: node.Config.ChannelID, Target: target, Text: text,
	}); err != nil {
		return NodeResult{}, err
	}
	return NodeResult{Output: map[string]any{"sent": true, "target": target}, FiredPorts: defaultPort}, nil
}

// ── llm_prompt ──────────────────────────────────────────────────────────────────

func runLLMPrompt(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	if rc.deps.RunLLM == nil {
		return NodeResult{}, fmt.Errorf("llm_prompt: LLM not available")
	}
	env := rc.env(in)
	prompt := interpolateStr(node.Config.TaskPrompt, env)
	if strings.TrimSpace(prompt) == "" {
		return NodeResult{}, fmt.Errorf("llm_prompt: prompt is empty")
	}
	text, err := rc.deps.RunLLM(ctx, LLMReq{
		NodeID: node.ID,
		System: interpolateStr(node.Config.SystemPrompt, env),
		Prompt: prompt,
	})
	if err != nil {
		return NodeResult{}, err
	}
	var out any = map[string]any{"text": text}
	if node.Config.LLMJSON {
		if v, ok := parseJSONish(text); ok {
			out = v
		}
	}
	return NodeResult{Output: out, FiredPorts: defaultPort}, nil
}

// ── web (search | fetch) ────────────────────────────────────────────────────────

func runWeb(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	env := rc.env(in)
	switch op := orDefault(node.Config.Operation, "search"); op {
	case "fetch":
		if rc.deps.WebFetch == nil {
			return NodeResult{}, fmt.Errorf("web: fetch not available")
		}
		url := interpolateStr(node.Config.URL, env)
		if strings.TrimSpace(url) == "" {
			return NodeResult{}, fmt.Errorf("web: url is required for fetch")
		}
		text, err := rc.deps.WebFetch(ctx, url)
		if err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"url": url, "content": text}, FiredPorts: defaultPort}, nil
	default: // search
		if rc.deps.WebSearch == nil {
			return NodeResult{}, fmt.Errorf("web: search not available")
		}
		q := interpolateStr(node.Config.SearchQuery, env)
		if strings.TrimSpace(q) == "" {
			return NodeResult{}, fmt.Errorf("web: query is required for search")
		}
		text, err := rc.deps.WebSearch(ctx, q, node.Config.MaxResults)
		if err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"query": q, "results": text}, FiredPorts: defaultPort}, nil
	}
}

// ── knowledge_base (search | ingest) ────────────────────────────────────────────

func runKnowledgeBase(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	env := rc.env(in)
	switch op := orDefault(node.Config.Operation, "search"); op {
	case "ingest":
		if rc.deps.KBIngest == nil {
			return NodeResult{}, fmt.Errorf("knowledge_base: ingest not available")
		}
		req := KBIngestReq{NodeID: node.ID, Name: interpolateStr(node.Config.KBSourceName, env)}
		switch {
		case node.Config.ContentSource == "input":
			req.Content = stringify(in)
		case node.Config.Content != "":
			req.Content = interpolateStr(node.Config.Content, env)
		default:
			req.FilePath = interpolateStr(node.Config.FilePath, env)
		}
		if req.Content == "" && req.FilePath == "" {
			return NodeResult{}, fmt.Errorf("knowledge_base: nothing to ingest")
		}
		msg, err := rc.deps.KBIngest(ctx, req)
		if err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"result": msg}, FiredPorts: defaultPort}, nil
	default: // search
		if rc.deps.KBSearch == nil {
			return NodeResult{}, fmt.Errorf("knowledge_base: search not available")
		}
		q := interpolateStr(node.Config.SearchQuery, env)
		if strings.TrimSpace(q) == "" {
			return NodeResult{}, fmt.Errorf("knowledge_base: query is required")
		}
		topK := node.Config.TopK
		if topK <= 0 {
			topK = 5
		}
		text, err := rc.deps.KBSearch(ctx, q, topK)
		if err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"query": q, "results": text}, FiredPorts: defaultPort}, nil
	}
}

// ── sql_query ───────────────────────────────────────────────────────────────────

func runSQL(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	if rc.deps.SQLQuery == nil {
		return NodeResult{}, fmt.Errorf("sql_query: database access not available")
	}
	env := rc.env(in)
	query := interpolateStr(node.Config.SQLQuery, env)
	if strings.TrimSpace(query) == "" {
		return NodeResult{}, fmt.Errorf("sql_query: query is empty")
	}
	res, err := rc.deps.SQLQuery(ctx, SQLReq{
		NodeID: node.ID,
		Driver: node.Config.SQLDriver,
		DSN:    interpolateStr(node.Config.SQLDSN, env),
		Query:  query,
	})
	if err != nil {
		return NodeResult{}, err
	}
	return NodeResult{Output: map[string]any{"rows": res.Rows, "rowCount": res.RowCount}, FiredPorts: defaultPort}, nil
}

// ── filter ──────────────────────────────────────────────────────────────────────

// runFilter keeps the input-array items for which FilterExpression is truthy. A
// non-array input is treated as a single-item set. Within the expression the
// current item is available as $json (and data).
func runFilter(rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	items := toSlice(in)
	if items == nil && in != nil {
		items = []any{in}
	}
	expr := node.Config.FilterExpression
	kept := make([]any, 0, len(items))
	for _, item := range items {
		if expr == "" || evalBool(expr, rc.env(item)) {
			kept = append(kept, item)
		}
	}
	return NodeResult{Output: kept, FiredPorts: defaultPort}, nil
}

// ── template ────────────────────────────────────────────────────────────────────

// runTemplate renders a {{ }} template against the node environment. With
// TemplateField set the rendered string is wrapped as { field: rendered }.
func runTemplate(rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	rendered := interpolateStr(node.Config.Template, rc.env(in))
	var out any = rendered
	if f := node.Config.TemplateField; f != "" {
		out = map[string]any{f: rendered}
	}
	return NodeResult{Output: out, FiredPorts: defaultPort}, nil
}

// ── approval (human-in-the-loop) ────────────────────────────────────────────────

func runApproval(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	if rc.deps.RequestApproval == nil {
		return NodeResult{}, fmt.Errorf("approval: approvals not available")
	}
	env := rc.env(in)
	timeout := time.Duration(node.Config.ApprovalTimeout) * time.Minute
	if timeout <= 0 {
		timeout = 60 * time.Minute
	}
	approved, err := rc.deps.RequestApproval(ctx, ApprovalReq{
		NodeID:    node.ID,
		Title:     interpolateStr(node.Config.ApprovalTitle, env),
		Message:   interpolateStr(node.Config.ApprovalMessage, env),
		Timeout:   timeout,
		OnTimeout: node.Config.ApprovalOnTimeout,
	})
	if err != nil {
		return NodeResult{}, err
	}
	port := "rejected"
	if approved {
		port = "approved"
	}
	return NodeResult{Output: map[string]any{"approved": approved, "data": in}, FiredPorts: []string{port}}, nil
}

// ── sub_workflow ────────────────────────────────────────────────────────────────

func runSubWorkflow(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	if rc.deps.RunSubWorkflow == nil {
		return NodeResult{}, fmt.Errorf("sub_workflow: nested workflows not available")
	}
	if node.Config.SubWorkflowID == "" {
		return NodeResult{}, fmt.Errorf("sub_workflow: no workflow selected")
	}
	payload := map[string]any{}
	if m, ok := in.(map[string]any); ok {
		payload = m
	} else if in != nil {
		payload = map[string]any{"input": in}
	}
	out, err := rc.deps.RunSubWorkflow(ctx, node.Config.SubWorkflowID, payload)
	if err != nil {
		return NodeResult{}, err
	}
	return NodeResult{Output: out, FiredPorts: defaultPort}, nil
}

// ── helpers ─────────────────────────────────────────────────────────────────────

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// parseJSONish best-effort parses an LLM reply as JSON, tolerating code fences
// and surrounding prose.
func parseJSONish(s string) (any, bool) {
	t := strings.TrimSpace(s)
	if i := strings.Index(t, "```"); i >= 0 {
		rest := strings.TrimPrefix(t[i+3:], "json")
		if j := strings.Index(rest, "```"); j >= 0 {
			t = strings.TrimSpace(rest[:j])
		}
	}
	var v any
	if json.Unmarshal([]byte(t), &v) == nil {
		return v, true
	}
	start := strings.IndexAny(t, "{[")
	end := strings.LastIndexAny(t, "}]")
	if start >= 0 && end > start {
		if json.Unmarshal([]byte(t[start:end+1]), &v) == nil {
			return v, true
		}
	}
	return nil, false
}

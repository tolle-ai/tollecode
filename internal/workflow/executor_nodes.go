package workflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// Deps injects all side effects the engine needs so the package imports no other
// internal package. It is constructed per-run by the caller (the selfhost handler)
// so RunAgent/RequestPerm/Emit close over the run id and event bus.
type Deps struct {
	// RunAgent executes an AI agent node and returns its final text + session id.
	RunAgent func(ctx context.Context, req AgentReq) (AgentResult, error)
	// RunShell executes a shell command node.
	RunShell func(ctx context.Context, req ShellReq) (ShellResult, error)
	// SendEmail delivers an email_send node's message via the caller's SMTP sender
	// (e.g. the org's configured SMTP settings). When nil, email_send nodes error.
	SendEmail func(ctx context.Context, req EmailReq) error
	// SendChannel delivers a send_channel node's message through a configured
	// messaging channel (Slack/Telegram/Discord/WhatsApp). When nil, the node errors.
	SendChannel func(ctx context.Context, req ChannelReq) error
	// RunLLM performs a single, no-tools LLM completion for the llm_prompt node.
	RunLLM func(ctx context.Context, req LLMReq) (string, error)
	// WebSearch / WebFetch back the web node (search the web, fetch a URL as text).
	WebSearch func(ctx context.Context, query string, maxResults int) (string, error)
	WebFetch  func(ctx context.Context, url string) (string, error)
	// KBSearch / KBIngest back the knowledge_base node (vector search / ingest).
	KBSearch func(ctx context.Context, query string, topK int) (string, error)
	KBIngest func(ctx context.Context, req KBIngestReq) (string, error)
	// SQLQuery runs the sql_query node against an external database.
	SQLQuery func(ctx context.Context, req SQLReq) (SQLResult, error)
	// RunSubWorkflow runs another workflow to completion and returns its outputs
	// (sub_workflow node). The caller enforces a recursion-depth guard.
	RunSubWorkflow func(ctx context.Context, workflowID string, payload map[string]any) (map[string]any, error)
	// RequestApproval blocks on a human decision for the approval node.
	RequestApproval func(ctx context.Context, req ApprovalReq) (bool, error)
	// FS performs workspace-confined file operations.
	FS FSOps
	// HTTPClient is used by http_request nodes.
	HTTPClient *http.Client
	// RequestPerm asks the caller to approve a shell command (allow, allowAll).
	RequestPerm func(ctx context.Context, nodeID, command string) (allow, allowAll bool)
	// Emit publishes a run event to the bus (run_started/node_started/... ).
	Emit func(ev map[string]any)
	// PersistNode upserts a node run-state row (executions inspector).
	PersistNode func(ns NodeRunState)
	// Suspend durably records a long-wait suspension so a scheduler can resume the
	// run at resumeAt. When nil, wait nodes fall back to an in-process timer.
	Suspend func(nodeID string, resumeAt time.Time, snap Snapshot)
	// Workspace is the resolved filesystem root for fs/shell nodes.
	Workspace string
	// MaxConcurrency bounds parallel node execution per run (default 8).
	MaxConcurrency int
}

type AgentReq struct {
	NodeID    string
	AgentID   string
	Prompt    string
	Workspace string
}

type AgentResult struct {
	Text      string
	SessionID string
}

type ShellReq struct {
	NodeID      string
	Command     string
	Cwd         string
	AutoApprove bool
}

type ShellResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type EmailReq struct {
	NodeID   string
	To       string
	Cc       []string
	Bcc      []string
	Subject  string
	Body     string
	HTML     bool
	FromName string
}

type ChannelReq struct {
	NodeID    string
	ChannelID string
	Target    string
	Text      string
}

type LLMReq struct {
	NodeID string
	System string
	Prompt string
}

type KBIngestReq struct {
	NodeID   string
	Name     string
	Content  string // when set, ingest this text directly
	FilePath string // otherwise, ingest a file at this workspace-relative path
}

type SQLReq struct {
	NodeID string
	Driver string
	DSN    string
	Query  string
}

type SQLResult struct {
	Rows     []map[string]any
	RowCount int
}

type ApprovalReq struct {
	NodeID    string
	Title     string
	Message   string
	Timeout   time.Duration
	OnTimeout string // "approve" | "reject"
}

// FSOps is the minimal workspace-confined filesystem surface for workspace_op.
type FSOps interface {
	Read(path string) (string, error)
	Write(path, content string) error
	List(path string) ([]string, error)
	Search(query string) ([]string, error)
}

// NodeResult is what a node body produces.
type NodeResult struct {
	Output         any      // becomes $json for downstream nodes
	FiredPorts     []string // output ports to follow ("" = default)
	AgentSessionID string   // set by agent nodes for inspector deep-linking
	Suspend        bool     // wait node: run suspended, resume scheduled durably
}

var defaultPort = []string{""}

// dispatch runs one node's body (no retry/onError — that wraps this in engine).
func dispatch(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	switch node.Type {
	case TypeTriggerManual, TypeTriggerSchedule, TypeTriggerWebhook, TypeTriggerEvent, TypeTriggerForm:
		return NodeResult{Output: in, FiredPorts: defaultPort}, nil
	case TypeAgent:
		return runAgentNode(ctx, rc, node, in, false)
	case TypeAgentHandoff:
		return runAgentNode(ctx, rc, node, in, true)
	case TypeCondition:
		return runCondition(rc, node, in)
	case TypeSwitch:
		return runSwitch(rc, node, in)
	case TypeHTTPRequest:
		return runHTTP(ctx, rc, node, in)
	case TypeCodeTransform:
		return runCode(ctx, rc, node, in)
	case TypeWorkspaceOp:
		return runWorkspace(rc, node, in)
	case TypeShellCommand:
		return runShell(ctx, rc, node, in)
	case TypeEmailSend:
		return runEmail(ctx, rc, node, in)
	case TypeSendChannel:
		return runSendChannel(ctx, rc, node, in)
	case TypeLLMPrompt:
		return runLLMPrompt(ctx, rc, node, in)
	case TypeWeb:
		return runWeb(ctx, rc, node, in)
	case TypeKnowledgeBase:
		return runKnowledgeBase(ctx, rc, node, in)
	case TypeSQLQuery:
		return runSQL(ctx, rc, node, in)
	case TypeFilter:
		return runFilter(rc, node, in)
	case TypeTemplate:
		return runTemplate(rc, node, in)
	case TypeApproval:
		return runApproval(ctx, rc, node, in)
	case TypeSubWorkflow:
		return runSubWorkflow(ctx, rc, node, in)
	case TypeWait:
		return runWait(ctx, rc, node, in)
	case TypeSetVariable:
		return runSetVar(rc, node, in)
	case TypeMerge:
		return NodeResult{Output: in, FiredPorts: defaultPort}, nil
	case TypeLoop:
		return runLoop(ctx, rc, node, in)
	default:
		return NodeResult{Output: in, FiredPorts: defaultPort}, nil
	}
}

func runAgentNode(ctx context.Context, rc *runContext, node WorkflowNode, in any, handoff bool) (NodeResult, error) {
	if rc.deps.RunAgent == nil {
		return NodeResult{}, fmt.Errorf("agent execution not available")
	}
	env := rc.env(in)
	prompt := interpolateStr(node.Config.TaskPrompt, env)
	// Handoff prepends the upstream node output as context.
	if handoff {
		if ctxText := stringify(in); ctxText != "" && ctxText != "null" {
			prompt = "Context from previous step:\n" + ctxText + "\n\n" + prompt
		}
	}
	ws := node.Config.WorkspacePath
	if ws == "" {
		ws = rc.deps.Workspace
	}
	res, err := rc.deps.RunAgent(ctx, AgentReq{
		NodeID:    node.ID,
		AgentID:   node.Config.AgentID,
		Prompt:    prompt,
		Workspace: ws,
	})
	if err != nil {
		return NodeResult{AgentSessionID: res.SessionID}, err
	}
	return NodeResult{
		Output:         map[string]any{"text": res.Text, "sessionId": res.SessionID},
		FiredPorts:     defaultPort,
		AgentSessionID: res.SessionID,
	}, nil
}

func runCondition(rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	ok := evalBool(node.Config.ConditionExpression, rc.env(in))
	port := "false"
	if ok {
		port = "true"
	}
	return NodeResult{Output: in, FiredPorts: []string{port}}, nil
}

func runSwitch(rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	env := rc.env(in)
	for _, c := range node.Config.Cases {
		if evalBool(c.Expression, env) {
			port := c.PortID
			if port == "" {
				port = "default"
			}
			return NodeResult{Output: in, FiredPorts: []string{port}}, nil
		}
	}
	return NodeResult{Output: in, FiredPorts: []string{"default"}}, nil
}

func runHTTP(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	env := rc.env(in)
	cfg := node.Config
	method := strings.ToUpper(cfg.Method)
	if method == "" {
		method = http.MethodGet
	}
	url := interpolateStr(cfg.URL, env)
	if url == "" {
		return NodeResult{}, fmt.Errorf("http_request: url is empty")
	}

	var body io.Reader
	bodyStr := ""
	if cfg.Body != "" {
		bodyStr = interpolateStr(cfg.Body, env)
		body = strings.NewReader(bodyStr)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return NodeResult{}, err
	}
	for k, v := range cfg.Headers {
		req.Header.Set(interpolateStr(k, env), interpolateStr(v, env))
	}
	switch cfg.AuthType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+interpolateStr(cfg.AuthValue, env))
	case "basic":
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(interpolateStr(cfg.AuthValue, env))))
	case "header":
		req.Header.Set("X-Api-Key", interpolateStr(cfg.AuthValue, env))
	}
	if bodyStr != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	client := rc.deps.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return NodeResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed any
	if strings.Contains(resp.Header.Get("Content-Type"), "json") && json.Unmarshal(raw, &parsed) == nil {
		// parsed holds structured JSON
	} else {
		parsed = string(raw)
	}
	out := map[string]any{
		"status": resp.StatusCode,
		"body":   parsed,
	}
	return NodeResult{Output: out, FiredPorts: defaultPort}, nil
}

func runCode(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	env := rc.env(in)
	// language:"js" runs the body in a sandboxed goja VM (full ES5.1+); anything
	// else is evaluated as an expr-lang expression.
	if node.Config.Language == "js" {
		out, err := runJS(ctx, node.Config.Code, env)
		if err != nil {
			return NodeResult{}, fmt.Errorf("code_transform (js): %w", err)
		}
		return NodeResult{Output: out, FiredPorts: defaultPort}, nil
	}
	src := prepCode(node.Config.Code)
	if src == "" {
		return NodeResult{Output: in, FiredPorts: defaultPort}, nil
	}
	out, err := evalExpr(src, env)
	if err != nil {
		return NodeResult{}, fmt.Errorf("code_transform: unsupported expression (use expr syntax): %w", err)
	}
	return NodeResult{Output: out, FiredPorts: defaultPort}, nil
}

// runJS executes a code_transform body as JavaScript in an isolated goja VM.
// goja has no host bindings (no require/fs/net), and a watchdog interrupts the VM
// on a hard timeout or run cancellation, so an org-authored script cannot hang or
// reach the host — the same trust level as the shell_command node.
func runJS(ctx context.Context, code string, env evalEnv) (any, error) {
	vm := goja.New()
	vm.Set("$json", env.input)     //nolint:errcheck
	vm.Set("json", env.input)      //nolint:errcheck
	vm.Set("$vars", env.vars)      //nolint:errcheck
	vm.Set("vars", env.vars)       //nolint:errcheck
	vm.Set("$node", env.nodes)     //nolint:errcheck
	vm.Set("trigger", env.trigger) //nolint:errcheck

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-time.After(5 * time.Second):
			vm.Interrupt("code_transform: timed out")
		case <-ctx.Done():
			vm.Interrupt("cancelled")
		case <-done:
		}
	}()

	// Wrap in an IIFE so a bare `return value;` (desktop style) captures the result.
	val, err := vm.RunString("(function(){\n" + code + "\n})()")
	if err != nil {
		return nil, err
	}
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, nil
	}
	return val.Export(), nil
}

// prepCode adapts a desktop `code_transform` body (JS-ish) to an expr expression:
// drops whole-line // comments, and strips a single `return ...;` wrapper.
func prepCode(code string) string {
	var b strings.Builder
	for _, line := range strings.Split(code, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	s := strings.TrimSpace(b.String())
	s = strings.TrimPrefix(s, "return ")
	s = strings.TrimSuffix(s, ";")
	return strings.TrimSpace(s)
}

func runWorkspace(rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	if rc.deps.FS == nil {
		return NodeResult{}, fmt.Errorf("workspace_op: filesystem not available")
	}
	env := rc.env(in)
	cfg := node.Config
	path := interpolateStr(cfg.FilePath, env)
	switch cfg.Operation {
	case "read_file":
		content, err := rc.deps.FS.Read(path)
		if err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"content": content, "path": path}, FiredPorts: defaultPort}, nil
	case "write_file":
		content := cfg.Content
		if cfg.ContentSource == "input" {
			content = stringify(in)
		} else {
			content = interpolateStr(content, env)
		}
		if err := rc.deps.FS.Write(path, content); err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"path": path, "written": len(content)}, FiredPorts: defaultPort}, nil
	case "list_files":
		files, err := rc.deps.FS.List(path)
		if err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"files": files}, FiredPorts: defaultPort}, nil
	case "search_files":
		q := interpolateStr(cfg.SearchQuery, env)
		matches, err := rc.deps.FS.Search(q)
		if err != nil {
			return NodeResult{}, err
		}
		return NodeResult{Output: map[string]any{"matches": matches}, FiredPorts: defaultPort}, nil
	default:
		return NodeResult{}, fmt.Errorf("workspace_op: unknown operation %q", cfg.Operation)
	}
}

func runShell(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	if rc.deps.RunShell == nil {
		return NodeResult{}, fmt.Errorf("shell_command: shell not available")
	}
	env := rc.env(in)
	cmd := interpolateStr(node.Config.ShellCommand, env)
	res, err := rc.deps.RunShell(ctx, ShellReq{
		NodeID:      node.ID,
		Command:     cmd,
		Cwd:         interpolateStr(node.Config.ShellCwd, env),
		AutoApprove: node.Config.ShellAutoApprove,
	})
	if err != nil {
		return NodeResult{}, err
	}
	out := map[string]any{"stdout": res.Stdout, "stderr": res.Stderr, "exitCode": res.ExitCode}
	if res.ExitCode != 0 {
		return NodeResult{Output: out}, fmt.Errorf("shell exited %d: %s", res.ExitCode, res.Stderr)
	}
	return NodeResult{Output: out, FiredPorts: defaultPort}, nil
}

func runEmail(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	if rc.deps.SendEmail == nil {
		return NodeResult{}, fmt.Errorf("email_send: email delivery not available")
	}
	env := rc.env(in)
	cfg := node.Config
	to := interpolateStr(cfg.To, env)
	if strings.TrimSpace(to) == "" {
		return NodeResult{}, fmt.Errorf("email_send: 'to' is required")
	}
	req := EmailReq{
		NodeID:   node.ID,
		To:       to,
		Cc:       splitAddrList(interpolateStr(cfg.Cc, env)),
		Bcc:      splitAddrList(interpolateStr(cfg.Bcc, env)),
		Subject:  interpolateStr(cfg.Subject, env),
		Body:     interpolateStr(cfg.Body, env),
		HTML:     cfg.EmailHTML,
		FromName: interpolateStr(cfg.FromName, env),
	}
	if err := rc.deps.SendEmail(ctx, req); err != nil {
		return NodeResult{}, fmt.Errorf("email_send: %w", err)
	}
	out := map[string]any{"to": to, "subject": req.Subject, "sent": true}
	return NodeResult{Output: out, FiredPorts: defaultPort}, nil
}

func splitAddrList(s string) []string {
	var out []string
	for _, a := range strings.Split(s, ",") {
		if t := strings.TrimSpace(a); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func runWait(ctx context.Context, rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	// On resume the wait has already elapsed — pass through immediately.
	if rc.opts.Resume && node.ID == rc.opts.StartNodeID {
		return NodeResult{Output: in, FiredPorts: defaultPort}, nil
	}
	d := waitDuration(node.Config.DelayValue, node.Config.DelayUnit)
	// Short waits run in-process; long waits suspend durably (survive restarts).
	const inProcessCap = 60 * time.Second
	if d > inProcessCap && rc.deps.Suspend != nil {
		rc.deps.Suspend(node.ID, time.Now().Add(d), rc.snapshot())
		return NodeResult{Suspend: true}, nil
	}
	select {
	case <-time.After(d):
		return NodeResult{Output: in, FiredPorts: defaultPort}, nil
	case <-ctx.Done():
		return NodeResult{}, ctx.Err()
	}
}

func waitDuration(value int, unit string) time.Duration {
	if value <= 0 {
		return 0
	}
	switch unit {
	case "minutes":
		return time.Duration(value) * time.Minute
	case "hours":
		return time.Duration(value) * time.Hour
	case "ms":
		return time.Duration(value) * time.Millisecond
	default: // seconds
		return time.Duration(value) * time.Second
	}
}

func runSetVar(rc *runContext, node WorkflowNode, in any) (NodeResult, error) {
	env := rc.env(in)
	for _, v := range node.Config.Variables {
		rc.setVar(v.Name, interpolate(v.Value, env))
	}
	return NodeResult{Output: in, FiredPorts: defaultPort}, nil
}

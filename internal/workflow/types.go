// Package workflow implements a server-side, n8n-style workflow execution
// engine: a DAG of typed nodes (triggers, AI agents, logic, actions, data)
// wired by ports, executed with join-gated scheduling, cancellation, and
// per-node run state suitable for a live executions inspector.
//
// The package is pure: it imports no other internal package. All side effects
// (running an agent, HTTP, filesystem, shell, persistence, event emission) are
// injected via Deps so the engine is unit-testable and reusable. The wire model
// (Workflow/WorkflowNode/WorkflowEdge/WorkflowNodeConfig) mirrors the Angular
// desktop `workflow.models.ts` byte-for-byte so definitions round-trip 1:1.
package workflow

import "time"

// ── Node types ────────────────────────────────────────────────────────────────

// Node type identifiers (must match the TS WorkflowNodeType union).
const (
	TypeTriggerManual   = "trigger_manual"
	TypeTriggerSchedule = "trigger_schedule"
	TypeTriggerWebhook  = "trigger_webhook"
	TypeTriggerEvent    = "trigger_event"
	TypeTriggerForm     = "trigger_form"
	TypeAgent           = "agent"
	TypeAgentHandoff    = "agent_handoff"
	TypeCondition       = "condition"
	TypeSwitch          = "switch"
	TypeHTTPRequest     = "http_request"
	TypeCodeTransform   = "code_transform"
	TypeWorkspaceOp     = "workspace_op"
	TypeShellCommand    = "shell_command"
	TypeWait            = "wait"
	TypeLoop            = "loop"
	TypeMerge           = "merge"
	TypeSetVariable     = "set_variable"
	TypeEmailSend       = "email_send"
	TypeTriggerEmail    = "trigger_email"
	TypeSendChannel     = "send_channel"
	TypeLLMPrompt       = "llm_prompt"
	TypeWeb             = "web"
	TypeKnowledgeBase   = "knowledge_base"
	TypeSQLQuery        = "sql_query"
	TypeFilter          = "filter"
	TypeTemplate        = "template"
	TypeApproval        = "approval"
	TypeSubWorkflow     = "sub_workflow"
)

// subPorts are agent configuration ports, not execution-flow inputs. Edges whose
// targetPort is one of these are excluded from a node's indegree so they never
// gate execution (they wire model/memory/tool config to an agent node).
var subPorts = map[string]bool{
	"sub_model":  true,
	"sub_memory": true,
	"sub_tool":   true,
}

// DefaultOutputPort is the port id the web UI assigns to a node's single default
// output. Standard nodes fire their default port as "" internally (see
// defaultPort in executor_nodes.go), so an edge saved from the UI's "output"
// port must be treated as that default when matching fired ports. Explicit
// branch ports (true/false/each/done/approved/rejected/default/case_*) carry
// their own ids on both sides and are returned unchanged.
const DefaultOutputPort = "output"

// canonicalPort maps the UI's default output port name to the engine's internal
// default (""), bridging the naming difference so default-output edges are
// matched as live instead of being skipped.
func canonicalPort(p string) string {
	if p == DefaultOutputPort {
		return ""
	}
	return p
}

// IsTriggerType reports whether a node type is a trigger (a run's seed node).
func IsTriggerType(t string) bool {
	switch t {
	case TypeTriggerManual, TypeTriggerSchedule, TypeTriggerWebhook, TypeTriggerEvent, TypeTriggerForm, TypeTriggerEmail:
		return true
	}
	return false
}

// ── Wire model (must byte-match the Angular/TS port) ──────────────────────────

type Workflow struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	WorkspacePath string         `json:"workspacePath,omitempty"`
	Nodes         []WorkflowNode `json:"nodes"`
	Edges         []WorkflowEdge `json:"edges"`
	Active        bool           `json:"active"`
	Tags          []string       `json:"tags,omitempty"`
	Variables     []WorkflowVar  `json:"variables,omitempty"`
	// PinnedData maps nodeID -> pinned output used during test runs.
	PinnedData map[string]any `json:"pinnedData,omitempty"`
	CreatedAt  time.Time      `json:"createdAt"`
	UpdatedAt  time.Time      `json:"updatedAt"`
}

// WorkflowVar matches the desktop { name, value, description? } shape.
type WorkflowVar struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type WorkflowNode struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Label    string             `json:"label,omitempty"`
	Sublabel string             `json:"sublabel,omitempty"`
	X        float64            `json:"x"`
	Y        float64            `json:"y"`
	Config   WorkflowNodeConfig `json:"config"`
	Disabled bool               `json:"disabled,omitempty"`
}

type WorkflowEdge struct {
	ID           string `json:"id"`
	SourceNodeID string `json:"sourceNodeId"`
	SourcePort   string `json:"sourcePort,omitempty"`
	TargetNodeID string `json:"targetNodeId"`
	TargetPort   string `json:"targetPort,omitempty"`
	Label        string `json:"label,omitempty"`
	Style        string `json:"style,omitempty"`
}

// WorkflowNodeConfig is a wide optional-field struct; omitempty keeps the JSON
// round-tripping 1:1 with the TS optional-field object.
type WorkflowNodeConfig struct {
	// agent / agent_handoff
	AgentID         string `json:"agentId,omitempty"`
	TaskPrompt      string `json:"taskPrompt,omitempty"`
	WorkspacePath   string `json:"workspacePath,omitempty"`
	WaitForComplete *bool  `json:"waitForCompletion,omitempty"`
	Timeout         int    `json:"timeout,omitempty"`
	// trigger_schedule
	Schedule string `json:"schedule,omitempty"`
	// trigger_webhook
	WebhookPath string `json:"webhookPath,omitempty"`
	// trigger_event
	EventType string `json:"eventType,omitempty"`
	// trigger_form
	FormFields []FormField `json:"formFields,omitempty"`
	// condition
	ConditionExpression string `json:"conditionExpression,omitempty"`
	TrueLabel           string `json:"trueLabel,omitempty"`
	FalseLabel          string `json:"falseLabel,omitempty"`
	// switch
	Cases []SwitchCase `json:"cases,omitempty"`
	// http_request
	URL       string            `json:"url,omitempty"`
	Method    string            `json:"method,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
	AuthType  string            `json:"authType,omitempty"`
	AuthValue string            `json:"authValue,omitempty"`
	// code_transform
	Code     string `json:"code,omitempty"`
	Language string `json:"language,omitempty"`
	// wait
	DelayValue int    `json:"delayValue,omitempty"`
	DelayUnit  string `json:"delayUnit,omitempty"`
	// loop
	IterateOver string `json:"iterateOver,omitempty"`
	LoopVar     string `json:"loopVar,omitempty"`
	// set_variable
	Variables []WorkflowVar `json:"variables,omitempty"`
	// workspace_op
	Operation     string `json:"operation,omitempty"`
	FilePath      string `json:"filePath,omitempty"`
	Content       string `json:"content,omitempty"`
	ContentSource string `json:"contentSource,omitempty"`
	SearchQuery   string `json:"searchQuery,omitempty"`
	// shell_command
	ShellCommand     string `json:"shellCommand,omitempty"`
	ShellCwd         string `json:"shellCwd,omitempty"`
	ShellAutoApprove bool   `json:"shellAutoApprove,omitempty"`
	// email_send (the email body reuses Body / "body" above)
	To        string `json:"to,omitempty"`
	Cc        string `json:"cc,omitempty"`
	Bcc       string `json:"bcc,omitempty"`
	Subject   string `json:"subject,omitempty"`
	EmailHTML bool   `json:"emailHtml,omitempty"`
	FromName  string `json:"fromName,omitempty"`
	// trigger_email (inbound IMAP poll)
	EmailFolder string `json:"emailFolder,omitempty"` // default INBOX
	// send_channel (the message text reuses Body / "body" above)
	ChannelID     string `json:"channelId,omitempty"`     // configured channel to send through
	ChannelTarget string `json:"channelTarget,omitempty"` // chat/channel id, or phone for whatsapp
	// llm_prompt (the user prompt reuses TaskPrompt / "taskPrompt" above)
	SystemPrompt string `json:"systemPrompt,omitempty"`
	LLMJSON      bool   `json:"llmJson,omitempty"` // parse the model's reply as JSON
	// web (operation reuses Operation: "search" | "fetch"; url reuses URL; query reuses SearchQuery)
	MaxResults int `json:"maxResults,omitempty"`
	// knowledge_base (operation reuses Operation: "search" | "ingest";
	//   search: SearchQuery + TopK; ingest file: FilePath; ingest text: Content/ContentSource + KBSourceName)
	TopK         int    `json:"topK,omitempty"`
	KBSourceName string `json:"kbSourceName,omitempty"`
	// sql_query
	SQLDriver string `json:"sqlDriver,omitempty"` // currently: postgres
	SQLDSN    string `json:"sqlDsn,omitempty"`    // connection string / DSN
	SQLQuery  string `json:"sqlQuery,omitempty"`
	// filter (keep the array items for which FilterExpression is truthy)
	FilterExpression string `json:"filterExpression,omitempty"`
	// template (render a {{ }} template; optionally wrap the result under TemplateField)
	Template      string `json:"template,omitempty"`
	TemplateField string `json:"templateField,omitempty"`
	// approval (human-in-the-loop gate)
	ApprovalTitle     string `json:"approvalTitle,omitempty"`
	ApprovalMessage   string `json:"approvalMessage,omitempty"`
	ApprovalTimeout   int    `json:"approvalTimeout,omitempty"`   // minutes; 0 = default 60
	ApprovalOnTimeout string `json:"approvalOnTimeout,omitempty"` // "approve" | "reject" (default reject)
	// sub_workflow
	SubWorkflowID string `json:"subWorkflowId,omitempty"`
	// error handling (all executable nodes)
	OnError      string `json:"onError,omitempty"` // stop|continue|retry (default stop)
	RetryCount   int    `json:"retryCount,omitempty"`
	RetryDelayMs int    `json:"retryDelayMs,omitempty"`
	// output binding
	OutputVar string `json:"outputVar,omitempty"`
}

type FormField struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// SwitchCase mirrors the desktop { expression, label, portId } shape. The engine
// evaluates Expression as a boolean; the first truthy case fires its PortID.
type SwitchCase struct {
	Expression string `json:"expression"`
	Label      string `json:"label,omitempty"`
	PortID     string `json:"portId"`
}

// ── Run model (server-side, persisted for the inspector) ──────────────────────

type RunStatus string

const (
	RunPending  RunStatus = "pending"
	RunRunning  RunStatus = "running"
	RunDone     RunStatus = "done"
	RunError    RunStatus = "error"
	RunCanceled RunStatus = "canceled"
	// RunWaiting means the run suspended on a long wait node and will be resumed
	// by the scheduler at the persisted resume time (survives restarts).
	RunWaiting RunStatus = "waiting"
)

// Snapshot captures a run's accumulated state at a suspend point so the run can
// be durably resumed from a wait node.
type Snapshot struct {
	Outputs map[string]any `json:"outputs"`
	Vars    map[string]any `json:"vars"`
}

type WorkflowRun struct {
	ID             string         `json:"id"`
	WorkflowID     string         `json:"workflowId"`
	Status         RunStatus      `json:"status"`
	TriggerType    string         `json:"triggerType"`
	TriggerPayload map[string]any `json:"triggerPayload,omitempty"`
	Error          string         `json:"error,omitempty"`
	StartedAt      time.Time      `json:"startedAt"`
	CompletedAt    *time.Time     `json:"completedAt,omitempty"`
	NodeStates     []NodeRunState `json:"nodeStates,omitempty"`
}

type NodeStatus string

const (
	NodePending NodeStatus = "pending"
	NodeRunning NodeStatus = "running"
	NodeDone    NodeStatus = "done"
	NodeError   NodeStatus = "error"
	NodeSkipped NodeStatus = "skipped"
	NodeWaiting NodeStatus = "waiting"
)

type NodeRunState struct {
	RunID          string     `json:"runId"`
	NodeID         string     `json:"nodeId"`
	Status         NodeStatus `json:"status"`
	Input          any        `json:"input,omitempty"`
	Output         any        `json:"output,omitempty"`
	Error          string     `json:"error,omitempty"`
	AgentSessionID string     `json:"agentSessionId,omitempty"`
	Attempts       int        `json:"attempts,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
}

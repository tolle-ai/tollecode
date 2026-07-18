package session

// Header is the first JSON line of every session JSONL file.
// Must stay byte-compatible with the Python sidecar's schema.
type Header struct {
	Type            string   `json:"type"`
	ID              string   `json:"id"`
	CreatedAt       string   `json:"createdAt"`
	UpdatedAt       string   `json:"updatedAt"`
	WorkspacePath   string   `json:"workspacePath"`
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	Mode            string   `json:"mode"`
	Title           *string  `json:"title"`
	ParentSessionID *string  `json:"parentSessionId"`
	Role            string   `json:"role"`
	AgentName       string   `json:"agentName"`
	Color           string   `json:"color"`
	Status          string   `json:"status"`
	Result          string   `json:"result"`
	ActiveSkills    []string `json:"activeSkills"`
	Todos           []Todo   `json:"todos"`
	MessageCount    int      `json:"messageCount"`
	InputTokens     int      `json:"inputTokens"`
	OutputTokens    int      `json:"outputTokens"`
	// CompactedSummary is set when the session history has been compacted.
	// Non-empty means the executor should use this summary as the initial context
	// and only include messages at index >= CompactedMessageCount.
	CompactedSummary      string `json:"compactedSummary,omitempty"`
	CompactedAt           string `json:"compactedAt,omitempty"`
	// CompactedMessageCount is the len(Messages) at the moment of compaction.
	// Post-compact messages are simply Messages[CompactedMessageCount:].
	// A value of 0 means unset (pre-feature sessions fall back to timestamp comparison).
	CompactedMessageCount int    `json:"compactedMessageCount,omitempty"`
	// SessionSource marks how this session was initiated.
	// "channel" = created by channels_chat; excluded from the dev-mode sessions list.
	SessionSource string `json:"sessionSource,omitempty"`
}

// Message is a subsequent JSON line in a session JSONL file.
type Message struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Thinking  string `json:"thinking,omitempty"`
	Timestamp string `json:"timestamp"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	// ToolUses and Items are stored as raw JSON to avoid schema churn.
	ToolUses    []map[string]any `json:"toolUses,omitempty"`
	Items       []map[string]any `json:"items,omitempty"`
	Interrupted bool             `json:"interrupted,omitempty"`
}

// Todo is stored in the session header.
type Todo struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Content  string `json:"content,omitempty"`
	Status   string `json:"status"`
	Checked  bool   `json:"checked"`
	Priority string `json:"priority"`
}

// APISession is the shape the frontend expects (returned by all session endpoints).
type APISession struct {
	ID              string           `json:"id"`
	CreatedAt       string           `json:"createdAt"`
	UpdatedAt       string           `json:"updatedAt"`
	WorkspacePath   string           `json:"workspacePath"`
	Provider        string           `json:"provider"`
	Model           string           `json:"model"`
	Mode            string           `json:"mode"`
	Title           *string          `json:"title"`
	ParentSessionID *string          `json:"parentSessionId"`
	Role            string           `json:"role"`
	AgentName       string           `json:"agentName"`
	Color           string           `json:"color"`
	Status          string           `json:"status"`
	Result          string           `json:"result"`
	ActiveSkills    []string         `json:"activeSkills"`
	Todos           []map[string]any `json:"todos"`
	MessageCount    *int             `json:"messageCount"`
	Messages        []map[string]any `json:"messages"`
	InputTokens      int              `json:"inputTokens"`
	OutputTokens     int              `json:"outputTokens"`
	CompactedSummary      string `json:"compactedSummary,omitempty"`
	CompactedAt           string `json:"compactedAt,omitempty"`
	CompactedMessageCount int    `json:"compactedMessageCount,omitempty"`
}

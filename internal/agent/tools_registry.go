package agent

import (
	"context"
	"fmt"

	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/mcp"
)

// WorkspaceTools returns the ToolDef list sent to the LLM.
// Pass memoryEnabled=true to include the save_memory tool.
// Pass desktopAvailable=true to include desktop screen-control tools.
// Pass browserAvailable=true to include browser tools (dev mode only).
// workspace is used to load MCP and custom tool definitions.
func WorkspaceTools(ctx context.Context, workspace string, memoryEnabled, desktopAvailable, browserAvailable bool, teamMemberIDs []string) []ai.ToolDef {
	isTeamMode := len(teamMemberIDs) > 0
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	bool_ := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	obj := func(props map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	tools := []ai.ToolDef{
		{
			Name: "read_file",
			Description: "Read the contents of a file in the project workspace, with line numbers. " +
				"Use start_line and end_line to read a specific range (e.g. start_line=1 end_line=100). " +
				"When omitted, returns the first 2000 lines with a note if the file is longer. " +
				"Always prefer reading targeted ranges for large files.",
			InputSchema: obj(map[string]any{
				"path":       str("Relative file path within the workspace"),
				"start_line": map[string]any{"type": "integer", "description": "First line to read (1-based, inclusive). Default: 1."},
				"end_line":   map[string]any{"type": "integer", "description": "Last line to read (1-based, inclusive). Default: start_line + 1999."},
			}, []string{"path"}),
		},
		{
			Name:        "write_file",
			Description: "Write (create or overwrite) a file in the project workspace. Creates parent directories automatically. Requires user permission before writing.",
			InputSchema: obj(map[string]any{
				"path":    str("Relative file path within the workspace"),
				"content": str("Full file content to write"),
			}, []string{"path", "content"}),
		},
		{
			Name: "edit_file",
			Description: "Make a targeted edit to an existing file by replacing an exact string. " +
				"Preferred over write_file when only a small part of the file changes. " +
				"old_string must match exactly (including whitespace/indentation). " +
				"Use read_file first to confirm the exact text. " +
				"If old_string appears more than once, add surrounding lines to make it unique, " +
				"or set replace_all=true. Requires user permission before writing.",
			InputSchema: obj(map[string]any{
				"path":        str("Relative file path within the workspace"),
				"old_string":  str("Exact text to find (must be unique unless replace_all)"),
				"new_string":  str("Replacement text"),
				"replace_all": bool_("Replace every occurrence (default: false)"),
			}, []string{"path", "old_string", "new_string"}),
		},
		{
			Name:        "web_search",
			Description: "Search the web for current information using the Ollama web search API. Returns titles, URLs, and relevant content snippets. Requires an Ollama API key configured in Settings → Providers.",
			InputSchema: obj(map[string]any{
				"query":       str("Search query"),
				"max_results": map[string]any{"type": "integer", "description": "Max results to return (default 5, max 10)"},
			}, []string{"query"}),
		},
		{
			Name:        "web_fetch",
			Description: "Fetch and extract the main content of a webpage using the Ollama web fetch API. Returns the page title, content, and discovered links. Requires an Ollama API key configured in Settings → Providers.",
			InputSchema: obj(map[string]any{
				"url": str("Full URL of the webpage to fetch"),
			}, []string{"url"}),
		},
		{
			Name:        "list_directory",
			Description: "List files and subdirectories at a path in the workspace.",
			InputSchema: obj(map[string]any{
				"path":        str("Relative directory path (empty string for workspace root)"),
				"depth":       map[string]any{"type": "integer", "description": "Recursion depth: 1=immediate children, 2-3=nested. Default 1."},
				"limit":       map[string]any{"type": "integer", "description": "Max entries to return (default 200, max 500)."},
				"offset":      map[string]any{"type": "integer", "description": "Pagination offset (default 0)."},
				"include_all": bool_("Include normally-excluded directories like node_modules, .git, etc. Default false."),
			}, []string{}),
		},
		{
			Name: "search_files",
			Description: "Search for a text or regex pattern across files in the workspace. " +
				"Returns file paths, line numbers, and matching lines. " +
				"Use to locate symbols, find usages, discover files containing specific text, " +
				"or explore an unfamiliar codebase.",
			InputSchema: obj(map[string]any{
				"pattern":        str("Text or regex pattern to search for"),
				"path":           str("Directory to search in (default: workspace root)"),
				"file_pattern":   str("Glob filter for filenames, e.g. '*.py' or '*.ts' (default: all files)"),
				"case_sensitive": bool_("Case-sensitive search (default: true)"),
				"max_results":    map[string]any{"type": "integer", "description": "Max matching lines to return (default 50, max 200)"},
			}, []string{"pattern"}),
		},
		{
			Name:        "create_plan",
			Description: "Create a Markdown plan file in .agent/plans/ for PLAN mode. Requires user permission before writing.",
			InputSchema: obj(map[string]any{
				"name":    str("Plan file name (without .md extension)"),
				"content": str("Markdown plan content"),
			}, []string{"name", "content"}),
		},
		{
			Name: "TodoWrite",
			Description: "Create or update the todo list. Replaces the entire list — include all todos you want to keep. " +
				"Rules: (1) mark a todo in_progress before starting it, (2) mark it completed the moment it is done, " +
				"(3) keep one todo in_progress at a time when working solo (a team lead may have one in_progress per active member), " +
				"(4) finish_task is BLOCKED until every todo is completed. " +
				"Only 'content' is required per todo; 'id' is auto-assigned if you omit it.",
			InputSchema: obj(map[string]any{
				"todos": map[string]any{
					"type":        "array",
					"description": "Full replacement todo list.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":       str("Optional short identifier (e.g. '1', 'setup-db'); auto-assigned if omitted"),
							"content":  str("What needs to be done (required)"),
							"status":   map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}, "description": "Current state (defaults to pending)"},
							"priority": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}, "description": "Importance (defaults to medium)"},
						},
						"required": []string{"content"},
					},
				},
			}, []string{"todos"}),
		},
		{
			Name:        "TodoRead",
			Description: "Read the current todo list. Use before finish_task to verify all work is completed.",
			InputSchema: obj(map[string]any{}, []string{}),
		},
		{
			Name: "finish_task",
			Description: "Signal that the task is fully complete and verified (build passes, tests pass, no errors). " +
				"Provide a concise summary of what was accomplished. " +
				"BLOCKED if any todo is not completed — call TodoRead to check, update with TodoWrite, then retry.",
			InputSchema: obj(map[string]any{
				"summary": str("Summary of completed work"),
			}, []string{"summary"}),
		},
		{
			Name: "task_out_of_scope",
			Description: "Call this immediately when the user's request requires skills or capabilities outside your active skill set. " +
				"Do NOT attempt to proceed or improvise — abort and explain clearly.",
			InputSchema: obj(map[string]any{
				"reason":         str("Clear explanation of what skill is missing and why this task cannot be completed."),
				"required_skill": str("Name of the skill or capability that would be needed to handle this task."),
			}, []string{"reason"}),
		},
		{
			Name: "ask_followup_question",
			Description: "MANDATORY: Ask the user a clarifying question before proceeding whenever the task is ambiguous, has multiple valid approaches, requires a user choice, or scope is unclear. " +
				"NEVER guess or proceed without clarification, and NEVER ask a question as plain message text — always use this tool so the user gets an interactive prompt. " +
				"Ask exactly ONE focused question per call. If you have several things to clarify, call this tool multiple times in sequence (ask, wait for the answer, then ask the next) — never bundle multiple questions into one `question`. " +
				"Provide 2-4 concrete, mutually distinct suggestions when there is a natural set of choices. " +
				"Set multi_choice=true ONLY when several suggestions can sensibly be picked together (e.g. 'Which features do you want?'); leave it false for pick-one questions.",
			InputSchema: obj(map[string]any{
				"question": str("A single, specific question to ask the user. One question only — do not combine multiple questions."),
				"suggestions": map[string]any{
					"type":        "array",
					"description": "Optional pre-written answers the user can pick (2-4 concise, distinct items). Omit when free-text is more appropriate.",
					"items":       map[string]any{"type": "string"},
				},
				"multi_choice": map[string]any{
					"type":        "boolean",
					"description": "True only when the user may select several suggestions at once (multi-select). False (default) for pick-one questions.",
				},
			}, []string{"question"}),
		},
		{
			Name: "run_shell",
			Description: "Execute a shell command in the workspace directory. " +
				"Output is streamed in real-time and returned when the command exits. " +
				"Default timeout is 120 seconds; use the 'timeout' parameter for commands that legitimately take longer (builds, test suites). " +
				"For daemons or dev servers that never exit (e.g. 'npm run dev'), set background=true — the process is started, " +
				"its first 10 seconds of output is captured, then it is left running and control returns to you. " +
				"Requires user permission before running.",
			InputSchema: obj(map[string]any{
				"command":    str("POSIX shell command to execute. Runs via 'sh -c' (a POSIX shell is provided on all platforms, including Windows), so use POSIX syntax regardless of the host OS."),
				"timeout":    str("Maximum wall-clock seconds to wait for the command (default 120, max 600). Increase for slow builds or long test suites."),
				"background": str("Set to true for daemons / dev servers that never exit. The process is started, 10 s of startup output is captured, then it runs in the background while control returns immediately."),
			}, []string{"command"}),
		},
	}

	tools = append(tools, ai.ToolDef{
		Name:        "send_alert",
		Description: "Send an alert notification to connected clients (e.g. mobile app). Use this when you find something important that the user should know about immediately — errors, anomalies, completed tasks, etc.",
		InputSchema: obj(map[string]any{
			"message": str("The alert message to send to the user"),
		}, []string{"message"}),
	})

	// Headless browser tools are only available in dev mode, never in channels.
	if browserAvailable {
		tools = append(tools,
			ai.ToolDef{
				Name:        "browser_navigate",
				Description: "Navigate the browser to a URL. The session keeps ONE persistent tab; the connection auto-heals if the tab is lost. A bare host like 'example.com' is upgraded to https automatically. Waits for the page DOM to be ready before returning.",
				InputSchema: obj(map[string]any{
					"url": str("URL to navigate to (scheme optional — 'example.com' becomes https://example.com)"),
				}, []string{"url"}),
			},
			ai.ToolDef{
				Name:        "browser_screenshot",
				Description: "Take a full-page screenshot of the current browser page (chromedp). Use this — not 'screenshot' — whenever you want to see what a page looks like after browser_navigate.",
				InputSchema: obj(map[string]any{}, []string{}),
			},
			ai.ToolDef{
				Name:        "browser_click",
				Description: "Click an element by CSS selector. Automatically waits for the element to be visible and scrolls it into view first, so you don't need a separate browser_wait_for. Prefer selectors from browser_get_inputs.",
				InputSchema: obj(map[string]any{
					"selector": str("CSS selector of the element to click"),
				}, []string{"selector"}),
			},
			ai.ToolDef{
				Name:        "browser_type",
				Description: "Type text into an input/textarea by CSS selector. Waits for the field, focuses it, and by default REPLACES its contents (framework-safe clear + input/change events fire, so React/Vue-controlled fields update correctly). Set clear=false to append instead. Prefer selectors from browser_get_inputs.",
				InputSchema: obj(map[string]any{
					"selector": str("CSS selector of the input element"),
					"text":     str("Text to type"),
					"clear":    map[string]any{"type": "boolean", "description": "Replace the field's existing value (default true). Set false to append."},
				}, []string{"selector", "text"}),
			},
			ai.ToolDef{
				Name:        "browser_key_press",
				Description: "Press a key in the browser (e.g. 'Enter', 'Tab', 'Escape').",
				InputSchema: obj(map[string]any{
					"key": str("Key name to press"),
				}, []string{"key"}),
			},
			ai.ToolDef{
				Name:        "browser_evaluate",
				Description: "Evaluate JavaScript in the browser and return the result.",
				InputSchema: obj(map[string]any{
					"script": str("JavaScript expression or statement to evaluate"),
				}, []string{"script"}),
			},
			ai.ToolDef{
				Name:        "browser_get_content",
				Description: "Get the inner HTML of an element (default: body) from the current page.",
				InputSchema: obj(map[string]any{
					"selector": str("CSS selector (default: 'body')"),
				}, []string{}),
			},
			ai.ToolDef{
				Name:        "browser_wait_for",
				Description: "Wait for a CSS selector to become visible on the page.",
				InputSchema: obj(map[string]any{
					"selector": str("CSS selector to wait for"),
					"timeout":  map[string]any{"type": "number", "description": "Timeout in seconds (default: 10)"},
				}, []string{"selector"}),
			},
			ai.ToolDef{
				Name:        "browser_close",
				Description: "Close the browser session for this agent session.",
				InputSchema: obj(map[string]any{}, []string{}),
			},
			ai.ToolDef{
				Name: "browser_get_inputs",
				Description: "Return all interactive form elements on the current page: " +
					"inputs, textareas, selects, and submit buttons. " +
					"Each entry includes 'selector' (use with browser_click/browser_type), " +
					"'type', 'label', 'placeholder', and current 'value'. " +
					"Call this before trying to fill a form — do NOT guess selectors from HTML.",
				InputSchema: obj(map[string]any{}, []string{}),
			},
		)
	}

	tools = append(tools,
		ai.ToolDef{
			Name: "search_knowledge_base",
			Description: "Perform a semantic search over the workspace knowledge base. " +
				"Returns the most relevant text chunks from indexed files and documents. " +
				"Use ingest_document first if results are empty.",
			InputSchema: obj(map[string]any{
				"query": str("Natural-language question or search phrase"),
				"top_k": map[string]any{"type": "integer", "description": "Max results to return (default 5, max 20)"},
			}, []string{"query"}),
		},
		ai.ToolDef{
			Name: "ingest_document",
			Description: "Index a file from the workspace into the knowledge base for semantic search. " +
				"Supports code, Markdown, text, config files, CSV, and similar text formats. " +
				"After ingesting, use search_knowledge_base to query the content.",
			InputSchema: obj(map[string]any{
				"path": str("Relative file path within the workspace to ingest"),
			}, []string{"path"}),
		},
	)

	if isTeamMode {
		// Team mode: lead delegates to pre-configured members, no ad-hoc spawning.
		tools = append(tools,
			ai.ToolDef{
				Name: "delegate_task",
				Description: "Delegate a task to a team member. " +
					"ONLY assign tasks that match the agent's role and skills — check the team member list in your instructions. " +
					"Always set task_label so other agents can declare dependencies. " +
					"Use wait_for to enforce ordering: the system blocks the dependent agent until listed labels complete. " +
					"For sequential work: delegate A (label=\"a\"), then delegate B with wait_for=[\"a\"]. " +
					"For parallel work: delegate A and B in the same response (no wait_for), then call wait_for_team. " +
					"You MUST NOT do any specialised work yourself — plan, delegate, and synthesise only.",
				InputSchema: obj(map[string]any{
					"agent_id":   str("ID of the team member whose role and skills match this task"),
					"task":       str("Clear, complete task description — the agent will implement exactly this"),
					"context":    str("Optional: prior outputs or background the agent needs to start (e.g. output from a previous agent)"),
					"task_label": str("Short unique label for this delegation used in other agents' wait_for (e.g. \"coding\", \"testing\", \"review\")"),
					"wait_for": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "task_labels that must complete before this agent starts. The system blocks automatically — no manual wait needed.",
					},
				}, []string{"agent_id", "task", "task_label"}),
			},
			ai.ToolDef{
				Name:        "wait_for_team",
				Description: "Block until ALL currently-delegated team members complete. Returns all their outputs. Use for parallel batches — after you've delegated multiple independent tasks at once.",
				InputSchema: obj(map[string]any{}, []string{}),
			},
			ai.ToolDef{
				Name: "wait_for_agent",
				Description: "Block until ONE specific delegated agent completes. Returns only that agent's output. " +
					"Use this for sequential workflows: delegate_task → wait_for_agent → pass output to next delegate_task. " +
					"agent_id is the 'delegated_to' session ID returned by delegate_task.",
				InputSchema: obj(map[string]any{
					"agent_id": str("The 'delegated_to' session ID returned by the delegate_task call you are waiting on"),
				}, []string{"agent_id"}),
			},
		)
	} else {
		// Solo mode: agent spawns ad-hoc sub-agents for parallel work.
		tools = append(tools,
			ai.ToolDef{
				Name: "spawn_sub_agent",
				Description: "Spawn an independent sub-agent to handle a subtask in parallel. " +
					"Choose a descriptive name and role that reflects what this agent will do. " +
					"Pass agent_id to use a configured agent's skills and model instead. " +
					"Returns immediately. Call wait_for_subagents to collect all results.",
				InputSchema: obj(map[string]any{
					"message":  str("Full task description for the sub-agent"),
					"name":     str("Short human-readable name for this sub-agent (e.g. 'Code Reviewer', 'Test Writer', 'Database Analyst')"),
					"role":     str("One-sentence description of this sub-agent's role and focus area"),
					"agent_id": str("Optional: ID of a configured agent — uses that agent's skills, model, and system prompt"),
				}, []string{"message", "name"}),
			},
			ai.ToolDef{
				Name:        "wait_for_subagents",
				Description: "Block until all spawned sub-agents complete. Returns each sub-agent's output.",
				InputSchema: obj(map[string]any{}, []string{}),
			},
		)
	}
	// Email tools — only registered when .agent/email_config.json exists.
	if _, err := loadEmailConfig(workspace); err == nil {
		tools = append(tools,
			ai.ToolDef{
				Name:        "read_email",
				Description: "Read emails from the configured mailbox via IMAP.",
				InputSchema: obj(map[string]any{
					"folder":      str("Mailbox folder to read (default: INBOX)"),
					"limit":       map[string]any{"type": "integer", "description": "Max messages to return (default 10, max 50)"},
					"unread_only": bool_("Return only unread messages (default false)"),
					"search":      str("Optional text to search for in message body/subject"),
				}, []string{}),
			},
			ai.ToolDef{
				Name:        "send_email",
				Description: "Send an email via SMTP using the configured mail account.",
				InputSchema: obj(map[string]any{
					"to":      str("Recipient address(es), comma-separated"),
					"subject": str("Email subject line"),
					"body":    str("Plain-text email body"),
					"cc":      str("CC address(es), comma-separated (optional)"),
					"bcc":     str("BCC address(es), comma-separated (optional)"),
				}, []string{"to", "subject", "body"}),
			},
		)
	}

	// Calendar tools — only registered when .agent/calendar_token.json exists.
	if _, err := loadCalendarToken(workspace); err == nil {
		tools = append(tools,
			ai.ToolDef{
				Name:        "list_events",
				Description: "List upcoming calendar events from Google Calendar.",
				InputSchema: obj(map[string]any{
					"calendar_id": str("Calendar ID (default: primary)"),
					"from":        str("Start time in RFC3339 format (default: now)"),
					"to":          str("End time in RFC3339 format (default: 7 days from now)"),
					"max_results": map[string]any{"type": "integer", "description": "Max events to return (default 10)"},
				}, []string{}),
			},
			ai.ToolDef{
				Name:        "create_event",
				Description: "Create a new event in Google Calendar.",
				InputSchema: obj(map[string]any{
					"title":       str("Event title/summary"),
					"start":       str("Start time in RFC3339 format (e.g. 2026-06-10T14:00:00Z)"),
					"end":         str("End time in RFC3339 format"),
					"description": str("Optional event description"),
					"location":    str("Optional location"),
					"calendar_id": str("Calendar ID (default: primary)"),
				}, []string{"title", "start", "end"}),
			},
		)
	}

	if memoryEnabled {
		tools = append(tools, ai.ToolDef{
			Name: "save_memory",
			Description: "Save an important fact, decision, pattern, or discovery to long-term memory. " +
				"Use when you learn something worth remembering across sessions: " +
				"key architectural decisions, recurring preferences, non-obvious gotchas, or solved problems. " +
				"Keep text concise (1-3 sentences).",
			InputSchema: obj(map[string]any{
				"text": str("The memory to save (1-3 concise sentences)"),
			}, []string{"text"}),
		})
	}
	if desktopAvailable {
		tools = append(tools,
			ai.ToolDef{
				Name:        "screenshot",
				Description: "Capture a screenshot of the physical desktop screen. Use ONLY for screen-control tasks (mouse/keyboard automation). Do NOT use this after browser_navigate — use browser_screenshot instead.",
				InputSchema: obj(map[string]any{}, []string{}),
			},
			ai.ToolDef{
				Name:        "mouse_move",
				Description: "Move the mouse cursor to an absolute screen position.",
				InputSchema: obj(map[string]any{
					"x": map[string]any{"type": "integer", "description": "X coordinate in pixels"},
					"y": map[string]any{"type": "integer", "description": "Y coordinate in pixels"},
				}, []string{"x", "y"}),
			},
			ai.ToolDef{
				Name:        "mouse_click",
				Description: "Click the mouse at the current or specified position.",
				InputSchema: obj(map[string]any{
					"button": str("Mouse button: 'left', 'right', or 'middle' (default: left)"),
					"double": bool_("Double-click (default: false)"),
					"x":      map[string]any{"type": "integer", "description": "Optional X coordinate"},
					"y":      map[string]any{"type": "integer", "description": "Optional Y coordinate"},
				}, []string{}),
			},
			ai.ToolDef{
				Name:        "keyboard_type",
				Description: "Type text using the keyboard.",
				InputSchema: obj(map[string]any{
					"text": str("Text to type"),
				}, []string{"text"}),
			},
			ai.ToolDef{
				Name:        "key_press",
				Description: "Press a keyboard key or key combination (e.g. 'enter', 'ctrl+c', 'cmd+v').",
				InputSchema: obj(map[string]any{
					"key": str("Key or combination to press"),
				}, []string{"key"}),
			},
			ai.ToolDef{
				Name:        "scroll_mouse",
				Description: "Scroll the mouse wheel up or down at the current cursor position.",
				InputSchema: obj(map[string]any{
					"direction": str("Scroll direction: 'up' or 'down'"),
					"amount":    map[string]any{"type": "integer", "description": "Number of scroll units (default 3)"},
				}, []string{"direction"}),
			},
		)
	}

	// Append MCP server tools (prefixed mcp__<server>__<tool>).
	if workspace != "" {
		mcpTools := mcp.Global.Get(workspace).Tools(ctx)
		tools = append(tools, mcpTools...)
	}

	// Append custom workspace tools (prefixed custom__<name>).
	if workspace != "" {
		customTools, _ := mcp.LoadCustomTools(workspace)
		for _, ct := range customTools {
			if !ct.Enabled {
				continue
			}
			schema := ct.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, ai.ToolDef{
				Name:        fmt.Sprintf("custom__%s", ct.Name),
				Description: ct.Description,
				InputSchema: schema,
			})
		}
	}

	return tools
}

// Dispatch executes a named tool and returns (textContent, imageData, isError).
// imageData is a base64-encoded image; empty string means no image.
func Dispatch(ctx context.Context, cfg *Config, toolName string, inp map[string]any) (string, string, bool) {
	// Wait out any permission prompt in flight anywhere in this agent's tree
	// before running ANY tool — including ones that never prompt themselves —
	// so a pending prompt genuinely blocks the whole agent, not just the call
	// that triggered it. See awaitPermissionGate's doc comment for why this is
	// necessary in addition to the per-tool checkPermission calls below.
	cfg.awaitPermissionGate()

	// Pro edition hook: intercept before the built-in switch.
	if cfg.ExtDispatch != nil {
		if out, img, isErr, handled := cfg.ExtDispatch(ctx, cfg, toolName, inp); handled {
			return out, img, isErr
		}
	}
	ws := cfg.Workspace
	sid := cfg.SessionID
	switch toolName {
	case "web_search":
		return toolWebSearch(cfg, inp)
	case "web_fetch":
		return toolWebFetch(cfg, inp)
	case "read_file":
		return toolReadFile(ws, inp), "", false
	case "write_file":
		return toolWriteFile(ctx, cfg, inp), "", false
	case "edit_file":
		return toolEditFile(ctx, cfg, inp), "", false
	case "list_directory":
		return toolListDirectory(ws, inp), "", false
	case "search_files":
		return toolSearchFiles(ws, inp), "", false
	case "create_plan":
		return toolCreatePlan(ctx, cfg, inp), "", false
	case "TodoWrite":
		return toolTodoWrite(ws, sid, inp, len(cfg.TeamMemberIDs) > 0), "", false
	case "TodoRead":
		return toolTodoRead(ws, sid), "", false
	case "send_alert":
		return toolSendAlert(cfg, inp), "", false
	case "finish_task", "finish":
		return toolFinishTask(cfg, inp, len(cfg.TeamMemberIDs) > 0), "", false
	case "task_out_of_scope":
		return toolTaskOutOfScope(ws, sid, inp), "", false
	case "ask_followup_question":
		return toolAskFollowupQuestion(ctx, cfg, inp)
	case "run_shell":
		out, isErr := toolRunShell(ctx, cfg, inp)
		return out, "", isErr
	case "screenshot":
		return toolScreenshot(ctx, cfg)
	case "mouse_move":
		out, isErr := toolMouseMove(ctx, cfg, inp)
		return out, "", isErr
	case "mouse_click":
		out, isErr := toolMouseClick(ctx, cfg, inp)
		return out, "", isErr
	case "keyboard_type":
		out, isErr := toolKeyboardType(ctx, cfg, inp)
		return out, "", isErr
	case "key_press":
		out, isErr := toolKeyPress(ctx, cfg, inp)
		return out, "", isErr
	case "scroll_mouse":
		out, isErr := toolScrollMouse(ctx, cfg, inp)
		return out, "", isErr
	case "browser_navigate":
		out, isErr := toolBrowserNavigate(ctx, cfg, inp)
		return out, "", isErr
	case "browser_screenshot":
		return toolBrowserScreenshot(ctx, cfg)
	case "browser_click":
		out, isErr := toolBrowserClick(ctx, cfg, inp)
		return out, "", isErr
	case "browser_type":
		out, isErr := toolBrowserType(ctx, cfg, inp)
		return out, "", isErr
	case "browser_key_press":
		out, isErr := toolBrowserKeyPress(ctx, cfg, inp)
		return out, "", isErr
	case "browser_evaluate":
		out, isErr := toolBrowserEvaluate(ctx, cfg, inp)
		return out, "", isErr
	case "browser_get_content":
		out, isErr := toolBrowserGetContent(ctx, cfg, inp)
		return out, "", isErr
	case "browser_wait_for":
		out, isErr := toolBrowserWaitFor(ctx, cfg, inp)
		return out, "", isErr
	case "browser_close":
		out, isErr := toolBrowserClose(ctx, cfg)
		return out, "", isErr
	case "browser_get_inputs":
		out, isErr := toolBrowserGetInputs(ctx, cfg)
		return out, "", isErr
	case "spawn_sub_agent":
		out, isErr := toolSpawnSubAgent(ctx, cfg, inp)
		return out, "", isErr
	case "wait_for_subagents":
		out, isErr := toolWaitForSubAgents(ctx, cfg)
		return out, "", isErr
	case "delegate_task":
		out, isErr := toolDelegateTask(ctx, cfg, inp)
		return out, "", isErr
	case "wait_for_team":
		out, isErr := toolWaitForTeam(ctx, cfg)
		return out, "", isErr
	case "wait_for_agent":
		out, isErr := toolWaitForAgent(ctx, cfg, inp)
		return out, "", isErr
	case "read_email":
		return toolReadEmail(ws, inp), "", false
	case "send_email":
		return toolSendEmail(ws, inp), "", false
	case "list_events":
		return toolListEvents(ws, inp), "", false
	case "create_event":
		return toolCreateEvent(ws, inp), "", false
	case "save_memory":
		return toolSaveMemory(ws, inp), "", false
	case "search_knowledge_base":
		return toolSearchKnowledgeBase(ctx, cfg, inp), "", false
	case "ingest_document":
		return toolIngestDocument(ctx, cfg, inp), "", false
	default:
		// Route MCP tools (mcp__<server>__<tool>)
		if mcp.IsMCPTool(toolName) {
			out, isErr := mcp.Global.Get(ws).Dispatch(ctx, toolName, inp)
			return out, "", isErr
		}
		// Route custom workspace tools (custom__<name>)
		if len(toolName) > 7 && toolName[:7] == "custom__" {
			out, isErr := toolRunCustom(ctx, cfg, toolName[7:], inp)
			return out, "", isErr
		}
		return "Unknown tool: " + toolName, "", true
	}
}

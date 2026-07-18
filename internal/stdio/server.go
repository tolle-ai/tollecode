package stdio

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/tolle-ai/tollecode/internal/httpserver"
	"github.com/tolle-ai/tollecode/internal/mcp"
)

// Run starts the JSON-over-stdio server. Blocks until stdin is closed.
// Called from main when --stdio flag is set.
func Run() {
	// Lite desktop: auto-connect locally-running MCP backends (e.g. Blender, Unity).
	mcp.EnableAutoDiscovery = true

	state := newServerState()

	// Announce readiness before reading any commands.
	EmitType("server_started")

	// Start HTTP/WebSocket server and announce the port.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if port, err := httpserver.Start(ctx); err == nil {
		Emit(map[string]any{"type": "ws_ready", "port": port})
	} else {
		fmt.Fprintf(os.Stderr, "[sidecar] HTTP server failed to start: %v\n", err)
	}

	// Background scheduler: fires due scheduled todo tasks every 30 s.
	StartTodoScheduler(state)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var cmd map[string]any
		if err := json.Unmarshal(line, &cmd); err != nil {
			fmt.Fprintf(os.Stderr, "[sidecar] bad JSON on stdin: %v\n", err)
			continue
		}

		dispatch(state, cmd)
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[sidecar] stdin error: %v\n", err)
	}
}

func dispatch(state *ServerState, cmd map[string]any) {
	typ, _ := cmd["type"].(string)
	switch typ {
	case "init":
		handleInit(state, cmd)
	case "kv_get_all":
		handleKVGetAll(state, cmd)
	case "kv_set":
		handleKVSet(state, cmd)
	case "kv_remove":
		handleKVRemove(state, cmd)
	case "get_providers":
		handleGetProviders(state, cmd)
	case "get_sessions":
		go handleGetSessions(state, cmd)
	case "get_active_sessions":
		handleGetActiveSessions(state, cmd)
	case "new_session":
		handleNewSession(state, cmd)
	case "load_session":
		go handleLoadSession(state, cmd)
	case "send_message":
		handleSendMessage(state, cmd)
	case "cancel":
		handleCancel(state, cmd)
	case "delete_session":
		handleDeleteSession(state, cmd)
	case "update_session":
		handleUpdateSession(state, cmd)
	case "append_message":
		handleAppendMessage(state, cmd)
	case "switch_model":
		handleSwitchModel(state, cmd)
	case "get_earlier_messages":
		go handleGetEarlierMessages(state, cmd)
	case "set_mode":
		handleSetMode(state, cmd)
	case "set_thinking":
		handleSetThinking(state, cmd)
	case "set_shell_auto_allow":
		handleSetShellAutoAllow(state, cmd)
	case "set_session_limit":
		handleSetSessionLimit(state, cmd)
	case "tool_permission":
		handleToolPermission(state, cmd)
	case "memory_permission":
		handleMemoryPermission(state, cmd)
	case "clarification_response":
		handleClarificationResponse(state, cmd)
	case "get_memory":
		handleGetMemoryFull(state, cmd)
	case "delete_memory":
		handleDeleteMemoryFull(state, cmd)
	case "toggle_memory":
		handleToggleMemoryFull(state, cmd)
	case "query_memory":
		handleQueryMemory(state, cmd)
	case "add_memory":
		handleAddMemoryFull(state, cmd)
	case "memory_status":
		handleMemoryStatus(state, cmd)
	case "memory_rebuild":
		handleMemoryRebuild(state, cmd)
	case "memory_search":
		handleMemorySearch(state, cmd)
	case "get_skills":
		handleGetSkills(state, cmd)
	case "set_skill":
		handleSetSkill(state, cmd)
	case "skills_list":
		handleSkillsList(state, cmd)
	case "skills_create":
		handleSkillsCreate(state, cmd)
	case "skills_delete":
		handleSkillsDelete(state, cmd)
	case "skills_session_get":
		handleSkillsSessionGet(state, cmd)
	case "skills_session_activate":
		handleSkillsSessionActivate(state, cmd)
	case "skills_session_deactivate":
		handleSkillsSessionDeactivate(state, cmd)
	case "skills_session_clear":
		handleSkillsSessionClear(state, cmd)
	case "agents_list":
		handleAgentsList(state, cmd)
	case "agents_create":
		handleAgentsCreate(state, cmd)
	case "agents_update":
		handleAgentsUpdate(state, cmd)
	case "agents_delete":
		handleAgentsDelete(state, cmd)
	case "agents_duplicate":
		handleAgentsDuplicate(state, cmd)
	// ── Teams ─────────────────────────────────────────────────────────────────
	case "get_teams":
		handleGetTeams(state, cmd)
	case "sync_teams":
		handleSyncTeams(state, cmd)
	case "upsert_team":
		handleUpsertTeam(state, cmd)
	case "delete_team":
		handleDeleteTeam(state, cmd)
	case "get_analytics":
		handleGetAnalytics(state, cmd)
	case "get_usage":
		handleGetUsage(state, cmd)
	case "get_todos":
		handleGetTodos(state, cmd)
	// ── Server-side todo task scheduling ──────────────────────────────────────
	case "add_todo_task":
		handleAddTodoTask(state, cmd)
	case "list_todo_tasks":
		go handleListTodoTasks(state, cmd)
	case "remove_todo_task":
		handleRemoveTodoTask(state, cmd)
	case "start_todo_task":
		handleStartTodoTask(state, cmd)
	case "cancel_todo_task":
		handleCancelTodoTask(state, cmd)
	case "update_todo_task":
		handleUpdateTodoTask(state, cmd)
	case "rerun_todo_task":
		handleRerunTodoTask(state, cmd)
	case "get_subagent_session":
		go handleGetSubagentSession(state, cmd)
	case "get_session_summary":
		handleGetSessionSummary(state, cmd)
	case "restore_to_message":
		handleRestoreToMessage(state, cmd)
	case "compact_session":
		go handleCompactSession(state, cmd)
	case "channels_list":
		handleChannelsList(state, cmd)
	case "channels_create":
		handleChannelsCreate(state, cmd)
	case "channels_get_messages":
		handleChannelsGetMessages(state, cmd)
	case "channels_send_message":
		handleChannelsSendMessage(state, cmd)
	case "channels_delete_message":
		handleChannelsDeleteMessage(state, cmd)
	case "channels_delete":
		handleChannelsDelete(state, cmd)
	case "channels_patch":
		handleChannelsPatch(state, cmd)
	case "channels_notify_done":
		handleChannelsNotifyDone(state, cmd)
	case "channels_command":
		handleChannelsCommand(state, cmd)
	case "channels_chat":
		go handleChannelsChat(state, cmd)
	case "mcp_list":
		handleMCPList(state, cmd)
	case "mcp_add":
		handleMCPAdd(state, cmd)
	case "mcp_remove":
		handleMCPRemove(state, cmd)
	case "mcp_list_tools":
		handleMCPListTools(state, cmd)
	case "mcp_reload":
		handleMCPReload(state, cmd)
	case "custom_tools_list":
		handleCustomToolsList(state, cmd)
	case "custom_tools_save":
		handleCustomToolsSave(state, cmd)
	case "custom_tools_delete":
		handleCustomToolsDelete(state, cmd)
	case "knowledge_ingest":
		handleKnowledgeIngest(state, cmd)
	case "knowledge_ingest_workspace":
		handleKnowledgeIngestWorkspace(state, cmd)
	case "knowledge_list":
		handleKnowledgeList(state, cmd)
	case "knowledge_delete":
		handleKnowledgeDelete(state, cmd)
	case "knowledge_search":
		handleKnowledgeSearch(state, cmd)
	case "workspace_files":
		handleWorkspaceFiles(state, cmd)
	case "workspace_ls":
		handleWorkspaceLs(state, cmd)
	case "workspace_search":
		handleWorkspaceSearch(state, cmd)
	case "workspace_grep":
		go handleWorkspaceGrep(state, cmd)
	case "workspace_file":
		handleWorkspaceFile(state, cmd)
	case "fs_browse":
		go handleFSBrowse(state, cmd)
	// ── Lite 2FA (shared by desktop + web) ────────────────────────────────────
	case "auth_begin_login":
		handleAuthBeginLogin(state, cmd)
	case "auth_register":
		handleAuthRegister(state, cmd)
	case "auth_verify_registration":
		handleAuthVerifyRegistration(state, cmd)
	case "auth_verify_login":
		handleAuthVerifyLogin(state, cmd)
	case "auth_validate_session":
		handleAuthValidateSession(state, cmd)
	case "auth_get_local_user":
		handleAuthGetLocalUser(state, cmd)
	case "auth_sign_out":
		handleAuthSignOut(state, cmd)
	// ── Server access key (web-mode "door"; access_key_status is answered
	//    pre-auth in the web bridge, not here) ──────────────────────────────────
	case "access_key_get":
		handleAccessKeyGet(state, cmd)
	case "access_key_generate":
		handleAccessKeyGenerate(state, cmd)
	case "access_key_disable":
		handleAccessKeyDisable(state, cmd)
	case "workspace_write_file":
		handleWorkspaceWriteFile(state, cmd)
	case "workspace_create":
		handleWorkspaceCreate(state, cmd)
	case "workspace_delete":
		handleWorkspaceDelete(state, cmd)
	case "workspace_rename":
		handleWorkspaceRename(state, cmd)
	case "workspace_copy":
		handleWorkspaceCopy(state, cmd)
	case "workspace_reveal":
		handleWorkspaceReveal(state, cmd)
	case "workspace_git_info":
		go handleWorkspaceGitInfo(state, cmd)
	case "workspace_git_original":
		go handleWorkspaceGitOriginal(state, cmd)
	case "workspace_git_branches":
		go handleWorkspaceGitBranches(state, cmd)
	case "workspace_git_checkout":
		go handleWorkspaceGitCheckout(state, cmd)
	case "workspace_git_create_branch":
		go handleWorkspaceGitCreateBranch(state, cmd)
	case "workspace_git_pull":
		go handleWorkspaceGitPull(state, cmd)
	case "workspace_git_push":
		go handleWorkspaceGitPush(state, cmd)
	case "workspace_generate_commit_msg":
		go handleWorkspaceGenCommitMsg(state, cmd)
	case "workspace_git_commit":
		go handleWorkspaceGitCommit(state, cmd)
	case "register_workspace":
		handleRegisterWorkspace(state, cmd)
	case "terminal_spawn":
		handleTerminalSpawn(state, cmd)
	case "terminal_status":
		handleTerminalStatus(state, cmd)
	case "terminal_kill":
		handleTerminalKill(state, cmd)
	case "terminal_input":
		handleTerminalInput(state, cmd)
	case "terminal_list":
		handleTerminalList(state, cmd)
	case "terminal_pty_create":
		go handleTerminalPTYCreate(state, cmd)
	case "terminal_pty_input":
		handleTerminalPTYInput(state, cmd)
	case "terminal_pty_resize":
		handleTerminalPTYResize(state, cmd)
	case "terminal_pty_close":
		handleTerminalPTYClose(state, cmd)
	case "save_providers":
		handleSaveProviders(state, cmd)
	case "discover_providers":
		go handleDiscoverProviders(state, cmd)
	case "get_model_info":
		go handleGetModelInfo(state, cmd)
	case "screenshot_response":
		// Tauri writes this back to our stdin after capturing the screen.
		requestID, _ := cmd["requestId"].(string)
		state.deliverScreenshot(requestID, cmd)
	case "system_permission_response":
		handleSystemPermissionResponse(state, cmd)
	case "check_system_permission":
		handleCheckSystemPermission(state, cmd)
	case "configure_provider":
		handleConfigureProvider(state, cmd)
	case "get_sidecar_settings":
		handleGetSidecarSettings(state, cmd)
	case "set_sidecar_settings":
		handleSetSidecarSettings(state, cmd)
	case "iteration_confirm_response":
		handleIterationConfirmResponse(state, cmd)
	default:
		fmt.Fprintf(os.Stderr, "[sidecar] unknown command type: %q\n", typ)
	}
}

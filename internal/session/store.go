// Package session provides JSONL-based session storage compatible with the Python sidecar.
// Format: first line = Header record, subsequent lines = Message records.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrMalformed is returned when a session file exists but its header cannot be decoded.
var ErrMalformed = errors.New("session file is malformed")

const (
	sessionsDir  = ".agent/sessions"
	subagentsDir = ".agent/sessions/subagents"
	outputLimit  = 4000 // chars — truncate tool outputs before sending to frontend
)

// ── cache ─────────────────────────────────────────────────────────────────────

type cacheEntry struct {
	mtime    float64
	sessions []APISession
}

var (
	cacheMu sync.RWMutex
	cache   = map[string]cacheEntry{}
)

func invalidateCache(wsPath string) {
	cacheMu.Lock()
	delete(cache, sessionsPath(wsPath))
	cacheMu.Unlock()
}

// ── path helpers ──────────────────────────────────────────────────────────────

func sessionsPath(wsPath string) string {
	return filepath.Join(wsPath, sessionsDir)
}

func sessionFile(wsPath, id string, isSubagent bool) string {
	dir := sessionsPath(wsPath)
	if isSubagent {
		dir = filepath.Join(wsPath, subagentsDir)
	}
	return filepath.Join(dir, id+".jsonl")
}

func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// ── JSONL I/O ─────────────────────────────────────────────────────────────────

func readHeader(path string) (*Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var h Header
	if err := dec.Decode(&h); err != nil {
		return nil, err
	}
	return &h, nil
}

func appendLine(path string, v any) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// atomicUpdateHeader rewrites the header line of a JSONL file in-place,
// preserving all subsequent message lines. Crash-safe via tmp→rename.
func atomicUpdateHeader(path string, h *Header, extraLine any) error {
	h.UpdatedAt = now()

	tmp := path + ".tmp"
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	dst, err := os.Create(tmp)
	if err != nil {
		src.Close()
		return err
	}

	// Skip the old header line
	dec := json.NewDecoder(src)
	dec.Buffered() // ensure buffered
	var discard json.RawMessage
	if err := dec.Decode(&discard); err != nil && err != io.EOF {
		src.Close()
		dst.Close()
		os.Remove(tmp)
		return err
	}

	// Write updated header
	enc := json.NewEncoder(dst)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h); err != nil {
		src.Close()
		dst.Close()
		os.Remove(tmp)
		return err
	}

	// Copy remaining body (already-buffered bytes first, then rest of file)
	if _, err := io.Copy(dst, dec.Buffered()); err == nil {
		_, _ = io.Copy(dst, src)
	}
	src.Close()

	// Append new line (e.g. a new message)
	if extraLine != nil {
		enc2 := json.NewEncoder(dst)
		enc2.SetEscapeHTML(false)
		_ = enc2.Encode(extraLine)
	}

	dst.Close()
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ── Public API ────────────────────────────────────────────────────────────────

// Create writes a new session header file and returns its API representation.
func Create(wsPath, provider, model, mode string, opts ...CreateOption) (APISession, error) {
	cfg := createConfig{role: "", agentName: "", color: ""}
	for _, o := range opts {
		o(&cfg)
	}

	if err := ensureDir(sessionsPath(wsPath)); err != nil {
		return APISession{}, err
	}
	if cfg.parentID != "" {
		if err := ensureDir(filepath.Join(wsPath, subagentsDir)); err != nil {
			return APISession{}, err
		}
	}

	t := now()
	h := Header{
		Type:          "session",
		ID:            uuid.NewString(),
		CreatedAt:     t,
		UpdatedAt:     t,
		WorkspacePath: wsPath,
		Provider:      provider,
		Model:         model,
		Mode:          mode,
		Role:          cfg.role,
		AgentName:     cfg.agentName,
		Color:         cfg.color,
		ActiveSkills:  cfg.skills,
		Todos:         []Todo{},
		MessageCount:  0,
		SessionSource: cfg.sessionSource,
	}
	if cfg.parentID != "" {
		h.ParentSessionID = &cfg.parentID
	}

	isSubagent := cfg.parentID != ""
	path := sessionFile(wsPath, h.ID, isSubagent)
	if err := appendLine(path, h); err != nil {
		return APISession{}, err
	}
	invalidateCache(wsPath)
	return headerToAPI(h, nil), nil
}

type createConfig struct {
	parentID      string
	role          string
	agentName     string
	color         string
	skills        []string
	sessionSource string
}

type CreateOption func(*createConfig)

func WithParent(id string) CreateOption      { return func(c *createConfig) { c.parentID = id } }
func WithRole(r string) CreateOption         { return func(c *createConfig) { c.role = r } }
func WithAgentName(n string) CreateOption    { return func(c *createConfig) { c.agentName = n } }
func WithColor(col string) CreateOption      { return func(c *createConfig) { c.color = col } }
func WithSkills(s []string) CreateOption     { return func(c *createConfig) { c.skills = s } }
// WithChannelSession marks the session as channel-initiated so it is excluded
// from the dev-mode sessions list and the active-sessions registry.
func WithChannelSession() CreateOption { return func(c *createConfig) { c.sessionSource = "channel" } }

// Load reads an entire session (all messages). Prefer LoadTail for the UI path.
func Load(wsPath, id string) (*APISession, error) {
	for _, isSubagent := range []bool{false, true} {
		path := sessionFile(wsPath, id, isSubagent)
		s, err := loadFromPath(path)
		if err == nil {
			return s, nil
		}
	}
	return nil, fmt.Errorf("session not found: %s", id)
}

// LoadPage returns up to n messages ending at (total - skip), and whether older
// messages still exist before that window. Uses a full file scan.
func LoadPage(wsPath, id string, skip, n int) (*APISession, bool, error) {
	for _, isSubagent := range []bool{false, true} {
		path := sessionFile(wsPath, id, isSubagent)
		s, hasMore, err := loadPageFromPath(path, skip, n)
		if err == nil {
			return s, hasMore, nil
		}
		if errors.Is(err, ErrMalformed) {
			return nil, false, ErrMalformed
		}
	}
	return nil, false, fmt.Errorf("session not found: %s", id)
}

func loadPageFromPath(path string, skip, n int) (*APISession, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	var h Header
	if err := dec.Decode(&h); err != nil {
		return nil, false, ErrMalformed
	}
	if h.ID == "" {
		return nil, false, ErrMalformed
	}

	var allMsgs []map[string]any
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if rec["type"] == "message" {
			allMsgs = append(allMsgs, truncateMessage(rec))
		}
	}

	total := len(allMsgs)
	end := total - skip
	if end <= 0 {
		s := headerToAPI(h, []map[string]any{})
		return &s, false, nil
	}
	start := end - n
	hasMore := start > 0
	if start < 0 {
		start = 0
	}
	msgs := allMsgs[start:end]
	s := headerToAPI(h, msgs)
	return &s, hasMore, nil
}

// LoadTail reads a session returning only the last n messages and the total count.
// Returns ErrMalformed if the file exists but the header cannot be decoded.
func LoadTail(wsPath, id string, n int) (*APISession, int, error) {
	for _, isSubagent := range []bool{false, true} {
		path := sessionFile(wsPath, id, isSubagent)
		s, total, err := loadTailFromPath(path, n)
		if err == nil {
			return s, total, nil
		}
		if errors.Is(err, ErrMalformed) {
			return nil, 0, ErrMalformed
		}
	}
	return nil, 0, fmt.Errorf("session not found: %s", id)
}

// List returns top-level sessions sorted by updatedAt desc, using dir-mtime cache.
func List(wsPath string) ([]APISession, error) {
	dir := sessionsPath(wsPath)
	info, err := os.Stat(dir)
	if err != nil {
		return []APISession{}, nil
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9

	cacheMu.RLock()
	if e, ok := cache[dir]; ok && e.mtime == mtime {
		out := make([]APISession, len(e.sessions))
		copy(out, e.sessions)
		cacheMu.RUnlock()
		return out, nil
	}
	cacheMu.RUnlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return []APISession{}, nil
	}

	type mtimeEntry struct {
		mtime float64
		path  string
	}
	var files []mtimeEntry
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, mtimeEntry{
			mtime: float64(fi.ModTime().UnixNano()) / 1e9,
			path:  filepath.Join(dir, e.Name()),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime > files[j].mtime })

	var sessions []APISession
	for _, f := range files {
		h, err := readHeader(f.path)
		if err != nil || h.ParentSessionID != nil {
			continue
		}
		// Channel sessions are ephemeral chat sessions; exclude from the
		// dev-mode sessions list so they don't appear in agent-session.component.
		if h.SessionSource == "channel" {
			continue
		}
		sessions = append(sessions, headerToAPI(*h, nil))
	}

	cacheMu.Lock()
	cache[dir] = cacheEntry{mtime: mtime, sessions: sessions}
	cacheMu.Unlock()

	return sessions, nil
}

// AppendMessage appends a message and atomically updates the header's messageCount and title.
func AppendMessage(wsPath, sessionID string, msg Message) error {
	path := findPath(wsPath, sessionID)
	if path == "" {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	h, err := readHeader(path)
	if err != nil {
		return err
	}
	h.MessageCount++
	if h.Title == nil && msg.Role == "user" && msg.Content != "" {
		t := msg.Content
		if len(t) > 60 {
			t = t[:60] + "…"
		}
		h.Title = &t
	}

	record := map[string]any{
		"type":      "message",
		"sessionId": sessionID,
		"id":        msg.ID,
		"role":      msg.Role,
		"content":   msg.Content,
		"timestamp": msg.Timestamp,
	}
	if msg.Thinking != "" {
		record["thinking"] = msg.Thinking
	}
	if msg.Provider != "" {
		record["provider"] = msg.Provider
	}
	if msg.Model != "" {
		record["model"] = msg.Model
	}
	if len(msg.ToolUses) > 0 {
		record["toolUses"] = msg.ToolUses
	}
	if len(msg.Items) > 0 {
		record["items"] = msg.Items
	}
	if msg.Interrupted {
		record["interrupted"] = true
	}

	if err := atomicUpdateHeader(path, h, record); err != nil {
		return err
	}
	invalidateCache(wsPath)
	return nil
}

// ReplaceMessages rewrites the session file, keeping the header but replacing
// all stored messages with the provided list. Used by compact_session to
// replace the full conversation history with a single summary message so
// the next LLM call starts from a clean, small context.
func ReplaceMessages(wsPath, sessionID string, messages []map[string]any) error {
	path := findPath(wsPath, sessionID)
	if path == "" {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	h, err := readHeader(path)
	if err != nil {
		return err
	}
	h.MessageCount = len(messages)
	h.UpdatedAt = now()

	tmp := path + ".tmp"
	dst, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(dst)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	for _, m := range messages {
		if err := enc.Encode(m); err != nil {
			dst.Close()
			os.Remove(tmp)
			return err
		}
	}
	dst.Close()
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	invalidateCache(wsPath)
	return nil
}

// UpdateFields updates arbitrary header fields (status, mode, provider, model, title…).
func UpdateFields(wsPath, sessionID string, fields map[string]any) (*APISession, error) {
	path := findPath(wsPath, sessionID)
	if path == "" {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	h, err := readHeader(path)
	if err != nil {
		return nil, err
	}
	applyFields(h, fields)
	if err := atomicUpdateHeader(path, h, nil); err != nil {
		return nil, err
	}
	invalidateCache(wsPath)
	s := headerToAPI(*h, nil)
	return &s, nil
}

// ResetStaleRunning marks any top-level session left in "running" state as
// "cancelled" when no live agent task owns it. A session is orphaned in
// "running" when the sidecar is force-quit (or crashes) mid-turn: the deferred
// status update in runAgentTask never executes, so the on-disk header still
// says "running". The frontend derives task.running from this status, so the
// orphan shows as a perpetual loading/"Thinking…" state that survives app
// restarts and workspace re-adds — and auto-reattaches to a dead stream.
//
// Call on workspace registration, AFTER PurgeDead() has cleaned the in-memory
// active registry. We cross-check ListActive() so a session genuinely streaming
// in this (or another live) process is never clobbered.
func ResetStaleRunning(wsPath string) {
	sessions, err := List(wsPath)
	if err != nil {
		return
	}
	// Prune registry entries whose owning process is dead so the active-set
	// check below reflects only genuinely-live tasks, regardless of whether
	// PurgeDead() was already called by the init path.
	PurgeDead()
	active := map[string]bool{}
	for _, e := range ListActive() {
		if e.Status == "running" {
			active[e.SessionID] = true
		}
	}
	for _, s := range sessions {
		if s.Status != "running" || active[s.ID] {
			continue
		}
		_, _ = UpdateFields(wsPath, s.ID, map[string]any{"status": "cancelled"})
	}
}

// Delete removes a session file.
func Delete(wsPath, sessionID string) bool {
	path := findPath(wsPath, sessionID)
	if path == "" {
		return false
	}
	os.Remove(path)
	invalidateCache(wsPath)
	return true
}

// MessageCount returns the message count stored in the session header, or 0 on error.
func MessageCount(wsPath, sessionID string) int {
	path := findPath(wsPath, sessionID)
	if path == "" {
		return 0
	}
	h, err := readHeader(path)
	if err != nil {
		return 0
	}
	return h.MessageCount
}

// GetTodos returns the todo list for a session.
func GetTodos(wsPath, sessionID string) ([]Todo, error) {
	path := findPath(wsPath, sessionID)
	if path == "" {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	h, err := readHeader(path)
	if err != nil {
		return nil, err
	}
	return migrateTodos(h.Todos), nil
}

// SetTodos replaces the todo list for a session.
func SetTodos(wsPath, sessionID string, todos []Todo) error {
	_, err := UpdateFields(wsPath, sessionID, map[string]any{"todos": todos})
	return err
}

// AddTokenUsage accumulates token counts for a session turn into the header.
func AddTokenUsage(wsPath, sessionID string, inputTokens, outputTokens int) error {
	path := findPath(wsPath, sessionID)
	if path == "" {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	h, err := readHeader(path)
	if err != nil {
		return err
	}
	h.InputTokens += inputTokens
	h.OutputTokens += outputTokens
	if err := atomicUpdateHeader(path, h, nil); err != nil {
		return err
	}
	invalidateCache(wsPath)
	return nil
}

// ── internal helpers ──────────────────────────────────────────────────────────

func findPath(wsPath, id string) string {
	for _, isSubagent := range []bool{false, true} {
		p := sessionFile(wsPath, id, isSubagent)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func loadFromPath(path string) (*APISession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	var h Header
	if err := dec.Decode(&h); err != nil {
		return nil, err
	}

	var msgs []map[string]any
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if rec["type"] == "message" {
			msgs = append(msgs, truncateMessage(rec))
		}
	}
	s := headerToAPI(h, msgs)
	return &s, nil
}

func loadTailFromPath(path string, n int) (*APISession, int, error) {
	const chunkSize = 65536
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	// Read header from first line
	dec := json.NewDecoder(f)
	var h Header
	if err := dec.Decode(&h); err != nil {
		return nil, 0, ErrMalformed
	}
	if h.ID == "" {
		return nil, 0, ErrMalformed
	}

	// Seek backward from EOF to collect last n message lines
	size, _ := f.Seek(0, io.SeekEnd)
	headerEnd, _ := f.Seek(0, io.SeekCurrent)
	_ = headerEnd

	var tailBuf []byte
	pos := size
	for pos > 0 && countNewlines(tailBuf) < n+4 {
		read := int64(chunkSize)
		if pos < read {
			read = pos
		}
		pos -= read
		f.Seek(pos, io.SeekStart)
		chunk := make([]byte, read)
		f.Read(chunk)
		tailBuf = append(chunk, tailBuf...)
	}

	var msgs []map[string]any
	for _, line := range splitLines(tailBuf) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["type"] == "message" {
			msgs = append(msgs, truncateMessage(rec))
		}
	}
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}

	total := len(msgs)
	if total >= n {
		total = n + 1 // sentinel: more exist
	}

	s := headerToAPI(h, msgs)
	return &s, total, nil
}

func countNewlines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

func headerToAPI(h Header, msgs []map[string]any) APISession {
	if msgs == nil {
		msgs = []map[string]any{}
	}
	skills := h.ActiveSkills
	if skills == nil {
		skills = []string{}
	}
	todos := todosToAPI(migrateTodos(h.Todos))
	mc := h.MessageCount
	return APISession{
		ID:              h.ID,
		CreatedAt:       h.CreatedAt,
		UpdatedAt:       h.UpdatedAt,
		WorkspacePath:   h.WorkspacePath,
		Provider:        h.Provider,
		Model:           h.Model,
		Mode:            h.Mode,
		Title:           h.Title,
		ParentSessionID: h.ParentSessionID,
		Role:            h.Role,
		AgentName:       h.AgentName,
		Color:           h.Color,
		Status:          h.Status,
		Result:          h.Result,
		ActiveSkills:    skills,
		Todos:           todos,
		MessageCount:    &mc,
		Messages:         msgs,
		InputTokens:      h.InputTokens,
		OutputTokens:     h.OutputTokens,
		CompactedSummary:      h.CompactedSummary,
		CompactedAt:           h.CompactedAt,
		CompactedMessageCount: h.CompactedMessageCount,
	}
}

func todosToAPI(todos []Todo) []map[string]any {
	out := make([]map[string]any, len(todos))
	for i, t := range todos {
		out[i] = map[string]any{
			"id":       t.ID,
			"text":     t.Text,
			"status":   t.Status,
			"checked":  t.Checked,
			"priority": t.Priority,
		}
	}
	return out
}

func migrateTodos(todos []Todo) []Todo {
	out := make([]Todo, len(todos))
	for i, t := range todos {
		if t.Status == "" {
			if t.Checked {
				t.Status = "completed"
			} else {
				t.Status = "pending"
			}
		}
		if t.Priority == "" {
			t.Priority = "medium"
		}
		if t.Text == "" && t.Content != "" {
			t.Text = t.Content
		}
		out[i] = t
	}
	return out
}

func truncateMessage(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "type" || k == "sessionId" {
			continue
		}
		out[k] = v
	}
	if toolUses, ok := out["toolUses"].([]any); ok {
		for i, tu := range toolUses {
			if tuMap, ok := tu.(map[string]any); ok {
				if output, ok := tuMap["output"].(string); ok && len(output) > outputLimit {
					newMap := make(map[string]any, len(tuMap))
					for k, v := range tuMap {
						newMap[k] = v
					}
					newMap["output"] = output[:outputLimit] + "\n…"
					toolUses[i] = newMap
				}
			}
		}
		out["toolUses"] = toolUses
	}
	return out
}

func applyFields(h *Header, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "status":
			if s, ok := v.(string); ok {
				h.Status = s
			}
		case "mode":
			if s, ok := v.(string); ok {
				h.Mode = s
			}
		case "provider":
			if s, ok := v.(string); ok {
				h.Provider = s
			}
		case "model":
			if s, ok := v.(string); ok {
				h.Model = s
			}
		case "title":
			if s, ok := v.(string); ok {
				h.Title = &s
			}
		case "result":
			if s, ok := v.(string); ok {
				h.Result = s
			}
		case "activeSkills":
			if ss, ok := v.([]string); ok {
				h.ActiveSkills = ss
			}
		case "todos":
			if ts, ok := v.([]Todo); ok {
				h.Todos = ts
			}
		case "messageCount":
			if n, ok := v.(int); ok {
				h.MessageCount = n
			}
		case "compactedSummary":
			if s, ok := v.(string); ok {
				h.CompactedSummary = s
			}
		case "compactedAt":
			if s, ok := v.(string); ok {
				h.CompactedAt = s
			}
		case "compactedMessageCount":
			if n, ok := v.(int); ok {
				h.CompactedMessageCount = n
			}
		}
	}
}

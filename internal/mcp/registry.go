package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tolle-ai/tollecode/internal/ai"
)

const toolPrefix = "mcp__"

// autoProbeInterval throttles re-probing for auto-discovered backends (e.g.
// Blender started after lite) so a not-running backend never adds more than one
// short dial per interval to a turn.
const autoProbeInterval = 10 * time.Second

// client is the common interface for stdio and SSE transports.
type client interface {
	Connect(ctx context.Context) error
	ListTools(ctx context.Context) ([]MCPTool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error)
	Close()
}

// entry holds a connected client + its cached tool list.
type entry struct {
	client client

	toolsMu sync.Mutex
	tools   []MCPTool // guarded by toolsMu; nil = not yet fetched
}

// Registry manages MCP server connections for one workspace.
type Registry struct {
	workspace string

	mu        sync.RWMutex
	servers   map[string]*entry // keyed by server name
	fileNames map[string]bool   // guarded by mu; names present in mcp.json (shadow auto-discovery)
	loaded    bool              // guarded by mu; true once file servers have been connected

	loadMu sync.Mutex // serializes the first lazy connect

	autoMu        sync.Mutex // serializes auto-discovery probes
	lastAutoProbe time.Time  // guarded by autoMu
}

// Global is the process-wide cache of per-workspace registries.
var Global = &globalRegistry{
	regs: map[string]*Registry{},
}

type globalRegistry struct {
	mu   sync.Mutex
	regs map[string]*Registry
}

// Get returns (creating if needed) the Registry for a workspace.
func (g *globalRegistry) Get(workspace string) *Registry {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.regs[workspace]; ok {
		return r
	}
	r := &Registry{workspace: workspace, servers: map[string]*entry{}, fileNames: map[string]bool{}}
	g.regs[workspace] = r
	return r
}

// Drop closes and removes a workspace's registry (e.g. workspace closed).
func (g *globalRegistry) Drop(workspace string) {
	g.mu.Lock()
	r, ok := g.regs[workspace]
	if ok {
		delete(g.regs, workspace)
	}
	g.mu.Unlock()
	if ok {
		r.closeAll()
	}
}

// ── Registry methods ──────────────────────────────────────────────────────────

// Reload re-reads the workspace MCP config and reconnects all enabled servers.
// Existing connections are closed before reconnecting.
func (r *Registry) Reload(ctx context.Context) error {
	cfgs, err := LoadConfig(r.workspace)
	if err != nil {
		return err
	}
	r.closeAll()

	r.mu.Lock()
	r.servers = map[string]*entry{}
	r.fileNames = fileConfigNames(cfgs)
	r.mu.Unlock()

	for _, cfg := range cfgs {
		if !cfg.Enabled {
			continue
		}
		if err := r.connect(ctx, cfg); err != nil {
			fmt.Printf("[mcp] warning: could not connect server %q: %v\n", cfg.Name, err)
		}
	}
	r.mu.Lock()
	r.loaded = true
	r.mu.Unlock()

	// Force the next Tools()/Dispatch() to re-probe immediately: a reload often
	// follows the user starting Blender (or editing mcp.json).
	r.autoMu.Lock()
	r.lastAutoProbe = time.Time{}
	r.autoMu.Unlock()

	r.ensureAutoDiscovered(ctx)
	return nil
}

// ensureConnected lazily connects all enabled servers from the workspace config
// the first time the registry is used. This makes MCP tools available from every
// entry point (CLI REPL, --task, subagents) — not just the desktop/stdio path,
// which explicitly calls Reload. Subsequent calls are cheap no-ops.
func (r *Registry) ensureConnected(ctx context.Context) {
	r.mu.RLock()
	loaded := r.loaded
	r.mu.RUnlock()
	if loaded {
		return
	}

	r.loadMu.Lock()
	defer r.loadMu.Unlock()

	// Re-check under loadMu: another goroutine may have loaded while we waited.
	r.mu.RLock()
	loaded = r.loaded
	r.mu.RUnlock()
	if loaded {
		return
	}

	cfgs, err := LoadConfig(r.workspace)
	if err != nil {
		fmt.Printf("[mcp] warning: could not read config for %q: %v\n", r.workspace, err)
		cfgs = nil
	}
	r.mu.Lock()
	r.fileNames = fileConfigNames(cfgs)
	r.mu.Unlock()
	for _, cfg := range cfgs {
		if !cfg.Enabled {
			continue
		}
		r.mu.RLock()
		_, exists := r.servers[cfg.Name]
		r.mu.RUnlock()
		if exists {
			continue
		}
		if err := r.connect(ctx, cfg); err != nil {
			fmt.Printf("[mcp] warning: could not connect server %q: %v\n", cfg.Name, err)
		}
	}

	r.mu.Lock()
	r.loaded = true
	r.mu.Unlock()
}

// ensureAutoDiscovered connects backends detected on this machine (e.g. Blender)
// when EnableAutoDiscovery is on. It is safe to call on every Tools()/Dispatch():
// a successful backend is only connected once, and a not-running backend is
// re-probed at most once per autoProbeInterval. An explicit mcp.json entry of the
// same name always wins — auto-discovery never shadows or overrides it.
func (r *Registry) ensureAutoDiscovered(ctx context.Context) {
	if !EnableAutoDiscovery {
		return
	}

	r.autoMu.Lock()
	defer r.autoMu.Unlock()

	now := time.Now()
	if !r.lastAutoProbe.IsZero() && now.Sub(r.lastAutoProbe) < autoProbeInterval {
		return
	}
	r.lastAutoProbe = now

	for _, cfg := range r.pendingAutoServers() {
		if err := r.connect(ctx, cfg); err != nil {
			fmt.Printf("[mcp] warning: could not auto-connect server %q: %v\n", cfg.Name, err)
		}
	}
}

// pendingAutoServers returns auto-discovered configs that still need connecting:
// neither already connected nor shadowed by an explicit mcp.json entry of the
// same name (explicit config always wins).
func (r *Registry) pendingAutoServers() []ServerConfig {
	auto := discoverAutoServers()
	if len(auto) == 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []ServerConfig
	for _, cfg := range auto {
		if _, connected := r.servers[cfg.Name]; connected {
			continue
		}
		if r.fileNames[cfg.Name] {
			continue
		}
		out = append(out, cfg)
	}
	return out
}

// fileConfigNames indexes the server names declared in mcp.json.
func fileConfigNames(cfgs []ServerConfig) map[string]bool {
	names := make(map[string]bool, len(cfgs))
	for _, c := range cfgs {
		names[c.Name] = true
	}
	return names
}

// Tools returns ai.ToolDef entries for all tools from all connected servers.
// Tool names are prefixed mcp__<server>__<tool> to avoid collisions.
func (r *Registry) Tools(ctx context.Context) []ai.ToolDef {
	r.ensureConnected(ctx)
	r.ensureAutoDiscovered(ctx)

	r.mu.RLock()
	servers := make(map[string]*entry, len(r.servers))
	for k, v := range r.servers {
		servers[k] = v
	}
	r.mu.RUnlock()

	var defs []ai.ToolDef
	for srvName, ent := range servers {
		ent.toolsMu.Lock()
		tools := ent.tools
		if tools == nil {
			var err error
			tools, err = ent.client.ListTools(ctx)
			if err != nil {
				ent.toolsMu.Unlock()
				continue
			}
			ent.tools = tools
		}
		ent.toolsMu.Unlock()
		for _, t := range tools {
			defs = append(defs, ai.ToolDef{
				Name:        toolPrefix + srvName + "__" + t.Name,
				Description: fmt.Sprintf("[MCP:%s] %s", srvName, t.Description),
				InputSchema: t.InputSchema,
			})
		}
	}
	return defs
}

// Dispatch routes an mcp__<server>__<tool> call to the appropriate server.
// Returns (output, isError).
func (r *Registry) Dispatch(ctx context.Context, qualifiedName string, args map[string]any) (string, bool) {
	r.ensureConnected(ctx)
	r.ensureAutoDiscovered(ctx)

	srvName, toolName, ok := parseQualifiedName(qualifiedName)
	if !ok {
		return "Error: malformed MCP tool name: " + qualifiedName, true
	}

	r.mu.RLock()
	ent, exists := r.servers[srvName]
	r.mu.RUnlock()
	if !exists {
		return "Error: MCP server not connected: " + srvName, true
	}

	out, isErr, err := ent.client.CallTool(ctx, toolName, args)
	if err != nil {
		return "Error calling MCP tool: " + err.Error(), true
	}
	return out, isErr
}

// IsMCPTool returns true when toolName was emitted by this registry.
func IsMCPTool(toolName string) bool {
	return strings.HasPrefix(toolName, toolPrefix)
}

// ── internals ─────────────────────────────────────────────────────────────────

func (r *Registry) connect(ctx context.Context, cfg ServerConfig) error {
	var c client
	switch cfg.Type {
	case "stdio":
		c = NewStdioClient(cfg)
	case "sse":
		c = NewSSEClient(cfg)
	default:
		return fmt.Errorf("unknown MCP transport type %q", cfg.Type)
	}
	if err := c.Connect(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	r.servers[cfg.Name] = &entry{client: c}
	r.mu.Unlock()
	return nil
}

func (r *Registry) closeAll() {
	r.mu.Lock()
	servers := r.servers
	r.servers = map[string]*entry{}
	r.mu.Unlock()
	for _, ent := range servers {
		ent.client.Close()
	}
}

func parseQualifiedName(name string) (server, tool string, ok bool) {
	// Format: mcp__<server>__<tool>
	stripped := strings.TrimPrefix(name, toolPrefix)
	idx := strings.Index(stripped, "__")
	if idx < 0 {
		return "", "", false
	}
	return stripped[:idx], stripped[idx+2:], true
}

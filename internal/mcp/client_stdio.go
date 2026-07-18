package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// StdioClient connects to an MCP server via a child process.
type StdioClient struct {
	cfg ServerConfig

	mu      sync.Mutex
	cmd     *exec.Cmd
	enc     *json.Encoder
	pending map[int64]chan response

	nextID atomic.Int64
	done   chan struct{}
}

// NewStdioClient creates (but does not connect) a stdio MCP client.
func NewStdioClient(cfg ServerConfig) *StdioClient {
	return &StdioClient{
		cfg:     cfg,
		pending: map[int64]chan response{},
		done:    make(chan struct{}),
	}
}

// Connect spawns the server process and performs the MCP handshake.
func (c *StdioClient) Connect(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.cfg.Command, c.cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range c.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcp stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp start %q: %w", c.cfg.Command, err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.enc = json.NewEncoder(stdin)
	c.mu.Unlock()

	// Background reader — routes responses to waiting callers.
	go c.readLoop(stdout)

	// MCP initialize handshake.
	var initResult initializeResult
	if err := c.call(ctx, "initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: "tollecode", Version: "1.0"},
	}, &initResult); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("mcp initialize %q: %w", c.cfg.Name, err)
	}

	// Send initialized notification (required by spec).
	_ = c.notify("notifications/initialized", nil)
	return nil
}

// ListTools fetches the server's tool catalog.
func (c *StdioClient) ListTools(ctx context.Context) ([]MCPTool, error) {
	var result toolsListResult
	if err := c.call(ctx, "tools/list", nil, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

// CallTool invokes a named tool and returns its text output and isError flag.
func (c *StdioClient) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	var result toolsCallResult
	if err := c.call(ctx, "tools/call", toolsCallParams{Name: name, Arguments: args}, &result); err != nil {
		return "", true, err
	}
	var sb strings.Builder
	for _, blk := range result.Content {
		if blk.Type == "text" {
			sb.WriteString(blk.Text)
		}
	}
	return sb.String(), result.IsError, nil
}

// Close kills the child process.
func (c *StdioClient) Close() {
	c.mu.Lock()
	cmd := c.cmd
	c.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-c.done
}

// ── internals ─────────────────────────────────────────────────────────────────

func (c *StdioClient) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)

	c.mu.Lock()
	c.pending[id] = ch
	if err := c.enc.Encode(request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("mcp %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		if out != nil && resp.Result != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

func (c *StdioClient) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(notification{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *StdioClient) readLoop(r io.Reader) {
	defer func() {
		// Unblock every pending call with a synthetic disconnect error so callers
		// don't hang forever when the server process exits unexpectedly.
		disconnected := response{Error: &rpcError{Code: -32000, Message: "MCP server disconnected"}}
		c.mu.Lock()
		for id, ch := range c.pending {
			delete(c.pending, id)
			ch <- disconnected
		}
		c.mu.Unlock()
		close(c.done)
	}()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		var resp response
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			continue // skip malformed lines (e.g. server startup logs)
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

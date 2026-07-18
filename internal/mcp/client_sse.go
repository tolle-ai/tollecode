package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// SSEClient connects to an MCP server via HTTP + Server-Sent Events.
// Implements the MCP 2024-11-05 SSE transport.
type SSEClient struct {
	cfg    ServerConfig
	client *http.Client

	mu         sync.Mutex
	postURL    string
	pending    map[int64]chan response
	nextID     atomic.Int64
	cancelSSE  context.CancelFunc
	done       chan struct{}
}

// NewSSEClient creates (but does not connect) an SSE MCP client.
func NewSSEClient(cfg ServerConfig) *SSEClient {
	return &SSEClient{
		cfg:     cfg,
		client:  &http.Client{},
		pending: map[int64]chan response{},
		done:    make(chan struct{}),
	}
}

// Connect opens the SSE stream and performs the MCP handshake.
func (c *SSEClient) Connect(ctx context.Context) error {
	sseCtx, cancel := context.WithCancel(ctx)
	c.cancelSSE = cancel

	// Open SSE stream to discover the POST endpoint.
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, c.cfg.URL+"/sse", nil)
	if err != nil {
		cancel()
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		cancel()
		return fmt.Errorf("mcp sse connect %q: %w", c.cfg.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		return fmt.Errorf("mcp sse %q: HTTP %d", c.cfg.Name, resp.StatusCode)
	}

	// The SSE stream first sends an "endpoint" event with the POST URL.
	postURL, err := readEndpointEvent(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		cancel()
		return fmt.Errorf("mcp sse endpoint event: %w", err)
	}

	c.mu.Lock()
	c.postURL = postURL
	c.mu.Unlock()

	// Background goroutine reads message events and routes them.
	go func() {
		defer close(c.done)
		defer resp.Body.Close()
		c.readSSELoop(resp.Body)
	}()

	// MCP initialize handshake.
	var initResult initializeResult
	if err := c.call(ctx, "initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: "tollecode", Version: "1.0"},
	}, &initResult); err != nil {
		c.cancelSSE()
		return fmt.Errorf("mcp initialize %q: %w", c.cfg.Name, err)
	}
	_ = c.notify("notifications/initialized", nil)
	return nil
}

// ListTools fetches the server's tool catalog.
func (c *SSEClient) ListTools(ctx context.Context) ([]MCPTool, error) {
	var result toolsListResult
	if err := c.call(ctx, "tools/list", nil, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

// CallTool invokes a named tool.
func (c *SSEClient) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
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

// Close cancels the SSE stream.
func (c *SSEClient) Close() {
	if c.cancelSSE != nil {
		c.cancelSSE()
	}
	<-c.done
}

// ── internals ─────────────────────────────────────────────────────────────────

func (c *SSEClient) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)

	c.mu.Lock()
	c.pending[id] = ch
	postURL := c.postURL
	c.mu.Unlock()

	body, _ := json.Marshal(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL, bytes.NewReader(body))
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("mcp post HTTP %d", resp.StatusCode)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("mcp %s: %s (code %d)", method, r.Error.Message, r.Error.Code)
		}
		if out != nil && r.Result != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

func (c *SSEClient) notify(method string, params any) error {
	c.mu.Lock()
	postURL := c.postURL
	c.mu.Unlock()
	body, _ := json.Marshal(notification{JSONRPC: "2.0", Method: method, Params: params})
	req, err := http.NewRequest(http.MethodPost, postURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *SSEClient) readSSELoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	var eventType, data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Blank line = end of event
			if eventType == "message" && data != "" {
				var resp response
				if err := json.Unmarshal([]byte(data), &resp); err == nil {
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
			eventType, data = "", ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func readEndpointEvent(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	var eventType, data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if eventType == "endpoint" {
				return data, nil
			}
			eventType, data = "", ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return "", fmt.Errorf("no endpoint event received")
}

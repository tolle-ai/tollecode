package mcp

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// listenLoopback starts a throwaway TCP listener on a loopback port and returns
// the port plus a close func. It stands in for Blender's addon socket.
func listenLoopback(t *testing.T) (port string, closeFn func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p, func() { _ = l.Close() }
}

func TestBlenderReachable(t *testing.T) {
	port, closeFn := listenLoopback(t)
	if !blenderReachable("127.0.0.1", port) {
		t.Errorf("expected reachable while listener is open")
	}
	closeFn()
	if blenderReachable("127.0.0.1", port) {
		t.Errorf("expected unreachable after listener closed")
	}
}

func TestBlenderAutoconnectDisabled(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"1":     false,
		"true":  false,
		"0":     true,
		"false": true,
		"OFF":   true,
		"no":    true,
	}
	for val, want := range cases {
		t.Setenv("TOLLECODE_BLENDER_AUTOCONNECT", val)
		if got := blenderAutoconnectDisabled(); got != want {
			t.Errorf("TOLLECODE_BLENDER_AUTOCONNECT=%q: got %v, want %v", val, got, want)
		}
	}
}

func TestResolveBlenderMCPCommandOverride(t *testing.T) {
	t.Setenv("TOLLECODE_BLENDER_MCP_CMD", "uvx blender-mcp")
	cmd, args, ok := resolveBlenderMCPCommand()
	if !ok {
		t.Fatal("expected ok with an override set")
	}
	if cmd != "uvx" || len(args) != 1 || args[0] != "blender-mcp" {
		t.Errorf("got cmd=%q args=%v", cmd, args)
	}
}

func TestSearchUpward(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("blender-mcp", ".venv", "bin", "blender-mcp")
	target := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Start deep inside the tree; the walk should climb to the marker.
	start := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := searchUpward(start, rel); got != target {
		t.Errorf("searchUpward = %q, want %q", got, target)
	}

	// A tree without the marker returns "".
	if got := searchUpward(t.TempDir(), rel); got != "" {
		t.Errorf("expected empty for a tree without the marker, got %q", got)
	}
}

func TestDiscoverBlenderBuildsConfig(t *testing.T) {
	port, closeFn := listenLoopback(t)
	defer closeFn()
	t.Setenv("BLENDER_MCP_HOST", "127.0.0.1")
	t.Setenv("BLENDER_MCP_PORT", port)
	t.Setenv("TOLLECODE_BLENDER_MCP_CMD", "/bin/echo hi")

	cfg, ok := discoverBlender()
	if !ok {
		t.Fatal("expected discovery to succeed against an open socket")
	}
	if cfg.Name != blenderServerName || cfg.Type != "stdio" {
		t.Errorf("unexpected identity: name=%q type=%q", cfg.Name, cfg.Type)
	}
	if cfg.Command != "/bin/echo" || len(cfg.Args) != 1 || cfg.Args[0] != "hi" {
		t.Errorf("unexpected command: %q %v", cfg.Command, cfg.Args)
	}
	if cfg.Env["BLENDER_MCP_PORT"] != port {
		t.Errorf("expected the bridge pinned to port %s, got env %v", port, cfg.Env)
	}
	if !cfg.Enabled {
		t.Error("auto-discovered config should be enabled")
	}
}

func TestDiscoverBlenderUnreachable(t *testing.T) {
	port, closeFn := listenLoopback(t)
	closeFn() // free the port so nothing is listening
	t.Setenv("BLENDER_MCP_HOST", "127.0.0.1")
	t.Setenv("BLENDER_MCP_PORT", port)
	t.Setenv("TOLLECODE_BLENDER_MCP_CMD", "/bin/echo hi")
	if _, ok := discoverBlender(); ok {
		t.Error("expected discovery to fail when nothing is listening")
	}
}

func TestDiscoverAutoServersGating(t *testing.T) {
	port, closeFn := listenLoopback(t)
	defer closeFn()
	t.Setenv("BLENDER_MCP_HOST", "127.0.0.1")
	t.Setenv("BLENDER_MCP_PORT", port)
	t.Setenv("TOLLECODE_BLENDER_MCP_CMD", "/bin/echo hi")
	// Isolate this test to the Blender backend: don't let a Unity Editor that
	// happens to be running on this machine add a second server.
	t.Setenv("TOLLECODE_UNITY_AUTOCONNECT", "0")

	restore := EnableAutoDiscovery
	defer func() { EnableAutoDiscovery = restore }()

	EnableAutoDiscovery = false
	if got := discoverAutoServers(); got != nil {
		t.Errorf("flag off should yield no servers, got %v", got)
	}

	EnableAutoDiscovery = true
	t.Setenv("TOLLECODE_BLENDER_AUTOCONNECT", "0")
	if got := discoverAutoServers(); got != nil {
		t.Errorf("opt-out env should yield no servers, got %v", got)
	}

	t.Setenv("TOLLECODE_BLENDER_AUTOCONNECT", "")
	got := discoverAutoServers()
	if len(got) != 1 || got[0].Name != blenderServerName {
		t.Errorf("expected one blender server, got %v", got)
	}
}

func TestUnityAutoconnectDisabled(t *testing.T) {
	cases := map[string]bool{
		"": false, "1": false, "true": false,
		"0": true, "false": true, "OFF": true, "no": true,
	}
	for val, want := range cases {
		t.Setenv("TOLLECODE_UNITY_AUTOCONNECT", val)
		if got := unityAutoconnectDisabled(); got != want {
			t.Errorf("TOLLECODE_UNITY_AUTOCONNECT=%q: got %v, want %v", val, got, want)
		}
	}
}

func TestResolveUnityMCPCommandOverride(t *testing.T) {
	t.Setenv("TOLLECODE_UNITY_MCP_CMD", "uvx unity-mcp")
	cmd, args, ok := resolveUnityMCPCommand()
	if !ok {
		t.Fatal("expected ok with an override set")
	}
	if cmd != "uvx" || len(args) != 1 || args[0] != "unity-mcp" {
		t.Errorf("got cmd=%q args=%v", cmd, args)
	}
}

func TestDiscoverUnityBuildsConfig(t *testing.T) {
	port, closeFn := listenLoopback(t)
	defer closeFn()
	t.Setenv("UNITY_MCP_HOST", "127.0.0.1")
	t.Setenv("UNITY_MCP_PORT", port)
	t.Setenv("TOLLECODE_UNITY_MCP_CMD", "/bin/echo hi")

	cfg, ok := discoverUnity()
	if !ok {
		t.Fatal("expected discovery to succeed against an open socket")
	}
	if cfg.Name != unityServerName || cfg.Type != "stdio" {
		t.Errorf("unexpected identity: name=%q type=%q", cfg.Name, cfg.Type)
	}
	if cfg.Command != "/bin/echo" || len(cfg.Args) != 1 || cfg.Args[0] != "hi" {
		t.Errorf("unexpected command: %q %v", cfg.Command, cfg.Args)
	}
	if cfg.Env["UNITY_MCP_PORT"] != port {
		t.Errorf("expected the bridge pinned to port %s, got env %v", port, cfg.Env)
	}
	if !cfg.Enabled {
		t.Error("auto-discovered config should be enabled")
	}
}

func TestDiscoverUnityUnreachable(t *testing.T) {
	port, closeFn := listenLoopback(t)
	closeFn() // free the port so nothing is listening
	t.Setenv("UNITY_MCP_HOST", "127.0.0.1")
	t.Setenv("UNITY_MCP_PORT", port)
	t.Setenv("TOLLECODE_UNITY_MCP_CMD", "/bin/echo hi")
	if _, ok := discoverUnity(); ok {
		t.Error("expected discovery to fail when nothing is listening")
	}
}

// With both editor sockets open, discovery yields both backends; the Unity
// opt-out env drops only Unity.
func TestDiscoverAutoServersBlenderAndUnity(t *testing.T) {
	bport, bClose := listenLoopback(t)
	defer bClose()
	uport, uClose := listenLoopback(t)
	defer uClose()
	t.Setenv("BLENDER_MCP_HOST", "127.0.0.1")
	t.Setenv("BLENDER_MCP_PORT", bport)
	t.Setenv("TOLLECODE_BLENDER_MCP_CMD", "/bin/echo hi")
	t.Setenv("UNITY_MCP_HOST", "127.0.0.1")
	t.Setenv("UNITY_MCP_PORT", uport)
	t.Setenv("TOLLECODE_UNITY_MCP_CMD", "/bin/echo hi")

	restore := EnableAutoDiscovery
	defer func() { EnableAutoDiscovery = restore }()
	EnableAutoDiscovery = true

	got := discoverAutoServers()
	names := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
	}
	if !names[blenderServerName] || !names[unityServerName] {
		t.Fatalf("expected both blender and unity, got %v", got)
	}

	// Opt out of Unity only → just Blender remains.
	t.Setenv("TOLLECODE_UNITY_AUTOCONNECT", "0")
	got = discoverAutoServers()
	if len(got) != 1 || got[0].Name != blenderServerName {
		t.Errorf("expected only blender after unity opt-out, got %v", got)
	}
}

func TestPendingAutoServersShadowingAndConnected(t *testing.T) {
	port, closeFn := listenLoopback(t)
	defer closeFn()
	t.Setenv("BLENDER_MCP_HOST", "127.0.0.1")
	t.Setenv("BLENDER_MCP_PORT", port)
	t.Setenv("TOLLECODE_BLENDER_MCP_CMD", "/bin/echo hi")
	t.Setenv("TOLLECODE_UNITY_AUTOCONNECT", "0") // isolate to the Blender backend
	restore := EnableAutoDiscovery
	defer func() { EnableAutoDiscovery = restore }()
	EnableAutoDiscovery = true

	// Nothing configured or connected → the blender server is pending.
	r := &Registry{servers: map[string]*entry{}, fileNames: map[string]bool{}}
	if got := r.pendingAutoServers(); len(got) != 1 || got[0].Name != blenderServerName {
		t.Fatalf("expected blender pending, got %v", got)
	}

	// An explicit mcp.json entry of the same name shadows it.
	r.fileNames[blenderServerName] = true
	if got := r.pendingAutoServers(); got != nil {
		t.Errorf("explicit config should shadow auto-discovery, got %v", got)
	}

	// Already connected → not pending.
	r.fileNames = map[string]bool{}
	r.servers[blenderServerName] = &entry{}
	if got := r.pendingAutoServers(); got != nil {
		t.Errorf("connected server should not be pending, got %v", got)
	}
}

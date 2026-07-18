package mcp

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// EnableAutoDiscovery gates zero-config detection of locally-running MCP
// backends (currently Blender and Unity). The lite desktop (stdio.Run) and lite
// web (webmode.Run) entrypoints set it true so the sidecar connects a backend
// the moment its editor plugin is reachable — no hand-editing of
// .agent/mcp.json. The pure CLI leaves it false so it never pays the probe cost.
var EnableAutoDiscovery bool

const (
	blenderServerName   = "blender"
	blenderDefaultHost  = "127.0.0.1"
	blenderDefaultPort  = "9876"
	blenderProbeTimeout = 400 * time.Millisecond

	unityServerName  = "unity"
	unityDefaultHost = "127.0.0.1"
	unityDefaultPort = "9877"
	// The Unity plugin's Roslyn compile can make the first command slow, but the
	// discovery probe only opens a bare TCP connection so it shares Blender's
	// short timeout.
	unityProbeTimeout = 400 * time.Millisecond
)

// discoverAutoServers returns synthetic MCP server configs for local backends
// detected on this machine. They are connected but never written to mcp.json —
// they are ephemeral and re-derived on each probe. Callers must skip any whose
// Name already appears in the user's mcp.json so explicit config always wins.
func discoverAutoServers() []ServerConfig {
	if !EnableAutoDiscovery {
		return nil
	}
	var out []ServerConfig
	if !blenderAutoconnectDisabled() {
		if cfg, ok := discoverBlender(); ok {
			out = append(out, cfg)
		}
	}
	if !unityAutoconnectDisabled() {
		if cfg, ok := discoverUnity(); ok {
			out = append(out, cfg)
		}
	}
	return out
}

// blenderAutoconnectDisabled lets a user opt out via TOLLECODE_BLENDER_AUTOCONNECT.
func blenderAutoconnectDisabled() bool {
	return autoconnectDisabled("TOLLECODE_BLENDER_AUTOCONNECT")
}

// unityAutoconnectDisabled lets a user opt out via TOLLECODE_UNITY_AUTOCONNECT.
func unityAutoconnectDisabled() bool {
	return autoconnectDisabled("TOLLECODE_UNITY_AUTOCONNECT")
}

func autoconnectDisabled(envVar string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envVar))) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// discoverBlender probes the Blender addon's socket and, if it answers, builds a
// stdio ServerConfig that launches the blender-mcp bridge (which in turn talks to
// that socket). Returns ok=false when Blender isn't running or no launcher for
// the bridge can be found.
func discoverBlender() (ServerConfig, bool) {
	host := envOr("BLENDER_MCP_HOST", blenderDefaultHost)
	port := envOr("BLENDER_MCP_PORT", blenderDefaultPort)
	if !socketReachable(host, port, blenderProbeTimeout) {
		return ServerConfig{}, false
	}
	command, args, ok := resolveBlenderMCPCommand()
	if !ok {
		return ServerConfig{}, false
	}
	return ServerConfig{
		Name:    blenderServerName,
		Type:    "stdio",
		Command: command,
		Args:    args,
		// Pin the bridge to the same addon we just found, in case the ambient
		// environment differs from what we probed.
		Env: map[string]string{
			"BLENDER_MCP_HOST": host,
			"BLENDER_MCP_PORT": port,
		},
		Enabled: true,
	}, true
}

// discoverUnity probes the Unity Editor plugin's socket and, if it answers,
// builds a stdio ServerConfig that launches the unity-mcp bridge. Returns
// ok=false when Unity isn't running (plugin server not started) or no launcher
// for the bridge can be found.
func discoverUnity() (ServerConfig, bool) {
	host := envOr("UNITY_MCP_HOST", unityDefaultHost)
	port := envOr("UNITY_MCP_PORT", unityDefaultPort)
	if !socketReachable(host, port, unityProbeTimeout) {
		return ServerConfig{}, false
	}
	command, args, ok := resolveUnityMCPCommand()
	if !ok {
		return ServerConfig{}, false
	}
	return ServerConfig{
		Name:    unityServerName,
		Type:    "stdio",
		Command: command,
		Args:    args,
		Env: map[string]string{
			"UNITY_MCP_HOST": host,
			"UNITY_MCP_PORT": port,
		},
		Enabled: true,
	}, true
}

// blenderReachable returns true when a TCP connection to the addon socket opens.
// Kept as a named wrapper for the existing tests; new callers use socketReachable.
func blenderReachable(host, port string) bool {
	return socketReachable(host, port, blenderProbeTimeout)
}

// socketReachable returns true when a TCP connection to host:port opens within
// timeout.
func socketReachable(host, port string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// resolveBlenderMCPCommand locates a launcher for the blender-mcp bridge server.
func resolveBlenderMCPCommand() (command string, args []string, ok bool) {
	return resolveMCPBridgeCommand("TOLLECODE_BLENDER_MCP_CMD", "blender-mcp")
}

// resolveUnityMCPCommand locates a launcher for the unity-mcp bridge server.
func resolveUnityMCPCommand() (command string, args []string, ok bool) {
	return resolveMCPBridgeCommand("TOLLECODE_UNITY_MCP_CMD", "unity-mcp")
}

// resolveMCPBridgeCommand locates a launcher for a bridge server named `bridge`
// (e.g. "blender-mcp" / "unity-mcp"), in priority order:
//  1. $<overrideEnv> — an explicit command line (split on spaces).
//  2. a `<bridge>` executable on PATH.
//  3. the repo-local venv (<bridge-dir>/.venv/bin/<bridge>), searched upward
//     from the sidecar binary and the working directory — covers `go run` / dev.
//  4. `uvx <bridge>` when uvx is on PATH — pulls the published package.
//
// The repo directory is the bridge name (blender-mcp → blender-mcp/,
// unity-mcp → unity-mcp/).
func resolveMCPBridgeCommand(overrideEnv, bridge string) (command string, args []string, ok bool) {
	if override := strings.TrimSpace(os.Getenv(overrideEnv)); override != "" {
		fields := strings.Fields(override)
		return fields[0], fields[1:], true
	}
	if p, err := exec.LookPath(bridge); err == nil {
		return p, nil, true
	}
	if p := findRepoVenvScript(bridge); p != "" {
		return p, nil, true
	}
	if p, err := exec.LookPath("uvx"); err == nil {
		return p, []string{bridge}, true
	}
	return "", nil, false
}

// findRepoVenvScript searches upward from the sidecar binary and the working
// directory for the repo bridge's virtualenv console script, e.g.
// unity-mcp/.venv/bin/unity-mcp.
func findRepoVenvScript(bridge string) string {
	rel := filepath.Join(bridge, ".venv", "bin", bridge)
	if runtime.GOOS == "windows" {
		rel = filepath.Join(bridge, ".venv", "Scripts", bridge+".exe")
	}
	var roots []string
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	for _, root := range roots {
		if p := searchUpward(root, rel); p != "" {
			return p
		}
	}
	return ""
}

// searchUpward walks from start toward the filesystem root looking for rel.
func searchUpward(start, rel string) string {
	dir := start
	for i := 0; i < 12; i++ {
		if candidate := filepath.Join(dir, rel); isExecutableFile(candidate) {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

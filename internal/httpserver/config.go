package httpserver

import (
	"os"
	"strings"

	"github.com/tolle-ai/tollecode/internal/ai"
	"gopkg.in/yaml.v3"
)

// ServerConfig holds the full configuration for the API server mode.
type ServerConfig struct {
	Port         int               `yaml:"port"`
	Host         string            `yaml:"host"`
	APIKeys      []string          `yaml:"api_keys"`
	AllowedRoots []string          `yaml:"allowed_roots"`
	AllowRootFS  bool              `yaml:"allow_root_fs"`
	Workspaces   []WorkspaceConfig `yaml:"workspaces"`
	Providers    []ProviderConfig  `yaml:"providers"`
	Tools        ToolsConfig       `yaml:"tools"`
	Alerts       AlertsConfig      `yaml:"alerts"`
	Channels     ChannelsConfig    `yaml:"channels"`
	// DefaultProvider pins which provider all channels use when no provider is
	// specified in the individual channel config.  Must match a provider ID in
	// the providers list or ~/.tollecode/config.json.
	DefaultProvider string `yaml:"default_provider"`
	// DefaultModel overrides the provider's own default model for channel sessions.
	DefaultModel    string `yaml:"default_model"`
}

// ChannelsConfig holds settings for each inbound chat-platform gateway.
type ChannelsConfig struct {
	Telegram  TelegramChannelConfig  `yaml:"telegram"`
	Discord   DiscordChannelConfig   `yaml:"discord"`
	Slack     SlackChannelConfig     `yaml:"slack"`
	WhatsApp  WhatsAppChannelConfig  `yaml:"whatsapp"`
	Signal    SignalChannelConfig    `yaml:"signal"`
}

// TelegramChannelConfig configures the Telegram bot gateway.
type TelegramChannelConfig struct {
	Token          string `yaml:"token"`
	WorkspaceID    string `yaml:"workspace_id"`
	AgentID        string `yaml:"agent_id"`
	// Provider and Model pin a specific provider/model for this channel.
	// Overrides the server-level default_provider / default_model.
	Provider       string `yaml:"provider"`
	Model          string `yaml:"model"`
	ShellAutoAllow bool   `yaml:"shell_auto_allow"`
	// MentionOnly: when true, respond only when the bot is @mentioned in group chats.
	MentionOnly    bool   `yaml:"mention_only"`
}

// DiscordChannelConfig configures the Discord bot gateway.
type DiscordChannelConfig struct {
	// Token is the bot token (include "Bot " prefix, e.g. "Bot MTI3...").
	Token          string `yaml:"token"`
	WorkspaceID    string `yaml:"workspace_id"`
	AgentID        string `yaml:"agent_id"`
	Provider       string `yaml:"provider"`
	Model          string `yaml:"model"`
	ShellAutoAllow bool   `yaml:"shell_auto_allow"`
	// MentionOnly: when true, ignore guild-channel messages unless the bot is @mentioned.
	MentionOnly    bool   `yaml:"mention_only"`
}

// SlackChannelConfig configures the Slack Events API gateway.
type SlackChannelConfig struct {
	// BotToken is the xoxb-… OAuth token for posting messages.
	BotToken       string `yaml:"bot_token"`
	// SigningSecret is used to verify X-Slack-Signature on incoming events.
	SigningSecret  string `yaml:"signing_secret"`
	WorkspaceID    string `yaml:"workspace_id"`
	AgentID        string `yaml:"agent_id"`
	Provider       string `yaml:"provider"`
	Model          string `yaml:"model"`
	ShellAutoAllow bool   `yaml:"shell_auto_allow"`
}

// WhatsAppChannelConfig configures the Meta Cloud API gateway.
type WhatsAppChannelConfig struct {
	// PhoneNumberID is the Meta phone number ID for the bot number.
	PhoneNumberID  string `yaml:"phone_number_id"`
	// AccessToken is the Meta Cloud API bearer token used to send messages.
	AccessToken    string `yaml:"access_token"`
	// AppSecret is the Meta App Secret used to verify X-Hub-Signature-256.
	AppSecret      string `yaml:"app_secret"`
	// VerifyToken is the secret string used during webhook GET verification.
	VerifyToken    string `yaml:"verify_token"`
	WorkspaceID    string `yaml:"workspace_id"`
	AgentID        string `yaml:"agent_id"`
	Provider       string `yaml:"provider"`
	Model          string `yaml:"model"`
	ShellAutoAllow bool   `yaml:"shell_auto_allow"`
}

// SignalChannelConfig configures the signal-cli gateway.
type SignalChannelConfig struct {
	// PhoneNumber is the Signal account number registered with signal-cli.
	PhoneNumber    string `yaml:"phone_number"`
	// CLIPath is the path to the signal-cli binary (default: "signal-cli").
	CLIPath        string `yaml:"cli_path"`
	WorkspaceID    string `yaml:"workspace_id"`
	AgentID        string `yaml:"agent_id"`
	Provider       string `yaml:"provider"`
	Model          string `yaml:"model"`
	ShellAutoAllow bool   `yaml:"shell_auto_allow"`
}

// WorkspaceConfig registers a named workspace path.
type WorkspaceConfig struct {
	ID   string `yaml:"id"`
	Path string `yaml:"path"`
	Name string `yaml:"name"`
}

// ProviderConfig maps to an AI provider entry in the config file.
type ProviderConfig struct {
	ID           string   `yaml:"id"`
	Type         string   `yaml:"type"`
	Name         string   `yaml:"name"`
	APIKey       string   `yaml:"api_key"`
	Endpoint     string   `yaml:"endpoint"`
	DefaultModel string   `yaml:"default_model"`
	Models       []string `yaml:"models"`
	// Enabled controls whether this provider is active. Uses a pointer so we
	// can distinguish "not set" (nil, defaults to true) from "explicitly false".
	// When omitted from YAML, the provider is enabled by default.
	Enabled *bool `yaml:"enabled"`
}

// ToolsConfig controls which tool categories are enabled server-wide.
type ToolsConfig struct {
	ShellAllowed   bool `yaml:"shell_allowed"`
	BrowserAllowed bool `yaml:"browser_allowed"`
}

// AlertsConfig holds alert delivery settings.
type AlertsConfig struct {
	Webhook string `yaml:"webhook"`
}

// DefaultServerConfig returns safe defaults.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Port: 47821,
		Host: "0.0.0.0",
	}
}

// Addr returns the TCP address to bind (e.g. "0.0.0.0:47821").
func (c ServerConfig) Addr() string {
	host := c.Host
	if host == "" {
		host = "0.0.0.0"
	}
	port := c.Port
	if port == 0 {
		port = 47821
	}
	return strings.Join([]string{host, itoa(port)}, ":")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 6)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// LoadConfig reads tollecode.yaml from the given path, expanding ${ENV_VAR}
// references in string values. Falls back to DefaultServerConfig on error.
func LoadConfig(path string) (ServerConfig, error) {
	cfg := DefaultServerConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	expanded := os.ExpandEnv(string(data))
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ConvertProviders converts server YAML ProviderConfigs into ai.ProviderConfig
// entries suitable for registration with ai.Global.InjectConfigs.
func ConvertProviders(in []ProviderConfig) []ai.ProviderConfig {
	out := make([]ai.ProviderConfig, 0, len(in))
	for _, p := range in {
		// Providers in tollecode.yaml default to enabled — the user added them
		// intentionally. Only disable when explicitly set to false.
		// p.Enabled is *bool: nil (not set) or true → enabled; explicit false → disabled.
		enabled := p.Enabled == nil || *p.Enabled
		name := p.Name
		if name == "" {
			name = p.ID
		}
		models := make([]ai.ModelEntry, 0, len(p.Models))
		for _, m := range p.Models {
			models = append(models, ai.ModelEntry{ID: m, Name: m})
		}
		out = append(out, ai.ProviderConfig{
			ID:           p.ID,
			Type:         p.Type,
			Name:         name,
			Enabled:      enabled,
			APIKey:       p.APIKey,
			Endpoint:     p.Endpoint,
			Models:       models,
			DefaultModel: p.DefaultModel,
		})
	}
	return out
}

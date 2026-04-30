// Package config handles application configuration.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// LLMBackend identifies the active LLM backend.
type LLMBackend string

const (
	BackendLocal    LLMBackend = "local"
	BackendVertexAI LLMBackend = "vertex_ai"
)

// LocalConfig holds local LLM settings.
type LocalConfig struct {
	Endpoint              string              `json:"endpoint"`
	Model                 string              `json:"model"`
	APIKeyEnv             string              `json:"api_key_env"`
	HotTokenLimit         int                 `json:"hot_token_limit,omitempty"`     // 0 = inherit from Memory.HotTokenLimit
	ContextBudget         ContextBudgetConfig `json:"context_budget,omitzero"`       // zero fields inherit from top-level ContextBudget
	RequestTimeoutSeconds int                 `json:"request_timeout_seconds,omitempty"` // 0 = use default (300)
}

// VertexAIConfig holds Vertex AI settings.
type VertexAIConfig struct {
	ProjectID             string              `json:"project_id"`
	Region                string              `json:"region"`
	Model                 string              `json:"model"`
	HotTokenLimit         int                 `json:"hot_token_limit,omitempty"`
	ContextBudget         ContextBudgetConfig `json:"context_budget,omitzero"`
	RequestTimeoutSeconds int                 `json:"request_timeout_seconds,omitempty"` // 0 = use default (180)
}

// LocalRequestTimeoutDefault is the fallback per-request timeout
// (in seconds) for the local LLM backend when LocalConfig.
// RequestTimeoutSeconds is 0. LM Studio is local so 5 minutes is
// generous but bounded.
const LocalRequestTimeoutDefault = 300

// VertexRequestTimeoutDefault is the fallback per-request timeout
// (in seconds) for the Vertex AI backend when
// VertexAIConfig.RequestTimeoutSeconds is 0. gemini-2.5-flash with
// thinking mode regularly takes 30-60s on complex prompts; 180s
// gives headroom while still bounding silent hangs.
const VertexRequestTimeoutDefault = 180

// LocalRequestTimeout returns the configured timeout for the local
// backend, falling back to LocalRequestTimeoutDefault when unset.
func (c LocalConfig) LocalRequestTimeout() int {
	if c.RequestTimeoutSeconds > 0 {
		return c.RequestTimeoutSeconds
	}
	return LocalRequestTimeoutDefault
}

// VertexRequestTimeout returns the configured timeout for the
// Vertex AI backend, falling back to VertexRequestTimeoutDefault
// when unset.
func (c VertexAIConfig) VertexRequestTimeout() int {
	if c.RequestTimeoutSeconds > 0 {
		return c.RequestTimeoutSeconds
	}
	return VertexRequestTimeoutDefault
}

// LLMConfig holds all LLM backend settings.
type LLMConfig struct {
	DefaultBackend LLMBackend     `json:"default_backend"`
	Local          LocalConfig    `json:"local"`
	VertexAI       VertexAIConfig `json:"vertex_ai"`
}

// MemoryConfig holds memory tier settings.
type MemoryConfig struct {
	HotTokenLimit int    `json:"hot_token_limit"`
	WarmRetention string `json:"warm_retention"`
	ColdRetention string `json:"cold_retention"`
	UseV2         bool   `json:"use_v2,omitempty"` // contextbuild package; opt-in
}

// MCPProfileConfig holds a single mcp-guardian profile configuration.
type MCPProfileConfig struct {
	Name        string `json:"name"`
	Binary      string `json:"binary"`
	ProfilePath string `json:"profile_path"`
	Enabled     bool   `json:"enabled"`
}

// ToolsConfig holds tool-related settings.
type ToolsConfig struct {
	ScriptDir     string             `json:"script_dir"`
	MCPProfiles   []MCPProfileConfig `json:"mcp_profiles"`
	DisabledTools []string           `json:"disabled_tools,omitempty"` // tool names to exclude from LLM
	MITLOverrides map[string]bool    `json:"mitl_overrides,omitempty"` // tool name → force MITL on/off
}

// WindowConfig holds window position and size for restoration.
type WindowConfig struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// UIConfig holds UI-related settings.
type UIConfig struct {
	Theme       string       `json:"theme"`
	StartupMode string       `json:"startup_mode"`
	Window      WindowConfig `json:"window"`
}

// ContextBudgetConfig controls how many tokens are sent to the LLM.
type ContextBudgetConfig struct {
	MaxContextTokens    int `json:"max_context_tokens"`     // total token budget (0 = unlimited)
	MaxWarmTokens       int `json:"max_warm_tokens"`        // budget for warm summaries
	MaxToolResultTokens int `json:"max_tool_result_tokens"` // per-tool-result truncation
}

// SandboxConfig controls the per-session container sandbox.
// Design: docs/en/sandbox-execution.md
type SandboxConfig struct {
	Enabled        bool   `json:"enabled"`
	Engine         string `json:"engine,omitempty"`          // "auto" | "podman" | "docker"
	Image          string `json:"image,omitempty"`
	Network        bool   `json:"network,omitempty"`
	CPULimit       string `json:"cpu_limit,omitempty"`
	MemoryLimit    string `json:"memory_limit,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// Config is the root application configuration.
type Config struct {
	LLM            LLMConfig           `json:"llm"`
	Memory         MemoryConfig        `json:"memory"`
	ContextBudget  ContextBudgetConfig `json:"context_budget"`
	Tools          ToolsConfig         `json:"tools"`
	UI             UIConfig            `json:"ui"`
	Sandbox        SandboxConfig       `json:"sandbox,omitzero"`
	Location       string              `json:"location,omitempty"`
	LastSession    string              `json:"last_session,omitempty"`
}

// Default returns a Config with default values.
func Default() *Config {
	return &Config{
		LLM: LLMConfig{
			DefaultBackend: BackendLocal,
			Local: LocalConfig{
				Endpoint:              "http://localhost:1234/v1",
				Model:                 "google/gemma-4-26b-a4b",
				APIKeyEnv:             "SHELL_AGENT_API_KEY",
				HotTokenLimit:         4096,
				RequestTimeoutSeconds: LocalRequestTimeoutDefault,
				ContextBudget: ContextBudgetConfig{
					MaxContextTokens:    16384,
					MaxWarmTokens:       1024,
					MaxToolResultTokens: 2048,
				},
			},
			VertexAI: VertexAIConfig{
				ProjectID:             "",
				Region:                "us-central1",
				Model:                 "gemini-2.5-flash",
				HotTokenLimit:         65536,
				RequestTimeoutSeconds: VertexRequestTimeoutDefault,
				ContextBudget: ContextBudgetConfig{
					MaxContextTokens:    524288,
					MaxWarmTokens:       16384,
					MaxToolResultTokens: 32768,
				},
			},
		},
		Memory: MemoryConfig{
			HotTokenLimit: 4096, // legacy fallback
			WarmRetention: "24h",
			ColdRetention: "7d",
		},
		ContextBudget: ContextBudgetConfig{ // legacy fallback
			MaxContextTokens:    0,
			MaxWarmTokens:       1024,
			MaxToolResultTokens: 2048,
		},
		Tools: ToolsConfig{
			ScriptDir:   filepath.Join(DataDir(), "tools"),
			MCPProfiles: []MCPProfileConfig{},
		},
		UI: UIConfig{
			Theme:       "dark",
			StartupMode: "last",
		},
		Sandbox: SandboxConfig{
			Enabled:        false,
			Engine:         "auto",
			Image:          "python:3.12-slim",
			Network:        false,
			CPULimit:       "2",
			MemoryLimit:    "1g",
			TimeoutSeconds: 60,
		},
	}
}

// ResolvedSandbox returns the sandbox config with empty fields filled
// from defaults — for callers (e.g. internal/sandbox.NewCLI) that
// need every field populated regardless of how the user wrote
// config.json.
func (c *Config) ResolvedSandbox() SandboxConfig {
	s := c.Sandbox
	if s.Engine == "" {
		s.Engine = "auto"
	}
	if s.Image == "" {
		s.Image = "python:3.12-slim"
	}
	if s.CPULimit == "" {
		s.CPULimit = "2"
	}
	if s.MemoryLimit == "" {
		s.MemoryLimit = "1g"
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = 60
	}
	return s
}

// ExpandPath expands ~ to the user's home directory.
func ExpandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// DataDir returns the application data directory.
func DataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "shell-agent-v2")
}

// ConfigPath returns the path to the config file.
func ConfigPath() string {
	return filepath.Join(DataDir(), "config.json")
}

// Load reads the config from disk, falling back to defaults.
func Load() (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	cfg.applyBackendInheritance()
	return cfg, nil
}

// applyBackendInheritance fills zero per-backend budget fields from the
// legacy top-level Memory.HotTokenLimit / ContextBudget so older configs
// keep working and unset fields fall back to a sensible default.
func (c *Config) applyBackendInheritance() {
	resolve := func(hot *int, b *ContextBudgetConfig) {
		if *hot == 0 {
			*hot = c.Memory.HotTokenLimit
		}
		if b.MaxContextTokens == 0 {
			b.MaxContextTokens = c.ContextBudget.MaxContextTokens
		}
		if b.MaxWarmTokens == 0 {
			b.MaxWarmTokens = c.ContextBudget.MaxWarmTokens
		}
		if b.MaxToolResultTokens == 0 {
			b.MaxToolResultTokens = c.ContextBudget.MaxToolResultTokens
		}
	}
	resolve(&c.LLM.Local.HotTokenLimit, &c.LLM.Local.ContextBudget)
	resolve(&c.LLM.VertexAI.HotTokenLimit, &c.LLM.VertexAI.ContextBudget)
}

// HotTokenLimitFor returns the active backend's HotTokenLimit, falling back
// to the legacy Memory.HotTokenLimit when unset.
func (c *Config) HotTokenLimitFor(backend LLMBackend) int {
	switch backend {
	case BackendVertexAI:
		if c.LLM.VertexAI.HotTokenLimit > 0 {
			return c.LLM.VertexAI.HotTokenLimit
		}
	default:
		if c.LLM.Local.HotTokenLimit > 0 {
			return c.LLM.Local.HotTokenLimit
		}
	}
	return c.Memory.HotTokenLimit
}

// ContextBudgetFor returns the active backend's ContextBudget, falling back
// per-field to the legacy top-level ContextBudget for any zero value.
func (c *Config) ContextBudgetFor(backend LLMBackend) ContextBudgetConfig {
	var b ContextBudgetConfig
	switch backend {
	case BackendVertexAI:
		b = c.LLM.VertexAI.ContextBudget
	default:
		b = c.LLM.Local.ContextBudget
	}
	if b.MaxContextTokens == 0 {
		b.MaxContextTokens = c.ContextBudget.MaxContextTokens
	}
	if b.MaxWarmTokens == 0 {
		b.MaxWarmTokens = c.ContextBudget.MaxWarmTokens
	}
	if b.MaxToolResultTokens == 0 {
		b.MaxToolResultTokens = c.ContextBudget.MaxToolResultTokens
	}
	return b
}

// Save writes the config to disk.
func (c *Config) Save() error {
	if err := os.MkdirAll(DataDir(), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), data, 0600)
}

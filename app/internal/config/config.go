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
	// MaxToolCallArgsBytes caps a single LLM-emitted tool call's
	// Arguments string. 0 → 1 MiB (llm.MaxToolCallArgsBytesDefault).
	// Garbage / attack detection threshold; not surfaced in the
	// Settings UI (security-hardening-2.md H6).
	MaxToolCallArgsBytes int `json:"max_tool_call_args_bytes,omitempty"`

	// Retry policy. 0 on any field means "use the package default
	// from internal/llm/retry.go" (3 attempts, 5s base, 120s cap,
	// 1s jitter). Only RetryMaxAttempts is exposed in Settings UI;
	// the backoff knobs are config-only since most users will never
	// tune them.
	RetryMaxAttempts        int `json:"retry_max_attempts,omitempty"`
	RetryBackoffBaseSeconds int `json:"retry_backoff_base_seconds,omitempty"`
	RetryBackoffMaxSeconds  int `json:"retry_backoff_max_seconds,omitempty"`
	RetryJitterSeconds      int `json:"retry_jitter_seconds,omitempty"`
}

// VertexAIConfig holds Vertex AI settings.
type VertexAIConfig struct {
	ProjectID             string              `json:"project_id"`
	Region                string              `json:"region"`
	Model                 string              `json:"model"`
	HotTokenLimit         int                 `json:"hot_token_limit,omitempty"`
	ContextBudget         ContextBudgetConfig `json:"context_budget,omitzero"`
	RequestTimeoutSeconds int                 `json:"request_timeout_seconds,omitempty"` // 0 = use default (180)
	// MaxToolCallArgsBytes — see LocalConfig. Vertex's genai SDK
	// returns tool calls already-decoded so the cap is enforced at
	// the wire-decode boundary (not a string-len check).
	MaxToolCallArgsBytes int `json:"max_tool_call_args_bytes,omitempty"`

	// Retry policy — see LocalConfig.
	RetryMaxAttempts        int `json:"retry_max_attempts,omitempty"`
	RetryBackoffBaseSeconds int `json:"retry_backoff_base_seconds,omitempty"`
	RetryBackoffMaxSeconds  int `json:"retry_backoff_max_seconds,omitempty"`
	RetryJitterSeconds      int `json:"retry_jitter_seconds,omitempty"`
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

	// HideAnalysisToolsUntilDataLoaded restores the pre-v0.1.21
	// behaviour where data-dependent analysis tools (query-sql,
	// describe-data, analyze-data, ...) only appear in the LLM
	// tool list after a successful load-data. Default false.
	// Opt-in for users on weaker local backends where exposing
	// 30+ tools measurably hurts selection accuracy. See
	// docs/en/agent-tool-visibility.md.
	HideAnalysisToolsUntilDataLoaded bool `json:"hide_analysis_tools_until_data_loaded,omitempty"`
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
	Theme            string       `json:"theme"`
	StartupMode      string       `json:"startup_mode"`
	Window           WindowConfig `json:"window"`
	SidebarWidth     int          `json:"sidebar_width,omitempty"`     // px, 0 = use default (280)
	SidebarCollapsed bool         `json:"sidebar_collapsed,omitempty"` // true → start collapsed
}

// DefaultSidebarWidth is the px width used when SidebarWidth is
// unset (or 0) in UIConfig. The resize handle in the frontend
// clamps user-driven changes to [180, 500].
const DefaultSidebarWidth = 280

// ContextBudgetConfig controls how many tokens are sent to the LLM.
type ContextBudgetConfig struct {
	MaxContextTokens    int `json:"max_context_tokens"`     // total token budget (0 = unlimited)
	MaxWarmTokens       int `json:"max_warm_tokens"`        // budget for warm summaries
	MaxToolResultTokens int `json:"max_tool_result_tokens"` // per-tool-result truncation
	OutputReserve       int `json:"output_reserve"`         // tokens reserved for the model's reply (subtracted from MaxContextTokens before packing context). 0 = use default
}

// DefaultOutputReserve is used when ContextBudgetConfig.OutputReserve
// is unset (0). 4096 fits a chunky tool-call + brief summary on
// every model we support today.
const DefaultOutputReserve = 4096

// OutputReserveResolved returns OutputReserve or the default when 0.
func (b ContextBudgetConfig) OutputReserveResolved() int {
	if b.OutputReserve > 0 {
		return b.OutputReserve
	}
	return DefaultOutputReserve
}

// SandboxConfig controls the per-session container sandbox.
// Design: docs/en/sandbox-execution.md, docs/en/sandbox-image-build.md
type SandboxConfig struct {
	Enabled        bool   `json:"enabled"`
	Engine         string `json:"engine,omitempty"`          // "auto" | "podman" | "docker"
	Image          string `json:"image,omitempty"`           // active image tag (set by build / library selection)
	Dockerfile     string `json:"dockerfile,omitempty"`      // user-edited Dockerfile body (empty = imagebuild.RecommendedDockerfile)
	Network        bool   `json:"network,omitempty"`
	CPULimit       string `json:"cpu_limit,omitempty"`
	MemoryLimit    string `json:"memory_limit,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	// MaxOutputBytes caps stdout / stderr per Exec call. Default 8 MiB
	// when 0; not surfaced in the Settings UI (config-file edit only)
	// since most users never need to change it (security-hardening-2.md C3).
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

// AgentConfig controls the agent execution loop.
type AgentConfig struct {
	// MaxToolRounds caps the agent loop's tool-call rounds per
	// user message. 0 = use the package default (10). Long
	// analyses occasionally legitimately need more; the loop-
	// detection ring buffer (Feature 1, v0.1.16) catches stuck
	// loops separately, so raising this is reasonably safe.
	MaxToolRounds int `json:"max_tool_rounds,omitempty"`
}

// DefaultMaxToolRounds is the cap when AgentConfig.MaxToolRounds
// is unset. Matches the long-time hardcoded value for backward
// compat.
const DefaultMaxToolRounds = 10

// MaxToolRoundsResolved returns MaxToolRounds or the default when 0.
func (a AgentConfig) MaxToolRoundsResolved() int {
	if a.MaxToolRounds > 0 {
		return a.MaxToolRounds
	}
	return DefaultMaxToolRounds
}

// Config is the root application configuration.
type Config struct {
	LLM            LLMConfig           `json:"llm"`
	Memory         MemoryConfig        `json:"memory"`
	ContextBudget  ContextBudgetConfig `json:"context_budget"`
	Tools          ToolsConfig         `json:"tools"`
	UI             UIConfig            `json:"ui"`
	Sandbox        SandboxConfig       `json:"sandbox,omitzero"`
	Agent          AgentConfig         `json:"agent,omitzero"`
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
					OutputReserve:       DefaultOutputReserve,
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
					OutputReserve:       DefaultOutputReserve,
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
			Image:          "", // populated after the user's first Build
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
	// Image intentionally left empty when unset — the user's
	// first Settings Build populates it. The agent's
	// readiness gate refuses to start sandbox tools until a
	// valid Image is set.
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

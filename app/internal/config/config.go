// Package config handles application configuration.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// LLMBackend identifies the active LLM backend.
type LLMBackend string

const (
	BackendLocal    LLMBackend = "local"
	BackendVertexAI LLMBackend = "vertex_ai"
)

// LocalConfig holds local LLM settings.
type LocalConfig struct {
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

// VertexAIConfig holds Vertex AI settings.
type VertexAIConfig struct {
	ProjectID string `json:"project_id"`
	Region    string `json:"region"`
	Model     string `json:"model"`
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
}

// MCPGuardianConfig holds mcp-guardian settings.
type MCPGuardianConfig struct {
	Binary     string `json:"binary"`
	ConfigFile string `json:"config"`
}

// ToolsConfig holds tool-related settings.
type ToolsConfig struct {
	ScriptDir   string            `json:"script_dir"`
	MCPGuardian MCPGuardianConfig `json:"mcp_guardian"`
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

// Config is the root application configuration.
type Config struct {
	LLM            LLMConfig           `json:"llm"`
	Memory         MemoryConfig        `json:"memory"`
	ContextBudget  ContextBudgetConfig `json:"context_budget"`
	Tools          ToolsConfig         `json:"tools"`
	UI             UIConfig            `json:"ui"`
	Location       string              `json:"location,omitempty"`
	LastSession    string              `json:"last_session,omitempty"`
}

// Default returns a Config with default values.
func Default() *Config {
	return &Config{
		LLM: LLMConfig{
			DefaultBackend: BackendLocal,
			Local: LocalConfig{
				Endpoint:  "http://localhost:1234/v1",
				Model:     "google/gemma-4-26b-a4b",
				APIKeyEnv: "SHELL_AGENT_API_KEY",
			},
			VertexAI: VertexAIConfig{
				ProjectID: "",
				Region:    "us-central1",
				Model:     "gemini-2.5-flash",
			},
		},
		Memory: MemoryConfig{
			HotTokenLimit: 4096,
			WarmRetention: "24h",
			ColdRetention: "7d",
		},
		ContextBudget: ContextBudgetConfig{
			MaxContextTokens:    0,    // 0 = unlimited (rely on [Calling:] exclusion + compaction)
			MaxWarmTokens:       1024,
			MaxToolResultTokens: 2048,
		},
		Tools: ToolsConfig{
			ScriptDir: filepath.Join(DataDir(), "tools"),
			MCPGuardian: MCPGuardianConfig{
				Binary:     "/usr/local/bin/mcp-guardian",
				ConfigFile: "~/.config/mcp-guardian/config.json",
			},
		},
		UI: UIConfig{
			Theme:       "dark",
			StartupMode: "last",
		},
	}
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
	return cfg, nil
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

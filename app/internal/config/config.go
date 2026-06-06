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
//
// v0.2.0: HotTokenLimit removed (it was the v1 destructive
// compaction trigger; contextbuild uses ContextBudget instead).
type LocalConfig struct {
	Endpoint              string              `json:"endpoint"`
	Model                 string              `json:"model"`
	APIKeyEnv             string              `json:"api_key_env"`
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

	// AutoExtractEnabled gates the after-turn memory-extraction LLM
	// call (ADR-0015). nil → use backend default (local: off).
	// Default false for local: the extraction call evicts llama.cpp's
	// single prefix-KV-cache slot and forces the next turn into a
	// cold re-encode of the whole history (ADR-0019 §1). Users who
	// prefer recall over latency can opt in via Settings.
	AutoExtractEnabled *bool `json:"auto_extract_enabled,omitempty"`

	// AutoTitleEnabled gates the after-first-turn title-generation
	// LLM call. Same cache-eviction concern as AutoExtractEnabled but
	// limited to turn 2 (title gen is one-shot per session). When
	// off, the session stays untitled until the user renames it.
	// See ADR-0020.
	AutoTitleEnabled *bool `json:"auto_title_enabled,omitempty"`
}

// LocalAutoExtractDefault is the default for LocalConfig.
// AutoExtractEnabled when the field is absent. False because the
// extraction call destroys local KV-cache prefix reuse (ADR-0019).
const LocalAutoExtractDefault = false

// LocalAutoTitleDefault is the default for LocalConfig.AutoTitleEnabled
// when the field is absent. False for the same prefix-cache reason as
// LocalAutoExtractDefault (ADR-0020).
const LocalAutoTitleDefault = false

// AutoExtract resolves the effective AutoExtractEnabled value,
// falling back to LocalAutoExtractDefault when nil.
func (c LocalConfig) AutoExtract() bool {
	if c.AutoExtractEnabled == nil {
		return LocalAutoExtractDefault
	}
	return *c.AutoExtractEnabled
}

// AutoTitle resolves the effective AutoTitleEnabled value,
// falling back to LocalAutoTitleDefault when nil.
func (c LocalConfig) AutoTitle() bool {
	if c.AutoTitleEnabled == nil {
		return LocalAutoTitleDefault
	}
	return *c.AutoTitleEnabled
}

// VertexAIConfig holds Vertex AI settings.
//
// v0.2.0: HotTokenLimit removed (see LocalConfig).
type VertexAIConfig struct {
	ProjectID             string              `json:"project_id"`
	Region                string              `json:"region"`
	Model                 string              `json:"model"`
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

	// AutoExtractEnabled — see LocalConfig. nil → use backend default
	// (vertex: on). Default true for Vertex because its server-side
	// KV cache is per-request-stream and is not evicted by auxiliary
	// extraction calls (ADR-0019 §1).
	AutoExtractEnabled *bool `json:"auto_extract_enabled,omitempty"`

	// AutoTitleEnabled — see LocalConfig. nil → use backend default
	// (vertex: on). Default true for Vertex because the title-gen
	// LLM call doesn't penalise its cache model (ADR-0020).
	AutoTitleEnabled *bool `json:"auto_title_enabled,omitempty"`
}

// VertexAutoExtractDefault is the default for VertexAIConfig.
// AutoExtractEnabled when the field is absent. True because Vertex's
// cache model does not penalise an auxiliary extraction call.
const VertexAutoExtractDefault = true

// VertexAutoTitleDefault is the default for VertexAIConfig.
// AutoTitleEnabled when the field is absent (ADR-0020).
const VertexAutoTitleDefault = true

// AutoExtract resolves the effective AutoExtractEnabled value,
// falling back to VertexAutoExtractDefault when nil.
func (c VertexAIConfig) AutoExtract() bool {
	if c.AutoExtractEnabled == nil {
		return VertexAutoExtractDefault
	}
	return *c.AutoExtractEnabled
}

// AutoTitle resolves the effective AutoTitleEnabled value,
// falling back to VertexAutoTitleDefault when nil.
func (c VertexAIConfig) AutoTitle() bool {
	if c.AutoTitleEnabled == nil {
		return VertexAutoTitleDefault
	}
	return *c.AutoTitleEnabled
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
//
// v0.12.0 (ADR-0016): the single (Local, VertexAI, default_backend)
// triple is replaced by a list of named profiles + a default-profile
// pointer. Profile contents (LocalConfig / VertexAIConfig) keep their
// v0.11.x shape, so per-backend retry / timeout / context-budget
// knobs work unchanged inside each profile. UnmarshalJSON in
// profile.go migrates v0.11.x configs on first load.
type LLMConfig struct {
	DefaultProfileID string       `json:"default_profile_id"`
	Profiles         []LLMProfile `json:"profiles"`
}

// MemoryConfig holds cross-session memory settings.
//
// v0.2.0: HotTokenLimit, WarmRetention, ColdRetention, UseV2 are
// gone. The legacy v1 destructive-compaction code that consulted
// them was removed; contextbuild handles older-tail folding
// non-destructively at LLM-call time and uses the per-backend
// ContextBudget for sizing.
type MemoryConfig struct {
	// Retention caps for cross-session / per-session stores.
	// Zero falls back to package defaults.
	MaxPinnedFacts int `json:"max_pinned_facts,omitempty"` // default 100 (Global Memory in v0.2.0)
	MaxFindings    int `json:"max_findings,omitempty"`     // default 100 per session (was global 200 in v0.1.x)

	// Lifecycle controls the per-entry state machine + relevance
	// decay for Global Memory and Session Memory (ADR-0031).
	Lifecycle LifecycleConfig `json:"lifecycle,omitzero"`
}

// LifecycleConfig tunes the memory entry lifecycle. Zero values
// fall back to safe defaults at the consumption site (the memory
// package owns its own LifecycleThresholds type with resolved()
// to keep the config → memory direction acyclic); in practice
// Default() populates these and most users never touch them.
//
// See ADR-0031 §3.5 for the rationale behind each default.
type LifecycleConfig struct {
	// DecayRate is the per-user-turn multiplier applied to every
	// non-fresh entry's relevance. Default 0.93.
	DecayRate float64 `json:"decay_rate,omitempty"`
	// FreshTurns is how many user turns an entry stays in the
	// fresh window after creation. Default 3.
	FreshTurns int `json:"fresh_turns,omitempty"`
	// ActiveThreshold is the lower bound (inclusive) of the
	// active state. Below this an entry becomes dormant.
	// Default 0.4.
	ActiveThreshold float64 `json:"active_threshold,omitempty"`
	// ArchiveThreshold is the upper bound (exclusive) of the
	// archived state. At or below this an entry becomes
	// archived. Default 0.1.
	ArchiveThreshold float64 `json:"archive_threshold,omitempty"`
	// TouchJaccardThreshold is the Jaccard score above which a
	// user turn is considered to reference an entry's fact, in
	// the lexical touch fallback path. Default 0.3.
	TouchJaccardThreshold float64 `json:"touch_jaccard_threshold,omitempty"`
	// ConsolidationJaccardThreshold is the Jaccard score above
	// which a new fact is merged into an existing one as a
	// touch rather than appended as a new entry. Default 0.5.
	ConsolidationJaccardThreshold float64 `json:"consolidation_jaccard_threshold,omitempty"`
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
	// docs/en/history/agent-tool-visibility.md.
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
// Design: docs/en/history/sandbox-execution.md, docs/en/history/sandbox-image-build.md
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

// AnalysisConfig controls the DuckDB analysis engine's row caps.
//
// Two distinct caps protect two distinct resources (see ADR-0029):
//   - MaxQueryRows bounds interactive chat-output queries
//     (query-sql / query-preview / quick-summary). Their rows are
//     JSON-serialised into the LLM tool result, so the cap protects
//     the model's context window.
//   - MaxExportRows bounds export-sql-to-csv, which writes rows to a
//     file in the sandbox /work dir. Those rows never enter the chat,
//     so the only ceiling that matters is memory — hence the much
//     higher default. Sharing the chat cap here was the bug fixed by
//     ADR-0029 (issue #14).
type AnalysisConfig struct {
	MaxQueryRows  int `json:"max_query_rows,omitempty"`  // 0 → DefaultMaxQueryRows
	MaxExportRows int `json:"max_export_rows,omitempty"` // 0 → DefaultMaxExportRows
}

// DefaultMaxQueryRows caps chat-output queries when
// AnalysisConfig.MaxQueryRows is unset. Matches the long-time
// hardcoded value (engine.MaxQueryRows) for backward compat.
const DefaultMaxQueryRows = 10_000

// DefaultMaxExportRows caps export-sql-to-csv when
// AnalysisConfig.MaxExportRows is unset. Matches analysis.MaxAnalyzeRows
// because both paths write rows to a file rather than the chat, so the
// ceiling is memory, not context (ADR-0029 §3.5).
const DefaultMaxExportRows = 1_000_000

// MaxQueryRowsResolved returns MaxQueryRows or the default when 0.
func (a AnalysisConfig) MaxQueryRowsResolved() int {
	if a.MaxQueryRows > 0 {
		return a.MaxQueryRows
	}
	return DefaultMaxQueryRows
}

// MaxExportRowsResolved returns MaxExportRows or the default when 0.
func (a AnalysisConfig) MaxExportRowsResolved() int {
	if a.MaxExportRows > 0 {
		return a.MaxExportRows
	}
	return DefaultMaxExportRows
}

// LoggerConfig holds app.log verbosity settings.
//
// Level controls which log calls reach the file:
//   - "debug": everything (incl. user message snippets, LLM
//     response heads, tool arguments). Use for diagnosis.
//   - "info" (default): events + lifecycle, no conversation
//     content. Privacy default.
//   - "warn" / "error": progressively quieter.
//
// Empty string maps to "info". See docs/en/reference/privacy-controls.md §3.
type LoggerConfig struct {
	Level string `json:"level,omitempty"`
}

// Config is the root application configuration.
type Config struct {
	LLM           LLMConfig           `json:"llm"`
	Memory        MemoryConfig        `json:"memory"`
	ContextBudget ContextBudgetConfig `json:"context_budget"`
	Tools         ToolsConfig         `json:"tools"`
	UI            UIConfig            `json:"ui"`
	Sandbox       SandboxConfig       `json:"sandbox,omitzero"`
	Agent         AgentConfig         `json:"agent,omitzero"`
	Analysis      AnalysisConfig      `json:"analysis,omitzero"`
	Logger        LoggerConfig        `json:"logger,omitzero"`
	Location      string              `json:"location,omitempty"`
}

// LogLevelString returns the configured logger level string with
// "info" as the fallback for empty / unknown values. Used by both
// the bindings layer and main.go to consult the same default.
func (c *Config) LogLevelString() string {
	switch c.Logger.Level {
	case "debug", "info", "warn", "error":
		return c.Logger.Level
	default:
		return "info"
	}
}

// Default returns a Config with default values.
func Default() *Config {
	localExtract := LocalAutoExtractDefault
	vertexExtract := VertexAutoExtractDefault
	localTitle := LocalAutoTitleDefault
	vertexTitle := VertexAutoTitleDefault
	defaultProfile := LLMProfile{
		ID:             NewProfileID(),
		Name:           DefaultProfileName,
		DefaultBackend: BackendLocal,
		Local: LocalConfig{
			Endpoint:              "http://localhost:1234/v1",
			Model:                 "google/gemma-4-26b-a4b",
			APIKeyEnv:             "SHELL_AGENT_API_KEY",
			RequestTimeoutSeconds: LocalRequestTimeoutDefault,
			ContextBudget: ContextBudgetConfig{
				MaxContextTokens:    16384,
				MaxWarmTokens:       1024,
				MaxToolResultTokens: 2048,
				OutputReserve:       DefaultOutputReserve,
			},
			AutoExtractEnabled: &localExtract,
			AutoTitleEnabled:   &localTitle,
		},
		VertexAI: VertexAIConfig{
			ProjectID:             "",
			Region:                "us-central1",
			Model:                 "gemini-2.5-flash",
			RequestTimeoutSeconds: VertexRequestTimeoutDefault,
			ContextBudget: ContextBudgetConfig{
				MaxContextTokens:    524288,
				MaxWarmTokens:       16384,
				MaxToolResultTokens: 32768,
				OutputReserve:       DefaultOutputReserve,
			},
			AutoExtractEnabled: &vertexExtract,
			AutoTitleEnabled:   &vertexTitle,
		},
	}
	return &Config{
		LLM: LLMConfig{
			DefaultProfileID: defaultProfile.ID,
			Profiles:         []LLMProfile{defaultProfile},
		},
		Memory: MemoryConfig{
			MaxPinnedFacts: 100,
			MaxFindings:    100,
			Lifecycle: LifecycleConfig{
				DecayRate:                     0.93,
				FreshTurns:                    3,
				ActiveThreshold:               0.4,
				ArchiveThreshold:              0.1,
				TouchJaccardThreshold:         0.3,
				ConsolidationJaccardThreshold: 0.5,
			},
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

// SystemRulesPath returns the path to the user-authored System
// Rules Markdown file. See ADR-0012.
func SystemRulesPath() string {
	return filepath.Join(DataDir(), "system_rules.md")
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
	cfg.repairProfiles()
	cfg.applyBackendInheritance()
	return cfg, nil
}

// repairProfiles fills in defaults when the loaded config has an
// empty or dangling LLM profile list. Guarantees that callers can
// rely on len(LLM.Profiles) >= 1 and that DefaultProfileID resolves
// to an existing entry. ADR-0016 §3.1 invariants.
func (c *Config) repairProfiles() {
	if len(c.LLM.Profiles) == 0 {
		// Empty {"llm":{}} or missing block — fall back to the
		// canonical single-profile default.
		c.LLM = Default().LLM
		return
	}
	// Dangling DefaultProfileID → repair to the first profile.
	if !c.LLM.HasProfile(c.LLM.DefaultProfileID) {
		c.LLM.DefaultProfileID = c.LLM.Profiles[0].ID
	}
}

// applyBackendInheritance fills zero per-backend ContextBudget
// fields from the top-level ContextBudget so older configs keep
// working and unset fields fall back to a sensible default.
//
// v0.2.0: HotTokenLimit-based inheritance is gone (the field
// itself was deleted from MemoryConfig).
// v0.12.0 (ADR-0016): iterates every profile, not just one pair.
func (c *Config) applyBackendInheritance() {
	resolve := func(b *ContextBudgetConfig) {
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
	for i := range c.LLM.Profiles {
		resolve(&c.LLM.Profiles[i].Local.ContextBudget)
		resolve(&c.LLM.Profiles[i].VertexAI.ContextBudget)
	}
}

// ContextBudgetFor returns the active backend's ContextBudget for the
// default profile, falling back per-field to the legacy top-level
// ContextBudget for any zero value.
//
// v0.12.0 (ADR-0016): commits-1 form reads from the default profile.
// Commit 3 plumbs the active session's profile through agent so the
// session-bound profile is honoured for /model toggling.
func (c *Config) ContextBudgetFor(backend LLMBackend) ContextBudgetConfig {
	profile := c.LLM.DefaultProfile()
	var b ContextBudgetConfig
	if profile != nil {
		switch backend {
		case BackendVertexAI:
			b = profile.VertexAI.ContextBudget
		default:
			b = profile.Local.ContextBudget
		}
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

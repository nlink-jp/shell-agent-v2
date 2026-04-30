package config

import (
	"os"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.LLM.DefaultBackend != BackendLocal {
		t.Errorf("default backend = %v, want %v", cfg.LLM.DefaultBackend, BackendLocal)
	}
	if cfg.LLM.Local.Endpoint == "" {
		t.Error("local endpoint is empty")
	}
	if cfg.LLM.VertexAI.Region == "" {
		t.Error("vertex AI region is empty")
	}
	if cfg.Memory.HotTokenLimit == 0 {
		t.Error("hot token limit is zero")
	}
}

func TestDefault_RequestTimeoutSeconds(t *testing.T) {
	cfg := Default()
	if cfg.LLM.Local.RequestTimeoutSeconds != LocalRequestTimeoutDefault {
		t.Errorf("Local.RequestTimeoutSeconds = %d, want %d",
			cfg.LLM.Local.RequestTimeoutSeconds, LocalRequestTimeoutDefault)
	}
	if cfg.LLM.VertexAI.RequestTimeoutSeconds != VertexRequestTimeoutDefault {
		t.Errorf("VertexAI.RequestTimeoutSeconds = %d, want %d",
			cfg.LLM.VertexAI.RequestTimeoutSeconds, VertexRequestTimeoutDefault)
	}
}

func TestRequestTimeout_FallbackWhenZero(t *testing.T) {
	if got := (LocalConfig{}).LocalRequestTimeout(); got != LocalRequestTimeoutDefault {
		t.Errorf("LocalConfig{}.LocalRequestTimeout() = %d, want %d", got, LocalRequestTimeoutDefault)
	}
	if got := (VertexAIConfig{}).VertexRequestTimeout(); got != VertexRequestTimeoutDefault {
		t.Errorf("VertexAIConfig{}.VertexRequestTimeout() = %d, want %d", got, VertexRequestTimeoutDefault)
	}
	if got := (LocalConfig{RequestTimeoutSeconds: 7}).LocalRequestTimeout(); got != 7 {
		t.Errorf("explicit value should be honoured, got %d", got)
	}
}

func TestDefault_PerBackendBudgets(t *testing.T) {
	cfg := Default()
	if cfg.LLM.Local.HotTokenLimit == 0 {
		t.Error("local hot token limit is zero")
	}
	if cfg.LLM.VertexAI.HotTokenLimit == 0 {
		t.Error("vertex hot token limit is zero")
	}
	if cfg.LLM.VertexAI.HotTokenLimit <= cfg.LLM.Local.HotTokenLimit {
		t.Errorf("vertex hot limit (%d) should exceed local (%d) since the model has a much larger window",
			cfg.LLM.VertexAI.HotTokenLimit, cfg.LLM.Local.HotTokenLimit)
	}
	if cfg.LLM.Local.ContextBudget.MaxToolResultTokens == 0 {
		t.Error("local tool-result cap is zero")
	}
}

func TestHotTokenLimitFor(t *testing.T) {
	cfg := Default()
	if got := cfg.HotTokenLimitFor(BackendLocal); got != cfg.LLM.Local.HotTokenLimit {
		t.Errorf("local: got %d want %d", got, cfg.LLM.Local.HotTokenLimit)
	}
	if got := cfg.HotTokenLimitFor(BackendVertexAI); got != cfg.LLM.VertexAI.HotTokenLimit {
		t.Errorf("vertex: got %d want %d", got, cfg.LLM.VertexAI.HotTokenLimit)
	}

	// Legacy fallback: per-backend zero, top-level non-zero.
	cfg.LLM.Local.HotTokenLimit = 0
	cfg.Memory.HotTokenLimit = 9999
	if got := cfg.HotTokenLimitFor(BackendLocal); got != 9999 {
		t.Errorf("legacy fallback: got %d want 9999", got)
	}
}

func TestContextBudgetFor_PerFieldFallback(t *testing.T) {
	cfg := Default()
	cfg.LLM.Local.ContextBudget = ContextBudgetConfig{MaxContextTokens: 1000} // others zero
	cfg.ContextBudget = ContextBudgetConfig{MaxContextTokens: 5, MaxWarmTokens: 50, MaxToolResultTokens: 500}

	b := cfg.ContextBudgetFor(BackendLocal)
	if b.MaxContextTokens != 1000 {
		t.Errorf("MaxContext: got %d want 1000 (per-backend wins)", b.MaxContextTokens)
	}
	if b.MaxWarmTokens != 50 {
		t.Errorf("MaxWarm: got %d want 50 (legacy fills zero field)", b.MaxWarmTokens)
	}
	if b.MaxToolResultTokens != 500 {
		t.Errorf("MaxToolResult: got %d want 500 (legacy fills zero field)", b.MaxToolResultTokens)
	}
}

func TestApplyBackendInheritance_LegacyMigration(t *testing.T) {
	// Simulates loading a pre-feature config with only top-level Memory/ContextBudget set.
	cfg := &Config{
		Memory:        MemoryConfig{HotTokenLimit: 4096},
		ContextBudget: ContextBudgetConfig{MaxContextTokens: 32000, MaxWarmTokens: 1024, MaxToolResultTokens: 2048},
	}
	cfg.applyBackendInheritance()

	if cfg.LLM.Local.HotTokenLimit != 4096 {
		t.Errorf("local hot inherit: got %d want 4096", cfg.LLM.Local.HotTokenLimit)
	}
	if cfg.LLM.VertexAI.ContextBudget.MaxContextTokens != 32000 {
		t.Errorf("vertex MaxContext inherit: got %d want 32000", cfg.LLM.VertexAI.ContextBudget.MaxContextTokens)
	}
}

func TestDataDir(t *testing.T) {
	dir := DataDir()
	if dir == "" {
		t.Error("data dir is empty")
	}
}

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.DefaultBackend != BackendLocal {
		t.Errorf("default backend = %v, want %v", cfg.LLM.DefaultBackend, BackendLocal)
	}
	if cfg.LLM.Local.HotTokenLimit == 0 {
		t.Error("default per-backend hot limit not populated")
	}
}

func TestLoad_MalformedJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := DataDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ConfigPath(), []byte(`{not json`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestLoad_PerBackendValuesInJSONWin(t *testing.T) {
	// Per-backend settings in the loaded JSON take precedence over both
	// the Default()'s per-backend values and the legacy top-level fields.
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := DataDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	custom := `{
		"memory": {"hot_token_limit": 9999},
		"llm": {
			"default_backend": "local",
			"local": {"hot_token_limit": 5000, "context_budget": {"max_context_tokens": 7777}},
			"vertex_ai": {}
		}
	}`
	if err := os.WriteFile(ConfigPath(), []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Local.HotTokenLimit != 5000 {
		t.Errorf("local hot: got %d want 5000 (JSON value)", cfg.LLM.Local.HotTokenLimit)
	}
	if cfg.LLM.Local.ContextBudget.MaxContextTokens != 7777 {
		t.Errorf("local MaxContext: got %d want 7777 (JSON value)", cfg.LLM.Local.ContextBudget.MaxContextTokens)
	}
	// Vertex was empty in JSON — Default's pre-populated per-backend
	// values apply (rather than the legacy memory.hot_token_limit).
	// This is current behaviour; see applyBackendInheritance comment.
	if cfg.LLM.VertexAI.HotTokenLimit == 0 {
		t.Error("vertex hot should be filled by Default's per-backend section")
	}
}

func TestSave_RoundtripsThroughLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	original := Default()
	original.LLM.Local.Endpoint = "http://custom:1234"
	original.LLM.Local.Model = "custom-model"
	original.LLM.Local.HotTokenLimit = 8192
	original.Location = "Tokyo"
	original.Memory.UseV2 = true
	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.LLM.Local.Endpoint != original.LLM.Local.Endpoint {
		t.Errorf("Endpoint not roundtripped: %q", loaded.LLM.Local.Endpoint)
	}
	if loaded.LLM.Local.Model != original.LLM.Local.Model {
		t.Errorf("Model not roundtripped: %q", loaded.LLM.Local.Model)
	}
	if loaded.LLM.Local.HotTokenLimit != original.LLM.Local.HotTokenLimit {
		t.Errorf("HotTokenLimit not roundtripped: %d", loaded.LLM.Local.HotTokenLimit)
	}
	if loaded.Location != "Tokyo" {
		t.Errorf("Location not roundtripped: %q", loaded.Location)
	}
	if !loaded.Memory.UseV2 {
		t.Error("Memory.UseV2 not roundtripped")
	}
}

func TestSave_PermissionsRestrictive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Default()
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	// 0600 — owner only. Config may contain locations / endpoints; keep
	// it out of group/world.
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("config file mode = %v, want 0600", mode)
	}
}

func TestSandboxDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Sandbox.Enabled {
		t.Error("default Sandbox.Enabled should be false")
	}
	if cfg.Sandbox.Engine != "auto" {
		t.Errorf("Engine = %q, want auto", cfg.Sandbox.Engine)
	}
	if cfg.Sandbox.Image == "" {
		t.Error("default Image should be populated")
	}
	if cfg.Sandbox.TimeoutSeconds != 60 {
		t.Errorf("TimeoutSeconds = %d, want 60", cfg.Sandbox.TimeoutSeconds)
	}
}

func TestResolvedSandbox_FillsEmptyFields(t *testing.T) {
	cfg := &Config{}
	rs := cfg.ResolvedSandbox()
	if rs.Engine != "auto" || rs.Image == "" || rs.CPULimit == "" || rs.MemoryLimit == "" || rs.TimeoutSeconds == 0 {
		t.Errorf("ResolvedSandbox missing defaults: %+v", rs)
	}
}

func TestResolvedSandbox_PreservesUserValues(t *testing.T) {
	cfg := &Config{Sandbox: SandboxConfig{
		Engine: "docker", Image: "myimg", CPULimit: "4", MemoryLimit: "4g", TimeoutSeconds: 120,
	}}
	rs := cfg.ResolvedSandbox()
	if rs.Engine != "docker" || rs.Image != "myimg" || rs.CPULimit != "4" || rs.MemoryLimit != "4g" || rs.TimeoutSeconds != 120 {
		t.Errorf("user values overwritten: %+v", rs)
	}
}

func TestExpandPath(t *testing.T) {
	t.Setenv("HOME", "/Users/test")
	for in, want := range map[string]string{
		"~/foo":    "/Users/test/foo",
		"/abs":     "/abs",
		"relative": "relative",
		"~no-slash": "~no-slash", // only "~/" prefix is expanded
	} {
		if got := ExpandPath(in); got != want {
			t.Errorf("ExpandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

package config

import (
	"os"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	prof := cfg.LLM.DefaultProfile()
	if prof == nil {
		t.Fatal("DefaultProfile() returned nil")
	}
	if prof.DefaultBackend != BackendLocal {
		t.Errorf("default backend = %v, want %v", prof.DefaultBackend, BackendLocal)
	}
	if prof.Local.Endpoint == "" {
		t.Error("local endpoint is empty")
	}
	if prof.VertexAI.Region == "" {
		t.Error("vertex AI region is empty")
	}
	if prof.Name != DefaultProfileName {
		t.Errorf("default profile name = %q, want %q", prof.Name, DefaultProfileName)
	}
	if prof.ID == "" {
		t.Error("default profile ID is empty")
	}
	// v0.2.0: Memory.HotTokenLimit removed; the legacy v1
	// destructive-compaction trigger is gone.
}

func TestDefault_RequestTimeoutSeconds(t *testing.T) {
	cfg := Default()
	prof := cfg.LLM.DefaultProfile()
	if prof.Local.RequestTimeoutSeconds != LocalRequestTimeoutDefault {
		t.Errorf("Local.RequestTimeoutSeconds = %d, want %d",
			prof.Local.RequestTimeoutSeconds, LocalRequestTimeoutDefault)
	}
	if prof.VertexAI.RequestTimeoutSeconds != VertexRequestTimeoutDefault {
		t.Errorf("VertexAI.RequestTimeoutSeconds = %d, want %d",
			prof.VertexAI.RequestTimeoutSeconds, VertexRequestTimeoutDefault)
	}
}

func TestDefault_AutoExtract(t *testing.T) {
	cfg := Default()
	prof := cfg.LLM.DefaultProfile()
	if prof.Local.AutoExtract() != LocalAutoExtractDefault {
		t.Errorf("Local.AutoExtract() = %v, want %v",
			prof.Local.AutoExtract(), LocalAutoExtractDefault)
	}
	if prof.VertexAI.AutoExtract() != VertexAutoExtractDefault {
		t.Errorf("VertexAI.AutoExtract() = %v, want %v",
			prof.VertexAI.AutoExtract(), VertexAutoExtractDefault)
	}
	// Sanity: the two defaults differ in the direction ADR-0019 §1
	// specifies — local off, vertex on.
	if LocalAutoExtractDefault != false || VertexAutoExtractDefault != true {
		t.Errorf("backend defaults drifted from ADR-0019: local=%v vertex=%v",
			LocalAutoExtractDefault, VertexAutoExtractDefault)
	}
}

// TestDefault_AutoTitle: ADR-0020 invariant — fresh config has local
// title-gen off and Vertex on, matching the per-backend cache-cost
// asymmetry.
func TestDefault_AutoTitle(t *testing.T) {
	cfg := Default()
	prof := cfg.LLM.DefaultProfile()
	if prof.Local.AutoTitle() != LocalAutoTitleDefault {
		t.Errorf("Local.AutoTitle() = %v, want %v",
			prof.Local.AutoTitle(), LocalAutoTitleDefault)
	}
	if prof.VertexAI.AutoTitle() != VertexAutoTitleDefault {
		t.Errorf("VertexAI.AutoTitle() = %v, want %v",
			prof.VertexAI.AutoTitle(), VertexAutoTitleDefault)
	}
	if LocalAutoTitleDefault != false || VertexAutoTitleDefault != true {
		t.Errorf("backend defaults drifted from ADR-0020: local=%v vertex=%v",
			LocalAutoTitleDefault, VertexAutoTitleDefault)
	}
}

func TestAutoTitle_FallbackWhenNil(t *testing.T) {
	if got := (LocalConfig{}).AutoTitle(); got != LocalAutoTitleDefault {
		t.Errorf("LocalConfig{}.AutoTitle() = %v, want %v", got, LocalAutoTitleDefault)
	}
	if got := (VertexAIConfig{}).AutoTitle(); got != VertexAutoTitleDefault {
		t.Errorf("VertexAIConfig{}.AutoTitle() = %v, want %v", got, VertexAutoTitleDefault)
	}
	tru, fls := true, false
	if got := (LocalConfig{AutoTitleEnabled: &tru}).AutoTitle(); got != true {
		t.Errorf("explicit true should win over local default; got %v", got)
	}
	if got := (VertexAIConfig{AutoTitleEnabled: &fls}).AutoTitle(); got != false {
		t.Errorf("explicit false should win over vertex default; got %v", got)
	}
}

func TestAutoExtract_FallbackWhenNil(t *testing.T) {
	if got := (LocalConfig{}).AutoExtract(); got != LocalAutoExtractDefault {
		t.Errorf("LocalConfig{}.AutoExtract() = %v, want %v", got, LocalAutoExtractDefault)
	}
	if got := (VertexAIConfig{}).AutoExtract(); got != VertexAutoExtractDefault {
		t.Errorf("VertexAIConfig{}.AutoExtract() = %v, want %v", got, VertexAutoExtractDefault)
	}
	tru, fls := true, false
	if got := (LocalConfig{AutoExtractEnabled: &tru}).AutoExtract(); got != true {
		t.Errorf("explicit true should win over local default; got %v", got)
	}
	if got := (VertexAIConfig{AutoExtractEnabled: &fls}).AutoExtract(); got != false {
		t.Errorf("explicit false should win over vertex default; got %v", got)
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
	prof := cfg.LLM.DefaultProfile()
	// v0.2.0: HotTokenLimit removed; per-backend ContextBudget is the
	// only remaining capacity knob.
	if prof.Local.ContextBudget.MaxContextTokens == 0 {
		t.Error("local MaxContextTokens is zero")
	}
	if prof.VertexAI.ContextBudget.MaxContextTokens == 0 {
		t.Error("vertex MaxContextTokens is zero")
	}
	if prof.VertexAI.ContextBudget.MaxContextTokens <= prof.Local.ContextBudget.MaxContextTokens {
		t.Errorf("vertex MaxContext (%d) should exceed local (%d) since Vertex models have a much larger window",
			prof.VertexAI.ContextBudget.MaxContextTokens, prof.Local.ContextBudget.MaxContextTokens)
	}
	if prof.Local.ContextBudget.MaxToolResultTokens == 0 {
		t.Error("local tool-result cap is zero")
	}
}

func TestContextBudgetFor_PerFieldFallback(t *testing.T) {
	cfg := Default()
	cfg.LLM.DefaultProfile().Local.ContextBudget = ContextBudgetConfig{MaxContextTokens: 1000} // others zero
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
	// v0.2.0: only ContextBudget remains as the inheritance source.
	// v0.12.0 (ADR-0016): applied to every profile.
	cfg := Default()
	// Wipe the per-backend budgets so inheritance from c.ContextBudget kicks in.
	prof := cfg.LLM.DefaultProfile()
	prof.Local.ContextBudget = ContextBudgetConfig{}
	prof.VertexAI.ContextBudget = ContextBudgetConfig{}
	cfg.ContextBudget = ContextBudgetConfig{MaxContextTokens: 32000, MaxWarmTokens: 1024, MaxToolResultTokens: 2048}
	cfg.applyBackendInheritance()

	if prof.VertexAI.ContextBudget.MaxContextTokens != 32000 {
		t.Errorf("vertex MaxContext inherit: got %d want 32000", prof.VertexAI.ContextBudget.MaxContextTokens)
	}
	if prof.Local.ContextBudget.MaxContextTokens != 32000 {
		t.Errorf("local MaxContext inherit: got %d want 32000", prof.Local.ContextBudget.MaxContextTokens)
	}
}

func TestOutputReserveResolved(t *testing.T) {
	if got := (ContextBudgetConfig{}).OutputReserveResolved(); got != DefaultOutputReserve {
		t.Errorf("zero → default: got %d want %d", got, DefaultOutputReserve)
	}
	if got := (ContextBudgetConfig{OutputReserve: 8192}).OutputReserveResolved(); got != 8192 {
		t.Errorf("explicit value should be honoured, got %d", got)
	}
	cfg := Default()
	prof := cfg.LLM.DefaultProfile()
	if prof.Local.ContextBudget.OutputReserve != DefaultOutputReserve {
		t.Errorf("local default OutputReserve: got %d want %d", prof.Local.ContextBudget.OutputReserve, DefaultOutputReserve)
	}
	if prof.VertexAI.ContextBudget.OutputReserve != DefaultOutputReserve {
		t.Errorf("vertex default OutputReserve: got %d want %d", prof.VertexAI.ContextBudget.OutputReserve, DefaultOutputReserve)
	}
}

// TestDefault_SandboxFieldsAreSane covers the sandbox
// section of Default(): no auto-image (the user picks
// after their first Build), but resource limits and
// engine still have defaults.
func TestDefault_SandboxFieldsAreSane(t *testing.T) {
	cfg := Default()
	if cfg.Sandbox.Image != "" {
		t.Errorf("Sandbox.Image default = %q, want empty (user picks after first build)", cfg.Sandbox.Image)
	}
	if cfg.Sandbox.Engine != "auto" {
		t.Errorf("Sandbox.Engine default = %q, want auto", cfg.Sandbox.Engine)
	}
	if cfg.Sandbox.CPULimit == "" || cfg.Sandbox.MemoryLimit == "" {
		t.Errorf("CPU/Memory limits should have defaults: %+v", cfg.Sandbox)
	}
	_ = strings.TrimSpace // keep strings used as imported
}

func TestMaxToolRoundsResolved(t *testing.T) {
	if got := (AgentConfig{}).MaxToolRoundsResolved(); got != DefaultMaxToolRounds {
		t.Errorf("zero → default: got %d want %d", got, DefaultMaxToolRounds)
	}
	if got := (AgentConfig{MaxToolRounds: 25}).MaxToolRoundsResolved(); got != 25 {
		t.Errorf("explicit value should be honoured, got %d", got)
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
	prof := cfg.LLM.DefaultProfile()
	if prof == nil {
		t.Fatal("DefaultProfile() returned nil after Load()")
	}
	if prof.DefaultBackend != BackendLocal {
		t.Errorf("default backend = %v, want %v", prof.DefaultBackend, BackendLocal)
	}
	if prof.Local.ContextBudget.MaxContextTokens == 0 {
		t.Error("default per-backend MaxContextTokens not populated")
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

func TestLoad_PerBackendValuesInJSONWin_LegacyShape(t *testing.T) {
	// v0.11.x-shape JSON (no profiles[]): UnmarshalJSON synthesises
	// a single "Default" profile from the legacy fields, and per-
	// backend settings inside it take precedence over Default()'s.
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := DataDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	custom := `{
		"llm": {
			"default_backend": "local",
			"local": {"context_budget": {"max_context_tokens": 7777}},
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
	prof := cfg.LLM.DefaultProfile()
	if prof == nil {
		t.Fatal("DefaultProfile() returned nil after legacy-shape Load()")
	}
	if prof.Local.ContextBudget.MaxContextTokens != 7777 {
		t.Errorf("local MaxContext: got %d want 7777 (JSON value)", prof.Local.ContextBudget.MaxContextTokens)
	}
	// Vertex side wasn't specified in the JSON, so applyBackendInheritance
	// keeps its zero value (legacy top-level ContextBudget is also zero
	// in this test, so inheritance produces zero). The first run that
	// observes such a config gets the per-backend defaults from
	// Default() when DefaultProfile() synthesises (lazy init); but in
	// this test we explicitly overrode local only — verify the load
	// path completed without panic and the local override survived.
}

func TestSave_RoundtripsThroughLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	original := Default()
	prof := original.LLM.DefaultProfile()
	prof.Local.Endpoint = "http://custom:1234"
	prof.Local.Model = "custom-model"
	prof.Local.ContextBudget.MaxContextTokens = 8192
	original.Location = "Tokyo"
	originalProfileID := prof.ID
	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loadedProf := loaded.LLM.DefaultProfile()
	if loadedProf == nil {
		t.Fatal("DefaultProfile() returned nil after roundtrip")
	}
	if loadedProf.ID != originalProfileID {
		t.Errorf("profile ID not roundtripped: got %q want %q", loadedProf.ID, originalProfileID)
	}
	if loadedProf.Local.Endpoint != prof.Local.Endpoint {
		t.Errorf("Endpoint not roundtripped: %q", loadedProf.Local.Endpoint)
	}
	if loadedProf.Local.Model != prof.Local.Model {
		t.Errorf("Model not roundtripped: %q", loadedProf.Local.Model)
	}
	if loadedProf.Local.ContextBudget.MaxContextTokens != prof.Local.ContextBudget.MaxContextTokens {
		t.Errorf("MaxContextTokens not roundtripped: %d", loadedProf.Local.ContextBudget.MaxContextTokens)
	}
	if loaded.Location != "Tokyo" {
		t.Errorf("Location not roundtripped: %q", loaded.Location)
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
	// Image is intentionally empty until the user's first Build in
	// the Sandbox tab — the readiness gate keeps sandbox-* tools
	// hidden until then. This used to assert non-empty, which
	// matched a removed default.
	if cfg.Sandbox.Image != "" {
		t.Errorf("default Image should stay empty until user builds, got %q", cfg.Sandbox.Image)
	}
	if cfg.Sandbox.TimeoutSeconds != 60 {
		t.Errorf("TimeoutSeconds = %d, want 60", cfg.Sandbox.TimeoutSeconds)
	}
}

func TestResolvedSandbox_FillsEmptyFields(t *testing.T) {
	cfg := &Config{}
	rs := cfg.ResolvedSandbox()
	// Image is intentionally empty until the user's first Build
	// in the Settings Sandbox tab; the agent's readiness gate
	// refuses to start sandbox tools until it's set. The other
	// fields still get defaults.
	if rs.Engine != "auto" || rs.CPULimit == "" || rs.MemoryLimit == "" || rs.TimeoutSeconds == 0 {
		t.Errorf("ResolvedSandbox missing defaults: %+v", rs)
	}
	if rs.Image != "" {
		t.Errorf("Image should not auto-populate; got %q", rs.Image)
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

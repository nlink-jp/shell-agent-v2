package config

import (
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

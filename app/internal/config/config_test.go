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

func TestDataDir(t *testing.T) {
	dir := DataDir()
	if dir == "" {
		t.Error("data dir is empty")
	}
}

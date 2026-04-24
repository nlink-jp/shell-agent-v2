package llm

import (
	"encoding/json"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

func TestLocalImplementsBackend(t *testing.T) {
	var _ Backend = NewLocal(config.LocalConfig{
		Endpoint: "http://localhost:1234/v1",
		Model:    "test",
	})
}

func TestVertexImplementsBackend(t *testing.T) {
	var _ Backend = NewVertex(config.VertexAIConfig{
		ProjectID: "test-project",
		Region:    "us-central1",
		Model:     "gemini-2.5-flash",
	})
}

func TestLocalName(t *testing.T) {
	l := NewLocal(config.LocalConfig{})
	if l.Name() != "local" {
		t.Errorf("Name() = %v, want local", l.Name())
	}
}

func TestVertexName(t *testing.T) {
	v := NewVertex(config.VertexAIConfig{})
	if v.Name() != "vertex_ai" {
		t.Errorf("Name() = %v, want vertex_ai", v.Name())
	}
}

func TestLocalBuildRequest(t *testing.T) {
	l := NewLocal(config.LocalConfig{Model: "test-model"})
	messages := []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	}
	tools := []ToolDef{
		{Name: "resolve-date", Description: "resolve dates", Parameters: map[string]any{}},
	}

	data := l.buildRequest(messages, tools, false)
	if len(data) == 0 {
		t.Error("buildRequest returned empty data")
	}

	// Verify it's valid JSON
	var req chatRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("buildRequest produced invalid JSON: %v", err)
	}
	if req.Model != "test-model" {
		t.Errorf("model = %v, want test-model", req.Model)
	}
	if len(req.Messages) != 2 {
		t.Errorf("messages count = %d, want 2", len(req.Messages))
	}
	if len(req.Tools) != 1 {
		t.Errorf("tools count = %d, want 1", len(req.Tools))
	}
	if req.Stream {
		t.Error("stream should be false")
	}
}

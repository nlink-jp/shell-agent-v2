package llm

import (
	"context"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Vertex is a Vertex AI (Gemini) backend.
type Vertex struct {
	cfg config.VertexAIConfig
}

// NewVertex creates a new Vertex AI backend.
func NewVertex(cfg config.VertexAIConfig) *Vertex {
	return &Vertex{cfg: cfg}
}

// Name returns the backend identifier.
func (v *Vertex) Name() string { return "vertex_ai" }

// Chat sends messages and returns the complete response.
func (v *Vertex) Chat(_ context.Context, _ []Message, _ []ToolDef) (*Response, error) {
	// TODO: implement Vertex AI via google/genai SDK (Phase 1)
	return &Response{}, nil
}

// ChatStream sends messages and streams the response.
func (v *Vertex) ChatStream(_ context.Context, _ []Message, _ []ToolDef, _ func(StreamChunk)) (*Response, error) {
	// TODO: implement Vertex AI streaming (Phase 1)
	return &Response{}, nil
}

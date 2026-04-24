package llm

import (
	"context"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Local is an OpenAI-compatible API backend (LM Studio, etc.).
type Local struct {
	cfg config.LocalConfig
}

// NewLocal creates a new local LLM backend.
func NewLocal(cfg config.LocalConfig) *Local {
	return &Local{cfg: cfg}
}

// Name returns the backend identifier.
func (l *Local) Name() string { return "local" }

// Chat sends messages and returns the complete response.
func (l *Local) Chat(_ context.Context, _ []Message, _ []ToolDef) (*Response, error) {
	// TODO: implement OpenAI-compatible API client (Phase 1)
	return &Response{}, nil
}

// ChatStream sends messages and streams the response.
func (l *Local) ChatStream(_ context.Context, _ []Message, _ []ToolDef, _ func(StreamChunk)) (*Response, error) {
	// TODO: implement streaming (Phase 1)
	return &Response{}, nil
}

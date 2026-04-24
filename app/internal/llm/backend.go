// Package llm provides a unified interface over LLM backends.
package llm

import (
	"context"
)

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// StreamChunk represents a piece of streamed output.
type StreamChunk struct {
	Content  string
	Done     bool
	ToolCall *ToolCallChunk
}

// ToolCallChunk represents a tool call in a stream.
type ToolCallChunk struct {
	ID        string
	Name      string
	Arguments string
}

// Backend is the interface that all LLM backends must implement.
type Backend interface {
	// Chat sends messages and returns the complete response.
	Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error)

	// ChatStream sends messages and streams the response.
	ChatStream(ctx context.Context, messages []Message, tools []ToolDef, handler func(StreamChunk)) (*Response, error)

	// Name returns the backend identifier.
	Name() string
}

// ToolDef describes a tool available to the LLM.
type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// Response is the complete response from an LLM call.
type Response struct {
	Content   string
	ToolCalls []ToolCall
}

// ToolCall represents a tool invocation requested by the LLM.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

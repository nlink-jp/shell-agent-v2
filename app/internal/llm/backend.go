// Package llm provides a unified interface over LLM backends.
// Design: docs/en/llm-abstraction.md
package llm

import (
	"context"
	"fmt"
)

// Role represents an application-level message role.
// Each backend maps these to its API-specific roles internally.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleReport    Role = "report"
	RoleSummary   Role = "summary"
	RoleSystem    Role = "system"
)

// Message represents a chat message with application-level roles.
// Backends are responsible for mapping roles to their API format.
type Message struct {
	Role      Role     `json:"role"`
	Content   string   `json:"content"`
	ImageURLs []string `json:"image_urls,omitempty"` // data URLs for VLM
	ObjectIDs []string `json:"object_ids,omitempty"` // parallel to ImageURLs; backend uses these to anchor each image to its persistent ID
	ToolName  string   `json:"tool_name,omitempty"`  // for RoleTool: which tool produced this
}

// StreamCallback is called for each streaming token.
type StreamCallback func(token string, toolCalls []ToolCall, done bool)

// Backend is the interface that all LLM backends must implement.
// Backends handle role mapping, tool format conversion, and multimodal
// format conversion internally.
type Backend interface {
	// Chat sends messages and returns the complete response.
	Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error)

	// ChatStream sends messages and streams the response via callback.
	ChatStream(ctx context.Context, messages []Message, tools []ToolDef, cb StreamCallback) (*Response, error)

	// Name returns the backend identifier.
	Name() string
}

// ToolDef describes a tool available to the LLM.
type ToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// Response is the complete response from an LLM call.
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	PromptTokens int
	OutputTokens int
}

// ToolCall represents a tool invocation requested by the LLM.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// imageIDPrefix returns the short text label that immediately
// precedes the i-th image part in a multimodal user message.
// Format follows Google's recommended Gemma multimodal pattern
// — short ID label, no wrapping markers — so the model sees the
// ID adjacent to the image with no intervening tokens that could
// dilute the binding.
//
// When ObjectIDs is missing or shorter than ImageURLs (legacy
// records), fall back to a positional "Image N:" label.
func imageIDPrefix(i int, objectIDs []string) string {
	if i < len(objectIDs) {
		return fmt.Sprintf("Image (object ID: %s):", objectIDs[i])
	}
	return fmt.Sprintf("Image %d:", i+1)
}

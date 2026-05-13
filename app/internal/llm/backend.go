// Package llm provides a unified interface over LLM backends.
// Design: docs/en/history/llm-abstraction.md
package llm

import (
	"context"
	"fmt"
	"strings"
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
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ImageURLs  []string   `json:"image_urls,omitempty"`  // data URLs for VLM
	ObjectIDs  []string   `json:"object_ids,omitempty"`  // parallel to ImageURLs; backend uses these to anchor each image to its persistent ID
	ToolName   string     `json:"tool_name,omitempty"`   // for RoleTool: which tool produced this
	ToolCallID string     `json:"tool_call_id,omitempty"` // for RoleTool: id of the matching call
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`  // for RoleAssistant: function calls the model issued

	// ThoughtPartSigs and TextPartSig preserve Gemini 3+ opaque
	// continuation tokens that arrived on the model's thought
	// and text parts respectively (see ADR-0009). Replayed on
	// subsequent Vertex requests to maintain reasoning continuity;
	// ignored by every other backend. Empty for older Gemini
	// families and for non-Gemini models. encoding/json emits
	// []byte as base64 automatically; omitempty keeps the on-disk
	// record clean for the common non-Gemini-3 case.
	ThoughtPartSigs [][]byte `json:"thought_part_sigs,omitempty"`
	TextPartSig     []byte   `json:"text_part_sig,omitempty"`
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
//
// ThoughtPartSigs and TextPartSig mirror the same-named fields
// on Message; they carry Gemini 3+ continuation tokens captured
// from thought and text parts of the response (ADR-0009). The
// agent loop propagates them into the assistant Record so they
// round-trip back to Vertex on the next request.
type Response struct {
	Content         string
	ToolCalls       []ToolCall
	PromptTokens    int
	OutputTokens    int
	ThoughtPartSigs [][]byte
	TextPartSig     []byte
}

// ToolCall represents a tool invocation requested by the LLM.
//
// ThoughtSignature is the opaque Gemini 3+ continuation token
// attached to the function-call part. Sending the same call back
// without replaying this signature triggers Vertex 400
// INVALID_ARGUMENT ("function call ... is missing a
// thought_signature"). See ADR-0009. Empty for non-Vertex
// backends and for older Gemini families.
type ToolCall struct {
	ID               string
	Name             string
	Arguments        string
	ThoughtSignature []byte
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

// DocumentIDPrefix returns the short text label that introduces
// a markdown / report attachment in a user message. Unlike
// imageIDPrefix this anchor is text-only — the document's
// content is NOT inlined into the message (the LLM reads it via
// analyze-text / grep-text / get-text). The anchor just tells
// the LLM "here's an attachment, its ID is X, here's a quick
// hint at its size so you can decide whether to read it whole
// or grep first".
//
// tokens=0 omits the size hint (legacy records without the
// Tokens field; rare after Load() backfill from v0.5).
func DocumentIDPrefix(id, origName string, tokens int) string {
	sizeStr := ""
	switch {
	case tokens >= 1_000_000:
		sizeStr = fmt.Sprintf(", %.1fM tokens", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		sizeStr = fmt.Sprintf(", %dk tokens", tokens/1_000)
	case tokens > 0:
		sizeStr = fmt.Sprintf(", %d tokens", tokens)
	}
	if origName != "" {
		return fmt.Sprintf("Document (object ID: %s, name: %s%s):", id, origName, sizeStr)
	}
	return fmt.Sprintf("Document (object ID: %s%s):", id, sizeStr)
}

// ObjectMetaLookup resolves an object ID to the metadata
// PrependDocumentAnchors needs. Defined as a func type rather
// than an interface so callers can wrap an objstore.Store with
// a one-line closure without importing objstore into the chat
// or contextbuild packages.
type ObjectMetaLookup func(id string) (origName string, tokens int, ok bool)

// PrependDocumentAnchors prefixes per-document anchor lines to
// content for a user record that carries one or more attached
// markdown / report references. Missing or unresolvable IDs are
// silently skipped (the document may have been deleted out from
// under the record — failing-closed here would block reload of
// otherwise-valid sessions).
//
// Caller contract: only invoke for user-role records. Anchors
// would be confusing on assistant turns and meaningless on tool
// results.
func PrependDocumentAnchors(content string, ids []string, lookup ObjectMetaLookup) string {
	if len(ids) == 0 || lookup == nil {
		return content
	}
	var sb strings.Builder
	emitted := 0
	for _, id := range ids {
		name, tokens, ok := lookup(id)
		if !ok {
			continue // stale ref; tolerate
		}
		sb.WriteString(DocumentIDPrefix(id, name, tokens))
		sb.WriteString("\n")
		emitted++
	}
	if emitted == 0 {
		return content
	}
	sb.WriteString(content)
	return sb.String()
}

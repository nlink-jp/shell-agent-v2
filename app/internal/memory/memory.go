// Package memory manages Hot/Warm/Cold message tiers, sessions, and pinned memory.
package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Tier identifies a memory tier.
type Tier string

const (
	TierHot  Tier = "hot"
	TierWarm Tier = "warm"
	TierCold Tier = "cold"
)

// Record is a single memory entry in a session.
type Record struct {
	Timestamp    time.Time        `json:"timestamp"`
	Role         string           `json:"role"`
	Content      string           `json:"content"`
	Tier         Tier             `json:"tier"`
	ToolCallID   string           `json:"tool_call_id,omitempty"`
	ToolName     string           `json:"tool_name,omitempty"`
	ToolCalls    []ToolCallRecord `json:"tool_calls,omitempty"`   // populated when assistant emits function calls
	ObjectIDs    []string         `json:"object_ids,omitempty"`   // references to objstore
	ImageURLs    []string         `json:"image_urls,omitempty"`   // deprecated: use ObjectIDs
	SummaryRange *TimeRange       `json:"summary_range,omitempty"`
	// Status is meaningful only when Role == "tool". Allowed
	// values: "success", "error". An empty Status on a tool record
	// indicates a session written before this field existed and is
	// treated as "success" at restore time. Design:
	// docs/en/tool-event-restore.md.
	Status string `json:"status,omitempty"`
}

// ToolCallRecord persists one function call the assistant
// issued, so it can be replayed verbatim on subsequent agent-loop
// runs. Without this, Vertex's FunctionResponse and OpenAI's
// `role:"tool"` end up "orphaned" — the spec requires the prior
// assistant turn to carry the matching FunctionCall / tool_call.
type ToolCallRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string the LLM emitted
}

// TimeRange represents a time span for Warm/Cold summaries.
type TimeRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Session holds the conversation state for a single chat session.
type Session struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Records []Record `json:"records"`
}

// AddUserMessage appends a user message to the session.
func (s *Session) AddUserMessage(content string) {
	s.Records = append(s.Records, Record{
		Timestamp: time.Now(),
		Role:      "user",
		Content:   content,
		Tier:      TierHot,
	})
}

// AddAssistantMessage appends an assistant message to the session.
func (s *Session) AddAssistantMessage(content string) {
	s.Records = append(s.Records, Record{
		Timestamp: time.Now(),
		Role:      "assistant",
		Content:   content,
		Tier:      TierHot,
	})
}

// AddAssistantMessageWithToolCalls appends an assistant message
// that issued one or more function calls. content may be empty
// (when the model emitted only tool calls and no narrative); in
// that case the chat UI substitutes a "Calling: foo" placeholder
// at render time. The persisted record stays clean (empty
// content + structured ToolCalls), and build pipelines reproduce
// the proper FunctionCall / tool_calls wire format on the next
// LLM turn.
func (s *Session) AddAssistantMessageWithToolCalls(content string, calls []ToolCallRecord) {
	s.Records = append(s.Records, Record{
		Timestamp: time.Now(),
		Role:      "assistant",
		Content:   content,
		ToolCalls: calls,
		Tier:      TierHot,
	})
}

// AddReportMessage appends a report to the session.
func (s *Session) AddReportMessage(title, content string) {
	s.Records = append(s.Records, Record{
		Timestamp: time.Now(),
		Role:      "report",
		Content:   content,
		ToolName:  title, // reuse ToolName for report title
		Tier:      TierHot,
	})
}

// AddToolResult appends a tool result to the session. status must
// be one of "success" or "error" — same source of truth as the
// ActivityEventStatus emitted by tool_end. Persisting it lets
// LoadSession rebuild tool-event bubbles on session restore with
// the right success / error styling. Design:
// docs/en/tool-event-restore.md.
func (s *Session) AddToolResult(toolCallID, toolName, content, status string) {
	s.Records = append(s.Records, Record{
		Timestamp:  time.Now(),
		Role:       "tool",
		Content:    content,
		ToolCallID: toolCallID,
		ToolName:   toolName,
		Status:     status,
		Tier:       TierHot,
	})
}

// SessionDir returns the directory for a given session.
func SessionDir(sessionID string) string {
	return filepath.Join(config.DataDir(), "sessions", sessionID)
}

// ChatPath returns the path to a session's chat file.
func ChatPath(sessionID string) string {
	return filepath.Join(SessionDir(sessionID), "chat.json")
}

// LoadSession reads a session from disk.
func LoadSession(sessionID string) (*Session, error) {
	data, err := os.ReadFile(ChatPath(sessionID))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Save writes the session to disk atomically (tmp+rename) so a
// crash mid-save leaves either the previous chat.json or the new
// one — never a torn file the next Load would mis-parse
// (security-hardening-2.md C4 / H10).
func (s *Session) Save() error {
	dir := SessionDir(s.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(ChatPath(s.ID), data, 0600)
}

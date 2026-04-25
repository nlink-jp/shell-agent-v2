package chat

import (
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestBuildMessagesGuardWrapsUserContent(t *testing.T) {
	e := New("test system prompt")
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "user", Content: "hello", Tier: memory.TierHot},
		},
	}

	msgs := e.BuildMessages(session, "", "")

	// User message should be guard-wrapped (contains nonce tag)
	userMsg := msgs[1]
	if userMsg.Role != "user" {
		t.Fatalf("expected user role, got %s", userMsg.Role)
	}
	// Guard wrapping adds XML-like tags around content
	if userMsg.Content == "hello" {
		t.Error("user content should be guard-wrapped, but was plain text")
	}
	if !strings.Contains(userMsg.Content, "hello") {
		t.Error("wrapped content should still contain original text")
	}
}

func TestBuildMessagesGuardWrapsToolContent(t *testing.T) {
	e := New("test")
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "tool", Content: "tool result", Tier: memory.TierHot},
		},
	}

	msgs := e.BuildMessages(session, "", "")
	toolMsg := msgs[1]
	if toolMsg.Content == "tool result" {
		t.Error("tool content should be guard-wrapped")
	}
}

func TestBuildMessagesDoesNotGuardAssistant(t *testing.T) {
	e := New("test")
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "assistant", Content: "I am the assistant", Tier: memory.TierHot},
		},
	}

	msgs := e.BuildMessages(session, "", "")
	assistantMsg := msgs[1]
	if assistantMsg.Content != "I am the assistant" {
		t.Errorf("assistant content should NOT be wrapped, got %q", assistantMsg.Content)
	}
}

func TestCleanResponse(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal text", "normal text"},
		{"<think>thinking</think>visible", "visible"},
		{"<thinking>internal</thinking>visible", "visible"},
	}

	for _, tt := range tests {
		got := CleanResponse(tt.input)
		got = strings.TrimSpace(got)
		if got != tt.want {
			t.Errorf("CleanResponse(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

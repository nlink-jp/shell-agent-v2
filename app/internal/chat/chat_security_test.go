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
			{Role: "user", Content: "hello"},
		},
	}

	msgs, err := e.BuildMessages(session, "", "")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}

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
			{Role: "tool", Content: "tool result"},
		},
	}

	msgs, err := e.BuildMessages(session, "", "")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	toolMsg := msgs[1]
	if toolMsg.Content == "tool result" {
		t.Error("tool content should be guard-wrapped")
	}
}

func TestBuildMessagesDoesNotGuardAssistant(t *testing.T) {
	e := New("test")
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "assistant", Content: "I am the assistant"},
		},
	}

	msgs, err := e.BuildMessages(session, "", "")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	assistantMsg := msgs[1]
	if assistantMsg.Content != "I am the assistant" {
		t.Errorf("assistant content should NOT be wrapped, got %q", assistantMsg.Content)
	}
}

// TestWrapUserToolContent_FailClosed pins security-hardening-2.md L1:
// when the underlying guard.Tag.Wrap returns an error (essentially a
// crypto/rand catastrophe — the only realistic failure mode), we
// must surface the error rather than silently feeding the unwrapped
// content into the LLM with elevated trust. We can't easily fault-
// inject crypto/rand from a unit test, so this test serves as a
// contract assertion: the signature is `(string, error)` and the
// happy path returns nil error. If the contract reverts to the old
// silent-fallback signature, this test fails to compile — making the
// regression visible at PR time.
func TestWrapUserToolContent_FailClosed(t *testing.T) {
	e := New("base")
	_ = e.BuildSystemPrompt("", "", "", "")
	wrapped, err := e.WrapUserToolContent("payload")
	if err != nil {
		t.Fatalf("happy path should not error: %v", err)
	}
	if wrapped == "payload" {
		t.Error("expected wrap to add markers")
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

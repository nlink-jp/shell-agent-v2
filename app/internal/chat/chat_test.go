package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestBuildTemporalContext(t *testing.T) {
	ctx := buildTemporalContext()
	now := time.Now()

	if !strings.Contains(ctx, now.Format("2006-01-02")) {
		t.Errorf("temporal context missing today's date: %s", ctx)
	}
	if !strings.Contains(ctx, now.Format("Monday")) {
		t.Errorf("temporal context missing day of week: %s", ctx)
	}
	if !strings.Contains(ctx, "Yesterday:") {
		t.Errorf("temporal context missing yesterday: %s", ctx)
	}
}

func TestBuildMessages(t *testing.T) {
	e := New("You are a helpful assistant.")
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
	}

	msgs := e.BuildMessages(session, "", "")
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("first message role = %v, want system", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content, "helpful assistant") {
		t.Error("system prompt not included")
	}
	if !strings.Contains(msgs[0].Content, "Current date and time") {
		t.Error("temporal context not included")
	}
}

func TestBuildMessagesWithFindings(t *testing.T) {
	e := New("test")
	session := &memory.Session{}

	msgs := e.BuildMessages(session, "", "Sales peak in April")
	if !strings.Contains(msgs[0].Content, "Sales peak in April") {
		t.Error("findings context not included in system prompt")
	}
}

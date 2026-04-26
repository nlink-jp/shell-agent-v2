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

func TestBuildMessagesWithBudget_DropOldMessages(t *testing.T) {
	e := New("test")
	session := &memory.Session{}
	// Add many messages to exceed budget
	for i := 0; i < 30; i++ {
		session.AddUserMessage(strings.Repeat("word ", 100))
		session.AddAssistantMessage(strings.Repeat("reply ", 100))
	}

	result := e.BuildMessagesWithBudget(session, "", "", BuildOptions{
		MaxConversationTokens: 2048,
	})

	if result.DroppedCount == 0 {
		t.Error("expected some messages to be dropped")
	}
	if result.TotalTokens > 2048 {
		t.Errorf("total tokens %d exceeds budget 2048", result.TotalTokens)
	}
	// System prompt should always be first
	if result.Messages[0].Role != "system" {
		t.Error("first message should be system")
	}
	// Most recent messages should be preserved
	lastMsg := result.Messages[len(result.Messages)-1]
	if lastMsg.Role != "assistant" {
		t.Errorf("last message role = %v, want assistant", lastMsg.Role)
	}
}

func TestBuildMessagesWithBudget_SkipCallingMessages(t *testing.T) {
	e := New("test")
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "user", Content: "hello", Tier: memory.TierHot},
			{Role: "assistant", Content: "[Calling: query-sql]", Tier: memory.TierHot},
			{Role: "tool", Content: "result data", Tier: memory.TierHot, ToolName: "query-sql"},
			{Role: "assistant", Content: "Here are the results", Tier: memory.TierHot},
		},
	}

	result := e.BuildMessagesWithBudget(session, "", "", BuildOptions{
		MaxConversationTokens: 8192,
	})

	// [Calling: query-sql] should be skipped
	for _, m := range result.Messages {
		if strings.HasPrefix(m.Content, "[Calling:") {
			t.Error("[Calling:] message should be excluded from budget messages")
		}
	}
	// Should have: system + user + tool + assistant = 4 (not 5)
	if len(result.Messages) != 4 {
		t.Errorf("got %d messages, want 4 (system+user+tool+assistant)", len(result.Messages))
	}
}

func TestBuildMessagesWithBudget_TruncateToolResult(t *testing.T) {
	e := New("test")
	longResult := strings.Repeat("data ", 500) // ~2500 chars
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "user", Content: "query", Tier: memory.TierHot},
			{Role: "tool", Content: longResult, Tier: memory.TierHot, ToolName: "analyze-data"},
			{Role: "assistant", Content: "done", Tier: memory.TierHot},
		},
	}

	result := e.BuildMessagesWithBudget(session, "", "", BuildOptions{
		MaxConversationTokens: 8192,
		MaxToolResultTokens:   100,
	})

	// Find the tool message and check it's truncated
	for _, m := range result.Messages {
		if m.ToolName == "analyze-data" {
			if len(m.Content) >= len(longResult) {
				t.Error("tool result should be truncated")
			}
			if !strings.Contains(m.Content, "truncated") {
				t.Error("truncated content should contain truncation marker")
			}
			break
		}
	}
}

func TestBuildMessagesWithBudget_WarmSummary(t *testing.T) {
	e := New("test")
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "summary", Content: "Earlier conversation summary", Tier: memory.TierWarm},
			{Role: "user", Content: "latest question", Tier: memory.TierHot},
			{Role: "assistant", Content: "latest answer", Tier: memory.TierHot},
		},
	}

	result := e.BuildMessagesWithBudget(session, "", "", BuildOptions{
		MaxConversationTokens: 8192,
		MaxWarmTokens:         1024,
	})

	// Should have: system + warm summary + user + assistant = 4
	if len(result.Messages) != 4 {
		t.Errorf("got %d messages, want 4", len(result.Messages))
	}
	if result.Messages[1].Role != "summary" {
		t.Errorf("second message role = %v, want summary", result.Messages[1].Role)
	}
}

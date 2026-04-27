package llm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// Regression: when the most recent message in a session is large enough to
// alone exceed the token budget, compaction previously moved every hot
// record to the warm summary, leaving the hot tier empty. The Vertex AI
// backend filters system+summary roles out of `contents`, so the request
// went out with an empty contents array and Vertex returned:
//
//	Error 400, Message: Unable to submit request because at least one contents field is required.
//
// This test simulates that scenario end-to-end (without hitting the API):
// build a session with the failing pattern, run CompactIfNeeded with a low
// budget, then verify that vertex.buildContents produces a non-empty result.
func TestVertex_BuildContents_NotEmptyAfterHugeRecentMessageCompact(t *testing.T) {
	now := time.Now()
	huge := strings.Repeat("payload ", 5000)
	s := &memory.Session{
		ID: "regression",
		Records: []memory.Record{
			{Timestamp: now.Add(-10 * time.Minute), Role: "user", Content: "hello", Tier: memory.TierHot},
			{Timestamp: now.Add(-9 * time.Minute), Role: "assistant", Content: "hi", Tier: memory.TierHot},
			{Timestamp: now.Add(-1 * time.Minute), Role: "tool", ToolName: "x", Content: huge, Tier: memory.TierHot},
		},
	}

	summarizer := func(_ context.Context, _ string) (string, error) {
		return "older turns summarized", nil
	}
	compacted, err := s.CompactIfNeeded(context.Background(), memory.CompactOptions{
		HotTokenLimit: 100,
		Summarizer:    summarizer,
	})
	if err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction to occur")
	}

	msgs := []Message{{Role: RoleSystem, Content: "system prompt"}}
	for _, r := range s.Records {
		msgs = append(msgs, Message{
			Role:     Role(r.Role),
			Content:  r.Content,
			ToolName: r.ToolName,
		})
	}

	v := &Vertex{}
	contents := v.buildContents(msgs)
	if len(contents) == 0 {
		t.Fatal("buildContents returned empty slice — Vertex would reject this request with 400")
	}
}

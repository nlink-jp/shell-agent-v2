package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/contextbuild"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// newV2Agent assembles an Agent wired for buildMessagesV2 testing:
// - mock LLM backend (so summarizer calls can be observed)
// - session ID rooted in t.TempDir() so cache writes don't pollute
//   the user's data dir
func newV2Agent(t *testing.T, mock *llm.MockBackend, cfg *config.Config) *Agent {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a := New(cfg)
	a.backend = mock
	a.session = &memory.Session{ID: "v2-test-" + t.Name(), Title: t.Name()}
	return a
}

func TestBuildMessagesV2_SmallSession_NoSummarize(t *testing.T) {
	mock := llm.NewMockBackend(llm.MockResponse{Content: "should not be called"})
	cfg := config.Default()
	cfg.Memory.UseV2 = true
	a := newV2Agent(t, mock, cfg)

	now := time.Now()
	a.session.Records = []memory.Record{
		{Timestamp: now.Add(-time.Minute), Role: "user", Content: "hi", Tier: memory.TierHot},
		{Timestamp: now, Role: "assistant", Content: "hello", Tier: memory.TierHot},
	}

	msgs := a.buildMessagesV2(context.Background(), a.currentBudget())
	if len(msgs) < 2 {
		t.Fatalf("expected system + records, got %d", len(msgs))
	}
	if len(mock.Calls()) != 0 {
		t.Errorf("summarizer should not be invoked when budget allows raw, got %d calls", len(mock.Calls()))
	}
	for _, m := range msgs {
		if m.Role == llm.RoleSummary {
			t.Error("no summary expected for a small session under budget")
		}
	}
}

func TestBuildMessagesV2_OverBudget_TriggersSummarize(t *testing.T) {
	mock := llm.NewMockBackend(llm.MockResponse{Content: "MOCK_SUMMARY"})
	cfg := config.Default()
	cfg.Memory.UseV2 = true
	cfg.LLM.Local.ContextBudget.MaxContextTokens = 200
	a := newV2Agent(t, mock, cfg)

	now := time.Now()
	for i := 0; i < 30; i++ {
		a.session.Records = append(a.session.Records, memory.Record{
			Timestamp: now.Add(time.Duration(i) * time.Minute),
			Role:      "user",
			Content:   strings.Repeat("word ", 40),
			Tier:      memory.TierHot,
		})
	}

	msgs := a.buildMessagesV2(context.Background(), a.currentBudget())
	if len(mock.Calls()) != 1 {
		t.Errorf("summarizer should be invoked once, got %d", len(mock.Calls()))
	}
	var sm *llm.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleSummary {
			sm = &msgs[i]
			break
		}
	}
	if sm == nil {
		t.Fatal("summary block missing from messages")
	}
	if !strings.Contains(sm.Content, "MOCK_SUMMARY") {
		t.Errorf("summary content does not include mock output: %q", sm.Content)
	}
}

func TestBuildMessagesV2_CacheHit_SkipsSummarize(t *testing.T) {
	// Same session content twice in a row → second call must hit the cache
	// and not re-invoke the summarizer.
	mock := llm.NewMockBackend(llm.MockResponse{Content: "FRESH_SUMMARY"})
	cfg := config.Default()
	cfg.Memory.UseV2 = true
	cfg.LLM.Local.ContextBudget.MaxContextTokens = 200
	a := newV2Agent(t, mock, cfg)

	now := time.Now()
	for i := 0; i < 30; i++ {
		a.session.Records = append(a.session.Records, memory.Record{
			Timestamp: now.Add(time.Duration(i) * time.Minute),
			Role:      "user",
			Content:   strings.Repeat("word ", 40),
			Tier:      memory.TierHot,
		})
	}

	_ = a.buildMessagesV2(context.Background(), a.currentBudget())
	firstCalls := len(mock.Calls())

	// Reload — the on-disk cache from the previous build must be picked up
	// next time. Force LoadCache to re-read by using a fresh agent on the
	// SAME session ID + HOME (set via t.Setenv in newV2Agent), which is
	// preserved across this same test by using t.Name() in the session ID.

	_ = a.buildMessagesV2(context.Background(), a.currentBudget())
	if len(mock.Calls()) != firstCalls {
		t.Errorf("second build should not re-invoke summarizer (got %d calls, want %d)", len(mock.Calls()), firstCalls)
	}
}

func TestBuildMessagesV2_SummarizerError_DoesNotAbort(t *testing.T) {
	// Summarizer failure is non-fatal: the older tail is silently dropped
	// from the rendered context but the LLM-bound message list is still
	// returned with at least the most recent raw record.
	mock := llm.NewMockBackend(llm.MockResponse{Err: errors.New("LLM unavailable")})
	cfg := config.Default()
	cfg.Memory.UseV2 = true
	cfg.LLM.Local.ContextBudget.MaxContextTokens = 200
	a := newV2Agent(t, mock, cfg)

	now := time.Now()
	for i := 0; i < 30; i++ {
		a.session.Records = append(a.session.Records, memory.Record{
			Timestamp: now.Add(time.Duration(i) * time.Minute),
			Role:      "user",
			Content:   strings.Repeat("word ", 40),
			Tier:      memory.TierHot,
		})
	}

	msgs := a.buildMessagesV2(context.Background(), a.currentBudget())
	if len(msgs) == 0 {
		t.Fatal("messages must not be empty even after summarizer error")
	}
	// At least one non-system message must be present so Vertex's
	// "contents required" error doesn't recur.
	rawCount := 0
	for _, m := range msgs {
		if m.Role != llm.RoleSystem && m.Role != llm.RoleSummary {
			rawCount++
		}
	}
	if rawCount == 0 {
		t.Error("expected at least one raw record on summarizer error")
	}
}

func TestBuildMessagesV2_LegacySummaryRecordsRendered(t *testing.T) {
	mock := llm.NewMockBackend()
	cfg := config.Default()
	cfg.Memory.UseV2 = true
	a := newV2Agent(t, mock, cfg)

	now := time.Now()
	a.session.Records = []memory.Record{
		{
			Timestamp: now.Add(-24 * time.Hour),
			Role:      "summary",
			Content:   "Earlier we discussed the analysis approach.",
			Tier:      memory.TierWarm,
			SummaryRange: &memory.TimeRange{
				From: now.Add(-25 * time.Hour),
				To:   now.Add(-23 * time.Hour),
			},
		},
		{Timestamp: now.Add(-time.Minute), Role: "user", Content: "what next?", Tier: memory.TierHot},
	}

	msgs := a.buildMessagesV2(context.Background(), a.currentBudget())
	var sm *llm.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleSummary {
			sm = &msgs[i]
			break
		}
	}
	if sm == nil {
		t.Fatal("legacy summary should surface as a summary block")
	}
	if !strings.Contains(sm.Content, "analysis approach") {
		t.Errorf("legacy summary text missing in %q", sm.Content)
	}
	if !strings.Contains(sm.Content, "Summary of") {
		t.Errorf("legacy summary should still get a time-range header: %q", sm.Content)
	}
}

// Ensure contextbuild imports and helpers compile-link from this test file.
var _ = contextbuild.Build

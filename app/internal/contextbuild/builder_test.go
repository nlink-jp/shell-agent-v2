package contextbuild

import (
	stdcontext "context"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// mockSummarize returns a fixed short summary regardless of input — same
// shape as a real summarizer, which is expected to compress significantly.
func mockSummarize(_ stdcontext.Context, records []memory.Record) (string, error) {
	return "MOCK SUMMARY", nil
}

func sysMessage(t *testing.T, msgs []llm.Message) llm.Message {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	return msgs[0]
}

func summaryMessage(msgs []llm.Message) *llm.Message {
	for i, m := range msgs {
		if m.Role == llm.RoleSummary {
			return &msgs[i]
		}
	}
	return nil
}

func TestBuild_EmptySession(t *testing.T) {
	res := Build(stdcontext.Background(), &memory.Session{}, &SummaryCache{}, BuildOptions{
		SystemPrompt: "you are an agent",
	})
	if len(res.Messages) != 1 {
		t.Fatalf("expected 1 (system) message, got %d", len(res.Messages))
	}
	if sysMessage(t, res.Messages).Role != llm.RoleSystem {
		t.Error("first message should be system")
	}
	if res.IncludedRaw != 0 || res.SummarizedSpan != 0 {
		t.Errorf("counters should be zero on empty session")
	}
}

func TestBuild_AlwaysIncludesRecent_HugeMessage(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	huge := strings.Repeat("payload ", 5000)
	s := &memory.Session{
		Records: []memory.Record{
			mkRec(now.Add(-10*time.Minute), "user", "hello"),
			mkRec(now.Add(-9*time.Minute), "assistant", "hi"),
			mkRec(now.Add(-1*time.Minute), "tool", huge),
		},
	}
	res := Build(stdcontext.Background(), s, &SummaryCache{}, BuildOptions{
		SystemPrompt:     "sys",
		MaxContextTokens: 100, // tiny — most records won't fit
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
	})
	if res.IncludedRaw < 1 {
		t.Errorf("at least one raw record must be included; got %d", res.IncludedRaw)
	}
	last := res.Messages[len(res.Messages)-1]
	if !strings.Contains(last.Content, "payload") {
		t.Errorf("last message should be the huge tool record; got %q", last.Content)
	}
}

func TestBuild_FoldsOlderIntoSummary(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	var recs []memory.Record
	for i := 0; i < 20; i++ {
		recs = append(recs, mkRec(now.Add(time.Duration(i)*time.Minute), "user", strings.Repeat("word ", 50)))
	}
	s := &memory.Session{Records: recs}
	res := Build(stdcontext.Background(), s, &SummaryCache{}, BuildOptions{
		SystemPrompt:     "sys",
		MaxContextTokens: 200, // forces folding
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
	})
	sm := summaryMessage(res.Messages)
	if sm == nil {
		t.Fatal("expected a summary message in result")
	}
	if !strings.Contains(sm.Content, "Summary of") {
		t.Errorf("summary should have time-range header; got %q", sm.Content)
	}
	if res.SummarizedSpan == 0 {
		t.Error("SummarizedSpan should be > 0 when folding occurred")
	}
}

func TestBuild_SummaryHasTimeRangeHeader(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	recs := []memory.Record{
		mkRec(now.Add(-90*time.Minute), "user", strings.Repeat("first ", 50)),
		mkRec(now.Add(-60*time.Minute), "assistant", strings.Repeat("a ", 50)),
		mkRec(now.Add(-30*time.Minute), "user", strings.Repeat("second ", 50)),
		mkRec(now.Add(-1*time.Minute), "user", "now"),
	}
	res := Build(stdcontext.Background(), &memory.Session{Records: recs}, &SummaryCache{}, BuildOptions{
		MaxContextTokens: 80,
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
	})
	sm := summaryMessage(res.Messages)
	if sm == nil {
		t.Fatal("expected summary message")
	}
	expectFrom := "2026-04-27 08:30 UTC" // 90min before 10:00
	expectTo := "2026-04-27 09:30 UTC"   // 30min before 10:00 (the latest folded record)
	if !strings.Contains(sm.Content, expectFrom) {
		t.Errorf("expected from-timestamp %q in summary, got %q", expectFrom, sm.Content)
	}
	if !strings.Contains(sm.Content, expectTo) {
		t.Errorf("expected to-timestamp %q in summary, got %q", expectTo, sm.Content)
	}
}

func TestBuild_LegacySummaryRecordsRespected(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	legacy := memory.Record{
		Timestamp: now.Add(-24 * time.Hour),
		Role:      "summary",
		Content:   "Earlier we discussed Tokyo trip.",
		Tier:      memory.TierWarm,
		SummaryRange: &memory.TimeRange{
			From: now.Add(-25 * time.Hour),
			To:   now.Add(-23 * time.Hour),
		},
	}
	s := &memory.Session{Records: []memory.Record{
		legacy,
		mkRec(now.Add(-5*time.Minute), "user", "what about Kyoto?"),
	}}
	res := Build(stdcontext.Background(), s, &SummaryCache{}, BuildOptions{
		MaxContextTokens: 0, // unlimited — recent stays raw, legacy still becomes summary block
		SummarizerID:     "test",
		Loc:              utc,
	})
	sm := summaryMessage(res.Messages)
	if sm == nil {
		t.Fatal("expected summary block carrying the legacy record")
	}
	if !strings.Contains(sm.Content, "Tokyo trip") {
		t.Errorf("legacy summary text missing in rendered block; got %q", sm.Content)
	}
	if !strings.Contains(sm.Content, "Summary of") {
		t.Errorf("legacy summary should also get the time-range header")
	}
}

func TestBuild_CacheHitSkipsSummarizer(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	older := []memory.Record{
		mkRec(now.Add(-2*time.Hour), "user", strings.Repeat("a ", 100)),
		mkRec(now.Add(-90*time.Minute), "assistant", strings.Repeat("b ", 100)),
	}
	recs := append([]memory.Record{}, older...)
	recs = append(recs, mkRec(now.Add(-1*time.Minute), "user", "recent"))
	s := &memory.Session{Records: recs}

	cache := &SummaryCache{}
	cache.Put(SummaryEntry{
		RangeKey:      ComputeRangeKey(older, "test"),
		SummarizerID:  "test",
		FromTimestamp: older[0].Timestamp,
		ToTimestamp:   older[len(older)-1].Timestamp,
		RecordCount:   len(older),
		Summary:       "PRE-CACHED SUMMARY",
	})

	calls := 0
	res := Build(stdcontext.Background(), s, cache, BuildOptions{
		MaxContextTokens: 30,
		SummarizerID:     "test",
		Summarize: func(ctx stdcontext.Context, _ []memory.Record) (string, error) {
			calls++
			return "should not be called", nil
		},
		Loc: utc,
	})
	if calls != 0 {
		t.Errorf("summarizer called %d times despite cache hit", calls)
	}
	sm := summaryMessage(res.Messages)
	if sm == nil || !strings.Contains(sm.Content, "PRE-CACHED SUMMARY") {
		t.Errorf("cached summary not used; got %v", sm)
	}
	if !res.UsedCache {
		t.Error("UsedCache should be true on cache hit")
	}
}

func TestBuild_FitsInBudget_BasicSanity(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	var recs []memory.Record
	for i := 0; i < 30; i++ {
		recs = append(recs, mkRec(now.Add(time.Duration(i)*time.Minute), "user", strings.Repeat("word ", 30)))
	}
	res := Build(stdcontext.Background(), &memory.Session{Records: recs}, &SummaryCache{}, BuildOptions{
		MaxContextTokens: 500,
		OutputReserve:    50,
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
	})
	if res.TotalTokens > 500 {
		// Heuristic — token estimation is approximate, so we allow modest
		// overshoot but flag obvious blowouts (>2x).
		if res.TotalTokens > 1000 {
			t.Errorf("total tokens %d significantly exceeds budget 500", res.TotalTokens)
		}
	}
}

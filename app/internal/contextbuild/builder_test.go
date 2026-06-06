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
	res, _ := Build(stdcontext.Background(), &memory.Session{}, &SummaryCache{}, BuildOptions{
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
	res, _ := Build(stdcontext.Background(), s, &SummaryCache{}, BuildOptions{
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
	for i := range 20 {
		recs = append(recs, mkRec(now.Add(time.Duration(i)*time.Minute), "user", strings.Repeat("word ", 50)))
	}
	s := &memory.Session{Records: recs}
	res, _ := Build(stdcontext.Background(), s, &SummaryCache{}, BuildOptions{
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
	res, _ := Build(stdcontext.Background(), &memory.Session{Records: recs}, &SummaryCache{}, BuildOptions{
		MaxContextTokens: 80,
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
	})
	// ADR-0032: summary is now split into far + near tiers. The
	// far tier carries the oldest folded record (08:30); the near
	// tier carries the rest of the folded span (up to 09:30). The
	// concatenation of both summary messages must cover both
	// endpoints.
	all := ""
	for _, m := range res.Messages {
		if m.Role == llm.RoleSummary {
			all += m.Content
		}
	}
	if all == "" {
		t.Fatal("expected at least one summary message")
	}
	expectFrom := "2026-04-27 08:30 UTC"
	expectTo := "2026-04-27 09:30 UTC"
	if !strings.Contains(all, expectFrom) {
		t.Errorf("expected from-timestamp %q across summaries, got %q", expectFrom, all)
	}
	if !strings.Contains(all, expectTo) {
		t.Errorf("expected to-timestamp %q across summaries, got %q", expectTo, all)
	}
}

func TestBuild_LegacySummaryRecordsRespected(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	legacy := memory.Record{
		Timestamp: now.Add(-24 * time.Hour),
		Role:      "summary",
		Content:   "Earlier we discussed Tokyo trip.",
		
		SummaryRange: &memory.TimeRange{
			From: now.Add(-25 * time.Hour),
			To:   now.Add(-23 * time.Hour),
		},
	}
	s := &memory.Session{Records: []memory.Record{
		legacy,
		mkRec(now.Add(-5*time.Minute), "user", "what about Kyoto?"),
	}}
	res, _ := Build(stdcontext.Background(), s, &SummaryCache{}, BuildOptions{
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

	// ADR-0032: cache is keyed by ComputeContentKey per tier.
	// With 2 older records, partitionForTiers puts 1 in each tier.
	// Seed both tiers' cache entries so the summarizer can be
	// fully short-circuited.
	farInput := older[:1]
	nearInput := older[1:]
	cache := &SummaryCache{}
	cache.Put(SummaryEntry{
		RangeKey:    ComputeContentKey(farInput, "test", nil, nil, "far"),
		Tier:        "far",
		Summary:     "PRE-CACHED FAR",
		RecordCount: len(farInput),
	})
	cache.Put(SummaryEntry{
		RangeKey:    ComputeContentKey(nearInput, "test", nil, nil, "near"),
		Tier:        "near",
		Summary:     "PRE-CACHED NEAR",
		RecordCount: len(nearInput),
	})

	calls := 0
	res, _ := Build(stdcontext.Background(), s, cache, BuildOptions{
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
	all := ""
	for _, m := range res.Messages {
		if m.Role == llm.RoleSummary {
			all += m.Content
		}
	}
	if !strings.Contains(all, "PRE-CACHED FAR") {
		t.Errorf("far tier cache hit missing; got %q", all)
	}
	if !strings.Contains(all, "PRE-CACHED NEAR") {
		t.Errorf("near tier cache hit missing; got %q", all)
	}
	if !res.UsedCache {
		t.Error("UsedCache should be true on cache hit")
	}
}

func TestBuild_ExcludesCallingMarkers(t *testing.T) {
	// Regression: gemma-style local models will mimic any [Calling: ...]
	// pattern they see in context as plain text instead of using the real
	// tool API. The legacy chat.BuildMessagesWithBudget filtered these
	// out; the v2 path must too.
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	s := &memory.Session{Records: []memory.Record{
		mkRec(now, "user", "load /tmp/x.csv"),
		mkRec(now.Add(time.Second), "assistant", "[Calling: load_data{file_path:/tmp/x.csv}]"),
		mkRec(now.Add(2*time.Second), "tool", "loaded 100 rows"),
		mkRec(now.Add(3*time.Second), "assistant", "Loaded successfully."),
	}}
	res, _ := Build(stdcontext.Background(), s, &SummaryCache{}, BuildOptions{
		MaxContextTokens: 0, // unlimited
		Loc:              utc,
	})
	for _, m := range res.Messages {
		if strings.Contains(m.Content, "[Calling:") {
			t.Errorf("[Calling: ...] marker leaked into LLM context: %q", m.Content)
		}
	}
	if res.IncludedRaw != 3 {
		t.Errorf("expected 3 raw records (user/tool/assistant), got %d", res.IncludedRaw)
	}
}

func TestBuild_FitsInBudget_BasicSanity(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	var recs []memory.Record
	for i := range 30 {
		recs = append(recs, mkRec(now.Add(time.Duration(i)*time.Minute), "user", strings.Repeat("word ", 30)))
	}
	res, _ := Build(stdcontext.Background(), &memory.Session{Records: recs}, &SummaryCache{}, BuildOptions{
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

// TestBuild_UserRecordPrefixByteStable confirms ADR-0017's load-bearing
// invariant at the Build level: two Build calls on the same session and
// options must produce a byte-identical message array. This is what
// llama.cpp's prompt prefix KV cache needs to fire across turns.
func TestBuild_UserRecordPrefixByteStable(t *testing.T) {
	ts := time.Date(2026, 5, 20, 12, 34, 56, 0, time.UTC)
	session := &memory.Session{
		Records: []memory.Record{
			{Timestamp: ts, Role: "user", Content: "Hello"},
			{Timestamp: ts.Add(2 * time.Second), Role: "assistant", Content: "Hi there"},
			{Timestamp: ts.Add(4 * time.Second), Role: "user", Content: "How are you?"},
		},
	}
	opts := BuildOptions{
		SystemPrompt: "you are an agent",
		Loc:          time.UTC,
		UserRecordTemporalPrefix: func(t time.Time) string {
			return "[Time: " + t.Format("2006-01-02 15:04:05 MST") + "]"
		},
	}
	resA, errA := Build(stdcontext.Background(), session, &SummaryCache{}, opts)
	resB, errB := Build(stdcontext.Background(), session, &SummaryCache{}, opts)
	if errA != nil || errB != nil {
		t.Fatalf("Build errors: A=%v B=%v", errA, errB)
	}
	if len(resA.Messages) != len(resB.Messages) {
		t.Fatalf("message-count drift: A=%d B=%d", len(resA.Messages), len(resB.Messages))
	}
	for i := range resA.Messages {
		if resA.Messages[i].Content != resB.Messages[i].Content {
			t.Errorf("message %d content drift:\nA: %q\nB: %q",
				i, resA.Messages[i].Content, resB.Messages[i].Content)
		}
	}
}

// TestBuild_UserRecordPrefixNotOnAssistantOrTool verifies that the
// temporal prefix is scoped to user records — assistant / tool / report
// messages must render without it regardless of the option being set.
func TestBuild_UserRecordPrefixNotOnAssistantOrTool(t *testing.T) {
	ts := time.Date(2026, 5, 20, 12, 34, 56, 0, time.UTC)
	session := &memory.Session{
		Records: []memory.Record{
			{Timestamp: ts, Role: "user", Content: "Hello"},
			{Timestamp: ts.Add(time.Second), Role: "assistant", Content: "Hi"},
			{Timestamp: ts.Add(2 * time.Second), Role: "tool", Content: "tool-result"},
		},
	}
	opts := BuildOptions{
		SystemPrompt: "sys",
		Loc:          time.UTC,
		UserRecordTemporalPrefix: func(t time.Time) string {
			return "<<TS:" + t.Format("2006-01-02") + ">>"
		},
	}
	res, err := Build(stdcontext.Background(), session, &SummaryCache{}, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// res.Messages: [system, user, assistant, tool]
	if len(res.Messages) < 4 {
		t.Fatalf("expected ≥4 messages, got %d", len(res.Messages))
	}
	// User: should contain the marker.
	if !strings.Contains(res.Messages[1].Content, "<<TS:") {
		t.Errorf("user message missing temporal prefix; got %q", res.Messages[1].Content)
	}
	// Assistant / tool: must NOT contain the marker.
	if strings.Contains(res.Messages[2].Content, "<<TS:") {
		t.Errorf("assistant message unexpectedly has temporal prefix; got %q", res.Messages[2].Content)
	}
	if strings.Contains(res.Messages[3].Content, "<<TS:") {
		t.Errorf("tool message unexpectedly has temporal prefix; got %q", res.Messages[3].Content)
	}
}

// --- ADR-0032 two-tier / anchor / dead-topic tests --------------

// TestBuild_TwoTierAssemblyOrder verifies the assembly order is
// system → far summary → near summary → anchored records → raw.
func TestBuild_TwoTierAssemblyOrder(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, utc)
	recs := []memory.Record{
		mkRec(now.Add(-180*time.Minute), "user", strings.Repeat("old ", 60)),
		mkRec(now.Add(-150*time.Minute), "assistant", strings.Repeat("old assistant ", 60)),
		mkRec(now.Add(-120*time.Minute), "user", strings.Repeat("mid ", 60)),
		mkRec(now.Add(-90*time.Minute), "assistant", strings.Repeat("mid assistant ", 60)),
		mkRec(now.Add(-1*time.Minute), "user", "recent"),
	}
	res, _ := Build(stdcontext.Background(), &memory.Session{Records: recs}, &SummaryCache{}, BuildOptions{
		SystemPrompt:     "you are a helpful assistant",
		MaxContextTokens: 60,
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
	})

	if len(res.Messages) < 3 {
		t.Fatalf("expected at least system + 2 summary tiers + raw, got %d msgs", len(res.Messages))
	}
	// system first
	if res.Messages[0].Role != llm.RoleSystem {
		t.Errorf("msgs[0].Role = %q, want system", res.Messages[0].Role)
	}
	// Summary messages come before non-summary in this fixture
	// (we have no anchors, so anchored block is empty).
	summariesEnd := 0
	for i, m := range res.Messages {
		if m.Role == llm.RoleSummary {
			summariesEnd = i
		}
	}
	for i, m := range res.Messages {
		if i > 0 && i <= summariesEnd && m.Role != llm.RoleSystem && m.Role != llm.RoleSummary {
			t.Errorf("non-summary msg at idx %d before last summary at %d (broke assembly order)", i, summariesEnd)
		}
	}

	// Both tiers should have at least one summarized record.
	if res.NearSummarizedSpan == 0 || res.FarSummarizedSpan == 0 {
		t.Errorf("expected non-zero span in both tiers, got near=%d far=%d",
			res.NearSummarizedSpan, res.FarSummarizedSpan)
	}
}

// TestBuild_AnchorRecordsRenderedVerbatim verifies that records
// whose content matches an AnchorSource are pulled into a verbatim
// position before raw records, not just paraphrased in a summary.
func TestBuild_AnchorRecordsRenderedVerbatim(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, utc)
	const anchorMarker = "DECISION: chose DuckDB over SQLite for sales analysis"
	recs := []memory.Record{
		mkRec(now.Add(-120*time.Minute), "user", anchorMarker),
		mkRec(now.Add(-90*time.Minute), "assistant", "I'll proceed with that choice."),
		mkRec(now.Add(-60*time.Minute), "user", strings.Repeat("middle chatter ", 30)),
		mkRec(now.Add(-1*time.Minute), "user", "recent"),
	}
	res, _ := Build(stdcontext.Background(), &memory.Session{Records: recs}, &SummaryCache{}, BuildOptions{
		MaxContextTokens:       40,
		SummarizerID:           "test",
		Summarize:              mockSummarize,
		Loc:                    utc,
		AnchorSources:          []string{"chose DuckDB over SQLite for sales analysis"},
		AnchorJaccardThreshold: 0.3,
	})

	if res.AnchoredRecords == 0 {
		t.Fatal("expected at least one anchored record")
	}
	// The verbatim anchor must appear as a non-summary message
	// (the rendered content for a user record includes the raw
	// text).
	foundVerbatim := false
	for _, m := range res.Messages {
		if m.Role == llm.RoleSummary {
			continue
		}
		if strings.Contains(m.Content, "chose DuckDB over SQLite") {
			foundVerbatim = true
			break
		}
	}
	if !foundVerbatim {
		t.Error("anchor record was not rendered verbatim outside summary blocks")
	}
}

// TestBuild_DeadTopicDrop_SuppressesAndCounts verifies that records
// matching a dormant fact (without a live overlap) are dropped from
// the summary input and surfaced as an elision marker, while the
// dropped count is propagated.
func TestBuild_DeadTopicDrop_SuppressesAndCounts(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, utc)
	recs := []memory.Record{
		mkRec(now.Add(-120*time.Minute), "user", "Weather in Tokyo is rainy today rainy today"),
		mkRec(now.Add(-90*time.Minute), "assistant", "Weather updates noted noted noted"),
		mkRec(now.Add(-60*time.Minute), "user", strings.Repeat("topic shift ", 30)),
		mkRec(now.Add(-1*time.Minute), "user", "recent"),
	}
	res, _ := Build(stdcontext.Background(), &memory.Session{Records: recs}, &SummaryCache{}, BuildOptions{
		MaxContextTokens:          40,
		SummarizerID:              "test",
		Summarize:                 mockSummarize,
		Loc:                       utc,
		DeadTopicSources:          []string{"Weather Tokyo rainy"},
		LiveTopicSources:          []string{},
		DeadTopicJaccardThreshold: 0.3,
	})

	if res.DroppedDeadTopics == 0 {
		t.Fatal("expected at least one dead-topic drop")
	}
	all := ""
	for _, m := range res.Messages {
		if m.Role == llm.RoleSummary {
			all += m.Content
		}
	}
	if !strings.Contains(all, "dead-topic turns suppressed") {
		t.Errorf("expected elision marker in summary block, got %q", all)
	}
}

// TestBuild_CacheStableAcrossTurnAdditions verifies the content-hash
// key keeps a tier's cache hit alive when an adjacent turn is added
// to the session without changing the tier's input.
func TestBuild_CacheStableAcrossTurnAdditions(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, utc)
	older := []memory.Record{
		mkRec(now.Add(-3*time.Hour), "user", strings.Repeat("a ", 80)),
		mkRec(now.Add(-2*time.Hour+30*time.Minute), "assistant", strings.Repeat("b ", 80)),
	}
	turn1 := append([]memory.Record{}, older...)
	turn1 = append(turn1, mkRec(now.Add(-1*time.Minute), "user", "recent 1"))

	cache := &SummaryCache{}
	res1, _ := Build(stdcontext.Background(), &memory.Session{Records: turn1}, cache, BuildOptions{
		MaxContextTokens: 30,
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
	})
	if res1.UsedCache {
		t.Fatal("turn 1 should populate cache, not hit it")
	}

	// Turn 2: add a new most-recent record. Older window unchanged.
	turn2 := append([]memory.Record{}, turn1...)
	turn2 = append(turn2, mkRec(now, "user", "recent 2"))

	res2, _ := Build(stdcontext.Background(), &memory.Session{Records: turn2}, cache, BuildOptions{
		MaxContextTokens: 30,
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
	})
	if !res2.UsedCache {
		t.Errorf("turn 2 should hit cache: near=%v far=%v", res2.NearCacheHit, res2.FarCacheHit)
	}
}

// TestBuild_LifecycleChangeInvalidatesCache verifies that a change
// in the DeadTopicSources set causes the content-hash key to change
// and forces regeneration.
func TestBuild_LifecycleChangeInvalidatesCache(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, utc)
	recs := []memory.Record{
		mkRec(now.Add(-2*time.Hour), "user", strings.Repeat("a ", 80)),
		mkRec(now.Add(-90*time.Minute), "assistant", strings.Repeat("b ", 80)),
		mkRec(now.Add(-1*time.Minute), "user", "recent"),
	}

	cache := &SummaryCache{}
	_, _ = Build(stdcontext.Background(), &memory.Session{Records: recs}, cache, BuildOptions{
		MaxContextTokens: 30,
		SummarizerID:     "test",
		Summarize:        mockSummarize,
		Loc:              utc,
		// No dead set in round 1.
	})

	// Round 2: introduce a dead-topic source. Even if no record
	// actually matches (so no drop fires), the cache key still
	// changes because deadFingerprints contribute to the key when
	// drops occur — but when no drop fires, fingerprints stay empty
	// and the key stays the same. Use a dead source that DOES match
	// to exercise the invalidation path properly.
	calls := 0
	_, _ = Build(stdcontext.Background(), &memory.Session{Records: recs}, cache, BuildOptions{
		MaxContextTokens: 30,
		SummarizerID:     "test",
		Summarize: func(ctx stdcontext.Context, _ []memory.Record) (string, error) {
			calls++
			return "regenerated", nil
		},
		Loc:                       utc,
		DeadTopicSources:          []string{"a a a a a"}, // matches "a a a a..." record
		DeadTopicJaccardThreshold: 0.3,
	})
	if calls == 0 {
		t.Error("expected summarizer to regenerate after dead-topic set introduced")
	}
}

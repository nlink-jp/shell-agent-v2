package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text string
		min  int
	}{
		{"hello world", 2},
		{"", 0},
		{"This is a longer sentence with several words.", 8},
		{"日本語のテスト", 10}, // CJK characters count more
	}

	for _, tt := range tests {
		got := EstimateTokens(tt.text)
		if got < tt.min {
			t.Errorf("EstimateTokens(%q) = %d, want >= %d", tt.text, got, tt.min)
		}
	}
}

func TestCompactIfNeeded_NoCompaction(t *testing.T) {
	s := &Session{
		ID: "test",
		Records: []Record{
			{Timestamp: time.Now(), Role: "user", Content: "hello", Tier: TierHot},
		},
	}

	compacted, err := s.CompactIfNeeded(context.Background(), CompactOptions{
		HotTokenLimit: 10000,
		Summarizer:    mockSummarizer,
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if compacted {
		t.Error("should not compact when under limit")
	}
}

func TestCompactIfNeeded_Compacts(t *testing.T) {
	now := time.Now()
	s := &Session{
		ID: "test",
		Records: []Record{
			{Timestamp: now.Add(-10 * time.Minute), Role: "user", Content: strings.Repeat("word ", 200), Tier: TierHot},
			{Timestamp: now.Add(-9 * time.Minute), Role: "assistant", Content: strings.Repeat("reply ", 200), Tier: TierHot},
			{Timestamp: now.Add(-8 * time.Minute), Role: "user", Content: strings.Repeat("more ", 200), Tier: TierHot},
			{Timestamp: now.Add(-7 * time.Minute), Role: "assistant", Content: strings.Repeat("data ", 200), Tier: TierHot},
			{Timestamp: now.Add(-1 * time.Minute), Role: "user", Content: "recent message", Tier: TierHot},
		},
	}

	compacted, err := s.CompactIfNeeded(context.Background(), CompactOptions{
		HotTokenLimit: 100, // very low limit to force compaction
		Summarizer:    mockSummarizer,
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !compacted {
		t.Error("should compact when over limit")
	}

	// Check we have a warm record
	hasWarm := false
	for _, r := range s.Records {
		if r.Tier == TierWarm {
			hasWarm = true
			if r.SummaryRange == nil {
				t.Error("warm record should have summary range")
			}
		}
	}
	if !hasWarm {
		t.Error("expected warm record after compaction")
	}

	// Recent message should still be hot
	lastHot := s.Records[len(s.Records)-1]
	if lastHot.Content != "recent message" {
		t.Errorf("last record = %q, want 'recent message'", lastHot.Content)
	}
	if lastHot.Tier != TierHot {
		t.Errorf("last record tier = %v, want hot", lastHot.Tier)
	}
}

func TestCompactIfNeeded_NoSummarizer(t *testing.T) {
	s := &Session{
		ID: "test",
		Records: []Record{
			{Timestamp: time.Now(), Role: "user", Content: strings.Repeat("word ", 500), Tier: TierHot},
		},
	}

	compacted, err := s.CompactIfNeeded(context.Background(), CompactOptions{
		HotTokenLimit: 10,
		Summarizer:    nil,
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if compacted {
		t.Error("should not compact without summarizer")
	}
}

func TestCompactPreservesWarmRecords(t *testing.T) {
	now := time.Now()
	s := &Session{
		ID: "test",
		Records: []Record{
			{Timestamp: now.Add(-20 * time.Minute), Role: "summary", Content: "old summary", Tier: TierWarm},
			{Timestamp: now.Add(-10 * time.Minute), Role: "user", Content: strings.Repeat("word ", 200), Tier: TierHot},
			{Timestamp: now.Add(-9 * time.Minute), Role: "assistant", Content: strings.Repeat("reply ", 200), Tier: TierHot},
			{Timestamp: now.Add(-1 * time.Minute), Role: "user", Content: "recent", Tier: TierHot},
		},
	}

	s.CompactIfNeeded(context.Background(), CompactOptions{
		HotTokenLimit: 50,
		Summarizer:    mockSummarizer,
	})

	// Old warm record should be preserved
	warmCount := 0
	for _, r := range s.Records {
		if r.Tier == TierWarm {
			warmCount++
		}
	}
	if warmCount < 2 {
		t.Errorf("warm count = %d, want >= 2 (old + new)", warmCount)
	}
}

func mockSummarizer(_ context.Context, text string) (string, error) {
	words := strings.Fields(text)
	if len(words) > 10 {
		return "Summary of " + strings.Join(words[:5], " ") + "...", nil
	}
	return "Summary: " + text, nil
}

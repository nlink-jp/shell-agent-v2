package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Summarizer generates summaries of conversation segments.
type Summarizer func(ctx context.Context, text string) (string, error)

// CompactOptions controls memory compaction behavior.
type CompactOptions struct {
	HotTokenLimit int
	Summarizer    Summarizer
}

// EstimateTokens provides a rough token count estimate.
// Uses a dual-method approach: max(word-based, char-based).
func EstimateTokens(text string) int {
	// Character-based: ~4 chars per token for English
	charBased := len(text) / 4

	// Word-based: count words with CJK awareness
	wordBased := 0
	for _, r := range text {
		if r >= 0x3000 && r <= 0x9FFF || r >= 0xF900 && r <= 0xFAFF {
			wordBased += 2 // CJK characters ~2 tokens
		}
	}
	words := len(strings.Fields(text))
	wordBased += int(float64(words) * 1.3)

	if charBased > wordBased {
		return charBased
	}
	return wordBased
}

// CompactIfNeeded compacts hot messages to warm when token budget is exceeded.
// Returns true if compaction occurred.
func (s *Session) CompactIfNeeded(ctx context.Context, opts CompactOptions) (bool, error) {
	if opts.Summarizer == nil {
		return false, nil
	}

	hotRecords := s.hotRecords()
	totalTokens := 0
	for _, r := range hotRecords {
		totalTokens += EstimateTokens(r.Content)
	}

	if totalTokens <= opts.HotTokenLimit {
		return false, nil
	}

	// Find the split point: keep the most recent messages that fit in budget.
	// Always keep at least the most recent message — even if it alone exceeds
	// the budget — so the LLM has a current turn to respond to. Otherwise
	// hot becomes empty and providers like Vertex reject empty contents.
	splitIdx := len(hotRecords) - 1
	keepTokens := EstimateTokens(hotRecords[splitIdx].Content)
	for i := len(hotRecords) - 2; i >= 0; i-- {
		tokens := EstimateTokens(hotRecords[i].Content)
		if keepTokens+tokens > opts.HotTokenLimit/2 {
			splitIdx = i + 1
			break
		}
		keepTokens += tokens
		splitIdx = i
	}

	if splitIdx <= 0 {
		return false, nil
	}

	// Summarize older messages
	toSummarize := hotRecords[:splitIdx]
	var sb strings.Builder
	for _, r := range toSummarize {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", r.Timestamp.Format("15:04"), r.Role, r.Content))
	}

	summary, err := opts.Summarizer(ctx, sb.String())
	if err != nil {
		return false, fmt.Errorf("summarize: %w", err)
	}

	// Create warm record
	warmRecord := Record{
		Timestamp: time.Now(),
		Role:      "summary",
		Content:   summary,
		Tier:      TierWarm,
		SummaryRange: &TimeRange{
			From: toSummarize[0].Timestamp,
			To:   toSummarize[len(toSummarize)-1].Timestamp,
		},
	}

	// Replace records: keep non-hot, add warm summary, keep recent hot
	var newRecords []Record
	for _, r := range s.Records {
		if r.Tier != TierHot {
			newRecords = append(newRecords, r)
		}
	}
	newRecords = append(newRecords, warmRecord)
	newRecords = append(newRecords, hotRecords[splitIdx:]...)

	s.Records = newRecords
	return true, nil
}

func (s *Session) hotRecords() []Record {
	var hot []Record
	for _, r := range s.Records {
		if r.Tier == TierHot {
			hot = append(hot, r)
		}
	}
	return hot
}

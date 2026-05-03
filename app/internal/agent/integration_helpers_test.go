//go:build lmstudio || vertexai
// +build lmstudio vertexai

package agent

import (
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func countSessionTokensH(s *memory.Session) int {
	total := 0
	for _, r := range s.Records {
		total += memory.EstimateTokens(r.Content)
	}
	return total
}

// v0.2.0: Tier was removed; warm summaries no longer live on
// records (contextbuild caches them outside the conversation).
// Helper kept as a no-op so existing call sites compile.
func countWarmH(_ *memory.Session) int {
	return 0
}

func wasToolCalledInLastTurnH(s *memory.Session) bool {
	lastUserIdx := -1
	for i := len(s.Records) - 1; i >= 0; i-- {
		if s.Records[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return false
	}
	for i := lastUserIdx + 1; i < len(s.Records); i++ {
		if s.Records[i].Role == "tool" {
			return true
		}
	}
	return false
}

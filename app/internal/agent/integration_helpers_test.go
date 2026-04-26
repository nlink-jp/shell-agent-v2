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

func countWarmH(s *memory.Session) int {
	count := 0
	for _, r := range s.Records {
		if r.Tier == memory.TierWarm {
			count++
		}
	}
	return count
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

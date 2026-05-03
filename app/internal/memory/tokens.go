package memory

import "strings"

// EstimateTokens returns a conservative token count for text,
// taking the max of character-based and word-based estimates.
//
// CJK characters count for ~2 tokens each (single ideograph
// often maps to multiple BPE tokens). The 1.3 multiplier on
// word count handles English BPE expansion of long words.
//
// Used by chat.BuildMessagesWithBudget for budget enforcement
// and by contextbuild for the same purpose. Originally lived in
// the deleted v1 compaction.go; pulled out into its own file
// during the v0.2.0 cleanup so the destructive-compaction code
// could be deleted without taking the estimator with it.
func EstimateTokens(text string) int {
	charBased := len(text) / 4

	wordBased := 0
	for _, r := range text {
		if r >= 0x3000 && r <= 0x9FFF || r >= 0xF900 && r <= 0xFAFF {
			wordBased += 2
		}
	}
	words := len(strings.Fields(text))
	wordBased += int(float64(words) * 1.3)

	if charBased > wordBased {
		return charBased
	}
	return wordBased
}

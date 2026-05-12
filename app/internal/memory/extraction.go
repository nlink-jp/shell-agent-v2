package memory

import (
	"regexp"
	"strings"
)

// ValidExtractionCategories is the master allowlist used by
// extractMemories — the LLM may emit any of these four; the
// agent then routes preference/decision to GlobalMemory and
// fact/context to SessionMemory. Anything outside this set is
// dropped at extraction time as a defense against attacker-
// invented categories like "system_rule".
//
// Originally lived in the deleted pinned.go as
// ValidPinnedCategories. v0.2.0 keeps the master 4-category
// shape because the extraction prompt still emits all four;
// the routing is what changed, not the recognition.
var ValidExtractionCategories = map[string]bool{
	"preference": true,
	"decision":   true,
	"fact":       true,
	"context":    true,
}

// selfReferentialPatterns lists tokens that mark a candidate
// fact as "self-referential" — describing the assistant or its
// internals rather than the user. Such facts, when re-injected
// into future sessions' system prompts, would directly steer
// the LLM and were the root cause of the THINK leakage incident
// (see docs/en/history/memory-injection-hardening.md).
//
// The list is intentionally over-broad: false positives (a
// user fact about "the model T" never being pinned) are
// cheaper than false negatives (a behaviour-overriding fact
// slipping through). Originally lived in pinned.go.
var selfReferentialPatterns = []string{
	"the assistant",
	"the model",
	"the llm",
	"the ai",
	"system prompt",
	"internal thought",
	"internal reasoning",
	"<think>",
	"</think>",
	"tool call",
	"tool output",
	"shell-agent",
}

var selfRefThinkRE = regexp.MustCompile(`(?i)\bthink\b`)

// IsSelfReferential reports whether the given fact looks like
// it describes the assistant itself rather than the user. Used
// by extractMemories to drop the THINK-incident class of fact
// before it reaches either store.
func IsSelfReferential(fact string) bool {
	low := strings.ToLower(fact)
	for _, p := range selfReferentialPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	if selfRefThinkRE.MatchString(low) {
		structural := []string{"tag", "marker", "mark", "internal", "output", "show", "display", "leak", "emit", "reveal", "format", "token"}
		for _, s := range structural {
			if strings.Contains(low, s) {
				return true
			}
		}
	}
	return false
}

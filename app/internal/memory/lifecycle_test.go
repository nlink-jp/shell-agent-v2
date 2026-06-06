package memory

import (
	"math"
	"testing"
)

func TestDecayedRelevance_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		r    float64
		rate float64
		want float64
	}{
		{"normal decay", 1.0, 0.93, 0.93},
		{"chained decay", 0.93, 0.93, 0.93 * 0.93},
		{"zero relevance stays zero", 0, 0.93, 0},
		{"negative relevance clamps to zero", -0.5, 0.93, 0},
		{"rate above 1 treated as 1 (no amplification)", 0.5, 1.5, 0.5},
		{"rate below 0 treated as 1 (no amplification)", 0.5, -0.1, 0.5},
		{"rate exactly 1 leaves relevance untouched", 0.7, 1.0, 0.7},
		{"rate exactly 0 treated as misconfig (no-op)", 0.7, 0.0, 0.7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecayedRelevance(tc.r, tc.rate)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("DecayedRelevance(%v, %v) = %v, want %v", tc.r, tc.rate, got, tc.want)
			}
		})
	}
}

func TestDecayedRelevance_RepeatedDecayConverges(t *testing.T) {
	// 12 turns at 0.93 should land ≈ 0.42 — just above the default
	// ActiveThreshold (0.4). 32 turns should land ≈ 0.1, the
	// archive boundary. These are the numbers quoted in ADR §2.1.
	r := 1.0
	for range 12 {
		r = DecayedRelevance(r, 0.93)
	}
	if r < 0.40 || r > 0.45 {
		t.Errorf("after 12 decays at 0.93, got %v, want ~0.42", r)
	}
	for range 20 {
		r = DecayedRelevance(r, 0.93)
	}
	if r < 0.09 || r > 0.12 {
		t.Errorf("after 32 decays at 0.93, got %v, want ~0.10", r)
	}
}

func TestDeriveState_FreshWindow(t *testing.T) {
	th := DefaultThresholds()
	// Created at turn 5, now turn 5..7 — within FreshTurns=3 → fresh
	for cur := 5; cur < 8; cur++ {
		if got := DeriveState(0.05, 5, cur, th); got != StateFresh {
			t.Errorf("created=5 current=%d expected fresh, got %s (relevance 0.05 should not matter)", cur, got)
		}
	}
	// turn 8 onward — fresh window closed, falls through to relevance bucketing
	if got := DeriveState(0.05, 5, 8, th); got != StateArchived {
		t.Errorf("expected archived at turn 8 with relevance 0.05, got %s", got)
	}
}

func TestDeriveState_RelevanceBuckets(t *testing.T) {
	th := DefaultThresholds()
	cases := []struct {
		r    float64
		want string
	}{
		{1.0, StateActive},
		{0.41, StateActive},
		{0.4, StateActive}, // ≥ ActiveThreshold
		{0.39, StateDormant},
		{0.11, StateDormant},
		{0.1, StateArchived}, // ≤ ArchiveThreshold
		{0.05, StateArchived},
		{0.0, StateArchived},
	}
	// Use createdTurn+FreshTurns gap so fresh window is closed.
	for _, tc := range cases {
		got := DeriveState(tc.r, 0, 100, th)
		if got != tc.want {
			t.Errorf("relevance %v: got %s, want %s", tc.r, got, tc.want)
		}
	}
}

func TestDeriveState_ZeroThresholdsResolveToDefaults(t *testing.T) {
	// A zero-valued LifecycleThresholds must not collapse every
	// entry to archived. resolved() fills defaults from zeros.
	if got := DeriveState(1.0, 0, 100, LifecycleThresholds{}); got != StateActive {
		t.Errorf("zero thresholds: relevance 1.0 should derive active, got %s", got)
	}
}

func TestTokenSet_Basic(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]struct{}
	}{
		{"hello world", set("hello", "world")},
		{"Hello, World!", set("hello", "world")},
		{"foo  bar\tbaz", set("foo", "bar", "baz")},
		{"", set()},
		{"日本語 テスト", set("日本語", "テスト")},
		{"mixed CJK と english 123", set("mixed", "cjk", "と", "english", "123")},
	}
	for _, tc := range cases {
		got := TokenSet(tc.in)
		if !sameSet(got, tc.want) {
			t.Errorf("TokenSet(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestJaccardScore(t *testing.T) {
	cases := []struct {
		a, b []string
		want float64
	}{
		{[]string{"a", "b"}, []string{"a", "b"}, 1.0},
		{[]string{"a", "b"}, []string{"c", "d"}, 0.0},
		{[]string{"a", "b"}, []string{"a", "c"}, 1.0 / 3.0},
		{[]string{}, []string{}, 0.0},
		{[]string{"a"}, []string{}, 0.0},
	}
	for _, tc := range cases {
		got := JaccardScore(toSet(tc.a), toSet(tc.b))
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("JaccardScore(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestConsolidationMatch(t *testing.T) {
	existing := []string{
		"User wants to analyse Q1 sales data",
		"Three datasets are loaded: sales, customers, returns",
		"Project deadline is March 31",
	}

	// Identical → match
	if i, ok := ConsolidationMatch(existing, "User wants to analyse Q1 sales data", 0.5); !ok || i != 0 {
		t.Errorf("identical fact: got (%d, %v), want (0, true)", i, ok)
	}

	// Paraphrase with high overlap → match. Same nouns + verb
	// stem, different connectives: this is the canonical case
	// the consolidation step is meant to catch (extractMemories
	// re-emitting the same fact phrased slightly differently).
	if i, ok := ConsolidationMatch(existing, "User wants to analyse Q1 sales numbers", 0.5); !ok || i != 0 {
		t.Errorf("paraphrase: got (%d, %v), want (0, true)", i, ok)
	}

	// Completely different topic → no match
	if i, ok := ConsolidationMatch(existing, "Tokyo is the capital of Japan", 0.5); ok || i != -1 {
		t.Errorf("unrelated fact: got (%d, %v), want (-1, false)", i, ok)
	}

	// Empty input → no match
	if i, ok := ConsolidationMatch(existing, "", 0.5); ok || i != -1 {
		t.Errorf("empty fact: got (%d, %v), want (-1, false)", i, ok)
	}

	// Empty existing list → no match
	if i, ok := ConsolidationMatch(nil, "any fact", 0.5); ok || i != -1 {
		t.Errorf("nil existing: got (%d, %v), want (-1, false)", i, ok)
	}
}

func TestLexicalTouchPredicate(t *testing.T) {
	// "Q1 sales data analysis" against "User wants to analyse
	// Q1 sales data" — 3 shared tokens (q1, sales, data), union
	// size 8, Jaccard 0.375 → crosses the 0.3 threshold.
	pred := LexicalTouchPredicate("Q1 sales data analysis", 0.3)

	if !pred("User wants to analyse Q1 sales data") {
		t.Error("expected touch on a fact with strong overlap")
	}

	if pred("Project deadline is March 31") {
		t.Error("expected no touch on unrelated fact")
	}

	// Empty turn → never touches
	emptyPred := LexicalTouchPredicate("", 0.3)
	if emptyPred("anything") {
		t.Error("empty turn must not touch anything")
	}

	// Zero threshold acts as disable
	zeroPred := LexicalTouchPredicate("identical sentence", 0)
	if zeroPred("identical sentence") {
		t.Error("zero threshold must disable touch entirely")
	}
}

// --- ADR-0032 Anchor / DeadTopic tests ---------------------------

func tokenizeAll(facts []string) []map[string]struct{} {
	out := make([]map[string]struct{}, len(facts))
	for i, f := range facts {
		out[i] = TokenSet(f)
	}
	return out
}

func TestAnchorRecord_MatchesAtThreshold(t *testing.T) {
	anchors := tokenizeAll([]string{
		"User prefers Go programming language",
		"Chose DuckDB over SQLite for analysis",
	})

	// "User prefers Go programming patterns" — 4 shared tokens
	// (user, prefers, go, programming) of union size 6 → Jaccard
	// 0.67, well above the 0.3 threshold.
	if !AnchorRecord("User prefers Go programming patterns", anchors, 0.3) {
		t.Error("expected anchor match on strong Go overlap")
	}
	// "Chose DuckDB SQLite analysis" — 4 shared tokens of union 6
	// → Jaccard 0.67 against the second anchor.
	if !AnchorRecord("Chose DuckDB SQLite analysis", anchors, 0.3) {
		t.Error("expected anchor match on DuckDB overlap")
	}
	// Unrelated content → no match.
	if AnchorRecord("The weather in Tokyo is rainy today", anchors, 0.3) {
		t.Error("unrelated content should not anchor")
	}
}

func TestAnchorRecord_EmptyInputs(t *testing.T) {
	anchors := tokenizeAll([]string{"User prefers Go"})

	if AnchorRecord("", anchors, 0.3) {
		t.Error("empty content must never anchor")
	}
	if AnchorRecord("anything", nil, 0.3) {
		t.Error("nil anchor list must never anchor")
	}
	if AnchorRecord("anything", []map[string]struct{}{}, 0.3) {
		t.Error("empty anchor list must never anchor")
	}
	// Zero threshold disables anchoring (matches LexicalTouchPredicate convention).
	if AnchorRecord("identical sentence", tokenizeAll([]string{"identical sentence"}), 0) {
		t.Error("zero threshold must disable anchoring")
	}
}

func TestDeadTopicRecord_DeadOnly(t *testing.T) {
	dead := tokenizeAll([]string{
		"Q1 sales dataset",
	})
	live := tokenizeAll([]string{
		"system architecture review",
	})

	// "Q1 sales dataset numbers" — 3 shared with dead of union 4
	// → Jaccard 0.75 against dead. No overlap with live (tokens
	// disjoint). Expect dead verdict.
	if !DeadTopicRecord("Q1 sales dataset numbers", dead, live, 0.3) {
		t.Error("expected dead verdict on Q1-overlap-only content")
	}
}

func TestDeadTopicRecord_LiveWins(t *testing.T) {
	dead := tokenizeAll([]string{"Q1 sales report"})
	live := tokenizeAll([]string{"Q1 sales dashboard"})

	// Content "Q1 sales review needed" — Jaccard 0.4 against both
	// dead and live fact (each shares q1, sales, of union 5). Dead
	// fires first, but the live safety net retracts the verdict.
	if DeadTopicRecord("Q1 sales review needed", dead, live, 0.3) {
		t.Error("live-clause safety net failed: content also matching a live topic must not be dropped")
	}
}

func TestDeadTopicRecord_NoMatch(t *testing.T) {
	dead := tokenizeAll([]string{"User loaded Q1 sales dataset"})
	live := tokenizeAll([]string{"User wants system review"})

	if DeadTopicRecord("Unrelated weather discussion in Tokyo", dead, live, 0.3) {
		t.Error("unrelated content must not be marked dead")
	}
}

func TestDeadTopicRecord_EmptyAndDisable(t *testing.T) {
	dead := tokenizeAll([]string{"some old topic content"})
	live := tokenizeAll([]string{"current live topic content"})

	if DeadTopicRecord("", dead, live, 0.3) {
		t.Error("empty content must not be marked dead")
	}
	if DeadTopicRecord("any content", nil, live, 0.3) {
		t.Error("nil dead list must not produce dead verdict")
	}
	if DeadTopicRecord("any content", dead, live, 0) {
		t.Error("zero threshold must disable dead-topic detection")
	}
}

// --- test helpers ------------------------------------------------

func set(items ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, k := range items {
		out[k] = struct{}{}
	}
	return out
}

func toSet(items []string) map[string]struct{} {
	return set(items...)
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

package memory

import (
	"strings"
	"unicode"
)

// Lifecycle states. Derived from Relevance and CreatedTurn at every
// touch / decay event, materialised on the entry so the UI badge and
// audit log have a string to render. See ADR-0031 §2.
const (
	StateFresh    = "fresh"
	StateActive   = "active"
	StateDormant  = "dormant"
	StateArchived = "archived"
)

// LifecycleThresholds bundles the tunable knobs used by the
// lifecycle pure functions. Mirrors config.LifecycleConfig in shape
// but is owned by the memory package to avoid a config→memory
// circular import.
type LifecycleThresholds struct {
	DecayRate                     float64
	FreshTurns                    int
	ActiveThreshold               float64
	ArchiveThreshold              float64
	TouchJaccardThreshold         float64
	ConsolidationJaccardThreshold float64
}

// DefaultThresholds returns the ADR-0031 §3.5 defaults. Used by
// tests and as a fallback when a zero LifecycleThresholds is passed
// (every threshold field zero is treated as "not configured").
func DefaultThresholds() LifecycleThresholds {
	return LifecycleThresholds{
		DecayRate:                     0.93,
		FreshTurns:                    3,
		ActiveThreshold:               0.4,
		ArchiveThreshold:              0.1,
		TouchJaccardThreshold:         0.3,
		ConsolidationJaccardThreshold: 0.5,
	}
}

// resolved returns t with zero fields replaced by the default. This
// lets callers pass a partially-configured LifecycleThresholds (e.g.
// when only DecayRate was set in config) without surprising
// behaviour from a zero ActiveThreshold collapsing everything to
// archived.
func (t LifecycleThresholds) resolved() LifecycleThresholds {
	d := DefaultThresholds()
	if t.DecayRate <= 0 {
		t.DecayRate = d.DecayRate
	}
	if t.FreshTurns <= 0 {
		t.FreshTurns = d.FreshTurns
	}
	if t.ActiveThreshold <= 0 {
		t.ActiveThreshold = d.ActiveThreshold
	}
	if t.ArchiveThreshold <= 0 {
		t.ArchiveThreshold = d.ArchiveThreshold
	}
	if t.TouchJaccardThreshold <= 0 {
		t.TouchJaccardThreshold = d.TouchJaccardThreshold
	}
	if t.ConsolidationJaccardThreshold <= 0 {
		t.ConsolidationJaccardThreshold = d.ConsolidationJaccardThreshold
	}
	return t
}

// DecayedRelevance returns r*rate clamped to [0, 1]. Negative inputs
// are clamped to 0; rates outside (0, 1] are treated as 1.0 (no-op).
// A rate of exactly 0 is misconfiguration — it would zero out the
// entire memory in a single tick — so we coerce it back to 1.0 and
// rely on config.Default() to populate a sensible value. Rates > 1
// would amplify relevance forever, also coerced to 1.0.
func DecayedRelevance(r, rate float64) float64 {
	if r <= 0 {
		return 0
	}
	if rate <= 0 || rate > 1 {
		rate = 1.0
	}
	out := r * rate
	if out < 0 {
		return 0
	}
	if out > 1 {
		return 1
	}
	return out
}

// DeriveState returns the lifecycle state for an entry with the
// given relevance, created at createdTurn, in a session that has
// reached currentTurn.
//
// Precedence:
//  1. If currentTurn - createdTurn < FreshTurns → fresh
//  2. r ≤ ArchiveThreshold → archived
//  3. r < ActiveThreshold → dormant
//  4. otherwise → active
//
// Note that the fresh window is independent of relevance: a fresh
// entry stays fresh even if a misconfigured DecayRate < 1 dragged
// its relevance below ActiveThreshold within the window. This
// matches ADR-0031 §2 where fresh is "created within FreshTurns,
// relevance 1.0".
func DeriveState(r float64, createdTurn, currentTurn int, t LifecycleThresholds) string {
	t = t.resolved()
	if currentTurn-createdTurn < t.FreshTurns {
		return StateFresh
	}
	if r <= t.ArchiveThreshold {
		return StateArchived
	}
	if r < t.ActiveThreshold {
		return StateDormant
	}
	return StateActive
}

// TokenSet splits s into a normalised token set for Jaccard scoring.
// Tokens are lowercased; separators are anything that is not a
// letter, digit, or non-spacing mark. Empty input returns an empty
// set (not nil) so the Jaccard branch on |A∪B| == 0 is well defined.
func TokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	if s == "" {
		return out
	}
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out[b.String()] = struct{}{}
			b.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// JaccardScore returns |A∩B| / |A∪B| in [0, 1]. Two empty sets
// return 0 (not 1) — "equally featureless" is not a useful signal.
func JaccardScore(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	// Iterate the smaller set for fewer hashmap lookups.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	for k := range small {
		if _, ok := large[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// ConsolidationMatch returns the index of the existing fact in
// existingFacts whose tokenised Jaccard against newFact is ≥
// threshold (highest-scoring among candidates). Returns (-1, false)
// if nothing crosses the threshold or if newFact is empty.
//
// Callers (memory stores' Add path) treat a positive match as
// "merge new into existing as a touch" rather than appending.
func ConsolidationMatch(existingFacts []string, newFact string, threshold float64) (int, bool) {
	if strings.TrimSpace(newFact) == "" || len(existingFacts) == 0 {
		return -1, false
	}
	newSet := TokenSet(newFact)
	bestIdx := -1
	bestScore := threshold // strict ≥; ties broken by first-seen
	for i, f := range existingFacts {
		score := JaccardScore(newSet, TokenSet(f))
		if score >= bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return -1, false
	}
	return bestIdx, true
}

// LexicalTouchPredicate returns a predicate that reports true when a
// candidate fact's tokenised Jaccard against turnContent is ≥
// threshold. Used by the agent loop to refresh memory entries that
// the just-arrived user turn lexically references.
//
// The closure captures turnContent's token set once so repeated
// invocations across the entry list are O(|fact|) per call rather
// than re-tokenising the turn N times.
func LexicalTouchPredicate(turnContent string, threshold float64) func(fact string) bool {
	turnSet := TokenSet(turnContent)
	if len(turnSet) == 0 || threshold <= 0 {
		// Empty turn or zero threshold: never touch. The latter
		// is the documented disable knob.
		return func(string) bool { return false }
	}
	return func(fact string) bool {
		return JaccardScore(turnSet, TokenSet(fact)) >= threshold
	}
}

// AnchorRecord reports whether content's token-set Jaccard against
// any precomputed anchor token set is ≥ threshold (ADR-0032).
// Used by contextbuild to lift records that reference a
// decision / preference Global Memory fact out of the summary
// input and render them verbatim.
//
// anchorTokenSets is precomputed once per Build call to avoid
// re-tokenising every Global Memory entry per record. Empty
// content / empty anchor list / threshold ≤ 0 → false (treated
// as "no anchor signal", matches the disable convention used by
// LexicalTouchPredicate).
func AnchorRecord(content string, anchorTokenSets []map[string]struct{}, threshold float64) bool {
	if content == "" || len(anchorTokenSets) == 0 || threshold <= 0 {
		return false
	}
	contentSet := TokenSet(content)
	if len(contentSet) == 0 {
		return false
	}
	for _, a := range anchorTokenSets {
		if len(a) == 0 {
			continue
		}
		if JaccardScore(contentSet, a) >= threshold {
			return true
		}
	}
	return false
}

// DeadTopicRecord reports whether content matches a dormant /
// archived Session Memory fact at Jaccard ≥ threshold AND does
// NOT also match any fresh / active Session Memory fact at the
// same threshold (ADR-0032).
//
// The live-clause is the safety net: a record that references
// both a dead and a live topic is kept (live wins). This avoids
// false positives where, for example, an old phrase resurfaces
// in a current discussion and would otherwise be dropped.
//
// Both token-set slices are precomputed once per Build call.
// Empty content / empty dead list / threshold ≤ 0 → false.
func DeadTopicRecord(content string, deadTokenSets, liveTokenSets []map[string]struct{}, threshold float64) bool {
	if content == "" || len(deadTokenSets) == 0 || threshold <= 0 {
		return false
	}
	contentSet := TokenSet(content)
	if len(contentSet) == 0 {
		return false
	}
	deadHit := false
	for _, d := range deadTokenSets {
		if len(d) == 0 {
			continue
		}
		if JaccardScore(contentSet, d) >= threshold {
			deadHit = true
			break
		}
	}
	if !deadHit {
		return false
	}
	// Live-clause safety net: if the content also matches a live
	// topic, retract the dead verdict.
	for _, l := range liveTokenSets {
		if len(l) == 0 {
			continue
		}
		if JaccardScore(contentSet, l) >= threshold {
			return false
		}
	}
	return true
}

package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// ValidPinnedCategories is the allowlist enforced by
// extractPinnedMemories — only facts with one of these categories
// are pinned. Any other category (including LLM-invented ones like
// "system_rule") is dropped at extraction time.
// See docs/en/memory-injection-hardening.md §5 Phase B-3.
var ValidPinnedCategories = map[string]bool{
	"preference": true,
	"decision":   true,
	"fact":       true,
	"context":    true,
}

// selfReferentialPatterns lists tokens that mark a candidate fact as
// "self-referential" — i.e. about the assistant or its internals
// rather than about the user. Such facts, when re-injected into
// future sessions' system prompts, directly steer the LLM's own
// behaviour and were the root cause of the THINK leakage incident.
//
// The list is intentionally over-broad: false positives (a user
// fact about "the model T" never being pinned) are cheaper than
// false negatives (a behaviour-overriding fact slipping through).
//
// Substring tokens match anywhere in the lowercased fact. The
// "think" entry is matched as a whole word via selfRefThinkRE so
// "I think Python is fine" does not get caught.
// See docs/en/memory-injection-hardening.md §5 Phase B-2.
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

// IsSelfReferential reports whether the given fact looks like it
// describes the assistant itself rather than the user. Used by
// extractPinnedMemories to drop the THINK-incident class of fact
// before it reaches the pinned store.
func IsSelfReferential(fact string) bool {
	low := strings.ToLower(fact)
	for _, p := range selfReferentialPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	// Whole-word "think" — catch THINK-as-internal-marker phrasings
	// without flagging "I think X" or "I don't think so".
	if selfRefThinkRE.MatchString(low) {
		// But require some additional context that suggests
		// internal-marker semantics; otherwise a benign user fact
		// like "user thinks Python is fine" would be dropped. We
		// look for THINK appearing alongside any of the structural
		// keywords that indicate it's being described as a marker.
		structural := []string{"tag", "marker", "mark", "internal", "output", "show", "display", "leak", "emit", "reveal", "format", "token"}
		for _, s := range structural {
			if strings.Contains(low, s) {
				return true
			}
		}
	}
	return false
}

// Source values for PinnedFact.Source. See
// docs/en/memory-injection-hardening.md §5 Phase A.
const (
	PinnedSourceUserTurn      = "user_turn"      // extracted from a [user] role record
	PinnedSourceAssistantTurn = "assistant_turn" // extracted from an [assistant] role record (lower trust)
	PinnedSourceManual        = "manual"         // pinned via Settings UI / Set()
)

// PinnedFact is a persistent cross-session fact with bilingual support.
//
// Source/SessionID/SourceTurnIndex/ToolOriginated were added in
// v0.1.26 for memory-injection provenance. See
// docs/en/memory-injection-hardening.md §5 Phase A. Existing
// entries from earlier versions have empty Source — those are
// rendered with the lower-trust [derived] tag (see FormatForPrompt).
type PinnedFact struct {
	Fact       string    `json:"fact"`
	NativeFact string    `json:"native_fact"`
	Category   string    `json:"category"` // preference, decision, fact, context
	SourceTime time.Time `json:"source_time"`
	CreatedAt  time.Time `json:"created_at"`

	// Provenance (v0.1.26+).
	SessionID       string `json:"session_id,omitempty"`
	SourceTurnIndex int    `json:"source_turn_index,omitempty"`
	Source          string `json:"source,omitempty"`          // PinnedSource* constants
	ToolOriginated  bool   `json:"tool_originated,omitempty"` // surrounding window included a tool record

	// Legacy fields for backward compat
	Key     string `json:"key,omitempty"`
	Content string `json:"content,omitempty"`
}

// DefaultMaxPinnedFacts is the soft cap on PinnedStore.Entries when
// the caller has not supplied an explicit MaxFacts override. The
// oldest entry is evicted on overflow. See
// docs/en/memory-injection-hardening.md §5 Phase C.
const DefaultMaxPinnedFacts = 100

// PinnedStore manages cross-session persistent facts.
type PinnedStore struct {
	path    string
	Entries []PinnedFact

	// MaxFacts is the soft cap on Entries. Zero falls back to
	// DefaultMaxPinnedFacts. The eviction strategy is FIFO — oldest
	// entry first.
	MaxFacts int
}

// NewPinnedStore creates a store backed by the default pinned file.
func NewPinnedStore() *PinnedStore {
	return &PinnedStore{
		path:     filepath.Join(config.DataDir(), "pinned.json"),
		MaxFacts: DefaultMaxPinnedFacts,
	}
}

// Load reads pinned facts from disk.
func (s *PinnedStore) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.Entries = []PinnedFact{}
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.Entries)
}

// Save writes pinned facts to disk atomically (tmp+rename) so a
// crash mid-write leaves the previous file intact rather than a
// torn / empty pinned.json (security-hardening-2.md C4 / H10).
func (s *PinnedStore) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.Entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(s.path, data, 0600)
}

// Add appends a new fact, deduplicating by content. When the store
// exceeds its MaxFacts soft cap the oldest entry is evicted (FIFO).
func (s *PinnedStore) Add(fact PinnedFact) bool {
	for _, existing := range s.Entries {
		if existing.Fact == fact.Fact {
			return false
		}
	}
	if fact.CreatedAt.IsZero() {
		fact.CreatedAt = time.Now()
	}
	if fact.SourceTime.IsZero() {
		fact.SourceTime = time.Now()
	}
	s.Entries = append(s.Entries, fact)

	cap := s.MaxFacts
	if cap <= 0 {
		cap = DefaultMaxPinnedFacts
	}
	if len(s.Entries) > cap {
		// Drop the oldest entry to keep the store bounded. We do not
		// log here (callers like extractPinnedMemories may invoke
		// Add many times in a row) — the audit UI exposes the
		// current store contents directly.
		s.Entries = s.Entries[1:]
	}
	return true
}

// Set creates or updates a pinned fact by key. Manual writes are
// stamped with Source=PinnedSourceManual so the audit UI can
// distinguish them from auto-extracted entries.
func (s *PinnedStore) Set(key, content string) {
	now := time.Now()
	for i, f := range s.Entries {
		if f.Key == key || f.Fact == key {
			s.Entries[i].Content = content
			s.Entries[i].NativeFact = content
			return
		}
	}
	s.Entries = append(s.Entries, PinnedFact{
		Fact: key, NativeFact: content, Content: content, Key: key,
		Category: "fact", CreatedAt: now, SourceTime: now,
		Source: PinnedSourceManual,
	})
}

// Delete removes a pinned fact by key or fact text.
func (s *PinnedStore) Delete(key string) bool {
	for i, f := range s.Entries {
		if f.Key == key || f.Fact == key {
			s.Entries = append(s.Entries[:i], s.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// DeleteByKeys removes facts whose Key or Fact matches any value in the
// given list. Returns the count actually deleted.
func (s *PinnedStore) DeleteByKeys(keys []string) int {
	if len(keys) == 0 {
		return 0
	}
	wanted := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		wanted[k] = struct{}{}
	}
	var kept []PinnedFact
	deleted := 0
	for _, f := range s.Entries {
		_, hitKey := wanted[f.Key]
		_, hitFact := wanted[f.Fact]
		if hitKey || hitFact {
			deleted++
			continue
		}
		kept = append(kept, f)
	}
	s.Entries = kept
	return deleted
}

// Get retrieves a pinned fact by key.
func (s *PinnedStore) Get(key string) (PinnedFact, bool) {
	for _, f := range s.Entries {
		if f.Key == key || f.Fact == key {
			return f, true
		}
	}
	return PinnedFact{}, false
}

// All returns all pinned facts.
func (s *PinnedStore) All() []PinnedFact {
	return s.Entries
}

// PinnedFormatBudget caps the total size of FormatForPrompt output.
// When the rendered facts exceed this budget the OLDEST entries are
// elided (most-recent are most likely to be relevant) and a marker
// is appended. See docs/en/memory-injection-hardening.md §5 Phase C.
const PinnedFormatBudget = 16 * 1024 // 16 KiB

// FormatForPrompt returns pinned facts formatted for system prompt injection.
// Facts are sanitized to prevent prompt injection — LLM-extracted facts
// may contain hostile content (newlines, control chars, instruction-like text).
//
// Each line carries a trust tag derived from the entry's Source:
//
//   - [user-stated]: came from a [user] role record or a manual pin
//     (Set via Settings UI). The model can treat these as authoritative.
//   - [derived]: came from an [assistant] role record, which means the
//     content traces back through the LLM's own output and is therefore
//     potentially attacker-influenced (a CSV cell, MCP response, web
//     page, image OCR — see docs/en/memory-injection-hardening.md).
//
// Legacy entries with no Source field default to [derived] — the safer
// choice when provenance is unknown.
//
// Output is bounded by PinnedFormatBudget (16 KiB). When the budget is
// exceeded the oldest entries are elided and a "(N earlier facts
// elided)" marker prefixes the output.
func (s *PinnedStore) FormatForPrompt() string {
	if len(s.Entries) == 0 {
		return ""
	}
	// Render each entry independently first, then assemble the
	// newest entries into the output until the budget is exhausted.
	lines := make([]string, len(s.Entries))
	for i, e := range s.Entries {
		fact := sanitizePinned(e.Fact, 300)
		native := sanitizePinned(e.NativeFact, 300)
		category := sanitizePinned(e.Category, 30)
		var lb strings.Builder
		lb.WriteString(fmt.Sprintf("- %s [%s] %s", trustTag(e.Source), category, fact))
		if native != "" && native != fact {
			lb.WriteString(fmt.Sprintf(" (%s)", native))
		}
		if !e.CreatedAt.IsZero() {
			lb.WriteString(fmt.Sprintf(" (learned %s)", e.CreatedAt.Format("2006-01-02")))
		}
		lb.WriteString("\n")
		lines[i] = lb.String()
	}

	// Walk newest → oldest, including lines until the budget runs
	// out. The oldest skipped count becomes the elision marker.
	included := make([]string, 0, len(lines))
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if used+len(lines[i]) > PinnedFormatBudget {
			break
		}
		included = append([]string{lines[i]}, included...)
		used += len(lines[i])
	}
	elided := len(lines) - len(included)

	var sb strings.Builder
	if elided > 0 {
		sb.WriteString(fmt.Sprintf("(%d earlier facts elided to fit budget)\n", elided))
	}
	for _, l := range included {
		sb.WriteString(l)
	}
	return sb.String()
}

// trustTag maps a PinnedFact.Source to the leading bracketed token in
// FormatForPrompt. Anything unknown (including legacy empty Source)
// gets [derived] — the lower-trust default.
func trustTag(source string) string {
	switch source {
	case PinnedSourceUserTurn, PinnedSourceManual:
		return "[user-stated]"
	default:
		return "[derived]"
	}
}

// sanitizePinned removes control chars and newlines, caps length.
func sanitizePinned(s string, maxLen int) string {
	var b []rune
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			b = append(b, ' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b = append(b, r)
		if len(b) >= maxLen {
			break
		}
	}
	return strings.TrimSpace(string(b))
}

// FormatExistingForExtraction returns facts list for the extraction prompt.
func (s *PinnedStore) FormatExistingForExtraction() string {
	if len(s.Entries) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, e := range s.Entries {
		sb.WriteString(fmt.Sprintf("- %s\n", e.Fact))
	}
	return sb.String()
}

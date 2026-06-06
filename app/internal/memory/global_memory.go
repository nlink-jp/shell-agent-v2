package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
)

// ValidGlobalMemoryCategories is the allowlist for Global Memory.
// Only `preference` (long-term user habits/preferences) and
// `decision` (architectural / persistent design choices) belong
// in the cross-session global pool. `fact` and `context` are
// session-scoped and route to SessionMemory instead.
// See docs/en/reference/memory-model.md §5.
var ValidGlobalMemoryCategories = map[string]bool{
	"preference": true,
	"decision":   true,
}

// Source values for GlobalMemoryEntry.Source.
//
// `manual` covers UI / API direct edits. `promoted_from_*`
// values record that the entry originated as a Session Memory
// or Findings entry that the user explicitly promoted via the
// Pin to Global Memory action — they get the high-trust
// `[user-stated]` tag because the user made an explicit choice.
const (
	GlobalSourceUserTurn               = "user_turn"
	GlobalSourceAssistantTurn          = "assistant_turn"
	GlobalSourceManual                 = "manual"
	GlobalSourcePromotedFromSession    = "promoted_from_session_memory"
	GlobalSourcePromotedFromFinding    = "promoted_from_finding"
	// GlobalSourceToolCall: written via the remember-fact builtin
	// tool (ADR-0019). Trust-tagged as [derived] since the
	// assistant chose to save it; the actual user statement, if
	// any, lives one turn earlier in the conversation.
	GlobalSourceToolCall = "tool_call"
)

// GlobalMemoryEntry is a cross-session user-identity fact.
// Only `preference` and `decision` categories are valid here;
// `fact`/`context` route to SessionMemory.
type GlobalMemoryEntry struct {
	Fact       string    `json:"fact"`
	NativeFact string    `json:"native_fact"`
	Category   string    `json:"category"` // preference | decision
	SourceTime time.Time `json:"source_time"`
	CreatedAt  time.Time `json:"created_at"`

	// Provenance. Source records how the entry arose (a portable
	// enum, see GlobalSource* constants) and drives the trust tag.
	// ToolOriginated marks tool-call origin. Machine-local session
	// back-references were removed in ADR-0028 (never read; unsafe
	// across machines).
	Source         string `json:"source,omitempty"`
	ToolOriginated bool   `json:"tool_originated,omitempty"`

	// Lifecycle fields (ADR-0031). Relevance decays per turn and
	// resets on Touch. State is derived from Relevance + CreatedTurn
	// at every mutation; persisted for UI consumers that don't know
	// the thresholds. Legacy entries (Relevance == 0 on load) get
	// filled with Relevance=1.0, State="active" — see legacyFill.
	Relevance       float64   `json:"relevance,omitempty"`
	CreatedTurn     int       `json:"created_turn,omitempty"`
	LastTouchedAt   time.Time `json:"last_touched_at,omitzero"`
	LastTouchedTurn int       `json:"last_touched_turn,omitempty"`
	TouchCount      int       `json:"touch_count,omitempty"`
	State           string    `json:"state,omitempty"`
}

// DefaultMaxGlobalMemory is the soft cap on GlobalMemoryStore
// entries. FIFO eviction on overflow.
const DefaultMaxGlobalMemory = 100

// GlobalMemoryFormatBudget caps the rendered output size when
// injected into the system prompt.
const GlobalMemoryFormatBudget = 16 * 1024 // 16 KiB

// GlobalMemoryStore manages cross-session global memory entries.
//
// Concurrency: Add / Delete / All / Load / Save / FormatForPrompt /
// Touch / DecayAll all take s.mu so the agent-loop touch path and
// the background extractMemories goroutine never race.
type GlobalMemoryStore struct {
	mu      sync.Mutex
	path    string
	Entries []GlobalMemoryEntry

	// MaxEntries is the soft cap. Zero falls back to
	// DefaultMaxGlobalMemory.
	MaxEntries int

	// Thresholds tunes the lifecycle (ADR-0031). Zero value is
	// resolved to DefaultThresholds() at use sites.
	Thresholds LifecycleThresholds
}

// NewGlobalMemoryStore creates a store backed by
// `{DataDir}/global_memory.json`.
func NewGlobalMemoryStore() *GlobalMemoryStore {
	return &GlobalMemoryStore{
		path:       filepath.Join(config.DataDir(), "global_memory.json"),
		MaxEntries: DefaultMaxGlobalMemory,
	}
}

// Load reads entries from disk. A missing file is treated as
// an empty store (no error). v0.1.x `pinned.json` is NOT read
// — v0.2.0 starts global memory empty (see memory-model.md §11).
//
// Legacy entries (written before ADR-0031) have no lifecycle
// fields; legacyFill populates them as active with Relevance=1.0.
func (s *GlobalMemoryStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.Entries = []GlobalMemoryEntry{}
			return nil
		}
		return err
	}
	if err := json.Unmarshal(data, &s.Entries); err != nil {
		return err
	}
	for i := range s.Entries {
		s.legacyFill(&s.Entries[i])
	}
	return nil
}

// legacyFill populates the lifecycle fields on an entry loaded
// from disk that predates ADR-0031. Detection: Relevance == 0
// (zero value). For such entries we synthesise a sane default
// so the first DecayAll has well-formed input. Already-populated
// entries are left untouched.
//
// Transitional note: legacy entries enter with CreatedTurn=0 and
// will appear "fresh" for the first FreshTurns user turns of any
// new session after upgrade. This is a one-time effect — once
// fresh expires, normal decay applies.
func (s *GlobalMemoryStore) legacyFill(e *GlobalMemoryEntry) {
	if e.Relevance > 0 {
		return
	}
	e.Relevance = 1.0
	if e.LastTouchedAt.IsZero() {
		e.LastTouchedAt = e.CreatedAt
	}
	if e.State == "" {
		e.State = StateActive
	}
}

// Save writes entries to disk atomically (tmp + rename) so a
// crash mid-write leaves the previous file intact.
func (s *GlobalMemoryStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.Entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(s.path, data, 0600)
}

// Add appends a new entry. If an existing entry's Fact is a
// near-duplicate (Jaccard ≥ ConsolidationJaccardThreshold) the
// new entry is treated as a touch on the existing one and not
// appended — TouchCount increments, Relevance resets to 1.0.
// Returns true if a new row was appended, false on consolidation.
//
// Eviction past MaxEntries selects the lowest-relevance entry
// first (archived → dormant → active → fresh, ties broken by
// oldest LastTouchedAt).
//
// Callers set e.CreatedTurn from the agent loop's currentTurn so
// the fresh-window calculation has meaningful input. Test paths
// that omit CreatedTurn get CreatedTurn=0, treated as "always
// past fresh" for any currentTurn ≥ FreshTurns.
func (s *GlobalMemoryStore) Add(e GlobalMemoryEntry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	th := s.Thresholds.resolved()
	if idx, ok := ConsolidationMatch(s.factsLocked(), e.Fact, th.ConsolidationJaccardThreshold); ok {
		existing := &s.Entries[idx]
		score := JaccardScore(TokenSet(existing.Fact), TokenSet(e.Fact))
		existing.TouchCount++
		existing.Relevance = 1.0
		existing.LastTouchedAt = time.Now()
		if e.CreatedTurn > existing.LastTouchedTurn {
			existing.LastTouchedTurn = e.CreatedTurn
		}
		existing.State = DeriveState(existing.Relevance, existing.CreatedTurn, e.CreatedTurn, th)
		logger.Info("memory: global-memory consolidated new entry into existing %q (jaccard %.2f, source: %s)",
			existing.Fact, score, e.Source)
		return false
	}

	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	if e.SourceTime.IsZero() {
		e.SourceTime = time.Now()
	}
	if e.Relevance == 0 {
		e.Relevance = 1.0
	}
	if e.LastTouchedAt.IsZero() {
		e.LastTouchedAt = e.CreatedAt
	}
	if e.LastTouchedTurn == 0 {
		e.LastTouchedTurn = e.CreatedTurn
	}
	e.State = DeriveState(e.Relevance, e.CreatedTurn, e.CreatedTurn, th)
	s.Entries = append(s.Entries, e)

	cap := s.MaxEntries
	if cap <= 0 {
		cap = DefaultMaxGlobalMemory
	}
	if len(s.Entries) > cap {
		s.evictLowestRelevanceLocked()
	}
	return true
}

// factsLocked returns the slice of Fact strings for consolidation
// scanning. Caller must hold s.mu.
func (s *GlobalMemoryStore) factsLocked() []string {
	out := make([]string, len(s.Entries))
	for i, e := range s.Entries {
		out[i] = e.Fact
	}
	return out
}

// evictLowestRelevanceLocked removes one entry: the one with the
// lowest Relevance, with archived/dormant preferred over active
// even if they were touched more recently. Ties on relevance are
// broken by oldest LastTouchedAt. Caller must hold s.mu.
func (s *GlobalMemoryStore) evictLowestRelevanceLocked() {
	if len(s.Entries) == 0 {
		return
	}
	statePriority := func(state string) int {
		switch state {
		case StateArchived:
			return 0
		case StateDormant:
			return 1
		case StateActive:
			return 2
		case StateFresh:
			return 3
		default:
			return 2
		}
	}
	worst := 0
	for i := 1; i < len(s.Entries); i++ {
		a, b := s.Entries[worst], s.Entries[i]
		pa, pb := statePriority(a.State), statePriority(b.State)
		switch {
		case pb < pa:
			worst = i
		case pb == pa && b.Relevance < a.Relevance:
			worst = i
		case pb == pa && b.Relevance == a.Relevance && b.LastTouchedAt.Before(a.LastTouchedAt):
			worst = i
		}
	}
	victim := s.Entries[worst]
	logger.Info("memory: global-memory evicted %q (relevance %.2f, state %s, last touched turn %d)",
		victim.Fact, victim.Relevance, victim.State, victim.LastTouchedTurn)
	s.Entries = append(s.Entries[:worst], s.Entries[worst+1:]...)
}

// Touch refreshes every entry whose Fact matches the predicate:
// Relevance ← 1.0, LastTouchedAt ← now, LastTouchedTurn ←
// currentTurn, TouchCount++, State recomputed. Returns the number
// of entries touched. source is logged for audit.
func (s *GlobalMemoryStore) Touch(matchFn func(fact string) bool, currentTurn int, source string) int {
	if matchFn == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	th := s.Thresholds
	touched := 0
	for i := range s.Entries {
		e := &s.Entries[i]
		if !matchFn(e.Fact) {
			continue
		}
		prev := e.Relevance
		prevState := e.State
		e.Relevance = 1.0
		e.LastTouchedAt = time.Now()
		e.LastTouchedTurn = currentTurn
		e.TouchCount++
		e.State = DeriveState(e.Relevance, e.CreatedTurn, currentTurn, th)
		logger.Info("memory: global-memory touched %q (relevance %.2f → 1.00, %s→%s, source: %s)",
			e.Fact, prev, prevState, e.State, source)
		touched++
	}
	return touched
}

// DecayAll multiplies every non-fresh entry's Relevance by
// DecayRate and recomputes State. State transitions are logged
// at Info; routine no-flip decays are aggregated at Debug. Returns
// the number of entries whose State changed (caller uses this to
// gate the memory:updated event emission).
func (s *GlobalMemoryStore) DecayAll(currentTurn int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	th := s.Thresholds.resolved()
	flips := 0
	decayed := 0
	for i := range s.Entries {
		e := &s.Entries[i]
		prevState := DeriveState(e.Relevance, e.CreatedTurn, currentTurn, th)
		// Fresh entries don't decay — their window is defined by
		// turn count alone, not relevance.
		if prevState != StateFresh {
			e.Relevance = DecayedRelevance(e.Relevance, th.DecayRate)
			decayed++
		}
		nextState := DeriveState(e.Relevance, e.CreatedTurn, currentTurn, th)
		e.State = nextState
		if prevState != nextState {
			logger.Info("memory: global-memory entry %q %s→%s (relevance %.2f, turn %d)",
				e.Fact, prevState, nextState, e.Relevance, currentTurn)
			flips++
		}
	}
	if decayed > 0 && flips == 0 {
		logger.Debug("memory: global-memory decayed %d entries (turn %d, no state flips)", decayed, currentTurn)
	}
	return flips
}

// Set creates or updates an entry by Fact text. Used by the
// settings UI direct-edit path. Stamped Source = manual.
//
// Manual entries enter with Relevance=1.0 and State=active —
// no fresh window (the user is explicitly authoring, not
// extracted-then-reinforced).
func (s *GlobalMemoryStore) Set(fact, native, category string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidGlobalMemoryCategories[category] {
		category = "decision"
	}
	now := time.Now()
	for i, e := range s.Entries {
		if e.Fact == fact {
			s.Entries[i].NativeFact = native
			s.Entries[i].Category = category
			return
		}
	}
	s.Entries = append(s.Entries, GlobalMemoryEntry{
		Fact: fact, NativeFact: native, Category: category,
		CreatedAt: now, SourceTime: now,
		Source:        GlobalSourceManual,
		Relevance:     1.0,
		LastTouchedAt: now,
		State:         StateActive,
	})
}

// Delete removes an entry by Fact text.
func (s *GlobalMemoryStore) Delete(fact string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.Entries {
		if e.Fact == fact {
			s.Entries = append(s.Entries[:i], s.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// DeleteByFacts removes entries whose Fact is in the given list.
// Returns the count actually deleted.
func (s *GlobalMemoryStore) DeleteByFacts(facts []string) int {
	if len(facts) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := make(map[string]struct{}, len(facts))
	for _, f := range facts {
		wanted[f] = struct{}{}
	}
	var kept []GlobalMemoryEntry
	deleted := 0
	for _, e := range s.Entries {
		if _, hit := wanted[e.Fact]; hit {
			deleted++
			continue
		}
		kept = append(kept, e)
	}
	s.Entries = kept
	return deleted
}

// All returns a copy of all entries. Copying avoids exposing the
// internal slice to callers that might mutate it concurrently
// with a Touch / DecayAll on the agent loop.
func (s *GlobalMemoryStore) All() []GlobalMemoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]GlobalMemoryEntry, len(s.Entries))
	copy(out, s.Entries)
	return out
}

// --- Export / Import (ADR-0027) ---

// GlobalMemoryExportKind / GlobalMemoryExportSchemaVersion identify a
// Global Memory export file. The kind discriminator lets import reject a
// session bundle or an arbitrary JSON file rather than mis-parsing it.
const (
	GlobalMemoryExportKind          = "shell-agent-v2-global-memory"
	GlobalMemoryExportSchemaVersion = 1
)

// GlobalMemoryExport is the on-disk envelope wrapping exported entries.
// Entries are stored verbatim; after ADR-0028 every field is portable
// (no machine-local session back-references).
type GlobalMemoryExport struct {
	Kind                 string              `json:"kind"`
	SchemaVersion        int                 `json:"schema_version"`
	ExportedAt           time.Time           `json:"exported_at"`
	ExportedByAppVersion string              `json:"exported_by_app_version,omitempty"`
	Entries              []GlobalMemoryEntry `json:"entries"`
}

// MarshalGlobalMemoryExport builds an indented export envelope around the
// given entries, stamped with the current UTC time and the app version.
func MarshalGlobalMemoryExport(entries []GlobalMemoryEntry, appVersion string) ([]byte, error) {
	if entries == nil {
		entries = []GlobalMemoryEntry{}
	}
	return json.MarshalIndent(GlobalMemoryExport{
		Kind:                 GlobalMemoryExportKind,
		SchemaVersion:        GlobalMemoryExportSchemaVersion,
		ExportedAt:           time.Now().UTC(),
		ExportedByAppVersion: appVersion,
		Entries:              entries,
	}, "", "  ")
}

// ParseGlobalMemoryImport validates an export envelope and returns its
// entries. It rejects non-JSON, a wrong kind, and an unsupported schema
// version with distinct errors so the caller can surface a precise reason.
func ParseGlobalMemoryImport(data []byte) ([]GlobalMemoryEntry, error) {
	var env GlobalMemoryExport
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("not a valid global-memory export (invalid JSON): %w", err)
	}
	if env.Kind != GlobalMemoryExportKind {
		return nil, fmt.Errorf("not a global-memory export: kind=%q, want %q", env.Kind, GlobalMemoryExportKind)
	}
	if env.SchemaVersion != GlobalMemoryExportSchemaVersion {
		return nil, fmt.Errorf("unsupported global-memory export schema version: %d", env.SchemaVersion)
	}
	return env.Entries, nil
}

// Import merges entries into the store using normal Add semantics: new
// facts are appended, facts whose text already exists are skipped (merge,
// skip-duplicates — ADR-0027). Invalid categories are coerced to
// `decision` (mirrors Set); entries with an empty Fact are skipped.
// Returns how many were added and how many were skipped. The caller is
// responsible for persisting via Save.
func (s *GlobalMemoryStore) Import(entries []GlobalMemoryEntry) (added, skipped int) {
	for _, e := range entries {
		if strings.TrimSpace(e.Fact) == "" {
			skipped++
			continue
		}
		if !ValidGlobalMemoryCategories[e.Category] {
			e.Category = "decision"
		}
		if s.Add(e) {
			added++
		} else {
			skipped++
		}
	}
	return added, skipped
}

// FormatForPrompt returns the entries formatted for system
// prompt injection. Sanitised for control chars; bounded by
// GlobalMemoryFormatBudget with newest-first inclusion and
// elision marker.
//
// Lifecycle filtering (ADR-0031): entries in StateDormant or
// StateArchived are skipped — they remain on disk for UI display
// but do not flow into the LLM's system prompt.
//
// Trust tag derivation:
//   - [user-stated]: user_turn / manual / promoted_from_*
//   - [derived]:     assistant_turn / empty (legacy)
func (s *GlobalMemoryStore) FormatForPrompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Entries) == 0 {
		return ""
	}
	type renderable struct {
		line string
	}
	rendered := make([]renderable, 0, len(s.Entries))
	for _, e := range s.Entries {
		if e.State == StateDormant || e.State == StateArchived {
			continue
		}
		fact := sanitizeMemoryText(e.Fact, 300)
		native := sanitizeMemoryText(e.NativeFact, 300)
		category := sanitizeMemoryText(e.Category, 30)
		var lb strings.Builder
		lb.WriteString(fmt.Sprintf("- %s [%s] %s", globalTrustTag(e.Source), category, fact))
		if native != "" && native != fact {
			lb.WriteString(fmt.Sprintf(" (%s)", native))
		}
		if !e.CreatedAt.IsZero() {
			lb.WriteString(fmt.Sprintf(" (learned %s)", e.CreatedAt.Format("2006-01-02")))
		}
		lb.WriteString("\n")
		rendered = append(rendered, renderable{line: lb.String()})
	}

	if len(rendered) == 0 {
		return ""
	}
	included := make([]string, 0, len(rendered))
	used := 0
	for i := len(rendered) - 1; i >= 0; i-- {
		if used+len(rendered[i].line) > GlobalMemoryFormatBudget {
			break
		}
		included = append([]string{rendered[i].line}, included...)
		used += len(rendered[i].line)
	}
	elided := len(rendered) - len(included)

	var sb strings.Builder
	if elided > 0 {
		sb.WriteString(fmt.Sprintf("(%d earlier facts elided to fit budget)\n", elided))
	}
	for _, l := range included {
		sb.WriteString(l)
	}
	return sb.String()
}

// FormatExistingForExtraction returns the entry list as plain
// "- fact\n" lines for the extraction prompt's "already known"
// section.
func (s *GlobalMemoryStore) FormatExistingForExtraction() string {
	if len(s.Entries) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, e := range s.Entries {
		sb.WriteString(fmt.Sprintf("- %s\n", e.Fact))
	}
	return sb.String()
}

// globalTrustTag derives [user-stated] / [derived] from the
// Source enum. Anything outside the user-stated set falls back
// to [derived] so legacy / unknown sources are treated cautiously.
func globalTrustTag(source string) string {
	switch source {
	case GlobalSourceUserTurn, GlobalSourceManual,
		GlobalSourcePromotedFromSession, GlobalSourcePromotedFromFinding:
		return "[user-stated]"
	default:
		return "[derived]"
	}
}

// sanitizeMemoryText is the shared sanitiser used by both
// GlobalMemory and SessionMemory FormatForPrompt paths. Strips
// control chars, collapses newlines/tabs to spaces, caps length.
func sanitizeMemoryText(s string, maxLen int) string {
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

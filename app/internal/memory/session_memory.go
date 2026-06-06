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
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
)

// ValidSessionMemoryCategories is the allowlist for Session
// Memory. Only `fact` (factual context for the current task)
// and `context` (situational awareness) belong here;
// `preference`/`decision` are for cross-session Global Memory.
// See docs/en/reference/memory-model.md §3.
var ValidSessionMemoryCategories = map[string]bool{
	"fact":    true,
	"context": true,
}

// Source values for SessionMemoryEntry.Source. No `manual`
// value: anything the user types directly is already in
// Records, so a manual entry path doesn't add value here.
const (
	SessionSourceUserTurn      = "user_turn"
	SessionSourceAssistantTurn = "assistant_turn"
	// SessionSourceToolCall: written via the remember-fact builtin
	// tool (ADR-0019). See GlobalSourceToolCall for rationale.
	SessionSourceToolCall = "tool_call"
)

// SessionMemoryEntry is an auto-extracted session-context fact.
// Lives in `sessions/<id>/session_memory.json` and is deleted
// with the session.
type SessionMemoryEntry struct {
	Fact       string    `json:"fact"`
	NativeFact string    `json:"native_fact"`
	Category   string    `json:"category"` // fact | context
	SourceTime time.Time `json:"source_time"`
	CreatedAt  time.Time `json:"created_at"`

	// Provenance.
	SourceTurnIndex int    `json:"source_turn_index,omitempty"`
	Source          string `json:"source,omitempty"` // SessionSource* constants
	ToolOriginated  bool   `json:"tool_originated,omitempty"`

	// Lifecycle fields (ADR-0031). See GlobalMemoryEntry for the
	// detailed semantics; mirrored here so a Session-Memory entry
	// goes through the same fresh/active/dormant/archived flow
	// inside its (shorter-lived) session.
	Relevance       float64   `json:"relevance,omitempty"`
	CreatedTurn     int       `json:"created_turn,omitempty"`
	LastTouchedAt   time.Time `json:"last_touched_at,omitzero"`
	LastTouchedTurn int       `json:"last_touched_turn,omitempty"`
	TouchCount      int       `json:"touch_count,omitempty"`
	State           string    `json:"state,omitempty"`
}

// DefaultMaxSessionMemory is the soft cap per session. Smaller
// than GlobalMemory because session noise should be tighter.
const DefaultMaxSessionMemory = 50

// SessionMemoryFormatBudget caps the rendered output size when
// injected into the system prompt.
const SessionMemoryFormatBudget = 16 * 1024 // 16 KiB

// SessionMemoryStore manages per-session memory. Construct via
// NewSessionMemoryStore(sessionID); the file path is derived
// from the session directory.
//
// Concurrency: same model as GlobalMemoryStore — s.mu guards
// every public mutation so the agent loop's Touch/DecayAll and
// the extractor goroutine's Add never race.
type SessionMemoryStore struct {
	mu      sync.Mutex
	path    string
	Entries []SessionMemoryEntry

	// MaxEntries is the soft cap. Zero falls back to
	// DefaultMaxSessionMemory.
	MaxEntries int

	// Thresholds tunes the lifecycle (ADR-0031). Zero value is
	// resolved to DefaultThresholds() at use sites.
	Thresholds LifecycleThresholds
}

// NewSessionMemoryStore creates a store backed by
// `sessions/<sessionID>/session_memory.json`.
func NewSessionMemoryStore(sessionID string) *SessionMemoryStore {
	return &SessionMemoryStore{
		path:       filepath.Join(SessionDir(sessionID), "session_memory.json"),
		MaxEntries: DefaultMaxSessionMemory,
	}
}

// Load reads entries from disk. Missing file = empty store.
// Pre-ADR-0031 entries are normalised via legacyFill.
func (s *SessionMemoryStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.Entries = []SessionMemoryEntry{}
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

// legacyFill mirrors GlobalMemoryStore.legacyFill — populates the
// lifecycle fields on entries written before ADR-0031 (detected by
// Relevance == 0). Entries that already carry lifecycle data are
// left untouched.
func (s *SessionMemoryStore) legacyFill(e *SessionMemoryEntry) {
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

// Save writes entries to disk atomically.
func (s *SessionMemoryStore) Save() error {
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

// Add appends a new entry. Near-duplicates (Jaccard ≥
// ConsolidationJaccardThreshold) are consolidated as a touch on
// the existing entry rather than appended. Eviction past
// MaxEntries selects the lowest-relevance victim, respecting
// state priority (archived first).
//
// Callers populate e.CreatedTurn from the agent loop's currentTurn
// so the fresh-window calculation has meaningful input.
func (s *SessionMemoryStore) Add(e SessionMemoryEntry) bool {
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
		logger.Info("memory: session-memory consolidated new entry into existing %q (jaccard %.2f, source: %s)",
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
		cap = DefaultMaxSessionMemory
	}
	if len(s.Entries) > cap {
		s.evictLowestRelevanceLocked()
	}
	return true
}

// factsLocked returns the slice of Fact strings for consolidation
// scanning. Caller must hold s.mu.
func (s *SessionMemoryStore) factsLocked() []string {
	out := make([]string, len(s.Entries))
	for i, e := range s.Entries {
		out[i] = e.Fact
	}
	return out
}

// evictLowestRelevanceLocked picks the lowest-priority victim and
// removes it. Priority: archived → dormant → active → fresh; ties
// broken by lowest relevance, then oldest LastTouchedAt. Caller
// must hold s.mu.
func (s *SessionMemoryStore) evictLowestRelevanceLocked() {
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
	logger.Info("memory: session-memory evicted %q (relevance %.2f, state %s, last touched turn %d)",
		victim.Fact, victim.Relevance, victim.State, victim.LastTouchedTurn)
	s.Entries = append(s.Entries[:worst], s.Entries[worst+1:]...)
}

// Touch refreshes every entry whose Fact matches the predicate.
// See GlobalMemoryStore.Touch.
func (s *SessionMemoryStore) Touch(matchFn func(fact string) bool, currentTurn int, source string) int {
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
		logger.Info("memory: session-memory touched %q (relevance %.2f → 1.00, %s→%s, source: %s)",
			e.Fact, prev, prevState, e.State, source)
		touched++
	}
	return touched
}

// DecayAll multiplies every non-fresh entry's Relevance by
// DecayRate and recomputes State. See GlobalMemoryStore.DecayAll.
func (s *SessionMemoryStore) DecayAll(currentTurn int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	th := s.Thresholds.resolved()
	flips := 0
	decayed := 0
	for i := range s.Entries {
		e := &s.Entries[i]
		prevState := DeriveState(e.Relevance, e.CreatedTurn, currentTurn, th)
		if prevState != StateFresh {
			e.Relevance = DecayedRelevance(e.Relevance, th.DecayRate)
			decayed++
		}
		nextState := DeriveState(e.Relevance, e.CreatedTurn, currentTurn, th)
		e.State = nextState
		if prevState != nextState {
			logger.Info("memory: session-memory entry %q %s→%s (relevance %.2f, turn %d)",
				e.Fact, prevState, nextState, e.Relevance, currentTurn)
			flips++
		}
	}
	if decayed > 0 && flips == 0 {
		logger.Debug("memory: session-memory decayed %d entries (turn %d, no state flips)", decayed, currentTurn)
	}
	return flips
}

// Delete removes an entry by Fact text.
func (s *SessionMemoryStore) Delete(fact string) bool {
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

// DeleteByFacts removes entries whose Fact is in the given
// list. Returns the count actually deleted.
func (s *SessionMemoryStore) DeleteByFacts(facts []string) int {
	if len(facts) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := make(map[string]struct{}, len(facts))
	for _, f := range facts {
		wanted[f] = struct{}{}
	}
	var kept []SessionMemoryEntry
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

// Get retrieves an entry by index (used by Pin to Global Memory
// flow — the frontend supplies the index from the rendered list).
func (s *SessionMemoryStore) Get(index int) (SessionMemoryEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.Entries) {
		return SessionMemoryEntry{}, false
	}
	return s.Entries[index], true
}

// GetByFact retrieves an entry by its Fact text. Used by the Pin
// to Global Memory binding which keys on fact for stability across
// list re-renders.
func (s *SessionMemoryStore) GetByFact(fact string) (SessionMemoryEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.Entries {
		if e.Fact == fact {
			return e, true
		}
	}
	return SessionMemoryEntry{}, false
}

// All returns a copy of all entries (same rationale as
// GlobalMemoryStore.All — defensive against concurrent mutation
// by the agent-loop lifecycle hooks).
func (s *SessionMemoryStore) All() []SessionMemoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionMemoryEntry, len(s.Entries))
	copy(out, s.Entries)
	return out
}

// FormatForPrompt returns the entries formatted for system
// prompt injection. Same trust-tag logic as GlobalMemory:
// user_turn → [user-stated], everything else → [derived].
//
// Lifecycle filtering (ADR-0031): entries in StateDormant or
// StateArchived are excluded from the prompt output. They remain
// in All() for UI consumers.
func (s *SessionMemoryStore) FormatForPrompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Entries) == 0 {
		return ""
	}
	rendered := make([]string, 0, len(s.Entries))
	for _, e := range s.Entries {
		if e.State == StateDormant || e.State == StateArchived {
			continue
		}
		fact := sanitizeMemoryText(e.Fact, 300)
		native := sanitizeMemoryText(e.NativeFact, 300)
		category := sanitizeMemoryText(e.Category, 30)
		var lb strings.Builder
		lb.WriteString(fmt.Sprintf("- %s [%s] %s", sessionTrustTag(e.Source), category, fact))
		if native != "" && native != fact {
			lb.WriteString(fmt.Sprintf(" (%s)", native))
		}
		if !e.CreatedAt.IsZero() {
			lb.WriteString(fmt.Sprintf(" (learned %s)", e.CreatedAt.Format("2006-01-02")))
		}
		lb.WriteString("\n")
		rendered = append(rendered, lb.String())
	}

	if len(rendered) == 0 {
		return ""
	}
	included := make([]string, 0, len(rendered))
	used := 0
	for i := len(rendered) - 1; i >= 0; i-- {
		if used+len(rendered[i]) > SessionMemoryFormatBudget {
			break
		}
		included = append([]string{rendered[i]}, included...)
		used += len(rendered[i])
	}
	elided := len(rendered) - len(included)

	var sb strings.Builder
	if elided > 0 {
		sb.WriteString(fmt.Sprintf("(%d earlier session-memory entries elided to fit budget)\n", elided))
	}
	for _, l := range included {
		sb.WriteString(l)
	}
	return sb.String()
}

// FormatExistingForExtraction returns entry list as plain
// "- fact\n" lines for the extraction prompt's "already known"
// section.
func (s *SessionMemoryStore) FormatExistingForExtraction() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Entries) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, e := range s.Entries {
		sb.WriteString(fmt.Sprintf("- %s\n", e.Fact))
	}
	return sb.String()
}

// sessionTrustTag mirrors globalTrustTag for session memory.
// SessionSourceUserTurn → [user-stated]; everything else
// (SessionSourceAssistantTurn, empty) → [derived].
func sessionTrustTag(source string) string {
	if source == SessionSourceUserTurn {
		return "[user-stated]"
	}
	return "[derived]"
}

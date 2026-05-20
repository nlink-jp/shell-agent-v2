package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
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
	Source          string `json:"source,omitempty"`          // SessionSource* constants
	ToolOriginated  bool   `json:"tool_originated,omitempty"`
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
type SessionMemoryStore struct {
	path    string
	Entries []SessionMemoryEntry

	// MaxEntries is the soft cap. Zero falls back to
	// DefaultMaxSessionMemory.
	MaxEntries int
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
func (s *SessionMemoryStore) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.Entries = []SessionMemoryEntry{}
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.Entries)
}

// Save writes entries to disk atomically.
func (s *SessionMemoryStore) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.Entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(s.path, data, 0600)
}

// Add appends a new entry, deduplicating by Fact. FIFO eviction
// past MaxEntries. Returns true if added.
func (s *SessionMemoryStore) Add(e SessionMemoryEntry) bool {
	for _, existing := range s.Entries {
		if existing.Fact == e.Fact {
			return false
		}
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	if e.SourceTime.IsZero() {
		e.SourceTime = time.Now()
	}
	s.Entries = append(s.Entries, e)

	cap := s.MaxEntries
	if cap <= 0 {
		cap = DefaultMaxSessionMemory
	}
	if len(s.Entries) > cap {
		s.Entries = s.Entries[1:]
	}
	return true
}

// Delete removes an entry by Fact text.
func (s *SessionMemoryStore) Delete(fact string) bool {
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
	if index < 0 || index >= len(s.Entries) {
		return SessionMemoryEntry{}, false
	}
	return s.Entries[index], true
}

// GetByFact retrieves an entry by its Fact text. Used by the Pin
// to Global Memory binding which keys on fact for stability across
// list re-renders.
func (s *SessionMemoryStore) GetByFact(fact string) (SessionMemoryEntry, bool) {
	for _, e := range s.Entries {
		if e.Fact == fact {
			return e, true
		}
	}
	return SessionMemoryEntry{}, false
}

// All returns all entries.
func (s *SessionMemoryStore) All() []SessionMemoryEntry {
	return s.Entries
}

// FormatForPrompt returns the entries formatted for system
// prompt injection. Same trust-tag logic as GlobalMemory:
// user_turn → [user-stated], everything else → [derived].
func (s *SessionMemoryStore) FormatForPrompt() string {
	if len(s.Entries) == 0 {
		return ""
	}
	lines := make([]string, len(s.Entries))
	for i, e := range s.Entries {
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
		lines[i] = lb.String()
	}

	included := make([]string, 0, len(lines))
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if used+len(lines[i]) > SessionMemoryFormatBudget {
			break
		}
		included = append([]string{lines[i]}, included...)
		used += len(lines[i])
	}
	elided := len(lines) - len(included)

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

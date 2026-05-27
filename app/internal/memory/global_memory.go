package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
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
}

// DefaultMaxGlobalMemory is the soft cap on GlobalMemoryStore
// entries. FIFO eviction on overflow.
const DefaultMaxGlobalMemory = 100

// GlobalMemoryFormatBudget caps the rendered output size when
// injected into the system prompt.
const GlobalMemoryFormatBudget = 16 * 1024 // 16 KiB

// GlobalMemoryStore manages cross-session global memory entries.
type GlobalMemoryStore struct {
	path    string
	Entries []GlobalMemoryEntry

	// MaxEntries is the soft cap. Zero falls back to
	// DefaultMaxGlobalMemory.
	MaxEntries int
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
func (s *GlobalMemoryStore) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.Entries = []GlobalMemoryEntry{}
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.Entries)
}

// Save writes entries to disk atomically (tmp + rename) so a
// crash mid-write leaves the previous file intact.
func (s *GlobalMemoryStore) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.Entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(s.path, data, 0600)
}

// Add appends a new entry, deduplicating by Fact text. FIFO
// eviction kicks in past MaxEntries. Returns true if added,
// false if dedup'd.
func (s *GlobalMemoryStore) Add(e GlobalMemoryEntry) bool {
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
		cap = DefaultMaxGlobalMemory
	}
	if len(s.Entries) > cap {
		s.Entries = s.Entries[1:]
	}
	return true
}

// Set creates or updates an entry by Fact text. Used by the
// settings UI direct-edit path. Stamped Source = manual.
func (s *GlobalMemoryStore) Set(fact, native, category string) {
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
		Source: GlobalSourceManual,
	})
}

// Delete removes an entry by Fact text.
func (s *GlobalMemoryStore) Delete(fact string) bool {
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

// All returns all entries.
func (s *GlobalMemoryStore) All() []GlobalMemoryEntry {
	return s.Entries
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
// Trust tag derivation:
//   - [user-stated]: user_turn / manual / promoted_from_*
//   - [derived]:     assistant_turn / empty (legacy)
func (s *GlobalMemoryStore) FormatForPrompt() string {
	if len(s.Entries) == 0 {
		return ""
	}
	lines := make([]string, len(s.Entries))
	for i, e := range s.Entries {
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
		lines[i] = lb.String()
	}

	included := make([]string, 0, len(lines))
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if used+len(lines[i]) > GlobalMemoryFormatBudget {
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

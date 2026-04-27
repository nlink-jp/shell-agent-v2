package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// PinnedFact is a persistent cross-session fact with bilingual support.
type PinnedFact struct {
	Fact       string    `json:"fact"`
	NativeFact string    `json:"native_fact"`
	Category   string    `json:"category"` // preference, decision, fact, context
	SourceTime time.Time `json:"source_time"`
	CreatedAt  time.Time `json:"created_at"`
	// Legacy fields for backward compat
	Key     string `json:"key,omitempty"`
	Content string `json:"content,omitempty"`
}

// PinnedStore manages cross-session persistent facts.
type PinnedStore struct {
	path    string
	Entries []PinnedFact
}

// NewPinnedStore creates a store backed by the default pinned file.
func NewPinnedStore() *PinnedStore {
	return &PinnedStore{
		path: filepath.Join(config.DataDir(), "pinned.json"),
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

// Save writes pinned facts to disk.
func (s *PinnedStore) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.Entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// Add appends a new fact, deduplicating by content.
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
	return true
}

// Set creates or updates a pinned fact by key.
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

// FormatForPrompt returns pinned facts formatted for system prompt injection.
// Facts are sanitized to prevent prompt injection — LLM-extracted facts
// may contain hostile content (newlines, control chars, instruction-like text).
func (s *PinnedStore) FormatForPrompt() string {
	if len(s.Entries) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, e := range s.Entries {
		fact := sanitizePinned(e.Fact, 300)
		native := sanitizePinned(e.NativeFact, 300)
		category := sanitizePinned(e.Category, 30)
		sb.WriteString(fmt.Sprintf("- [%s] %s", category, fact))
		if native != "" && native != fact {
			sb.WriteString(fmt.Sprintf(" (%s)", native))
		}
		if !e.CreatedAt.IsZero() {
			sb.WriteString(fmt.Sprintf(" (learned %s)", e.CreatedAt.Format("2006-01-02")))
		}
		sb.WriteString("\n")
	}
	return sb.String()
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

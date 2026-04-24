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

// PinnedFact is a persistent cross-session fact.
type PinnedFact struct {
	Key       string `json:"key"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// PinnedStore manages cross-session persistent facts.
type PinnedStore struct {
	path  string
	facts []PinnedFact
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
			s.facts = []PinnedFact{}
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.facts)
}

// Save writes pinned facts to disk.
func (s *PinnedStore) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.facts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// Set creates or updates a pinned fact.
func (s *PinnedStore) Set(key, content string) {
	now := time.Now().Format(time.RFC3339)
	for i, f := range s.facts {
		if f.Key == key {
			s.facts[i].Content = content
			s.facts[i].UpdatedAt = now
			return
		}
	}
	s.facts = append(s.facts, PinnedFact{
		Key:       key,
		Content:   content,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// Delete removes a pinned fact by key.
func (s *PinnedStore) Delete(key string) bool {
	for i, f := range s.facts {
		if f.Key == key {
			s.facts = append(s.facts[:i], s.facts[i+1:]...)
			return true
		}
	}
	return false
}

// Get retrieves a pinned fact by key.
func (s *PinnedStore) Get(key string) (PinnedFact, bool) {
	for _, f := range s.facts {
		if f.Key == key {
			return f, true
		}
	}
	return PinnedFact{}, false
}

// All returns all pinned facts.
func (s *PinnedStore) All() []PinnedFact {
	return s.facts
}

// FormatForPrompt returns pinned facts formatted for system prompt injection.
func (s *PinnedStore) FormatForPrompt() string {
	if len(s.facts) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, f := range s.facts {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", f.Key, f.Content))
	}
	return sb.String()
}

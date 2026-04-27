package memory

import (
	"strings"
	"testing"
)

func TestPinnedSetAndGet(t *testing.T) {
	s := &PinnedStore{path: "/tmp/test-pinned.json", Entries: []PinnedFact{}}

	s.Set("name", "Alice")
	f, ok := s.Get("name")
	if !ok {
		t.Fatal("expected to find fact")
	}
	if f.Content != "Alice" {
		t.Errorf("content = %v, want Alice", f.Content)
	}

	// Update
	s.Set("name", "Bob")
	f, _ = s.Get("name")
	if f.Content != "Bob" {
		t.Errorf("updated content = %v, want Bob", f.Content)
	}
	if len(s.All()) != 1 {
		t.Errorf("facts count = %d, want 1 (update should not duplicate)", len(s.All()))
	}
}

func TestPinnedDelete(t *testing.T) {
	s := &PinnedStore{path: "/tmp/test-pinned.json", Entries: []PinnedFact{}}

	s.Set("key1", "value1")
	s.Set("key2", "value2")

	if !s.Delete("key1") {
		t.Error("delete should return true for existing key")
	}
	if s.Delete("nonexistent") {
		t.Error("delete should return false for missing key")
	}
	if len(s.All()) != 1 {
		t.Errorf("facts count = %d, want 1", len(s.All()))
	}
}

func TestPinnedDeleteByKeys(t *testing.T) {
	s := &PinnedStore{path: "/tmp/test-pinned.json", Entries: []PinnedFact{}}
	s.Set("name", "Alice")
	s.Set("role", "data scientist")
	s.Set("city", "Tokyo")

	deleted := s.DeleteByKeys([]string{"name", "city", "missing"})
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	if len(s.Entries) != 1 || s.Entries[0].Key != "role" {
		t.Errorf("remaining = %+v", s.Entries)
	}
}

func TestPinnedDeleteByKeys_MatchesByFact(t *testing.T) {
	// LLM-extracted facts have separate Key and Fact values; the frontend
	// passes p.fact when deleting, so the lookup must hit on Fact too.
	s := &PinnedStore{path: "/tmp/test-pinned.json", Entries: []PinnedFact{
		{Key: "preference-1", Fact: "user prefers dark mode", Category: "fact"},
	}}
	if got := s.DeleteByKeys([]string{"user prefers dark mode"}); got != 1 {
		t.Errorf("delete by fact: got %d, want 1", got)
	}
	if len(s.Entries) != 0 {
		t.Errorf("entries should be empty")
	}
}

func TestPinnedFormatForPrompt(t *testing.T) {
	s := &PinnedStore{path: "/tmp/test-pinned.json", Entries: []PinnedFact{}}

	// Empty
	if s.FormatForPrompt() != "" {
		t.Error("empty store should return empty string")
	}

	s.Set("name", "Alice")
	s.Set("role", "data scientist")

	prompt := s.FormatForPrompt()
	if !strings.Contains(prompt, "Alice") {
		t.Error("prompt missing name")
	}
	if !strings.Contains(prompt, "data scientist") {
		t.Error("prompt missing role")
	}
	// learned-date suffix lets the LLM weigh fact recency.
	if !strings.Contains(prompt, "(learned ") {
		t.Errorf("prompt should include learned-date suffix; got %q", prompt)
	}
}

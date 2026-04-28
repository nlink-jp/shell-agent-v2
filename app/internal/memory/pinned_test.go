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

func TestNewPinnedStore_LoadSaveRoundtrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Fresh store: Load on missing file leaves Entries empty without erroring.
	s := NewPinnedStore()
	if err := s.Load(); err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if len(s.Entries) != 0 {
		t.Errorf("entries on missing file = %d, want 0", len(s.Entries))
	}

	// Add and save.
	added := s.Add(PinnedFact{Key: "name", Fact: "user-name", NativeFact: "Alice", Content: "Alice", Category: "fact"})
	if !added {
		t.Fatal("first Add should return true")
	}
	dup := s.Add(PinnedFact{Key: "different", Fact: "user-name", NativeFact: "Alice2", Category: "fact"})
	if dup {
		t.Error("Add must deduplicate on Fact text — second insert returned true")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload from disk.
	s2 := NewPinnedStore()
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s2.Entries) != 1 {
		t.Fatalf("entries after reload = %d, want 1", len(s2.Entries))
	}
	if s2.Entries[0].Key != "name" || s2.Entries[0].Fact != "user-name" {
		t.Errorf("roundtripped fact = %+v", s2.Entries[0])
	}
	if s2.Entries[0].CreatedAt.IsZero() {
		t.Error("CreatedAt should have been auto-set on Add")
	}
}

func TestPinnedFormatExistingForExtraction(t *testing.T) {
	s := &PinnedStore{Entries: []PinnedFact{
		{Category: "fact", Fact: "user is in Tokyo"},
		{Category: "preference", Fact: "prefers dark mode"},
	}}
	out := s.FormatExistingForExtraction()
	for _, want := range []string{"Tokyo", "dark mode"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output: %s", want, out)
		}
	}
	empty := (&PinnedStore{}).FormatExistingForExtraction()
	if empty == "" {
		t.Error("empty store should return a placeholder, not an empty string")
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

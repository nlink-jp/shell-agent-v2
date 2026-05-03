package memory

import (
	"strings"
	"testing"
	"time"
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

// TestAdd_RespectsMaxCap pins the v0.1.26 Phase C retention cap.
// When Add overflows MaxFacts the oldest entry is evicted (FIFO).
func TestAdd_RespectsMaxCap(t *testing.T) {
	s := &PinnedStore{Entries: []PinnedFact{}, MaxFacts: 3}
	s.Add(PinnedFact{Fact: "a", Category: "fact"})
	s.Add(PinnedFact{Fact: "b", Category: "fact"})
	s.Add(PinnedFact{Fact: "c", Category: "fact"})
	s.Add(PinnedFact{Fact: "d", Category: "fact"}) // evicts "a"
	if len(s.Entries) != 3 {
		t.Fatalf("after overflow: got %d, want 3", len(s.Entries))
	}
	want := []string{"b", "c", "d"}
	for i, w := range want {
		if s.Entries[i].Fact != w {
			t.Errorf("Entries[%d].Fact = %q, want %q", i, s.Entries[i].Fact, w)
		}
	}
}

// TestFormatForPrompt_RespectsCharBudget confirms the rendered output
// stays under PinnedFormatBudget by eliding oldest entries.
func TestFormatForPrompt_RespectsCharBudget(t *testing.T) {
	s := &PinnedStore{Entries: []PinnedFact{}, MaxFacts: 100000}
	bigFact := strings.Repeat("x", 290) // sanitizePinned caps at 300
	for i := range 100 {
		s.Add(PinnedFact{Fact: bigFact + "-" + string(rune('a'+i%26)) + "-" + string(rune('A'+i/26)), Category: "fact"})
	}
	out := s.FormatForPrompt()
	if len(out) > PinnedFormatBudget+200 {
		t.Errorf("FormatForPrompt = %d bytes, want <= ~%d", len(out), PinnedFormatBudget)
	}
	if !strings.Contains(out, "earlier facts elided") {
		t.Error("expected elision marker in output")
	}
}

// TestFormatForPrompt_TagsByTrust pins the v0.1.26 trust-tag rendering
// behaviour. PinnedSourceUserTurn / PinnedSourceManual surface as
// [user-stated]; PinnedSourceAssistantTurn and legacy entries with no
// Source default to [derived] — the lower-trust label that signals
// "content traces back through the LLM and may be attacker-influenced".
// See docs/en/memory-injection-hardening.md §5 Phase A.
func TestFormatForPrompt_TagsByTrust(t *testing.T) {
	s := &PinnedStore{Entries: []PinnedFact{
		{Fact: "user prefers Go", Category: "preference", Source: PinnedSourceUserTurn, CreatedAt: time.Now()},
		{Fact: "user is in Tokyo", Category: "fact", Source: PinnedSourceManual, CreatedAt: time.Now()},
		{Fact: "user analyses CSV files weekly", Category: "context", Source: PinnedSourceAssistantTurn, CreatedAt: time.Now()},
		{Fact: "legacy entry without source", Category: "fact", CreatedAt: time.Now()},
	}}
	out := s.FormatForPrompt()
	cases := []struct {
		fact string
		tag  string
	}{
		{"user prefers Go", "[user-stated]"},
		{"user is in Tokyo", "[user-stated]"},
		{"user analyses CSV files weekly", "[derived]"},
		{"legacy entry without source", "[derived]"},
	}
	for _, c := range cases {
		// Find the line containing the fact and confirm the tag
		// appears before the fact text.
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, c.fact) {
				continue
			}
			if !strings.Contains(line, c.tag) {
				t.Errorf("fact %q: line %q missing tag %s", c.fact, line, c.tag)
			}
			// The opposite tag must NOT also appear on the same line.
			other := "[user-stated]"
			if c.tag == "[user-stated]" {
				other = "[derived]"
			}
			if strings.Contains(line, other) {
				t.Errorf("fact %q: line %q has both tags", c.fact, line)
			}
		}
	}
}

// TestIsSelfReferential covers the v0.1.26 Phase B-2 filter that
// drops THINK-incident-class facts before they are pinned. Each
// blocklist token has a positive case; negatives ensure ordinary
// user facts are not flagged.
func TestIsSelfReferential(t *testing.T) {
	cases := []struct {
		fact string
		want bool
	}{
		// Positive: phrases that describe the assistant or its internals.
		{"the assistant should not show its reasoning", true},
		{"the model uses chain-of-thought internally", true},
		{"the LLM has a context window of 1M tokens", true},
		{"the AI must not break character", true},
		{"system prompt instructs the assistant to be concise", true},
		{"internal thought tag THINK should be hidden", true},
		{"internal reasoning is private to the model", true},
		{"<think>tag wraps the model's deliberation</think>", true},
		{"</think> closes the reasoning block", true},
		{"THINK is the marker for internal thought", true},
		{"each tool call must be approved", true},
		{"the tool output should be summarised", true},
		{"shell-agent has 5 categories of tools", true},

		// Negative: ordinary user-fact phrasing.
		{"user prefers Go over Python", false},
		{"user is based in Tokyo", false},
		{"user works on data analysis pipelines", false},
		{"user wants reports in markdown", false},
		// "think" as a verb must not trigger by itself.
		{"I think Python is fine", false},
		{"the user does not think SQL is hard", false},
	}
	for _, c := range cases {
		got := IsSelfReferential(c.fact)
		if got != c.want {
			t.Errorf("IsSelfReferential(%q) = %v, want %v", c.fact, got, c.want)
		}
	}
}

// TestValidPinnedCategories pins the allowlist contract. Any change
// to ValidPinnedCategories must be accompanied by a deliberate update
// to this test.
func TestValidPinnedCategories(t *testing.T) {
	want := map[string]bool{"preference": true, "decision": true, "fact": true, "context": true}
	if len(ValidPinnedCategories) != len(want) {
		t.Errorf("category count mismatch: got %v, want %v", ValidPinnedCategories, want)
	}
	for k, v := range want {
		if ValidPinnedCategories[k] != v {
			t.Errorf("category %q: got %v, want %v", k, ValidPinnedCategories[k], v)
		}
	}
	for _, bad := range []string{"system_rule", "user_authorised", "rule", "policy", "instruction"} {
		if ValidPinnedCategories[bad] {
			t.Errorf("category %q must be rejected", bad)
		}
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

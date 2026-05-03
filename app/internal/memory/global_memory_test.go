package memory

import (
	"strings"
	"testing"
	"time"
)

func TestGlobalMemory_AddDedup(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{}, MaxEntries: 100}
	if !s.Add(GlobalMemoryEntry{Fact: "x", Category: "preference"}) {
		t.Error("first add should succeed")
	}
	if s.Add(GlobalMemoryEntry{Fact: "x", Category: "decision"}) {
		t.Error("duplicate fact should dedup")
	}
	if len(s.Entries) != 1 {
		t.Errorf("got %d entries, want 1", len(s.Entries))
	}
}

func TestGlobalMemory_FIFOEviction(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{}, MaxEntries: 3}
	for _, f := range []string{"a", "b", "c", "d"} {
		s.Add(GlobalMemoryEntry{Fact: f, Category: "preference"})
	}
	if len(s.Entries) != 3 {
		t.Fatalf("got %d, want 3", len(s.Entries))
	}
	want := []string{"b", "c", "d"}
	for i, w := range want {
		if s.Entries[i].Fact != w {
			t.Errorf("Entries[%d] = %q, want %q", i, s.Entries[i].Fact, w)
		}
	}
}

func TestGlobalMemory_Set_RestrictsCategory(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{}}
	// `fact` is NOT a valid Global Memory category — it should
	// be coerced to the safe default `decision`.
	s.Set("test fact", "テスト", "fact")
	if s.Entries[0].Category != "decision" {
		t.Errorf("invalid category should coerce to decision, got %q", s.Entries[0].Category)
	}
}

func TestGlobalMemory_FormatForPrompt_TrustTag(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{
		{Fact: "u-fact", Category: "preference", Source: GlobalSourceUserTurn, CreatedAt: time.Now()},
		{Fact: "a-fact", Category: "decision", Source: GlobalSourceAssistantTurn, CreatedAt: time.Now()},
		{Fact: "m-fact", Category: "preference", Source: GlobalSourceManual, CreatedAt: time.Now()},
		{Fact: "p-fact", Category: "decision", Source: GlobalSourcePromotedFromFinding, CreatedAt: time.Now()},
		{Fact: "legacy-fact", Category: "preference", CreatedAt: time.Now()},
	}}
	out := s.FormatForPrompt()
	cases := []struct {
		fact string
		tag  string
	}{
		{"u-fact", "[user-stated]"},
		{"a-fact", "[derived]"},
		{"m-fact", "[user-stated]"},
		{"p-fact", "[user-stated]"},
		{"legacy-fact", "[derived]"},
	}
	for _, c := range cases {
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, c.fact) {
				continue
			}
			if !strings.Contains(line, c.tag) {
				t.Errorf("fact %q: line %q missing tag %s", c.fact, line, c.tag)
			}
		}
	}
}

func TestGlobalMemory_DeleteByFacts(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{
		{Fact: "a"}, {Fact: "b"}, {Fact: "c"},
	}}
	got := s.DeleteByFacts([]string{"a", "c", "missing"})
	if got != 2 {
		t.Errorf("deleted = %d, want 2", got)
	}
	if len(s.Entries) != 1 || s.Entries[0].Fact != "b" {
		t.Errorf("remaining = %+v", s.Entries)
	}
}

func TestGlobalMemory_FormatForPrompt_BudgetElision(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{}, MaxEntries: 100000}
	big := strings.Repeat("x", 290)
	for i := range 100 {
		s.Add(GlobalMemoryEntry{
			Fact:     big + "-" + string(rune('a'+i%26)) + "-" + string(rune('A'+i/26)),
			Category: "preference",
		})
	}
	out := s.FormatForPrompt()
	if len(out) > GlobalMemoryFormatBudget+200 {
		t.Errorf("output = %d bytes, want <= ~%d", len(out), GlobalMemoryFormatBudget)
	}
	if !strings.Contains(out, "earlier facts elided") {
		t.Error("expected elision marker")
	}
}

package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionMemory_AddDedup(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{}, MaxEntries: 50}
	if !s.Add(SessionMemoryEntry{Fact: "x", Category: "context"}) {
		t.Error("first add should succeed")
	}
	if s.Add(SessionMemoryEntry{Fact: "x", Category: "fact"}) {
		t.Error("duplicate should dedup")
	}
}

func TestSessionMemory_FIFOEviction(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{}, MaxEntries: 3}
	for _, f := range []string{"a", "b", "c", "d"} {
		s.Add(SessionMemoryEntry{Fact: f, Category: "fact"})
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

func TestSessionMemory_FormatForPrompt_TrustTag(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{
		{Fact: "u-fact", Category: "context", Source: SessionSourceUserTurn, CreatedAt: time.Now()},
		{Fact: "a-fact", Category: "fact", Source: SessionSourceAssistantTurn, CreatedAt: time.Now()},
		{Fact: "legacy-fact", Category: "fact", CreatedAt: time.Now()},
	}}
	out := s.FormatForPrompt()
	cases := []struct {
		fact string
		tag  string
	}{
		{"u-fact", "[user-stated]"},
		{"a-fact", "[derived]"},
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

func TestSessionMemory_Get(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{
		{Fact: "a"}, {Fact: "b"},
	}}
	if e, ok := s.Get(0); !ok || e.Fact != "a" {
		t.Errorf("Get(0) = %+v, %v", e, ok)
	}
	if _, ok := s.Get(2); ok {
		t.Error("out-of-range Get should return false")
	}
	if _, ok := s.Get(-1); ok {
		t.Error("negative Get should return false")
	}
}

func TestSessionMemory_DeleteByFacts(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{
		{Fact: "a"}, {Fact: "b"}, {Fact: "c"},
	}}
	got := s.DeleteByFacts([]string{"a", "c"})
	if got != 2 {
		t.Errorf("deleted = %d, want 2", got)
	}
	if len(s.Entries) != 1 || s.Entries[0].Fact != "b" {
		t.Errorf("remaining = %+v", s.Entries)
	}
}

// --- ADR-0031 lifecycle tests ------------------------------------

func TestSessionMemory_DecayDriveStateTransitions(t *testing.T) {
	s := &SessionMemoryStore{
		MaxEntries: 50,
		Thresholds: DefaultThresholds(),
	}
	s.Add(SessionMemoryEntry{Fact: "user wants Q1 sales analysis", Category: "context", CreatedTurn: 1})
	if s.Entries[0].State != StateFresh {
		t.Fatalf("just-added entry should be fresh, got %s", s.Entries[0].State)
	}
	for turn := 4; turn <= 5; turn++ {
		s.DecayAll(turn)
	}
	if s.Entries[0].State != StateActive {
		t.Errorf("after 2 decays past fresh: state=%s relevance=%.2f, want active",
			s.Entries[0].State, s.Entries[0].Relevance)
	}
	for turn := 6; turn <= 20; turn++ {
		s.DecayAll(turn)
	}
	if s.Entries[0].State != StateDormant {
		t.Errorf("after many decays: state=%s relevance=%.2f, want dormant",
			s.Entries[0].State, s.Entries[0].Relevance)
	}
	for turn := 21; turn <= 50; turn++ {
		s.DecayAll(turn)
	}
	if s.Entries[0].State != StateArchived {
		t.Errorf("after extreme decays: state=%s relevance=%.2f, want archived",
			s.Entries[0].State, s.Entries[0].Relevance)
	}
}

func TestSessionMemory_Touch_ResetsRelevanceAndState(t *testing.T) {
	s := &SessionMemoryStore{
		MaxEntries: 50,
		Thresholds: DefaultThresholds(),
	}
	s.Add(SessionMemoryEntry{Fact: "User wants Q1 sales analysis", Category: "context", CreatedTurn: 1})
	for turn := 5; turn <= 20; turn++ {
		s.DecayAll(turn)
	}
	if s.Entries[0].State != StateDormant {
		t.Fatalf("setup: want dormant, got %s (r=%.2f)", s.Entries[0].State, s.Entries[0].Relevance)
	}

	pred := LexicalTouchPredicate("Q1 sales analysis update please", 0.3)
	n := s.Touch(pred, 25, "lexical_user_turn")
	if n != 1 {
		t.Errorf("Touch returned %d, want 1", n)
	}
	if s.Entries[0].State != StateActive {
		t.Errorf("post-touch state = %s, want active", s.Entries[0].State)
	}
	if s.Entries[0].Relevance != 1.0 {
		t.Errorf("post-touch relevance = %v, want 1.0", s.Entries[0].Relevance)
	}
	if s.Entries[0].LastTouchedTurn != 25 {
		t.Errorf("LastTouchedTurn = %d, want 25", s.Entries[0].LastTouchedTurn)
	}
}

func TestSessionMemory_Add_Consolidates_NearDuplicates(t *testing.T) {
	s := &SessionMemoryStore{
		MaxEntries: 50,
		Thresholds: DefaultThresholds(),
	}
	s.Add(SessionMemoryEntry{Fact: "User wants to analyse Q1 sales data", Category: "context", CreatedTurn: 1})
	ok := s.Add(SessionMemoryEntry{Fact: "User wants to analyse Q1 sales numbers", Category: "context", CreatedTurn: 5})
	if ok {
		t.Error("near-duplicate should consolidate (Add returns false)")
	}
	if len(s.Entries) != 1 {
		t.Errorf("after consolidation: %d entries, want 1", len(s.Entries))
	}
	if s.Entries[0].TouchCount != 1 {
		t.Errorf("TouchCount = %d, want 1", s.Entries[0].TouchCount)
	}
}

func TestSessionMemory_Add_EvictsLowestRelevanceFirst(t *testing.T) {
	s := &SessionMemoryStore{
		MaxEntries: 3,
		Thresholds: DefaultThresholds(),
	}
	s.Add(SessionMemoryEntry{Fact: "alpha alpha alpha", Category: "context", CreatedTurn: 1})
	s.Add(SessionMemoryEntry{Fact: "bravo bravo bravo", Category: "context", CreatedTurn: 2})
	s.Add(SessionMemoryEntry{Fact: "charlie charlie charlie", Category: "context", CreatedTurn: 3})
	for turn := 4; turn <= 30; turn++ {
		s.DecayAll(turn)
	}
	// Force "bravo" to archived state.
	s.Entries[1].Relevance = 0.05
	s.Entries[1].State = StateArchived
	s.Entries[1].LastTouchedAt = time.Now()

	s.Add(SessionMemoryEntry{Fact: "delta delta delta", Category: "context", CreatedTurn: 31})
	if len(s.Entries) != 3 {
		t.Fatalf("after add+evict: %d entries, want 3", len(s.Entries))
	}
	for _, e := range s.Entries {
		if e.Fact == "bravo bravo bravo" {
			t.Errorf("archived entry was not evicted: %+v", e)
		}
	}
}

func TestSessionMemory_LegacyLoad_FillsFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_memory.json")
	legacyJSON := `[
		{"fact":"User loaded Q1 sales","native_fact":"ユーザーがQ1売上をロード","category":"context","source":"user_turn","created_at":"2026-05-01T09:00:00Z"}
	]`
	if err := os.WriteFile(path, []byte(legacyJSON), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := &SessionMemoryStore{path: path, MaxEntries: 50}
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(s.Entries))
	}
	got := s.Entries[0]
	if got.Relevance != 1.0 {
		t.Errorf("legacy entry relevance = %v, want 1.0", got.Relevance)
	}
	if got.State != StateActive {
		t.Errorf("legacy entry state = %q, want active", got.State)
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !strings.Contains(string(raw), `"relevance": 1`) {
		t.Errorf("after Save, relevance missing from disk:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"state": "active"`) {
		t.Errorf("after Save, state missing from disk:\n%s", raw)
	}
}

func TestSessionMemory_FormatForPrompt_SkipsDormantAndArchived(t *testing.T) {
	now := time.Now()
	s := &SessionMemoryStore{
		MaxEntries: 50,
		Entries: []SessionMemoryEntry{
			{Fact: "active fact", Category: "context", Source: SessionSourceUserTurn, CreatedAt: now, Relevance: 0.8, State: StateActive},
			{Fact: "fresh fact", Category: "context", Source: SessionSourceUserTurn, CreatedAt: now, Relevance: 1.0, State: StateFresh},
			{Fact: "dormant fact", Category: "context", Source: SessionSourceUserTurn, CreatedAt: now, Relevance: 0.2, State: StateDormant},
			{Fact: "archived fact", Category: "context", Source: SessionSourceUserTurn, CreatedAt: now, Relevance: 0.05, State: StateArchived},
		},
	}
	out := s.FormatForPrompt()
	if !strings.Contains(out, "active fact") || !strings.Contains(out, "fresh fact") {
		t.Errorf("active/fresh facts missing from prompt:\n%s", out)
	}
	if strings.Contains(out, "dormant fact") {
		t.Error("dormant fact should be dropped from prompt")
	}
	if strings.Contains(out, "archived fact") {
		t.Error("archived fact should be dropped from prompt")
	}
	if got := len(s.All()); got != 4 {
		t.Errorf("All() = %d, want 4", got)
	}
}

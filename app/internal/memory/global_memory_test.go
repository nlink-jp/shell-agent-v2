package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGlobalMemory_LoadDropsLegacyProvenanceKeys verifies ADR-0028's
// backward compatibility: a global_memory.json written before the
// provenance fields were removed still loads (unknown keys ignored),
// and once re-saved the dropped keys disappear from disk while the
// surviving fields stay intact.
func TestGlobalMemory_LoadDropsLegacyProvenanceKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := NewGlobalMemoryStore()

	legacy := `[{"fact":"f1","native_fact":"n1","category":"preference",` +
		`"source":"promoted_from_session_memory","tool_originated":true,` +
		`"session_id":"sess-123","source_turn_index":4,"promoted_from_id":"7"}]`
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.WriteFile(s.path, []byte(legacy), 0600); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	if err := s.Load(); err != nil {
		t.Fatalf("Load legacy file: %v", err)
	}
	if len(s.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(s.Entries))
	}
	e := s.Entries[0]
	if e.Fact != "f1" || e.NativeFact != "n1" || e.Category != "preference" ||
		e.Source != GlobalSourcePromotedFromSession || !e.ToolOriginated {
		t.Errorf("surviving fields not preserved: %+v", e)
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, k := range []string{"session_id", "source_turn_index", "promoted_from_id"} {
		if strings.Contains(string(raw), k) {
			t.Errorf("re-saved file still contains dropped key %q:\n%s", k, raw)
		}
	}
	if !strings.Contains(string(raw), `"fact": "f1"`) {
		t.Errorf("re-saved file lost the fact:\n%s", raw)
	}
}

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

func TestGlobalMemory_ExportImport_Roundtrip(t *testing.T) {
	src := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	created := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)
	entries := []GlobalMemoryEntry{
		{Fact: "f1", NativeFact: "n1", Category: "preference", Source: GlobalSourceManual, ToolOriginated: true, SourceTime: src, CreatedAt: created},
		{Fact: "f2", NativeFact: "n2", Category: "decision", Source: GlobalSourceUserTurn, SourceTime: src, CreatedAt: created},
	}
	data, err := MarshalGlobalMemoryExport(entries, "0.15.0")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := ParseGlobalMemoryImport(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Fact != "f1" || got[0].NativeFact != "n1" || got[0].Category != "preference" ||
		got[0].Source != GlobalSourceManual || !got[0].ToolOriginated ||
		!got[0].SourceTime.Equal(src) || !got[0].CreatedAt.Equal(created) {
		t.Errorf("roundtrip lost fields: %+v", got[0])
	}
}

func TestGlobalMemory_ParseImport_Rejects(t *testing.T) {
	good := func() []byte {
		b, _ := MarshalGlobalMemoryExport([]GlobalMemoryEntry{{Fact: "x", Category: "preference"}}, "")
		return b
	}()

	if _, err := ParseGlobalMemoryImport([]byte("{not json")); err == nil {
		t.Error("expected error for non-JSON")
	}
	wrongKind := strings.Replace(string(good), GlobalMemoryExportKind, "some-other-thing", 1)
	if _, err := ParseGlobalMemoryImport([]byte(wrongKind)); err == nil {
		t.Error("expected error for wrong kind")
	}
	wrongVer := strings.Replace(string(good), `"schema_version": 1`, `"schema_version": 2`, 1)
	if _, err := ParseGlobalMemoryImport([]byte(wrongVer)); err == nil {
		t.Error("expected error for unsupported schema version")
	}
	if _, err := ParseGlobalMemoryImport(good); err != nil {
		t.Errorf("valid envelope rejected: %v", err)
	}
}

func TestGlobalMemory_Import_MergeSkipDedup(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{
		{Fact: "existing", Category: "preference"},
	}, MaxEntries: 100}

	added, skipped := s.Import([]GlobalMemoryEntry{
		{Fact: "existing", Category: "decision"}, // dup → skip
		{Fact: "new-1", Category: "preference"},   // add
		{Fact: "  ", Category: "decision"},         // empty fact → skip
		{Fact: "new-2", Category: "bogus"},         // add, category coerced
	})
	if added != 2 || skipped != 2 {
		t.Errorf("added=%d skipped=%d, want 2/2", added, skipped)
	}
	if len(s.Entries) != 3 {
		t.Fatalf("store has %d entries, want 3", len(s.Entries))
	}
	// coerced category
	for _, e := range s.Entries {
		if e.Fact == "new-2" && e.Category != "decision" {
			t.Errorf("new-2 category = %q, want decision (coerced)", e.Category)
		}
	}
}

func TestGlobalMemory_Import_PreservesAndStampsTimestamps(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{}, MaxEntries: 100}
	keep := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.Import([]GlobalMemoryEntry{
		{Fact: "with-ts", Category: "preference", SourceTime: keep, CreatedAt: keep},
		{Fact: "no-ts", Category: "preference"},
	})
	byFact := map[string]GlobalMemoryEntry{}
	for _, e := range s.Entries {
		byFact[e.Fact] = e
	}
	if !byFact["with-ts"].CreatedAt.Equal(keep) {
		t.Errorf("with-ts CreatedAt = %v, want preserved %v", byFact["with-ts"].CreatedAt, keep)
	}
	if byFact["no-ts"].CreatedAt.IsZero() {
		t.Error("no-ts CreatedAt should be stamped, got zero")
	}
}

func TestGlobalMemory_Import_FIFOBounded(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{}, MaxEntries: 3}
	var in []GlobalMemoryEntry
	for i := range 10 {
		in = append(in, GlobalMemoryEntry{Fact: "f" + string(rune('0'+i)), Category: "preference"})
	}
	s.Import(in)
	if len(s.Entries) != 3 {
		t.Errorf("after import store has %d entries, want capped at 3", len(s.Entries))
	}
}

func TestGlobalMemory_FormatForPrompt_BudgetElision(t *testing.T) {
	s := &GlobalMemoryStore{Entries: []GlobalMemoryEntry{}, MaxEntries: 100000}
	// Each fact uses 50 repetitions of a single per-entry token so
	// the fact is ~300 chars long but tokenises to a single unique
	// token — preventing Jaccard consolidation (ADR-0031) from
	// collapsing the test set down to one entry.
	for i := range 100 {
		fact := strings.Repeat(fmt.Sprintf("u%04d ", i), 50)
		s.Add(GlobalMemoryEntry{
			Fact:     fact,
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

// --- ADR-0031 lifecycle tests ------------------------------------

// TestGlobalMemory_DecayDriveStateTransitions exercises the
// per-turn relevance multiplier and the resulting fresh → active
// → dormant → archived progression.
func TestGlobalMemory_DecayDriveStateTransitions(t *testing.T) {
	s := &GlobalMemoryStore{
		MaxEntries: 100,
		Thresholds: DefaultThresholds(),
	}
	s.Add(GlobalMemoryEntry{Fact: "stable preference", Category: "preference", CreatedTurn: 1})

	if s.Entries[0].State != StateFresh {
		t.Fatalf("just-added entry should be fresh, got %s", s.Entries[0].State)
	}

	// Turn 1+3 = past fresh window. Decay twice — still active.
	s.DecayAll(4)
	s.DecayAll(5)
	if s.Entries[0].State != StateActive {
		t.Errorf("after 2 decays past fresh: state=%s relevance=%.2f, want active",
			s.Entries[0].State, s.Entries[0].Relevance)
	}

	// Drive relevance below ActiveThreshold (0.4) by repeated decay.
	// 0.93^12 ≈ 0.42 — borderline. Push further to land in dormant.
	for turn := 6; turn <= 20; turn++ {
		s.DecayAll(turn)
	}
	if s.Entries[0].State != StateDormant {
		t.Errorf("after many decays: state=%s relevance=%.2f, want dormant",
			s.Entries[0].State, s.Entries[0].Relevance)
	}

	// Drive past ArchiveThreshold (0.1).
	for turn := 21; turn <= 50; turn++ {
		s.DecayAll(turn)
	}
	if s.Entries[0].State != StateArchived {
		t.Errorf("after extreme decays: state=%s relevance=%.2f, want archived",
			s.Entries[0].State, s.Entries[0].Relevance)
	}
}

// TestGlobalMemory_Touch_ResetsRelevanceAndState verifies a touch
// brings a dormant entry back to active (or above) with bookkeeping.
func TestGlobalMemory_Touch_ResetsRelevanceAndState(t *testing.T) {
	s := &GlobalMemoryStore{
		MaxEntries: 100,
		Thresholds: DefaultThresholds(),
	}
	s.Add(GlobalMemoryEntry{Fact: "user prefers Go", Category: "preference", CreatedTurn: 1})
	// Decay into dormant.
	for turn := 5; turn <= 20; turn++ {
		s.DecayAll(turn)
	}
	if s.Entries[0].State != StateDormant {
		t.Fatalf("setup: want dormant, got %s (r=%.2f)", s.Entries[0].State, s.Entries[0].Relevance)
	}
	prevTouches := s.Entries[0].TouchCount

	// Strong overlap on "user prefers Go" (3 shared tokens of 5
	// union members → Jaccard 0.6, above the 0.3 threshold).
	pred := LexicalTouchPredicate("User prefers Go programming language", 0.3)
	n := s.Touch(pred, 25, "lexical_user_turn")
	if n != 1 {
		t.Errorf("Touch returned %d, want 1", n)
	}
	got := s.Entries[0]
	if got.Relevance != 1.0 {
		t.Errorf("relevance = %v, want 1.0", got.Relevance)
	}
	if got.State != StateActive {
		t.Errorf("state = %s, want active", got.State)
	}
	if got.TouchCount != prevTouches+1 {
		t.Errorf("touch count = %d, want %d", got.TouchCount, prevTouches+1)
	}
	if got.LastTouchedTurn != 25 {
		t.Errorf("LastTouchedTurn = %d, want 25", got.LastTouchedTurn)
	}
}

// TestGlobalMemory_Add_Consolidates_NearDuplicates verifies the
// assistant-drift mitigation: a paraphrased fact lands as a touch
// on the existing entry, not a new row.
func TestGlobalMemory_Add_Consolidates_NearDuplicates(t *testing.T) {
	s := &GlobalMemoryStore{
		MaxEntries: 100,
		Thresholds: DefaultThresholds(),
	}
	s.Add(GlobalMemoryEntry{Fact: "User wants to analyse Q1 sales data", Category: "decision", CreatedTurn: 1})
	if len(s.Entries) != 1 {
		t.Fatalf("setup: want 1 entry, got %d", len(s.Entries))
	}

	// Paraphrase with high Jaccard overlap (≥ 0.5).
	ok := s.Add(GlobalMemoryEntry{Fact: "User wants to analyse Q1 sales numbers", Category: "decision", CreatedTurn: 5})
	if ok {
		t.Error("near-duplicate Add should return false (consolidated)")
	}
	if len(s.Entries) != 1 {
		t.Errorf("after consolidation: %d entries, want 1", len(s.Entries))
	}
	if s.Entries[0].TouchCount != 1 {
		t.Errorf("TouchCount = %d, want 1", s.Entries[0].TouchCount)
	}
	if s.Entries[0].Fact != "User wants to analyse Q1 sales data" {
		t.Errorf("existing Fact text should be preserved, got %q", s.Entries[0].Fact)
	}
}

// TestGlobalMemory_Add_EvictsLowestRelevanceFirst verifies the new
// eviction policy: archived → dormant → active → fresh priority,
// then lowest relevance, then oldest LastTouchedAt.
func TestGlobalMemory_Add_EvictsLowestRelevanceFirst(t *testing.T) {
	s := &GlobalMemoryStore{
		MaxEntries: 3,
		Thresholds: DefaultThresholds(),
	}

	// Seed three entries with distinct, no-token-overlap facts so
	// consolidation doesn't fire.
	s.Add(GlobalMemoryEntry{Fact: "alpha alpha alpha", Category: "preference", CreatedTurn: 1})
	s.Add(GlobalMemoryEntry{Fact: "bravo bravo bravo", Category: "preference", CreatedTurn: 2})
	s.Add(GlobalMemoryEntry{Fact: "charlie charlie charlie", Category: "preference", CreatedTurn: 3})

	// Decay so they're past the fresh window then artificially set
	// "bravo"'s state to archived — it should be the eviction
	// victim regardless of how recently it was touched.
	for turn := 4; turn <= 30; turn++ {
		s.DecayAll(turn)
	}
	s.Entries[1].Relevance = 0.05
	s.Entries[1].State = StateArchived
	s.Entries[1].LastTouchedAt = time.Now() // most recent — but state wins

	// Adding a fourth entry triggers eviction.
	s.Add(GlobalMemoryEntry{Fact: "delta delta delta", Category: "preference", CreatedTurn: 31})
	if len(s.Entries) != 3 {
		t.Fatalf("after add+evict: %d entries, want 3", len(s.Entries))
	}
	for _, e := range s.Entries {
		if e.Fact == "bravo bravo bravo" {
			t.Errorf("archived entry was not evicted: %+v", e)
		}
	}
}

// TestGlobalMemory_LegacyLoad_FillsFields verifies a global_memory.json
// written before ADR-0031 (no lifecycle fields) loads with sensible
// defaults and persists the new fields on save.
func TestGlobalMemory_LegacyLoad_FillsFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)

	path := filepath.Join(dir, "legacy.json")
	legacyJSON := `[
		{"fact":"User prefers Go","native_fact":"ユーザーはGoを好む","category":"preference","source":"manual","created_at":"2026-05-01T09:00:00Z"}
	]`
	if err := os.WriteFile(path, []byte(legacyJSON), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := &GlobalMemoryStore{path: path, MaxEntries: 10}
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
	if got.LastTouchedAt.IsZero() {
		t.Error("legacy entry LastTouchedAt should be filled from CreatedAt")
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !strings.Contains(string(raw), `"relevance": 1`) {
		t.Errorf("after Save, relevance field missing from disk:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"state": "active"`) {
		t.Errorf("after Save, state field missing from disk:\n%s", raw)
	}
}

// TestGlobalMemory_FormatForPrompt_SkipsDormantAndArchived
// verifies that the system-prompt injection drops entries whose
// state has fallen out of the active band, while keeping them
// on disk and visible to UI consumers via All().
func TestGlobalMemory_FormatForPrompt_SkipsDormantAndArchived(t *testing.T) {
	now := time.Now()
	s := &GlobalMemoryStore{
		MaxEntries: 100,
		Entries: []GlobalMemoryEntry{
			{Fact: "active fact", Category: "preference", Source: GlobalSourceUserTurn, CreatedAt: now, Relevance: 0.8, State: StateActive},
			{Fact: "fresh fact", Category: "preference", Source: GlobalSourceUserTurn, CreatedAt: now, Relevance: 1.0, State: StateFresh},
			{Fact: "dormant fact", Category: "preference", Source: GlobalSourceUserTurn, CreatedAt: now, Relevance: 0.2, State: StateDormant},
			{Fact: "archived fact", Category: "preference", Source: GlobalSourceUserTurn, CreatedAt: now, Relevance: 0.05, State: StateArchived},
		},
	}
	out := s.FormatForPrompt()
	if !strings.Contains(out, "active fact") {
		t.Error("active entry should appear in prompt")
	}
	if !strings.Contains(out, "fresh fact") {
		t.Error("fresh entry should appear in prompt")
	}
	if strings.Contains(out, "dormant fact") {
		t.Error("dormant entry should be dropped from prompt")
	}
	if strings.Contains(out, "archived fact") {
		t.Error("archived entry should be dropped from prompt")
	}
	// All() still surfaces them for UI consumption.
	if got := len(s.All()); got != 4 {
		t.Errorf("All() = %d, want 4 (UI must still see dormant/archived)", got)
	}
}

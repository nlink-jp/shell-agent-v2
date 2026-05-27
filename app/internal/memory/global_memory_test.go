package memory

import (
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

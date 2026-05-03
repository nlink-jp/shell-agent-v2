package findings

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestAddFinding(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}

	f := s.Add("Sales peak in April", "sess-001", "Sales Analysis", []string{"sales"}, SourceLLMPromoted, false)

	if f.Content != "Sales peak in April" {
		t.Errorf("content = %v, want 'Sales peak in April'", f.Content)
	}
	if f.OriginSessionID != "sess-001" {
		t.Errorf("origin session = %v, want sess-001", f.OriginSessionID)
	}
	if f.CreatedLabel == "" {
		t.Error("created label is empty")
	}
	if !strings.Contains(f.CreatedLabel, "(") {
		t.Errorf("created label missing day of week: %s", f.CreatedLabel)
	}
	if len(s.All()) != 1 {
		t.Errorf("findings count = %d, want 1", len(s.All()))
	}
}

func TestFormatForPrompt(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}
	s.Add("Sales peak in April", "sess-001", "Sales Analysis", []string{"sales"}, SourceLLMPromoted, false)

	prompt := s.FormatForPrompt()
	if !strings.Contains(prompt, "Sales peak in April") {
		t.Error("prompt missing finding content")
	}
	if !strings.Contains(prompt, "Sales Analysis") {
		t.Error("prompt missing session title")
	}
}

func TestFormatForPromptEmpty(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}
	if s.FormatForPrompt() != "" {
		t.Error("empty store should return empty string")
	}
}

func TestDeleteByIDs(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}
	s.Add("first", "sess-1", "S1", nil, SourceLLMPromoted, false)
	s.Add("second", "sess-1", "S1", nil, SourceLLMPromoted, false)
	s.Add("third", "sess-2", "S2", nil, SourceLLMPromoted, false)
	all := s.All()
	if len(all) != 3 {
		t.Fatalf("setup: got %d findings", len(all))
	}
	deleted := s.DeleteByIDs([]string{all[0].ID, all[2].ID, "missing"})
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	remaining := s.All()
	if len(remaining) != 1 || remaining[0].Content != "second" {
		t.Errorf("remaining = %+v", remaining)
	}
}

func TestDeleteByIDs_Empty(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}
	s.Add("a", "sess", "S", nil, SourceLLMPromoted, false)
	if got := s.DeleteByIDs(nil); got != 0 {
		t.Errorf("nil ids: deleted = %d, want 0", got)
	}
	if len(s.All()) != 1 {
		t.Error("store should be unchanged")
	}
}

func TestNewStore_LoadSaveRoundtrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Fresh store loads empty (file doesn't exist).
	s := NewStore()
	if err := s.Load(); err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if len(s.All()) != 0 {
		t.Errorf("expected empty store, got %d", len(s.All()))
	}

	s.Add("first", "sess-1", "Title 1", []string{"info"}, SourceLLMPromoted, false)
	s.Add("second", "sess-2", "Title 2", []string{"warning"}, SourceLLMPromoted, false)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2 := NewStore()
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s2.All()) != 2 {
		t.Fatalf("after reload: %d findings, want 2", len(s2.All()))
	}
	if s2.All()[0].Content != "first" || s2.All()[1].Content != "second" {
		t.Errorf("content not preserved: %+v", s2.All())
	}
}

// TestStore_AddIsThreadSafe pins the security-hardening-2.md H9 fix:
// concurrent Add calls used to race on the shared len(s.findings) ID
// derivation, producing duplicate IDs and silent miss-targeted
// DeleteByIDs. With the mutex in place every concurrent caller now
// observes its own count slot and produces a unique ID.
func TestStore_AddIsThreadSafe(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-race.json", findings: []Finding{}}

	const N = 64
	var wg sync.WaitGroup
	for range N {
		wg.Go(func() {
			s.Add("content", "sess", "title", nil, SourceLLMPromoted, false)
		})
	}
	wg.Wait()

	all := s.All()
	if len(all) != N {
		t.Fatalf("got %d findings, want %d", len(all), N)
	}
	seen := make(map[string]struct{}, N)
	for _, f := range all {
		if _, dup := seen[f.ID]; dup {
			t.Errorf("duplicate ID generated: %s", f.ID)
		}
		seen[f.ID] = struct{}{}
	}
}

// TestStore_AddOverflowFormat exercises the >999-per-day fallback
// ID format. We seed the store with 999 findings sharing today's
// date prefix, then add one more and assert the new ID matches the
// extended format.
func TestStore_AddOverflowFormat(t *testing.T) {
	// Disable the v0.1.26 retention cap so we can actually reach
	// 999-per-day. The cap is a separate concern (covered by
	// TestAdd_RespectsMaxCap).
	s := &Store{path: "/tmp/test-findings-overflow.json", findings: []Finding{}, MaxFindings: 100000}
	for range 999 {
		s.Add("seed", "sess", "title", nil, SourceLLMPromoted, false)
	}
	got := s.Add("after-overflow", "sess", "title", nil, SourceLLMPromoted, false)
	// After 999, the next ID should be longer than the legacy
	// f-YYYYMMDD-NNN form (which is 14 chars long).
	if len(got.ID) <= 14 {
		t.Errorf("overflow ID = %q (len %d), want extended format", got.ID, len(got.ID))
	}
	if !strings.HasPrefix(got.ID, "f-") {
		t.Errorf("overflow ID = %q, missing f- prefix", got.ID)
	}
}

// TestAdd_RespectsMaxCap pins the v0.1.26 Phase C retention cap.
// When Add overflows MaxFindings the oldest entry is evicted (FIFO)
// so noisy sessions cannot inflate the store indefinitely.
func TestAdd_RespectsMaxCap(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-cap.json", findings: []Finding{}, MaxFindings: 3}
	s.Add("a", "s", "t", nil, SourceLLMPromoted, false)
	s.Add("b", "s", "t", nil, SourceLLMPromoted, false)
	s.Add("c", "s", "t", nil, SourceLLMPromoted, false)
	s.Add("d", "s", "t", nil, SourceLLMPromoted, false) // triggers eviction of "a"

	all := s.All()
	if len(all) != 3 {
		t.Fatalf("after overflow: got %d, want 3", len(all))
	}
	contents := []string{all[0].Content, all[1].Content, all[2].Content}
	want := []string{"b", "c", "d"}
	for i := range want {
		if contents[i] != want[i] {
			t.Errorf("contents[%d] = %q, want %q (full: %v)", i, contents[i], want[i], contents)
		}
	}
}

// TestFormatForPrompt_RespectsCharBudget confirms the rendered output
// stays under FindingsFormatBudget by eliding oldest entries.
func TestFormatForPrompt_RespectsCharBudget(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-budget.json", findings: []Finding{}, MaxFindings: 100000}
	// Each finding renders to ~600 bytes given the 500-byte content
	// cap; 50 of them comfortably exceed the 16 KiB budget.
	bigContent := strings.Repeat("x", 500)
	for i := range 50 {
		s.Add(fmt.Sprintf("%s-%d", bigContent, i), "s", "t", nil, SourceLLMPromoted, false)
	}
	out := s.FormatForPrompt()
	if len(out) > FindingsFormatBudget+200 { // small margin for the elision marker
		t.Errorf("FormatForPrompt = %d bytes, want <= ~%d", len(out), FindingsFormatBudget)
	}
	if !strings.Contains(out, "earlier findings elided") {
		t.Error("expected elision marker in output")
	}
}

// TestFormatForPrompt_TagsByTrust pins the v0.1.26 trust-tag rendering.
// SourceManual → [user-stated]; SourceLLMPromoted and legacy empty
// Source → [derived] (the lower-trust default — see
// docs/en/memory-injection-hardening.md §5 Phase A).
func TestFormatForPrompt_TagsByTrust(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-trust.json", findings: []Finding{
		{ID: "f-1", Content: "manual finding", OriginSessionID: "s1", OriginSessionTitle: "T1", CreatedLabel: "2026-05-03", Source: SourceManual},
		{ID: "f-2", Content: "llm-promoted finding", OriginSessionID: "s1", OriginSessionTitle: "T1", CreatedLabel: "2026-05-03", Source: SourceLLMPromoted, ToolOriginated: true},
		{ID: "f-3", Content: "legacy finding", OriginSessionID: "s1", OriginSessionTitle: "T1", CreatedLabel: "2026-05-03"},
	}}
	out := s.FormatForPrompt()
	cases := []struct {
		content string
		tag     string
	}{
		{"manual finding", "[user-stated]"},
		{"llm-promoted finding", "[derived]"},
		{"legacy finding", "[derived]"},
	}
	for _, c := range cases {
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, c.content) {
				continue
			}
			if !strings.Contains(line, c.tag) {
				t.Errorf("content %q: line %q missing tag %s", c.content, line, c.tag)
			}
			other := "[user-stated]"
			if c.tag == "[user-stated]" {
				other = "[derived]"
			}
			if strings.Contains(line, other) {
				t.Errorf("content %q: line %q has both tags", c.content, line)
			}
		}
	}
}

// TestAdd_StampsSource confirms Add records the Source value passed
// by callers. promote-finding callers pass SourceLLMPromoted; the
// /finding slash command and Settings UI pass SourceManual.
func TestAdd_StampsSource(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-source.json", findings: []Finding{}}
	llm := s.Add("from llm", "s1", "T1", nil, SourceLLMPromoted, true)
	if llm.Source != SourceLLMPromoted {
		t.Errorf("LLM Source = %q, want %q", llm.Source, SourceLLMPromoted)
	}
	if !llm.ToolOriginated {
		t.Error("LLM finding should be ToolOriginated")
	}
	manual := s.Add("from user", "s1", "T1", nil, SourceManual, false)
	if manual.Source != SourceManual {
		t.Errorf("manual Source = %q, want %q", manual.Source, SourceManual)
	}
	if manual.ToolOriginated {
		t.Error("manual finding should not be ToolOriginated")
	}
}

func TestDeleteBySession(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-session.json", findings: []Finding{}}
	s.Add("a", "keep", "K", nil, SourceLLMPromoted, false)
	s.Add("b", "remove", "R", nil, SourceLLMPromoted, false)
	s.Add("c", "remove", "R", nil, SourceLLMPromoted, false)
	s.Add("d", "keep", "K", nil, SourceLLMPromoted, false)

	s.DeleteBySession("remove")

	remaining := s.All()
	if len(remaining) != 2 {
		t.Fatalf("remaining = %d, want 2", len(remaining))
	}
	for _, f := range remaining {
		if f.OriginSessionID != "keep" {
			t.Errorf("unexpected origin: %+v", f)
		}
	}
}

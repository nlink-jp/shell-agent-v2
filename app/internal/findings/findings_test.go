package findings

import (
	"strings"
	"sync"
	"testing"
)

func TestAddFinding(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}

	f := s.Add("Sales peak in April", "sess-001", "Sales Analysis", []string{"sales"})

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
	s.Add("Sales peak in April", "sess-001", "Sales Analysis", []string{"sales"})

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
	s.Add("first", "sess-1", "S1", nil)
	s.Add("second", "sess-1", "S1", nil)
	s.Add("third", "sess-2", "S2", nil)
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
	s.Add("a", "sess", "S", nil)
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

	s.Add("first", "sess-1", "Title 1", []string{"info"})
	s.Add("second", "sess-2", "Title 2", []string{"warning"})
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
			s.Add("content", "sess", "title", nil)
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
	s := &Store{path: "/tmp/test-findings-overflow.json", findings: []Finding{}}
	for range 999 {
		s.Add("seed", "sess", "title", nil)
	}
	got := s.Add("after-overflow", "sess", "title", nil)
	// After 999, the next ID should be longer than the legacy
	// f-YYYYMMDD-NNN form (which is 14 chars long).
	if len(got.ID) <= 14 {
		t.Errorf("overflow ID = %q (len %d), want extended format", got.ID, len(got.ID))
	}
	if !strings.HasPrefix(got.ID, "f-") {
		t.Errorf("overflow ID = %q, missing f- prefix", got.ID)
	}
}

func TestDeleteBySession(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-session.json", findings: []Finding{}}
	s.Add("a", "keep", "K", nil)
	s.Add("b", "remove", "R", nil)
	s.Add("c", "remove", "R", nil)
	s.Add("d", "keep", "K", nil)

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

package findings

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// newTestStore returns an in-memory Store with a fixed path so
// tests don't fight the v0.2.0 per-session file layout.
func newTestStore(name string) *Store {
	return &Store{
		path:        "/tmp/test-findings-" + name + ".json",
		findings:    []Finding{},
		MaxFindings: DefaultMaxFindings,
	}
}

func TestAddFinding(t *testing.T) {
	s := newTestStore("add")
	f := s.Add("Sales peak in April", []string{"sales"}, SourceLLMPromoted, false)

	if f.Content != "Sales peak in April" {
		t.Errorf("content = %v, want 'Sales peak in April'", f.Content)
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
	s := newTestStore("format")
	s.Add("Sales peak in April", []string{"sales"}, SourceLLMPromoted, false)

	prompt := s.FormatForPrompt()
	if !strings.Contains(prompt, "Sales peak in April") {
		t.Error("prompt missing finding content")
	}
	// v0.2.0: per-session storage means session/title aren't in
	// the rendered output anymore.
	if strings.Contains(prompt, "from:") {
		t.Errorf("v0.2.0 prompt should not include 'from:' suffix; got %q", prompt)
	}
	// All findings render with [derived] (no manual source in v0.2.0).
	if !strings.Contains(prompt, "[derived]") {
		t.Errorf("expected [derived] tag in prompt; got %q", prompt)
	}
}

func TestFormatForPromptEmpty(t *testing.T) {
	s := newTestStore("empty")
	if s.FormatForPrompt() != "" {
		t.Error("empty store should return empty string")
	}
}

func TestDeleteByIDs(t *testing.T) {
	s := newTestStore("delete-ids")
	s.Add("first", nil, SourceLLMPromoted, false)
	s.Add("second", nil, SourceLLMPromoted, false)
	s.Add("third", nil, SourceLLMPromoted, false)
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
	s := newTestStore("delete-empty")
	s.Add("a", nil, SourceLLMPromoted, false)
	if got := s.DeleteByIDs(nil); got != 0 {
		t.Errorf("nil ids: deleted = %d, want 0", got)
	}
	if len(s.All()) != 1 {
		t.Error("store should be unchanged")
	}
}

// TestStore_AddIsThreadSafe pins the H9 fix from v0.1.20 — concurrent
// Add calls used to race on len(s.findings) for ID derivation.
// v0.2.0: each Add call needs distinct content because Add now
// dedups against existing entries; the test would otherwise reduce
// to a single insert.
func TestStore_AddIsThreadSafe(t *testing.T) {
	s := newTestStore("race")

	const N = 64
	var wg sync.WaitGroup
	for i := range N {
		idx := i
		wg.Go(func() {
			// Each entry needs disjoint word sets or v0.2.0
			// dedup will collapse the run. The hex-derived
			// suffix dominates the token set so Jaccard stays
			// well below the 0.5 threshold between any two.
			s.Add(fmt.Sprintf("threadtest tokenset%d marker%d slot%d", idx*7919+1, idx*4093+9, idx*1721), nil, SourceLLMPromoted, false)
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
// ID format. Bypass the FIFO cap by setting MaxFindings high. Each
// seed needs distinct word-set or v0.2.0 dedup will collapse the run.
func TestStore_AddOverflowFormat(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-overflow.json", findings: []Finding{}, MaxFindings: 100000}
	for i := range 999 {
		s.Add(fmt.Sprintf("seedrow tokena%d tokenb%d tokenc%d", i*7919+1, i*4093+9, i*1721), nil, SourceLLMPromoted, false)
	}
	got := s.Add("overflowprobe distinct echo foxtrot golf hotel", nil, SourceLLMPromoted, false)
	if got == nil {
		t.Fatal("Add returned nil — content should be unique enough to bypass dedup")
	}
	if len(got.ID) <= 14 {
		t.Errorf("overflow ID = %q (len %d), want extended format", got.ID, len(got.ID))
	}
	if !strings.HasPrefix(got.ID, "f-") {
		t.Errorf("overflow ID = %q, missing f- prefix", got.ID)
	}
}

// TestStore_AddDedup pins the v0.2.0 dedup contract.
//   - exact equality: dropped
//   - normalised (whitespace / case / punctuation) equality: dropped
//   - high word-set Jaccard overlap: dropped
//   - genuinely distinct content: kept
func TestStore_AddDedup(t *testing.T) {
	s := newTestStore("dedup")
	first := s.Add("Tokyo Widget sales spiked to 99999 on 2026-02-16, an outlier", nil, SourceLLMPromoted, false)
	if first == nil {
		t.Fatal("first Add returned nil")
	}
	if got := s.Add("Tokyo Widget sales spiked to 99999 on 2026-02-16, an outlier", nil, SourceLLMPromoted, false); got != nil {
		t.Error("exact-duplicate Add should return nil")
	}
	if got := s.Add("  Tokyo  Widget   sales spiked to 99999 on 2026-02-16: an outlier!", nil, SourceLLMPromoted, false); got != nil {
		t.Error("normalised-duplicate Add (whitespace + punctuation) should return nil")
	}
	if got := s.Add("On 2026-02-16 the Tokyo Widget sales hit an outlier value of 99999", nil, SourceLLMPromoted, false); got != nil {
		t.Errorf("Jaccard-similar Add should return nil; got %+v", got)
	}
	if got := s.Add("Osaka Gadget revenue plunged to 12 yen on 2026-03-09, a striking low value worth review", nil, SourceLLMPromoted, false); got == nil {
		t.Error("genuinely distinct Add should be accepted")
	}
	all := s.All()
	if len(all) != 2 {
		t.Errorf("expected 2 findings after dedup, got %d", len(all))
	}
}

// TestAdd_RespectsMaxCap pins the v0.1.26 retention cap behaviour.
func TestAdd_RespectsMaxCap(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-cap.json", findings: []Finding{}, MaxFindings: 3}
	s.Add("a", nil, SourceLLMPromoted, false)
	s.Add("b", nil, SourceLLMPromoted, false)
	s.Add("c", nil, SourceLLMPromoted, false)
	s.Add("d", nil, SourceLLMPromoted, false) // evicts "a"

	all := s.All()
	if len(all) != 3 {
		t.Fatalf("after overflow: got %d, want 3", len(all))
	}
	want := []string{"b", "c", "d"}
	for i := range want {
		if all[i].Content != want[i] {
			t.Errorf("contents[%d] = %q, want %q", i, all[i].Content, want[i])
		}
	}
}

// TestFormatForPrompt_RespectsCharBudget confirms the rendered
// output stays under FindingsFormatBudget. Each entry needs to
// be word-set distinct from the others or v0.2.0 dedup rejects
// them; pad each with a unique 500-char "u<i>" run so Jaccard
// stays below the dedup threshold.
func TestFormatForPrompt_RespectsCharBudget(t *testing.T) {
	s := &Store{path: "/tmp/test-findings-budget.json", findings: []Finding{}, MaxFindings: 100000}
	for i := range 50 {
		// Each entry needs disjoint token sets or v0.2.0 dedup
		// drops it. Generate 6 unique-per-i words plus one
		// long padding token so size approximates the real
		// "many findings" stress while staying below dedup.
		s.Add(fmt.Sprintf("alpha%da beta%db gamma%dc delta%dd epsilon%de zeta%df %s",
			i, i, i, i, i, i,
			strings.Repeat(fmt.Sprintf("pad%dz", i), 80)), nil, SourceLLMPromoted, false)
	}
	out := s.FormatForPrompt()
	if len(out) > FindingsFormatBudget+200 {
		t.Errorf("FormatForPrompt = %d bytes, want <= ~%d", len(out), FindingsFormatBudget)
	}
	if !strings.Contains(out, "earlier findings elided") {
		t.Error("expected elision marker in output")
	}
}

// TestGet returns a finding by ID (used by Pin to Global Memory).
func TestGet(t *testing.T) {
	s := newTestStore("get")
	added := s.Add("findme", nil, SourceLLMPromoted, false)
	got, ok := s.Get(added.ID)
	if !ok {
		t.Fatal("Get returned !ok for existing ID")
	}
	if got.Content != "findme" {
		t.Errorf("Get content = %q, want findme", got.Content)
	}
	if _, ok := s.Get("missing"); ok {
		t.Error("Get for missing ID should return false")
	}
}

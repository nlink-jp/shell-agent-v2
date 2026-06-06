package contextbuild

import (
	"os"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestComputeRangeKey_StableAcrossCalls(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	records := []memory.Record{
		mkRec(now, "user", "hello"),
		mkRec(now.Add(time.Minute), "assistant", "hi"),
	}
	a := ComputeRangeKey(records, "vertex/gemini-2.5-flash")
	b := ComputeRangeKey(records, "vertex/gemini-2.5-flash")
	if a != b {
		t.Errorf("same input, different keys: %s vs %s", a, b)
	}
}

func TestComputeRangeKey_SummarizerIDChanges(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	records := []memory.Record{mkRec(now, "user", "hi")}
	a := ComputeRangeKey(records, "vertex/gemini-2.5-flash")
	b := ComputeRangeKey(records, "local/gemma-4-26b")
	if a == b {
		t.Error("different summarizers should yield different keys")
	}
}

func TestComputeRangeKey_ContentMutationInvalidates(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	r1 := []memory.Record{mkRec(now, "user", "hello")}
	r2 := []memory.Record{mkRec(now, "user", "hello!")}
	a := ComputeRangeKey(r1, "id")
	b := ComputeRangeKey(r2, "id")
	if a == b {
		t.Error("content edit should invalidate cache key")
	}
}

func TestSummaryCache_PutGet(t *testing.T) {
	c := &SummaryCache{}
	e := SummaryEntry{RangeKey: "k1", Summary: "summary text", CreatedAt: time.Now()}
	c.Put(e)
	got := c.Get("k1")
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Summary != "summary text" {
		t.Errorf("Summary = %q", got.Summary)
	}
	if c.Get("missing") != nil {
		t.Error("missing key should return nil")
	}
}

func TestSummaryCache_Eviction(t *testing.T) {
	c := &SummaryCache{MaxItems: 3}
	now := time.Now()
	for i, k := range []string{"a", "b", "c", "d"} {
		c.Put(SummaryEntry{RangeKey: k, CreatedAt: now.Add(time.Duration(i) * time.Second)})
	}
	if len(c.Entries) != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", len(c.Entries))
	}
	if c.Get("a") != nil {
		t.Error("oldest entry 'a' should have been evicted")
	}
	if c.Get("d") == nil {
		t.Error("newest entry 'd' should remain")
	}
}

func TestSummaryCache_LoadSaveRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	sessionID := "test-session"

	c := &SummaryCache{}
	c.Put(SummaryEntry{
		RangeKey: "abc", SummarizerID: "vertex",
		FromTimestamp: time.Date(2026, 4, 27, 10, 0, 0, 0, utc),
		ToTimestamp:   time.Date(2026, 4, 27, 10, 30, 0, 0, utc),
		RecordCount:   5, Summary: "hi", CreatedAt: time.Now(),
	})
	if err := c.Save(sessionID); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadCache(sessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Get("abc") == nil {
		t.Fatal("entry not roundtripped")
	}
	if loaded.Get("abc").Summary != "hi" {
		t.Errorf("Summary = %q", loaded.Get("abc").Summary)
	}
}

func TestLoadCache_MissingFileIsEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	c, err := LoadCache("never-saved")
	if err != nil {
		t.Fatalf("missing file should not error, got: %v", err)
	}
	if len(c.Entries) != 0 {
		t.Errorf("expected empty cache, got %d entries", len(c.Entries))
	}
}

// Verify memory.SessionDir respects $HOME.
var _ = memory.SessionDir

// --- ADR-0032 content-hash key tests -----------------------------

func TestComputeContentKey_StableAcrossCalls(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, utc)
	records := []memory.Record{
		mkRec(now, "user", "hello"),
		mkRec(now.Add(time.Minute), "assistant", "hi"),
	}
	a := ComputeContentKey(records, "vertex/gemini", []string{"old fact"}, []int{2, 5}, "near")
	b := ComputeContentKey(records, "vertex/gemini", []string{"old fact"}, []int{2, 5}, "near")
	if a != b {
		t.Errorf("same input produced different keys: %s vs %s", a, b)
	}
}

func TestComputeContentKey_StableAcrossTimestampShift(t *testing.T) {
	// Timestamps are NOT part of the v2 key (turn additions shift
	// the surrounding timestamps but don't change what was said).
	t1 := time.Date(2026, 5, 1, 9, 0, 0, 0, utc)
	t2 := time.Date(2026, 5, 1, 9, 5, 0, 0, utc) // 5 min later
	records1 := []memory.Record{mkRec(t1, "user", "same content")}
	records2 := []memory.Record{mkRec(t2, "user", "same content")}
	a := ComputeContentKey(records1, "vertex/gemini", nil, nil, "near")
	b := ComputeContentKey(records2, "vertex/gemini", nil, nil, "near")
	if a != b {
		t.Errorf("timestamp-only shift should not change v2 key: %s vs %s", a, b)
	}
}

func TestComputeContentKey_ChangesOnDeadShift(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, utc)
	records := []memory.Record{mkRec(now, "user", "hi")}
	a := ComputeContentKey(records, "vertex/gemini", []string{"topic A dead"}, nil, "near")
	b := ComputeContentKey(records, "vertex/gemini", []string{"topic A dead", "topic B dead"}, nil, "near")
	if a == b {
		t.Error("adding a dead fingerprint should change the key (drop set differs)")
	}
}

func TestComputeContentKey_ChangesOnAnchorShift(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, utc)
	records := []memory.Record{mkRec(now, "user", "hi")}
	a := ComputeContentKey(records, "vertex/gemini", nil, []int{2}, "near")
	b := ComputeContentKey(records, "vertex/gemini", nil, []int{2, 5}, "near")
	if a == b {
		t.Error("adding an anchor index should change the key")
	}
}

func TestComputeContentKey_DifferentTiers(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, utc)
	records := []memory.Record{mkRec(now, "user", "hi")}
	near := ComputeContentKey(records, "vertex/gemini", nil, nil, "near")
	far := ComputeContentKey(records, "vertex/gemini", nil, nil, "far")
	if near == far {
		t.Error("near and far tiers must produce distinct keys for the same content")
	}
}

func TestComputeContentKey_SortedInputsDeterministic(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, utc)
	records := []memory.Record{mkRec(now, "user", "hi")}
	a := ComputeContentKey(records, "id", []string{"b", "a", "c"}, []int{5, 1, 3}, "near")
	b := ComputeContentKey(records, "id", []string{"c", "a", "b"}, []int{3, 5, 1}, "near")
	if a != b {
		t.Errorf("input ordering must not affect the key: %s vs %s", a, b)
	}
}

func TestSummaryCache_Put_StampsKindContentV2(t *testing.T) {
	c := &SummaryCache{}
	c.Put(SummaryEntry{RangeKey: "k1", Summary: "x"})
	if c.Entries[0].Kind != SummaryEntryKindContentV2 {
		t.Errorf("Put should stamp Kind=content_v2, got %q", c.Entries[0].Kind)
	}
}

func TestSummaryCache_Get_IgnoresLegacy(t *testing.T) {
	c := &SummaryCache{
		// Hand-construct a legacy entry (Kind=="") in-memory.
		Entries: []SummaryEntry{
			{RangeKey: "legacy-key", Summary: "legacy", CreatedAt: time.Now()},
		},
	}
	if got := c.Get("legacy-key"); got != nil {
		t.Errorf("legacy entry must not match in v2 flow, got %+v", got)
	}
}

func TestSummaryCache_Load_AcceptsLegacy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	sessionID := "legacy-session"

	// Pre-write a summaries.json with a legacy entry (no Kind field).
	dir := memory.SessionDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := dir + "/summaries.json"
	legacy := `{"entries":[{"range_key":"old-key","summarizer_id":"v1","record_count":3,"summary":"legacy","created_at":"2025-12-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	c, err := LoadCache(sessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Entries) != 1 {
		t.Fatalf("legacy entry should load, got %d entries", len(c.Entries))
	}
	if c.Entries[0].Kind != "" {
		t.Errorf("legacy entry Kind should remain empty after load, got %q", c.Entries[0].Kind)
	}
	if got := c.Get("old-key"); got != nil {
		t.Error("legacy entry must not match Get in v2 flow")
	}
}

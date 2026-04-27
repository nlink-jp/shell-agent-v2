package contextbuild

import (
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

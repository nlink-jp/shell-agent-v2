package agent

import (
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestGlobalMemoryImportJSON_MergesAndPersists verifies the agent-level
// import (ADR-0027): it parses an export envelope, merges with
// skip-duplicates, persists to disk, and reports accurate counts.
func TestGlobalMemoryImportJSON_MergesAndPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := New(config.Default())
	a.globalMemory = memory.NewGlobalMemoryStore()
	if err := a.globalMemory.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Pre-existing entry to exercise dedup.
	a.globalMemory.Add(memory.GlobalMemoryEntry{Fact: "existing", Category: "preference"})
	if err := a.globalMemory.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	data, err := memory.MarshalGlobalMemoryExport([]memory.GlobalMemoryEntry{
		{Fact: "existing", Category: "decision"},   // dup → skip
		{Fact: "imported-1", Category: "preference"}, // add
		{Fact: "imported-2", Category: "decision"},   // add
	}, "test")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	added, skipped, err := a.GlobalMemoryImportJSON(data)
	if err != nil {
		t.Fatalf("GlobalMemoryImportJSON: %v", err)
	}
	if added != 2 || skipped != 1 {
		t.Errorf("added=%d skipped=%d, want 2/1", added, skipped)
	}

	// A fresh store reading the same path must see the merged set.
	reloaded := memory.NewGlobalMemoryStore()
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload Load: %v", err)
	}
	if len(reloaded.All()) != 3 {
		t.Errorf("persisted entries = %d, want 3: %+v", len(reloaded.All()), reloaded.All())
	}
}

// TestGlobalMemoryImportJSON_RejectsBadFile confirms a malformed file is
// surfaced as an error and does not mutate the store.
func TestGlobalMemoryImportJSON_RejectsBadFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := New(config.Default())
	a.globalMemory = memory.NewGlobalMemoryStore()

	if _, _, err := a.GlobalMemoryImportJSON([]byte(`{"kind":"something-else"}`)); err == nil {
		t.Error("expected error for wrong kind")
	}
	if len(a.globalMemory.All()) != 0 {
		t.Errorf("store mutated on rejected import: %+v", a.globalMemory.All())
	}
}

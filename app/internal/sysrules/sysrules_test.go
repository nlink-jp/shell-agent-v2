package sysrules

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStoreAt(filepath.Join(t.TempDir(), "system_rules.md"))
}

func TestStore_LoadMissing(t *testing.T) {
	s := newTestStore(t)
	if err := s.Load(); err != nil {
		t.Fatalf("Load on missing file should not error: %v", err)
	}
	if got := s.Get(); got != "" {
		t.Errorf("Get after Load on missing file: got %q, want empty", got)
	}
}

func TestStore_SaveLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save("a\nb"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	want := "a\nb\n"
	if got := s.Get(); got != want {
		t.Errorf("Get after Save: got %q, want %q", got, want)
	}

	// Disk reflects normalised content too.
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != want {
		t.Errorf("disk content: got %q, want %q", string(data), want)
	}

	// Fresh Store re-reads the same value.
	s2 := NewStoreAt(s.Path())
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := s2.Get(); got != want {
		t.Errorf("fresh Load: got %q, want %q", got, want)
	}
}

func TestStore_NormalisesCRLF(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save("a\r\nb\r\n\r\n"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := s.Get(); got != "a\nb\n" {
		t.Errorf("CRLF normalisation: got %q, want %q", got, "a\nb\n")
	}
}

func TestStore_SaveEmpty(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(""); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	if got := s.Get(); got != "" {
		t.Errorf("empty Save: got %q, want empty", got)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("disk content for empty Save: got %q, want empty", string(data))
	}
}

func TestStore_SaveOnlyWhitespace(t *testing.T) {
	// Whitespace-only input collapses to empty after normalisation,
	// matching the "explicitly cleared" semantics.
	s := newTestStore(t)
	if err := s.Save("\n\n\n"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := s.Get(); got != "" {
		t.Errorf("whitespace-only Save: got %q, want empty", got)
	}
}

func TestStore_LastWriteWins(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save("first"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if err := s.Save("second"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if got := s.Get(); got != "second\n" {
		t.Errorf("last-write-wins: got %q, want %q", got, "second\n")
	}
}

func TestStore_PreservesInternalTrailingNewlines(t *testing.T) {
	// Internal blank lines must survive; only trailing newlines are
	// collapsed to exactly one.
	s := newTestStore(t)
	if err := s.Save("rule 1\n\nrule 2"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := s.Get(); got != "rule 1\n\nrule 2\n" {
		t.Errorf("internal blanks: got %q, want %q", got, "rule 1\n\nrule 2\n")
	}
}

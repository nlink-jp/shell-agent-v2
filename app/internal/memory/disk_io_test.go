package memory

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAddReportMessage(t *testing.T) {
	s := &Session{ID: "rpt"}
	s.AddReportMessage("Quarterly Report", "# Quarterly\n\nbody")
	if len(s.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(s.Records))
	}
	r := s.Records[0]
	if r.Role != "report" {
		t.Errorf("role = %s, want report", r.Role)
	}
	if r.ToolName != "Quarterly Report" {
		t.Errorf("ToolName (used as title) = %s", r.ToolName)
	}
	if r.Content == "" || r.Tier != TierHot || r.Timestamp.IsZero() {
		t.Errorf("metadata not set: %+v", r)
	}
}

func TestSessionDir_ChatPath(t *testing.T) {
	t.Setenv("HOME", "/Users/test")
	dir := SessionDir("sess-1")
	if filepath.Base(dir) != "sess-1" {
		t.Errorf("session dir basename = %s", filepath.Base(dir))
	}
	if filepath.Base(ChatPath("sess-1")) != "chat.json" {
		t.Errorf("chat path basename should be chat.json")
	}
}

func TestSession_SaveLoad_Roundtrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	original := &Session{
		ID:    "rt-test",
		Title: "Round Trip",
		Records: []Record{
			{Timestamp: now, Role: "user", Content: "hi", Tier: TierHot},
			{Timestamp: now.Add(time.Second), Role: "assistant", Content: "hello", Tier: TierHot},
		},
	}
	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadSession("rt-test")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.ID != "rt-test" || loaded.Title != "Round Trip" {
		t.Errorf("identity not preserved: %+v", loaded)
	}
	if len(loaded.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(loaded.Records))
	}
	if loaded.Records[0].Content != "hi" || loaded.Records[1].Content != "hello" {
		t.Errorf("record contents not preserved: %+v", loaded.Records)
	}
}

func TestLoadSession_MissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := LoadSession("never-existed"); err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestListSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, id := range []string{"a", "b", "c"} {
		s := &Session{ID: id, Title: "T-" + id}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d sessions, want 3", len(got))
	}
}

func TestListSessions_NoSessionsDirReturnsNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := ListSessions()
	if err != nil {
		t.Fatalf("nonexistent dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %d", len(got))
	}
}

func TestDeleteSessionDir_RemovesPersistedSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &Session{ID: "to-delete"}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	// Confirm it exists.
	if _, err := LoadSession("to-delete"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := DeleteSessionDir("to-delete"); err != nil {
		t.Fatalf("DeleteSessionDir: %v", err)
	}
	if _, err := LoadSession("to-delete"); err == nil {
		t.Error("session should be gone after DeleteSessionDir")
	}
}

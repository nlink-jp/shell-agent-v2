package memory

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSessionConfig_LoadMissing_ReturnsNotExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, exists, err := LoadSessionConfig("never-saved")
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if exists {
		t.Error("exists = true for a session that was never saved")
	}
	if cfg != (SessionConfig{}) {
		t.Errorf("expected zero SessionConfig, got %+v", cfg)
	}
}

func TestSessionConfig_LoadMalformed_ReturnsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := "broken-session"
	dir := SessionDir(id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Write a malformed session.json — Load should surface a parse
	// error so the caller can decide whether to delete or recover.
	if err := os.WriteFile(SessionConfigPath(id), []byte(`{ not json`), 0600); err != nil {
		t.Fatal(err)
	}
	_, exists, err := LoadSessionConfig(id)
	if err == nil {
		t.Fatal("expected error for malformed session.json")
	}
	if exists {
		t.Error("exists = true after parse error")
	}
}

func TestSessionConfig_SaveRoundtripsThroughLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := "rt-session"
	want := SessionConfig{ProfileID: "abc-123-uuid"}
	if err := SaveSessionConfig(id, want); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}
	got, exists, err := LoadSessionConfig(id)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if !exists {
		t.Fatal("exists = false after save")
	}
	if got.ProfileID != want.ProfileID {
		t.Errorf("ProfileID = %q, want %q", got.ProfileID, want.ProfileID)
	}
	if got.SchemaVersion != SessionConfigSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d (Save stamps it)", got.SchemaVersion, SessionConfigSchemaVersion)
	}
}

func TestSessionConfig_SaveStampsSchemaVersion(t *testing.T) {
	// Caller may pass schema_version=0; Save must rewrite to the
	// current canonical version so every on-disk record is
	// self-describing.
	t.Setenv("HOME", t.TempDir())
	id := "stamp-session"
	if err := SaveSessionConfig(id, SessionConfig{SchemaVersion: 0, ProfileID: "p"}); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}
	got, _, err := LoadSessionConfig(id)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if got.SchemaVersion != SessionConfigSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, SessionConfigSchemaVersion)
	}
}

func TestSessionConfig_SavePermissionsRestrictive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := "perm-session"
	if err := SaveSessionConfig(id, SessionConfig{ProfileID: "p"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(SessionConfigPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("session.json mode = %v, want 0600", mode)
	}
}

func TestSessionConfig_SaveConcurrent_NoTear(t *testing.T) {
	// Atomic tmp+rename: concurrent saves should always leave a
	// valid (parsable) session.json behind. We don't care which
	// writer wins; we care that the reader never sees half-written
	// bytes.
	t.Setenv("HOME", t.TempDir())
	id := "concurrent-session"
	const writers = 20
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = SaveSessionConfig(id, SessionConfig{ProfileID: "id-" + filepath.Base(SessionDir("x"))})
		}()
	}
	wg.Wait()
	cfg, exists, err := LoadSessionConfig(id)
	if err != nil {
		t.Fatalf("LoadSessionConfig after concurrent saves: %v", err)
	}
	if !exists {
		t.Fatal("exists = false after concurrent saves")
	}
	if cfg.SchemaVersion != SessionConfigSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", cfg.SchemaVersion, SessionConfigSchemaVersion)
	}
}

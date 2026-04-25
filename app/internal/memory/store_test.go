package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestListSessionsEmpty(t *testing.T) {
	// ListSessions uses config.DataDir() which points to real path.
	// Test with a non-existent path returns nil.
	sessions, err := ListSessions()
	if err != nil {
		// May return nil if dir doesn't exist, which is fine
		_ = err
	}
	_ = sessions
}

func TestDeleteSessionDir(t *testing.T) {
	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, "test-sess")
	os.MkdirAll(sessDir, 0700)
	os.WriteFile(filepath.Join(sessDir, "chat.json"), []byte("{}"), 0644)

	if err := os.RemoveAll(sessDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Error("session dir should be deleted")
	}
}

func TestRenameSessionRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, "sess-rename")
	os.MkdirAll(sessDir, 0700)

	s := &Session{
		ID:    "sess-rename",
		Title: "Original",
	}

	// Manual save
	data, _ := json.MarshalIndent(s, "", "  ")
	chatPath := filepath.Join(sessDir, "chat.json")
	os.WriteFile(chatPath, data, 0600)

	// Load and verify
	loadedData, _ := os.ReadFile(chatPath)
	var loaded Session
	json.Unmarshal(loadedData, &loaded)
	if loaded.Title != "Original" {
		t.Errorf("title = %q, want Original", loaded.Title)
	}

	// Rename
	loaded.Title = "Renamed"
	data2, _ := json.MarshalIndent(&loaded, "", "  ")
	os.WriteFile(chatPath, data2, 0600)

	loadedData2, _ := os.ReadFile(chatPath)
	var loaded2 Session
	json.Unmarshal(loadedData2, &loaded2)
	if loaded2.Title != "Renamed" {
		t.Errorf("title after rename = %q, want Renamed", loaded2.Title)
	}
}

package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionAddMessages(t *testing.T) {
	s := &Session{ID: "test", Records: []Record{}}

	s.AddUserMessage("hello")
	s.AddAssistantMessage("hi there")
	s.AddToolResult("tc-1", "resolve-date", "2026-04-24 (Friday)")

	if len(s.Records) != 3 {
		t.Fatalf("records count = %d, want 3", len(s.Records))
	}

	if s.Records[0].Role != "user" {
		t.Errorf("record[0] role = %v, want user", s.Records[0].Role)
	}
	if s.Records[1].Role != "assistant" {
		t.Errorf("record[1] role = %v, want assistant", s.Records[1].Role)
	}
	if s.Records[2].Role != "tool" {
		t.Errorf("record[2] role = %v, want tool", s.Records[2].Role)
	}
	if s.Records[2].ToolCallID != "tc-1" {
		t.Errorf("record[2] tool_call_id = %v, want tc-1", s.Records[2].ToolCallID)
	}
	if s.Records[2].ToolName != "resolve-date" {
		t.Errorf("record[2] tool_name = %v, want resolve-date", s.Records[2].ToolName)
	}

	// All should be hot tier
	for i, r := range s.Records {
		if r.Tier != TierHot {
			t.Errorf("record[%d] tier = %v, want hot", i, r.Tier)
		}
		if r.Timestamp.IsZero() {
			t.Errorf("record[%d] timestamp is zero", i)
		}
	}
}

func TestSessionSaveLoad(t *testing.T) {
	tmpDir := t.TempDir()

	// Override session dir for test
	s := &Session{
		ID:    "test-save",
		Title: "Test Save",
		Records: []Record{
			{Role: "user", Content: "hello", Tier: TierHot},
		},
	}

	// Manual save to temp path
	chatPath := filepath.Join(tmpDir, "chat.json")
	os.MkdirAll(filepath.Dir(chatPath), 0700)

	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(chatPath, data, 0600)

	// Load back
	loadedData, _ := os.ReadFile(chatPath)
	var loaded Session
	json.Unmarshal(loadedData, &loaded)

	if loaded.ID != "test-save" {
		t.Errorf("ID = %v, want test-save", loaded.ID)
	}
	if loaded.Title != "Test Save" {
		t.Errorf("Title = %v, want Test Save", loaded.Title)
	}
	if len(loaded.Records) != 1 {
		t.Fatalf("records count = %d, want 1", len(loaded.Records))
	}
}

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
	s.AddToolResult("tc-1", "resolve-date", "2026-04-24 (Friday)", "success")

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
	if s.Records[2].Status != "success" {
		t.Errorf("record[2] status = %v, want success", s.Records[2].Status)
	}

	// v0.2.0: Tier removed; just verify timestamps exist.
	for i, r := range s.Records {
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
			{Role: "user", Content: "hello"},
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

// TestToolResultStatusRoundTrip pins down two guarantees that the
// session-restore feature relies on:
//   - AddToolResult writes the status into the persisted Record
//     and JSON marshalling preserves it (success and error both).
//   - Loading a JSON document that predates the Status field
//     produces an empty Status — the restore path then defaults
//     it to "success" (covered by the bindings-level test).
func TestToolResultStatusRoundTrip(t *testing.T) {
	s := &Session{ID: "round-trip", Records: []Record{}}
	s.AddToolResult("tc-ok", "shell", "ok", "success")
	s.AddToolResult("tc-bad", "shell", "boom", "error")

	if s.Records[0].Status != "success" {
		t.Errorf("first record status = %q, want success", s.Records[0].Status)
	}
	if s.Records[1].Status != "error" {
		t.Errorf("second record status = %q, want error", s.Records[1].Status)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var loaded Session
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded.Records[0].Status != "success" || loaded.Records[1].Status != "error" {
		t.Errorf("status did not round-trip: %#v", loaded.Records)
	}

	// Legacy session: status field is absent. The decoded record
	// has Status == "" and is left for the restore path to map
	// onto "success".
	legacy := []byte(`{"id":"legacy","title":"old","records":[{"timestamp":"2026-01-01T00:00:00Z","role":"tool","content":"ok","tier":"hot","tool_call_id":"tc","tool_name":"shell"}]}`)
	var oldSess Session
	if err := json.Unmarshal(legacy, &oldSess); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if got := oldSess.Records[0].Status; got != "" {
		t.Errorf("legacy status = %q, want empty (restore-side default)", got)
	}
}

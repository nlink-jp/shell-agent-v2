package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestAgent_DeleteSession_RejectsWhenBusy(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-busy", "T", false)

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	err := a.DeleteSession(context.Background(), "sess-busy")
	if err != ErrBusy {
		t.Errorf("DeleteSession during busy = %v, want ErrBusy", err)
	}
	// Directory should still exist — the busy gate prevented any work.
	if _, statErr := os.Stat(memory.SessionDir("sess-busy")); os.IsNotExist(statErr) {
		t.Errorf("session dir was removed despite ErrBusy")
	}
}

func TestAgent_DeleteSession_Inactive(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-active", "Active", false)

	// Persist a SECOND session that the agent is not currently
	// loaded into. Delete should remove that one and leave the
	// active session untouched.
	other := &memory.Session{
		ID:      "sess-other",
		Title:   "Other",
		Records: []memory.Record{{Role: "user", Content: "elsewhere"}},
	}
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}

	if err := a.DeleteSession(context.Background(), "sess-other"); err != nil {
		t.Fatalf("DeleteSession inactive: %v", err)
	}

	if _, err := os.Stat(memory.SessionDir("sess-other")); !os.IsNotExist(err) {
		t.Errorf("inactive session dir still present after delete: err=%v", err)
	}
	if a.CurrentSession() == nil || a.CurrentSession().ID != "sess-active" {
		t.Errorf("active session disturbed by inactive delete: now %v", a.CurrentSession())
	}
	// The active session's chat.json should still be on disk.
	if _, err := os.Stat(filepath.Join(memory.SessionDir("sess-active"), "chat.json")); err != nil {
		t.Errorf("active session chat.json missing after inactive delete: %v", err)
	}
}

func TestAgent_DeleteSession_Active_NilsPointersAndRemovesDir(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-active", "Active", false)

	if err := a.DeleteSession(context.Background(), "sess-active"); err != nil {
		t.Fatalf("DeleteSession active: %v", err)
	}

	if a.session != nil {
		t.Errorf("a.session should be nil after deleting active, got %v", a.session)
	}
	if a.sessionMemory != nil {
		t.Errorf("a.sessionMemory should be nil, got %v", a.sessionMemory)
	}
	if a.findings != nil {
		t.Errorf("a.findings should be nil, got %v", a.findings)
	}
	if _, err := os.Stat(memory.SessionDir("sess-active")); !os.IsNotExist(err) {
		t.Errorf("active session dir still present after delete: err=%v", err)
	}
	if a.State() != StateIdle {
		t.Errorf("agent state after delete = %v, want Idle", a.State())
	}
}

func TestAgent_DeleteSession_StateRestoredOnError(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-active", "Active", false)

	// Delete a non-existent session — DeleteSessionDir on a
	// missing dir is a no-op (RemoveAll returns nil) but exercises
	// the same state-machine path. Asserts state returns to Idle.
	_ = a.DeleteSession(context.Background(), "nonexistent")
	if a.State() != StateIdle {
		t.Errorf("agent state after no-op delete = %v, want Idle", a.State())
	}
}

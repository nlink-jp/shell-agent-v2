package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

// newTestAgentWithSession builds an Agent rooted in a fresh HOME and
// loads a session that's persisted on disk so SessionDir paths
// resolve correctly. Returns the agent and the destination
// directory caller-owned for output files (typically the parent of
// the sessions/ tree).
func newTestAgentWithSession(t *testing.T, sessID, title string, private bool) *Agent {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a := New(config.Default())
	// Wire an objstore so ExportSession's collectObjectsForExport
	// path is exercised even when there are no objects.
	a.objects = objstore.NewStoreAt(filepath.Join(config.DataDir(), "objects"))
	if err := a.objects.Load(); err != nil {
		t.Fatalf("objstore load: %v", err)
	}

	session := &memory.Session{
		ID:      sessID,
		Title:   title,
		Private: private,
		Records: []memory.Record{
			{Role: "user", Content: "hello world"},
		},
	}
	if err := session.Save(); err != nil {
		t.Fatalf("session save: %v", err)
	}
	if err := a.LoadSession(session); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	return a
}

func TestAgent_ExportSession_RejectsWhenBusy(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-busy", "T", false)

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	dest := filepath.Join(t.TempDir(), "out.shellagent")
	_, _, err := a.ExportSession("sess-busy", dest, "test")
	if err != ErrBusy {
		t.Errorf("ExportSession during busy = %v, want ErrBusy", err)
	}
}

func TestAgent_ExportSession_ActiveRoundtrip(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-active", "Active Investigation", true)

	dest := filepath.Join(t.TempDir(), "active.shellagent")
	size, objCount, err := a.ExportSession("sess-active", dest, "v0.4.0-test")
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if size <= 0 {
		t.Errorf("expected non-zero bundle size, got %d", size)
	}
	if objCount != 0 {
		t.Errorf("expected 0 objects, got %d", objCount)
	}
	if a.State() != StateIdle {
		t.Errorf("agent state after export = %v, want Idle", a.State())
	}

	// Re-import as a fresh session and verify roundtrip preserved
	// the privacy flag + title.
	newID, private, _, _, err := a.ImportSession(dest)
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if newID == "" || newID == "sess-active" {
		t.Errorf("imported ID should be fresh and non-empty, got %q", newID)
	}
	if !private {
		t.Errorf("private flag lost in roundtrip")
	}

	// Title-collision suffix expected (the original session is still
	// listed in memory.ListSessions when import runs).
	loaded, err := memory.LoadSession(newID)
	if err != nil {
		t.Fatalf("re-load imported session: %v", err)
	}
	if loaded.Title != "Active Investigation (imported)" {
		t.Errorf("title collision suffix wrong: %q", loaded.Title)
	}
	if !loaded.Private {
		t.Errorf("private flag missing in chat.json after import")
	}
	if loaded.ID != newID {
		t.Errorf("chat.json id field not rewritten: got %q want %q", loaded.ID, newID)
	}

	// Records survived.
	if len(loaded.Records) != 1 || loaded.Records[0].Content != "hello world" {
		t.Errorf("records lost: %+v", loaded.Records)
	}
}

func TestAgent_ExportSession_InactiveSession(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-active", "Active", false)

	// Create and persist a SECOND session that the agent is not
	// currently loaded into. Export should still succeed, going
	// through the on-disk read path rather than touching a.session.
	other := &memory.Session{
		ID:      "sess-other",
		Title:   "Other",
		Records: []memory.Record{{Role: "user", Content: "elsewhere"}},
	}
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "other.shellagent")
	_, _, err := a.ExportSession("sess-other", dest, "test")
	if err != nil {
		t.Fatalf("ExportSession inactive: %v", err)
	}
	if a.CurrentSession().ID != "sess-active" {
		t.Errorf("active session disturbed by inactive export: now %q", a.CurrentSession().ID)
	}
}

func TestAgent_ExportSession_RegeneratesObjectIDs(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-obj", "WithObjects", false)

	// Register an object owned by sess-obj. Capture its ID so we can
	// assert it's NOT the same after import.
	meta, err := a.objects.Store(strings.NewReader("PNG-DATA"), objstore.TypeImage, "image/png", "x.png", "sess-obj")
	if err != nil {
		t.Fatalf("seed object: %v", err)
	}
	originalObjectID := meta.ID

	// Add a record that references the object both structurally and
	// in markdown so the rewriter is exercised on both forms.
	a.session.Records = append(a.session.Records, memory.Record{
		Role:      "assistant",
		Content:   "see ![pic](object:" + originalObjectID + ")",
		ObjectIDs: []string{originalObjectID},
	})
	if err := a.session.Save(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "obj.shellagent")
	_, count, err := a.ExportSession("sess-obj", dest, "test")
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 object exported, got %d", count)
	}

	newID, _, _, gotCount, err := a.ImportSession(dest)
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if gotCount != 1 {
		t.Errorf("expected 1 object imported, got %d", gotCount)
	}

	// The object should now exist under a NEW ID owned by the new session.
	owners := a.objects.ListBySession(newID)
	if len(owners) != 1 {
		t.Fatalf("expected 1 object for new session, got %d", len(owners))
	}
	newObjectID := owners[0].ID
	if newObjectID == originalObjectID {
		t.Errorf("object ID should have been regenerated, got original %q", newObjectID)
	}

	// And the imported chat.json should reference the NEW ID.
	loaded, err := memory.LoadSession(newID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range loaded.Records {
		for _, oid := range r.ObjectIDs {
			if oid == newObjectID {
				found = true
			}
			if oid == originalObjectID {
				t.Errorf("imported record still references old object ID %q", oid)
			}
		}
		if strings.Contains(r.Content, originalObjectID) {
			t.Errorf("imported record content still contains old object ID")
		}
	}
	if !found {
		t.Errorf("no record references the new object ID %q", newObjectID)
	}
}

func TestAgent_ImportSession_RejectsWhenBusy(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-busy", "T", false)

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	_, _, _, _, err := a.ImportSession("/nonexistent.shellagent")
	if err != ErrBusy {
		t.Errorf("ImportSession during busy = %v, want ErrBusy", err)
	}
}

func TestAgent_MultiImport_SameBundle(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-src", "Source", false)

	dest := filepath.Join(t.TempDir(), "src.shellagent")
	if _, _, err := a.ExportSession("sess-src", dest, "test"); err != nil {
		t.Fatalf("ExportSession: %v", err)
	}

	first, _, _, _, err := a.ImportSession(dest)
	if err != nil {
		t.Fatalf("first ImportSession: %v", err)
	}
	second, _, _, _, err := a.ImportSession(dest)
	if err != nil {
		t.Fatalf("second ImportSession: %v", err)
	}
	if first == second {
		t.Errorf("expected distinct IDs from two imports, both got %q", first)
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/agent"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

func newTestBindings(t *testing.T) (*Bindings, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	objBase := filepath.Join(home, "objects")
	store := objstore.NewStoreAt(objBase)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	a := agent.New(cfg)
	a.SetObjects(store)

	b := &Bindings{
		agent:   a,
		cfg:     cfg,
		objects: store,
	}
	return b, home
}

func saveTestObject(t *testing.T, b *Bindings, objType objstore.ObjectType, mime, content, sessionID string) string {
	t.Helper()
	meta, err := b.objects.Store(strings.NewReader(content), objType, mime, "test."+strings.ReplaceAll(strings.SplitN(mime, "/", 2)[1], "+xml", ""), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return meta.ID
}

func TestBindings_ListObjects_PopulatesMetadata(t *testing.T) {
	b, _ := newTestBindings(t)

	_ = saveTestObject(t, b, objstore.TypeBlob, "text/plain", "first", "")
	_ = saveTestObject(t, b, objstore.TypeImage, "image/png", "\x89PNG...", "")
	idReport := saveTestObject(t, b, objstore.TypeReport, "text/markdown", "# Report\nbody", "sess-x")

	infos := b.ListObjects()
	if len(infos) != 3 {
		t.Fatalf("got %d infos, want 3", len(infos))
	}
	// Locate the report entry by ID and assert its surface metadata.
	var report *ObjectInfo
	for i := range infos {
		if infos[i].ID == idReport {
			report = &infos[i]
		}
	}
	if report == nil {
		t.Fatal("report object not in ListObjects output")
	}
	if report.Type != string(objstore.TypeReport) {
		t.Errorf("type = %s, want report", report.Type)
	}
	if report.SessionID != "sess-x" {
		t.Errorf("session not surfaced: %+v", *report)
	}
	if report.MimeType != "text/markdown" {
		t.Errorf("mime = %s", report.MimeType)
	}
	if report.Size == 0 {
		t.Error("size = 0")
	}
	if report.CreatedAt == "" {
		t.Error("created_at empty")
	}
}

func TestBindings_ListObjects_OrdersByCreatedAt(t *testing.T) {
	b, _ := newTestBindings(t)

	_ = saveTestObject(t, b, objstore.TypeBlob, "text/plain", "earliest", "")
	time.Sleep(1100 * time.Millisecond) // CreatedAt is formatted to second precision
	idLatest := saveTestObject(t, b, objstore.TypeBlob, "text/plain", "latest", "")

	infos := b.ListObjects()
	if len(infos) != 2 {
		t.Fatalf("got %d, want 2", len(infos))
	}
	if infos[0].ID != idLatest {
		t.Errorf("first should be the most recent (%s), got %s", idLatest, infos[0].ID)
	}
}

func TestBindings_GetObjectText(t *testing.T) {
	b, _ := newTestBindings(t)
	id := saveTestObject(t, b, objstore.TypeReport, "text/markdown", "# title\nhello", "")
	got, err := b.GetObjectText(id)
	if err != nil {
		t.Fatalf("GetObjectText: %v", err)
	}
	if got != "# title\nhello" {
		t.Errorf("got %q", got)
	}
	if _, err := b.GetObjectText("nope"); err == nil {
		t.Error("missing id should error")
	}
}

func TestBindings_DeleteObject_AndDeleteObjects(t *testing.T) {
	b, _ := newTestBindings(t)
	id1 := saveTestObject(t, b, objstore.TypeBlob, "text/plain", "a", "")
	id2 := saveTestObject(t, b, objstore.TypeBlob, "text/plain", "b", "")
	id3 := saveTestObject(t, b, objstore.TypeBlob, "text/plain", "c", "")

	if err := b.DeleteObject(id1); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, ok := b.objects.Get(id1); ok {
		t.Error("id1 should be gone")
	}

	deleted, err := b.DeleteObjects([]string{id2, id3, "missing"})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
}

// writeSession creates a chat.json on disk for ObjectReferences to scan.
func writeSession(t *testing.T, home, id string, records []memory.Record) {
	t.Helper()
	dir := filepath.Join(home, "Library", "Application Support", "shell-agent-v2", "sessions", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := memory.Session{ID: id, Title: id, Records: records}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chat.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBindings_ObjectReferences_DetectsBothChannels(t *testing.T) {
	b, home := newTestBindings(t)
	id1 := saveTestObject(t, b, objstore.TypeImage, "image/png", "x", "")
	id2 := saveTestObject(t, b, objstore.TypeImage, "image/png", "y", "")
	id3 := saveTestObject(t, b, objstore.TypeImage, "image/png", "z", "") // unreferenced

	// Session A: references id1 via Record.ObjectIDs
	writeSession(t, home, "sessA", []memory.Record{
		{Role: "user", Content: "look", ObjectIDs: []string{id1}},
	})
	// Session B: references id2 via "object:<id>" string in Content
	writeSession(t, home, "sessB", []memory.Record{
		{Role: "report", Content: "see ![](object:" + id2 + ")"},
	})

	refs, err := b.ObjectReferences([]string{id1, id2, id3})
	if err != nil {
		t.Fatalf("ObjectReferences: %v", err)
	}
	if refs[id1] != 1 {
		t.Errorf("id1 refs = %d, want 1", refs[id1])
	}
	if refs[id2] != 1 {
		t.Errorf("id2 refs = %d, want 1", refs[id2])
	}
	if refs[id3] != 0 {
		t.Errorf("id3 refs = %d, want 0", refs[id3])
	}
}

func TestBindings_ObjectReferences_EmptyInput(t *testing.T) {
	b, _ := newTestBindings(t)
	refs, err := b.ObjectReferences(nil)
	if err != nil {
		t.Fatalf("nil input: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty map, got %v", refs)
	}
}

func TestBindings_DeleteFindings_AndPinned(t *testing.T) {
	// Seed findings.json and pinned.json on disk before agent.New so the
	// agent's stores load them automatically.
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "Library", "Application Support", "shell-agent-v2")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	findingsJSON := `[
		{"id":"f-1","content":"first finding","origin_session_id":"s","origin_session_title":"S","tags":["info"],"created_at":"2026-04-28T00:00:00Z","created_label":"2026-04-28"},
		{"id":"f-2","content":"second finding","origin_session_id":"s","origin_session_title":"S","tags":["info"],"created_at":"2026-04-28T00:01:00Z","created_label":"2026-04-28"}
	]`
	if err := os.WriteFile(filepath.Join(dataDir, "findings.json"), []byte(findingsJSON), 0600); err != nil {
		t.Fatal(err)
	}
	pinnedJSON := `[
		{"key":"name","fact":"name","native_fact":"Alice","content":"Alice","category":"fact","created_at":"2026-04-28T00:00:00Z","source_time":"2026-04-28T00:00:00Z"},
		{"key":"city","fact":"city","native_fact":"Tokyo","content":"Tokyo","category":"fact","created_at":"2026-04-28T00:01:00Z","source_time":"2026-04-28T00:01:00Z"}
	]`
	if err := os.WriteFile(filepath.Join(dataDir, "pinned.json"), []byte(pinnedJSON), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	a := agent.New(cfg)
	store := objstore.NewStoreAt(filepath.Join(home, "objects"))
	a.SetObjects(store)
	b := &Bindings{agent: a, cfg: cfg, objects: store}

	findings := b.GetFindings()
	if len(findings) != 2 {
		t.Fatalf("seed findings = %d", len(findings))
	}
	deleted, err := b.DeleteFindings([]string{findings[0].ID, "missing"})
	if err != nil {
		t.Fatalf("DeleteFindings: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted findings = %d, want 1", deleted)
	}
	if got := len(b.GetFindings()); got != 1 {
		t.Errorf("remaining findings = %d, want 1", got)
	}

	pinned := b.GetPinnedMemories()
	if len(pinned) != 2 {
		t.Fatalf("seed pinned = %d", len(pinned))
	}
	pdel, err := b.DeletePinnedMemories([]string{"name"})
	if err != nil {
		t.Fatalf("DeletePinnedMemories: %v", err)
	}
	if pdel != 1 {
		t.Errorf("deleted pinned = %d, want 1", pdel)
	}
	if got := len(b.GetPinnedMemories()); got != 1 {
		t.Errorf("remaining pinned = %d, want 1", got)
	}
}

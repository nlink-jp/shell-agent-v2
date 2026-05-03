package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/agent"
	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
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

func TestGetSessionObjects_FiltersBySession(t *testing.T) {
	b, _ := newTestBindings(t)
	idA := saveTestObject(t, b, objstore.TypeBlob, "text/plain", "from sess A", "sessA")
	saveTestObject(t, b, objstore.TypeBlob, "text/plain", "from sess B", "sessB")

	got := b.GetSessionObjects("sessA")
	if len(got) != 1 {
		t.Fatalf("want 1 object for sessA, got %d", len(got))
	}
	if got[0].ID != idA {
		t.Errorf("got id %q, want %q", got[0].ID, idA)
	}

	if got := b.GetSessionObjects(""); len(got) != 0 {
		t.Errorf("empty sessionID should return nothing, got %d", len(got))
	}
}

func TestGetSessionTables_EmptyWhenNoEngine(t *testing.T) {
	b, _ := newTestBindings(t)
	// b.analysis is nil — no engine wired up in test setup.
	got := b.GetSessionTables("sessA")
	if len(got) != 0 {
		t.Errorf("expected empty when no analysis engine, got %d", len(got))
	}
}

func TestGetSessionTables_WithEngine(t *testing.T) {
	b, home := newTestBindings(t)

	// Wire an analysis engine and load a CSV so it has a table.
	csvPath := filepath.Join(home, "people.csv")
	if err := os.WriteFile(csvPath, []byte("name,age\nAlice,30\nBob,25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e := analysis.NewWithPath("sess-x", filepath.Join(home, "x.duckdb"))
	defer e.Close()
	if err := e.LoadCSV("people", csvPath); err != nil {
		t.Fatal(err)
	}
	b.analysis = e
	b.agent.SetAnalysis(e)

	got := b.GetSessionTables("sess-x")
	if len(got) != 1 {
		t.Fatalf("want 1 table, got %d", len(got))
	}
	if got[0].Name != "people" {
		t.Errorf("name = %q", got[0].Name)
	}
	if got[0].RowCount != 2 {
		t.Errorf("RowCount = %d, want 2", got[0].RowCount)
	}
}

func TestPreviewTable_Binding(t *testing.T) {
	b, home := newTestBindings(t)
	csvPath := filepath.Join(home, "data.csv")
	if err := os.WriteFile(csvPath, []byte("a,b\n1,x\n2,y\n3,z\n4,w\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e := analysis.NewWithPath("sess-p", filepath.Join(home, "p.duckdb"))
	defer e.Close()
	if err := e.LoadCSV("data", csvPath); err != nil {
		t.Fatal(err)
	}
	b.analysis = e

	res, err := b.PreviewTable("data", 2)
	if err != nil {
		t.Fatalf("PreviewTable: %v", err)
	}
	if len(res.Rows) != 2 || res.Total != 4 || !res.Truncated {
		t.Errorf("unexpected preview: rows=%d total=%d truncated=%v", len(res.Rows), res.Total, res.Truncated)
	}
}

func TestPreviewTable_NoEngine(t *testing.T) {
	b, _ := newTestBindings(t)
	if _, err := b.PreviewTable("anything", 10); err == nil {
		t.Error("expected error when analysis engine is nil")
	}
}

func TestGetWorkFiles_ListsHostMount(t *testing.T) {
	b, home := newTestBindings(t)
	// Frame the session's /work dir manually since tests don't
	// spin up an actual engine. Path layout follows
	// memory.SessionDir(sid) / "work".
	sessDir := memory.SessionDir("sess-w")
	workDir := filepath.Join(sessDir, "work")
	if err := os.MkdirAll(workDir, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Join(home, "sessions"))

	if err := os.WriteFile(filepath.Join(workDir, "alpha.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "beta.csv"), []byte("a,b\n1,2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got := b.GetWorkFiles("sess-w")
	if len(got) != 2 {
		t.Fatalf("want 2 files, got %d (%v)", len(got), got)
	}
	// Paths must be relative to /work, slash-form (no platform
	// backslashes) so the frontend can render them as-is.
	for _, f := range got {
		if strings.Contains(f.Path, "\\") {
			t.Errorf("Path %q must use forward slashes", f.Path)
		}
		if filepath.IsAbs(f.Path) {
			t.Errorf("Path %q must be relative to /work", f.Path)
		}
	}
}

func TestGetWorkFiles_MissingDirReturnsEmpty(t *testing.T) {
	b, _ := newTestBindings(t)
	got := b.GetWorkFiles("session-with-no-work")
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
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


func TestBindings_DeleteFindings_AndPinned(t *testing.T) {
	// v0.2.0: findings live per-session at
	// sessions/<id>/findings.json. Seed the per-session file and
	// load the session before asserting against b.GetFindings().
	// Pinned still lives at the global pinned.json.
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "Library", "Application Support", "shell-agent-v2")
	sessionID := "test-session-1"
	sessionDir := filepath.Join(dataDir, "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	findingsJSON := `[
		{"id":"f-1","content":"first finding","tags":["info"],"created_at":"2026-04-28T00:00:00Z","created_label":"2026-04-28","source":"llm_promoted"},
		{"id":"f-2","content":"second finding","tags":["info"],"created_at":"2026-04-28T00:01:00Z","created_label":"2026-04-28","source":"llm_promoted"}
	]`
	if err := os.WriteFile(filepath.Join(sessionDir, "findings.json"), []byte(findingsJSON), 0600); err != nil {
		t.Fatal(err)
	}
	// Seed a minimal chat.json so the session is loadable.
	chatJSON := `{"id":"` + sessionID + `","title":"Test","records":[]}`
	if err := os.WriteFile(filepath.Join(sessionDir, "chat.json"), []byte(chatJSON), 0600); err != nil {
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

	// Load the session so the per-session findings store is populated.
	session, err := memory.LoadSession(sessionID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if err := a.LoadSession(session); err != nil {
		t.Fatalf("agent.LoadSession: %v", err)
	}

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

// TestLoadSession_RestoresToolEventBubbles pins the contract
// behind the session-restore feature: tool turns persisted via
// AddToolResult come back to the frontend as `tool-event` rows
// with the same status they were given live. Legacy records
// (status field absent) default to "success" so old sessions
// remain readable.
func TestLoadSession_RestoresToolEventBubbles(t *testing.T) {
	b, _ := newTestBindings(t)

	sid := "sess-restore"
	sess := &memory.Session{
		ID:    sid,
		Title: "Restore Test",
		Records: []memory.Record{
			{Timestamp: time.Now(), Role: "user", Content: "hi"},
			// Tool-call assistant turn — must be skipped at
			// restore time (its narrative was a live activity).
			{Timestamp: time.Now(), Role: "assistant", Content: "calling tools",
				ToolCalls: []memory.ToolCallRecord{{ID: "tc-1", Name: "shell", Arguments: "{}"}}},
			// Tool result — should restore as a tool-event bubble.
			{Timestamp: time.Now(), Role: "tool", Content: "ok",
				ToolCallID: "tc-1", ToolName: "shell", Status: "success"},
			// Tool result that errored.
			{Timestamp: time.Now(), Role: "tool", Content: "boom",
				ToolCallID: "tc-2", ToolName: "shell", Status: "error"},
			// Legacy tool record (no status field on disk).
			{Timestamp: time.Now(), Role: "tool", Content: "old",
				ToolCallID: "tc-3", ToolName: "legacy-tool"},
			{Timestamp: time.Now(), Role: "assistant", Content: "done"},
		},
	}

	// Persist via the same path the live agent uses, so DataDir
	// resolution matches what bindings.LoadSession reads.
	if err := os.MkdirAll(memory.SessionDir(sid), 0700); err != nil {
		t.Fatal(err)
	}
	chatBytes, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory.ChatPath(sid), chatBytes, 0600); err != nil {
		t.Fatal(err)
	}

	got, err := b.LoadSession(sid)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	// Expected: user, tool-event(success), tool-event(error),
	// tool-event(success default for legacy), assistant.
	want := []struct {
		role, content, status string
	}{
		{"user", "hi", ""},
		{"tool-event", "shell", "success"},
		{"tool-event", "shell", "error"},
		{"tool-event", "legacy-tool", "success"},
		{"assistant", "done", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d: %#v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Role != w.role || got[i].Content != w.content || got[i].Status != w.status {
			t.Errorf("row[%d] = {%s, %s, %s}, want {%s, %s, %s}",
				i, got[i].Role, got[i].Content, got[i].Status,
				w.role, w.content, w.status)
		}
	}
}

// --- MITL slot tests (Phase 4 hardening) ---

// TestMITL_StrayApproveBeforeRequest_NoOps verifies that a click
// fired when no MITL request is pending does nothing — instead
// of being captured by a buffered channel and silently
// auto-approving the next prompt.
func TestMITL_StrayApproveBeforeRequest_NoOps(t *testing.T) {
	b := &Bindings{}
	b.ApproveMITL()                // no panic, no goroutine leak
	b.RejectMITL()                 // ditto
	b.RejectMITLWithFeedback("no") // ditto

	if b.mitlReq != nil {
		t.Errorf("mitlReq should remain nil after stray clicks")
	}
}

// TestMITL_DoubleApproveSameRequest_Idempotent verifies that
// firing Approve twice resolves the request once and
// ignores the second click.
func TestMITL_DoubleApproveSameRequest_Idempotent(t *testing.T) {
	b := &Bindings{}
	ch := make(chan agent.MITLResponse, 1)
	b.mitlReq = &mitlSlot{req: agent.MITLRequest{ToolName: "x"}, ch: ch}

	b.ApproveMITL()
	b.ApproveMITL() // must not block on a full channel; must not panic

	select {
	case resp := <-ch:
		if !resp.Approved {
			t.Errorf("first approve should have set Approved=true; got %v", resp)
		}
	default:
		t.Fatal("first approve should have written to ch")
	}
	// Channel is drained; if a stale value were left we'd see it on
	// the next read, but the slot's per-request ch is single-use.
	select {
	case extra := <-ch:
		t.Errorf("second approve should have been a no-op; got %v", extra)
	default:
	}
}

// TestMITL_TwoRequestsInSeries_NoLeakBetween simulates the
// production flow: handler 1 emits, gets resolved, then a second
// request comes in. With a per-request channel, the second
// request gets a fresh channel and the first one's resolved
// value can't leak into it.
func TestMITL_TwoRequestsInSeries_NoLeakBetween(t *testing.T) {
	b := &Bindings{}

	// First request: install slot, approve, drain.
	ch1 := make(chan agent.MITLResponse, 1)
	b.mitlReq = &mitlSlot{req: agent.MITLRequest{ToolName: "first"}, ch: ch1}
	b.ApproveMITL()
	resp1 := <-ch1
	if !resp1.Approved {
		t.Fatalf("first: Approved=%v", resp1.Approved)
	}
	// Production handler clears mitlReq after <-ch returns.
	b.mitlReq = nil

	// Stray click between requests must NOT be captured.
	b.ApproveMITL()

	// Second request: install fresh slot.
	ch2 := make(chan agent.MITLResponse, 1)
	b.mitlReq = &mitlSlot{req: agent.MITLRequest{ToolName: "second"}, ch: ch2}

	// ch2 must be empty — the stray click between the two
	// shouldn't have leaked into it.
	select {
	case extra := <-ch2:
		t.Fatalf("ch2 should be empty before its own resolve; got %v", extra)
	default:
	}

	b.RejectMITL()
	resp2 := <-ch2
	if resp2.Approved {
		t.Errorf("second: Approved=%v, want false", resp2.Approved)
	}
}

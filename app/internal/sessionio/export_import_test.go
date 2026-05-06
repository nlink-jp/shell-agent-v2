package sessionio

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/contextbuild"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

// fakeObjstore implements ObjstoreWriter for tests. It records
// every Store call and assigns deterministic but distinct IDs so
// tests can verify the ID-rewrite behaviour without depending on
// the live objstore's RNG.
type fakeObjstore struct {
	stored map[string][]byte // newID -> content
	metas  map[string]*objstore.ObjectMeta
	next   int
	failOn int // index at which Store should fail; -1 to never fail
}

func newFakeObjstore() *fakeObjstore {
	return &fakeObjstore{
		stored: map[string][]byte{},
		metas:  map[string]*objstore.ObjectMeta{},
		failOn: -1,
	}
}

func (f *fakeObjstore) Store(reader io.Reader, t objstore.ObjectType, mime, origName, sessionID string) (*objstore.ObjectMeta, error) {
	idx := f.next
	f.next++
	if f.failOn == idx {
		return nil, errors.New("fake: simulated objstore failure")
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	// Deterministic 32-hex IDs so tests can assert exact post-import content.
	newID := strings.Repeat("1", 31) + string(rune('a'+idx))
	meta := &objstore.ObjectMeta{
		ID:        newID,
		Type:      t,
		MimeType:  mime,
		OrigName:  origName,
		SessionID: sessionID,
		Size:      int64(len(data)),
		CreatedAt: time.Now(),
	}
	f.stored[newID] = data
	f.metas[newID] = meta
	return meta, nil
}

func (f *fakeObjstore) Delete(id string) error {
	delete(f.stored, id)
	delete(f.metas, id)
	return nil
}

// makeFixtureSession writes a session directory with the given
// chat / session_memory / findings / summaries content under
// baseDir/<id>/. Object IDs in the fixture content are bare
// (no `object:` prefix) so the rewriter is exercised on both
// markdown and structured forms.
func makeFixtureSession(t *testing.T, baseDir, id, title string, private bool, oldObjectIDs []string) string {
	t.Helper()
	dir := filepath.Join(baseDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	session := memory.Session{
		ID:      id,
		Title:   title,
		Private: private,
		Records: []memory.Record{
			{
				Timestamp: time.Now(),
				Role:      "user",
				Content:   "see ![pic](object:" + oldObjectIDs[0] + ") and " + oldObjectIDs[1],
				ObjectIDs: []string{oldObjectIDs[0], oldObjectIDs[1]},
			},
			{
				Timestamp: time.Now(),
				Role:      "assistant",
				Content:   "no refs here",
			},
		},
	}
	sessionData, _ := json.MarshalIndent(&session, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "chat.json"), sessionData, 0600); err != nil {
		t.Fatal(err)
	}

	// session_memory.json — empty array (covers the "missing fact data"
	// path; rewriter intentionally never touches this file).
	if err := os.WriteFile(filepath.Join(dir, "session_memory.json"), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	// findings.json — empty array.
	if err := os.WriteFile(filepath.Join(dir, "findings.json"), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}

	// summaries.json — contains a paraphrased reference to one of
	// the bundled object IDs to verify the regex sweep on summary
	// text (design §5.3 emphasises this is the only non-chat
	// rewrite locus).
	cache := contextbuild.SummaryCache{
		Entries: []contextbuild.SummaryEntry{
			{
				RangeKey: "k1",
				Summary:  "user shared an image (object:" + oldObjectIDs[0] + ") then asked about it",
			},
		},
	}
	cacheData, _ := json.MarshalIndent(&cache, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "summaries.json"), cacheData, 0600); err != nil {
		t.Fatal(err)
	}

	// work/ with one nested file so the recursive walk gets exercised.
	workDir := filepath.Join(dir, "work", "subdir")
	if err := os.MkdirAll(workDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "result.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRoundtrip_WithObjectsAndReferences(t *testing.T) {
	tmp := t.TempDir()
	srcSessionsDir := filepath.Join(tmp, "src", "sessions")
	dstSessionsDir := filepath.Join(tmp, "dst", "sessions")
	bundlePath := filepath.Join(tmp, "out.shellagent")

	const (
		oldA = "0123456789abcdef0123456789abcdef"
		oldB = "fedcba9876543210fedcba9876543210"
	)
	srcDir := makeFixtureSession(t, srcSessionsDir, "sess-orig", "My Investigation", true, []string{oldA, oldB})

	objects := []ObjectExport{
		{
			Meta: &objstore.ObjectMeta{ID: oldA, Type: objstore.TypeImage, MimeType: "image/png", OrigName: "a.png", Size: 5},
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("AAAAA")), nil },
		},
		{
			Meta: &objstore.ObjectMeta{ID: oldB, Type: objstore.TypeReport, MimeType: "text/markdown", OrigName: "b.md", Size: 3},
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("BBB")), nil },
		},
	}
	manifest := &Manifest{
		SchemaVersion:        SchemaVersion,
		ExportedAt:           time.Now().UTC(),
		ExportedByAppVersion: "test",
		Session: SessionMeta{
			OriginalID: "sess-orig",
			Title:      "My Investigation",
			Private:    true,
		},
	}
	size, err := ExportSession(srcDir, bundlePath, manifest, objects)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if size <= 0 {
		t.Errorf("expected non-zero bundle size, got %d", size)
	}

	fake := newFakeObjstore()
	res, err := ImportSession(bundlePath, dstSessionsDir, fake, []string{"My Investigation"})
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if res.NewSessionID == "sess-orig" {
		t.Errorf("imported session ID should be fresh, got %q", res.NewSessionID)
	}
	if res.ObjectCount != 2 {
		t.Errorf("ObjectCount: got %d want 2", res.ObjectCount)
	}
	if res.Manifest.Session.Private != true {
		t.Errorf("private flag lost in roundtrip")
	}

	// Title collision: existingTitles included "My Investigation" so suffix expected.
	importedDir := filepath.Join(dstSessionsDir, res.NewSessionID)
	chat, err := os.ReadFile(filepath.Join(importedDir, "chat.json"))
	if err != nil {
		t.Fatal(err)
	}
	var loaded memory.Session
	if err := json.Unmarshal(chat, &loaded); err != nil {
		t.Fatalf("re-parse imported chat.json: %v", err)
	}
	if loaded.Title != "My Investigation (imported)" {
		t.Errorf("title collision suffix wrong: got %q", loaded.Title)
	}
	if loaded.ID != res.NewSessionID {
		t.Errorf("chat.json id field not rewritten: got %q want %q", loaded.ID, res.NewSessionID)
	}
	if !loaded.Private {
		t.Errorf("private flag missing in chat.json after import")
	}

	// Verify ObjectIDs[] in record have been remapped to the IDs the fake
	// objstore returned (deterministic: 31x'1' + 'a' for index 0, 'b' for 1).
	want0 := strings.Repeat("1", 31) + "a"
	want1 := strings.Repeat("1", 31) + "b"
	if loaded.Records[0].ObjectIDs[0] != want0 || loaded.Records[0].ObjectIDs[1] != want1 {
		t.Errorf("Record.ObjectIDs not remapped: got %v want [%s %s]", loaded.Records[0].ObjectIDs, want0, want1)
	}
	// Verify Content was swept.
	if !strings.Contains(loaded.Records[0].Content, want0) || !strings.Contains(loaded.Records[0].Content, want1) {
		t.Errorf("Content not rewritten: got %q", loaded.Records[0].Content)
	}
	if strings.Contains(loaded.Records[0].Content, oldA) || strings.Contains(loaded.Records[0].Content, oldB) {
		t.Errorf("Content still contains old IDs: %q", loaded.Records[0].Content)
	}

	// Verify summaries.json was rewritten.
	summariesData, err := os.ReadFile(filepath.Join(importedDir, "summaries.json"))
	if err != nil {
		t.Fatal(err)
	}
	var loadedCache contextbuild.SummaryCache
	if err := json.Unmarshal(summariesData, &loadedCache); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loadedCache.Entries[0].Summary, want0) {
		t.Errorf("summary not rewritten: got %q", loadedCache.Entries[0].Summary)
	}

	// Verify objstore now holds both blobs under new IDs with correct content.
	if string(fake.stored[want0]) != "AAAAA" {
		t.Errorf("blob A content wrong: got %q", string(fake.stored[want0]))
	}
	if string(fake.stored[want1]) != "BBB" {
		t.Errorf("blob B content wrong: got %q", string(fake.stored[want1]))
	}

	// work/subdir/result.txt must survive the roundtrip.
	got, err := os.ReadFile(filepath.Join(importedDir, "work", "subdir", "result.txt"))
	if err != nil {
		t.Fatalf("work file missing after import: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("work file content lost: got %q", string(got))
	}
}

func TestRoundtrip_NoObjects(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src", "no-obj")
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatal(err)
	}
	session := memory.Session{
		ID:      "no-obj",
		Title:   "Plain",
		Records: []memory.Record{{Role: "user", Content: "hello"}},
	}
	d, _ := json.Marshal(&session)
	if err := os.WriteFile(filepath.Join(srcDir, "chat.json"), d, 0600); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(tmp, "no-obj.shellagent")
	manifest := &Manifest{
		SchemaVersion: SchemaVersion,
		ExportedAt:    time.Now().UTC(),
		Session:       SessionMeta{OriginalID: "no-obj", Title: "Plain"},
	}
	if _, err := ExportSession(srcDir, bundle, manifest, nil); err != nil {
		t.Fatalf("ExportSession: %v", err)
	}

	dst := filepath.Join(tmp, "dst")
	res, err := ImportSession(bundle, dst, newFakeObjstore(), nil)
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if res.ObjectCount != 0 {
		t.Errorf("ObjectCount: got %d want 0", res.ObjectCount)
	}
}

func TestImport_RejectsBadVersion(t *testing.T) {
	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "bad.shellagent")
	buf := makeBundleWithManifest(t, []byte(`{"schema_version":99,"session":{}}`))
	if err := os.WriteFile(bundle, buf, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ImportSession(bundle, tmp, newFakeObjstore(), nil)
	if !errors.Is(err, ErrUnsupportedSchemaVersion) {
		t.Errorf("expected ErrUnsupportedSchemaVersion, got %v", err)
	}
}

func TestImport_RejectsZipSlip(t *testing.T) {
	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "slip.shellagent")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	mf := `{"schema_version":1,"session":{"original_id":"x","title":"x","private":false}}`
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(mf))
	w, _ = zw.Create("chat.json")
	_, _ = w.Write([]byte(`{"id":"x","title":"x","records":[]}`))
	// The slip entry.
	w, _ = zw.Create("../escape.txt")
	_, _ = w.Write([]byte("nope"))
	_ = zw.Close()
	if err := os.WriteFile(bundle, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ImportSession(bundle, tmp, newFakeObjstore(), nil)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Errorf("expected unsafe-path rejection, got %v", err)
	}
}

func TestImport_RejectsMissingChatJSON(t *testing.T) {
	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "no-chat.shellagent")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(`{"schema_version":1,"session":{"original_id":"x","title":"x","private":false}}`))
	_ = zw.Close()
	_ = os.WriteFile(bundle, b.Bytes(), 0600)
	_, err := ImportSession(bundle, tmp, newFakeObjstore(), nil)
	if err == nil || !strings.Contains(err.Error(), "chat.json") {
		t.Errorf("expected chat.json error, got %v", err)
	}
}

func TestImport_RejectsObjectIndexBlobMismatch(t *testing.T) {
	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "mismatch.shellagent")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(`{"schema_version":1,"session":{"original_id":"x","title":"x","private":false}}`))
	w, _ = zw.Create("chat.json")
	_, _ = w.Write([]byte(`{"id":"x","title":"x","records":[]}`))
	// Index references an ID that has no blob.
	w, _ = zw.Create("objects/index.json")
	_, _ = w.Write([]byte(`[{"id":"abc","type":"image","mime_type":"image/png","size":1}]`))
	_ = zw.Close()
	_ = os.WriteFile(bundle, b.Bytes(), 0600)
	_, err := ImportSession(bundle, tmp, newFakeObjstore(), nil)
	if err == nil || !strings.Contains(err.Error(), "out of sync") {
		t.Errorf("expected out-of-sync rejection, got %v", err)
	}
}

func TestImport_RejectsOrphanBlob(t *testing.T) {
	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "orphan.shellagent")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(`{"schema_version":1,"session":{"original_id":"x","title":"x","private":false}}`))
	w, _ = zw.Create("chat.json")
	_, _ = w.Write([]byte(`{"id":"x","title":"x","records":[]}`))
	// Blob with no index entry.
	w, _ = zw.Create("objects/data/orphan-id")
	_, _ = w.Write([]byte("data"))
	_ = zw.Close()
	_ = os.WriteFile(bundle, b.Bytes(), 0600)
	_, err := ImportSession(bundle, tmp, newFakeObjstore(), nil)
	if err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Errorf("expected orphan blob rejection, got %v", err)
	}
}

func TestImport_TitleCollisionSecondaryNumbering(t *testing.T) {
	got := resolveTitle("Investigation", []string{"Investigation", "Investigation (imported)"})
	if got != "Investigation (imported 2)" {
		t.Errorf("expected '(imported 2)' suffix, got %q", got)
	}
}

func TestImport_RollbackOnObjstoreError(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src", "sess")
	const obj0 = "00112233445566778899aabbccddeeff"
	const obj1 = "ffeeddccbbaa99887766554433221100"
	makeFixtureSession(t, filepath.Join(tmp, "src"), "sess", "T", false, []string{obj0, obj1})

	objects := []ObjectExport{
		{Meta: &objstore.ObjectMeta{ID: obj0, Type: objstore.TypeImage, MimeType: "image/png", Size: 1},
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("X")), nil }},
		{Meta: &objstore.ObjectMeta{ID: obj1, Type: objstore.TypeImage, MimeType: "image/png", Size: 1},
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("Y")), nil }},
	}
	bundle := filepath.Join(tmp, "rb.shellagent")
	if _, err := ExportSession(srcDir, bundle, &Manifest{
		SchemaVersion: SchemaVersion,
		Session:       SessionMeta{OriginalID: "sess", Title: "T"},
	}, objects); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(tmp, "dst")
	fake := newFakeObjstore()
	fake.failOn = 1 // succeed on object 0, fail on object 1
	_, err := ImportSession(bundle, dst, fake, nil)
	if err == nil {
		t.Fatal("expected error from objstore failure")
	}
	if len(fake.stored) != 0 {
		t.Errorf("expected rollback to delete the object that succeeded, still have %d", len(fake.stored))
	}
	// The new session dir should also have been removed.
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("expected dst to be empty after rollback, got %d entries", len(entries))
	}
}

// makeBundleWithManifest writes a minimal zip whose manifest.json
// has the given raw bytes plus a stub chat.json. Used to construct
// targeted invalid-manifest test inputs.
func makeBundleWithManifest(t *testing.T, manifestBytes []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write(manifestBytes)
	w, _ = zw.Create("chat.json")
	_, _ = w.Write([]byte(`{"id":"x","title":"x","records":[]}`))
	_ = zw.Close()
	return b.Bytes()
}

func TestSafeBundleFilename(t *testing.T) {
	when := time.Date(2026, 5, 7, 12, 34, 56, 0, time.UTC)
	cases := []struct {
		name      string
		title, id string
		want      string
	}{
		{"normal title", "My Session", "sess-1", "My Session-20260507-123456.shellagent"},
		{"forbidden chars stripped", "a/b\\c:d*e?f", "sess-1", "a_b_c_d_e_f-20260507-123456.shellagent"},
		{"empty title falls back", "", "abcdef0123", "session-abcdef01-20260507-123456.shellagent"},
		{"control chars become _", "hi\x01there", "sess-1", "hi_there-20260507-123456.shellagent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SafeBundleFilename(tc.title, tc.id, when)
			if got != tc.want {
				t.Errorf("SafeBundleFilename:\n  got:  %q\n  want: %q", got, tc.want)
			}
		})
	}
}

package objstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestObjectMeta_Migration_LegacyIndexLoadsWithoutLinesTokens
// pins backward compat for index.json files written by v0.4.x.
// Such files have no `lines` / `tokens` keys at all; on load the
// fields must default to zero (before the backfill loop touches
// them — we use a non-text MIME so backfill does NOT fire).
func TestObjectMeta_Migration_LegacyIndexLoadsWithoutLinesTokens(t *testing.T) {
	dir := t.TempDir()
	indexJSON := `{
  "deadbeefdeadbeefdeadbeefdeadbeef": {
    "id": "deadbeefdeadbeefdeadbeefdeadbeef",
    "type": "image",
    "mime_type": "image/png",
    "orig_name": "legacy.png",
    "created_at": "2026-01-01T00:00:00Z",
    "size": 1024
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(indexJSON), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewStoreAt(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	meta, ok := s.Get("deadbeefdeadbeefdeadbeefdeadbeef")
	if !ok {
		t.Fatal("legacy meta not loaded")
	}
	if meta.Lines != 0 {
		t.Errorf("Lines = %d, want 0 (legacy image, no backfill)", meta.Lines)
	}
	if meta.Tokens != 0 {
		t.Errorf("Tokens = %d, want 0 (legacy image, no backfill)", meta.Tokens)
	}
}

// TestObjstoreLoad_BackfillsLegacyTextObjectMetadata pins the
// self-heal property: a pre-v0.5 TypeReport written through the
// old Store() (no Lines/Tokens computed) gets its metadata filled
// on Load and the updated index is persisted to disk.
func TestObjstoreLoad_BackfillsLegacyTextObjectMetadata(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Plant a data file with known content.
	body := "# Title\n\nHello world.\nSecond line.\n"
	objID := "cafebabecafebabecafebabecafebabe"
	if err := os.WriteFile(filepath.Join(dataDir, objID), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	// Plant a legacy-shaped index.json: Lines/Tokens absent.
	indexJSON := `{
  "` + objID + `": {
    "id": "` + objID + `",
    "type": "report",
    "mime_type": "text/markdown",
    "orig_name": "legacy.md",
    "created_at": "2026-01-01T00:00:00Z",
    "session_id": "sess-old",
    "size": ` + intStr(len(body)) + `
  }
}`
	indexPath := filepath.Join(dir, "index.json")
	if err := os.WriteFile(indexPath, []byte(indexJSON), 0600); err != nil {
		t.Fatal(err)
	}

	s := NewStoreAt(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	meta, ok := s.Get(objID)
	if !ok {
		t.Fatal("legacy meta not loaded")
	}

	// In-memory: filled.
	wantLines := strings.Count(body, "\n") + 1
	if meta.Lines != wantLines {
		t.Errorf("in-memory Lines = %d, want %d", meta.Lines, wantLines)
	}
	wantTokens := memory.EstimateTokens(body)
	if meta.Tokens != wantTokens {
		t.Errorf("in-memory Tokens = %d, want %d", meta.Tokens, wantTokens)
	}

	// On-disk: re-saved with the filled values.
	persisted, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("re-read index: %v", err)
	}
	var idx map[string]*ObjectMeta
	if err := json.Unmarshal(persisted, &idx); err != nil {
		t.Fatalf("parse re-saved index: %v", err)
	}
	got, ok := idx[objID]
	if !ok {
		t.Fatal("object missing from re-saved index")
	}
	if got.Lines != wantLines {
		t.Errorf("on-disk Lines = %d, want %d", got.Lines, wantLines)
	}
	if got.Tokens != wantTokens {
		t.Errorf("on-disk Tokens = %d, want %d", got.Tokens, wantTokens)
	}
}

// TestObjstoreLoad_DoesNotBackfillImagesOrBinaries ensures the
// backfill predicate is gated on text/* MIME. An image with
// Lines=0 must stay at Lines=0 (we never want to read pixel bytes
// and run them through EstimateTokens).
func TestObjstoreLoad_DoesNotBackfillImagesOrBinaries(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}

	imgID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := os.WriteFile(filepath.Join(dataDir, imgID), []byte("fakepng\n"), 0600); err != nil {
		t.Fatal(err)
	}
	jsonBlobID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := os.WriteFile(filepath.Join(dataDir, jsonBlobID), []byte(`{"k":"v"}`), 0600); err != nil {
		t.Fatal(err)
	}

	indexJSON := `{
  "` + imgID + `": {"id": "` + imgID + `", "type": "image", "mime_type": "image/png", "orig_name": "x.png", "created_at": "2026-01-01T00:00:00Z", "size": 8},
  "` + jsonBlobID + `": {"id": "` + jsonBlobID + `", "type": "blob", "mime_type": "application/json", "orig_name": "x.json", "created_at": "2026-01-01T00:00:00Z", "size": 9}
}`
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(indexJSON), 0600); err != nil {
		t.Fatal(err)
	}

	s := NewStoreAt(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, id := range []string{imgID, jsonBlobID} {
		meta, _ := s.Get(id)
		if meta.Lines != 0 {
			t.Errorf("%s Lines = %d, want 0 (non-text MIME, must not backfill)", id, meta.Lines)
		}
		if meta.Tokens != 0 {
			t.Errorf("%s Tokens = %d, want 0", id, meta.Tokens)
		}
	}
}

// TestObjstoreLoad_ToleratesMissingDataFile pins the
// failure-mode contract: an index entry that points at a missing
// data file must NOT fail the whole Load — the entry stays at
// Lines=0, the rest of the index keeps working.
func TestObjstoreLoad_ToleratesMissingDataFile(t *testing.T) {
	dir := t.TempDir()
	// Deliberately do NOT create the data file.
	objID := "deadbeef" + "deadbeef" + "deadbeef" + "deadbeef"
	indexJSON := `{
  "` + objID + `": {"id": "` + objID + `", "type": "report", "mime_type": "text/markdown", "orig_name": "ghost.md", "created_at": "2026-01-01T00:00:00Z", "size": 100}
}`
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(indexJSON), 0600); err != nil {
		t.Fatal(err)
	}

	s := NewStoreAt(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load must tolerate missing data file, got: %v", err)
	}
	meta, ok := s.Get(objID)
	if !ok {
		t.Fatal("entry dropped during Load (should have been preserved)")
	}
	if meta.Lines != 0 {
		t.Errorf("Lines = %d, want 0 (data file missing, backfill skipped)", meta.Lines)
	}
}

// TestStore_AutoFillsLinesAndTokensForTextMIME pins the forward
// direction: new writes with text/* MIME get Lines/Tokens at
// Store() time, no Load round-trip required.
func TestStore_AutoFillsLinesAndTokensForTextMIME(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	body := "line1\nline2\nline3\n"
	meta, err := s.Store(strings.NewReader(body), TypeBlob, "text/csv", "data.csv", "sess-1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if meta.Lines != strings.Count(body, "\n")+1 {
		t.Errorf("Lines = %d, want %d", meta.Lines, strings.Count(body, "\n")+1)
	}
	if meta.Tokens != memory.EstimateTokens(body) {
		t.Errorf("Tokens = %d, want %d", meta.Tokens, memory.EstimateTokens(body))
	}
}

// TestStore_DoesNotAutoFillForImage ensures the predicate
// gates on MIME, not on Type. Even a TypeImage with a
// (nonsensical) text MIME shouldn't have Lines computed —
// but more practically, a real image/png never does.
func TestStore_DoesNotAutoFillForImage(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	// Pass fake PNG-ish bytes with image MIME.
	meta, err := s.Store(strings.NewReader("\x89PNG\r\n\x1a\nfakedata"), TypeImage, "image/png", "x.png", "")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if meta.Lines != 0 {
		t.Errorf("Lines = %d, want 0 (image MIME, no auto-fill)", meta.Lines)
	}
	if meta.Tokens != 0 {
		t.Errorf("Tokens = %d, want 0", meta.Tokens)
	}
}

// TestStore_TypeReportGetsLinesAndTokens mimics what
// toolCreateReport does: Store(reader, TypeReport,
// "text/markdown", title+".md", sessionID). The auto-fill
// MUST fire so legacy and new reports converge on the same
// metadata shape going forward.
func TestStore_TypeReportGetsLinesAndTokens(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	body := "# Report\n\nFindings:\n- A\n- B\n"
	meta, err := s.Store(strings.NewReader(body), TypeReport, "text/markdown", "Report.md", "sess-1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if meta.Lines == 0 {
		t.Errorf("TypeReport with text/markdown MIME should auto-fill Lines (got 0)")
	}
	if meta.Tokens == 0 {
		t.Errorf("TypeReport with text/markdown MIME should auto-fill Tokens (got 0)")
	}
}

// intStr is a tiny helper to inline ints into JSON literals in
// the test fixtures without pulling in fmt.Sprintf.
func intStr(n int) string {
	// Use a switch for small values; fall through to fmt for larger.
	// In tests we always feed short bodies so this is enough.
	if n < 0 {
		return "0"
	}
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

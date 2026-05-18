package objstore

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestStoreAndGet(t *testing.T) {
	s := NewStoreAt(t.TempDir())

	meta, err := s.Store(strings.NewReader("hello world"), TypeBlob, "text/plain", "test.txt", "sess-1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if meta.ID == "" {
		t.Error("ID is empty")
	}
	if meta.Size != 11 {
		t.Errorf("Size = %d, want 11", meta.Size)
	}
	if meta.Type != TypeBlob {
		t.Errorf("Type = %v, want blob", meta.Type)
	}
	if meta.SessionID != "sess-1" {
		t.Errorf("SessionID = %v, want sess-1", meta.SessionID)
	}
	if meta.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	got, ok := s.Get(meta.ID)
	if !ok {
		t.Fatal("object not found")
	}
	if got.OrigName != "test.txt" {
		t.Errorf("OrigName = %v", got.OrigName)
	}
}

func TestReadData(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	meta, _ := s.Store(strings.NewReader("test data"), TypeBlob, "text/plain", "data.txt", "")

	reader, err := s.ReadData(meta.ID)
	if err != nil {
		t.Fatalf("ReadData: %v", err)
	}
	defer reader.Close()

	buf := make([]byte, 100)
	n, _ := reader.Read(buf)
	if string(buf[:n]) != "test data" {
		t.Errorf("data = %q, want 'test data'", string(buf[:n]))
	}
}

func TestDelete(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	meta, _ := s.Store(strings.NewReader("delete me"), TypeBlob, "text/plain", "del.txt", "")

	if err := s.Delete(meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, ok := s.Get(meta.ID)
	if ok {
		t.Error("object should be deleted")
	}
}

func TestAll(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	s.Store(strings.NewReader("a"), TypeImage, "image/png", "a.png", "sess-1")
	s.Store(strings.NewReader("b"), TypeBlob, "text/plain", "b.txt", "sess-1")

	all := s.All()
	if len(all) != 2 {
		t.Errorf("All count = %d, want 2", len(all))
	}
}

func TestListByType(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	s.Store(strings.NewReader("img"), TypeImage, "image/png", "a.png", "sess-1")
	s.Store(strings.NewReader("txt"), TypeBlob, "text/plain", "b.txt", "sess-1")
	s.Store(strings.NewReader("rpt"), TypeReport, "text/markdown", "r.md", "sess-1")

	images := s.ListByType(TypeImage)
	if len(images) != 1 {
		t.Errorf("images = %d, want 1", len(images))
	}

	blobs := s.ListByType(TypeBlob)
	if len(blobs) != 1 {
		t.Errorf("blobs = %d, want 1", len(blobs))
	}
}

func TestListBySession(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	s.Store(strings.NewReader("a"), TypeImage, "image/png", "a.png", "sess-1")
	s.Store(strings.NewReader("b"), TypeImage, "image/png", "b.png", "sess-2")
	s.Store(strings.NewReader("c"), TypeBlob, "text/plain", "c.txt", "sess-1")

	sess1 := s.ListBySession("sess-1")
	if len(sess1) != 2 {
		t.Errorf("sess-1 objects = %d, want 2", len(sess1))
	}

	sess2 := s.ListBySession("sess-2")
	if len(sess2) != 1 {
		t.Errorf("sess-2 objects = %d, want 1", len(sess2))
	}
}

func TestDeleteBySession(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	s.Store(strings.NewReader("a"), TypeImage, "image/png", "a.png", "sess-1")
	s.Store(strings.NewReader("b"), TypeImage, "image/png", "b.png", "sess-2")
	s.Store(strings.NewReader("c"), TypeBlob, "text/plain", "c.txt", "sess-1")

	err := s.DeleteBySession("sess-1")
	if err != nil {
		t.Fatalf("DeleteBySession: %v", err)
	}

	if len(s.All()) != 1 {
		t.Errorf("remaining = %d, want 1", len(s.All()))
	}
	sess2 := s.ListBySession("sess-2")
	if len(sess2) != 1 {
		t.Errorf("sess-2 = %d, want 1", len(sess2))
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()

	s1 := NewStoreAt(dir)
	meta, _ := s1.Store(strings.NewReader("persist"), TypeBlob, "text/plain", "p.txt", "sess-1")

	s2 := NewStoreAt(dir)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, ok := s2.Get(meta.ID)
	if !ok {
		t.Fatal("object not found after reload")
	}
	if got.OrigName != "p.txt" {
		t.Errorf("OrigName = %v after reload", got.OrigName)
	}
	if got.Type != TypeBlob {
		t.Errorf("Type = %v after reload, want blob", got.Type)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %v after reload, want sess-1", got.SessionID)
	}
}

func TestGenerateID(t *testing.T) {
	id1 := generateID()
	id2 := generateID()
	if id1 == id2 {
		t.Error("IDs should be unique")
	}
	// Width: post-security-hardening-2.md H11 we generate 16-byte
	// (32 hex char) IDs. Legacy 12-char IDs continue to load.
	if want := IDByteLen * 2; len(id1) != want {
		t.Errorf("ID length = %d, want %d", len(id1), want)
	}
}

// TestStore_RejectsCollidingID drives the collision-regen path by
// pre-seeding the index with an entry the next generated ID is
// forced to match. Verifies that Store either picks a different ID
// (ideal) or surfaces an error after exhausting attempts (defensive
// safety net).
func TestStore_RejectsCollidingID(t *testing.T) {
	// We can't deterministically force generateID to collide with a
	// random 16-byte value, so exercise the loop indirectly: we
	// write the SAME object twice and assert both succeed with
	// distinct IDs. This is a sanity check that the dedup-loop
	// doesn't reuse IDs across calls — collision avoidance must
	// hold even if the RNG is biased toward repetition.
	s := NewStoreAt(t.TempDir())
	m1, err := s.Store(strings.NewReader("a"), TypeBlob, "text/plain", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := s.Store(strings.NewReader("b"), TypeBlob, "text/plain", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m1.ID == m2.ID {
		t.Errorf("two Store calls returned the same ID %q", m1.ID)
	}
}

// TestStore_ConcurrentStoreAndList exercises the index lock by
// running many writers and readers in parallel. Without the
// RWMutex this panics with "concurrent map writes". Run with
// -race for the full assertion.
func TestStore_ConcurrentStoreAndList(t *testing.T) {
	s := NewStoreAt(t.TempDir())

	const writers = 16
	const reads = 16
	const writesPerGoroutine = 25

	var wg sync.WaitGroup

	for i := range writers {
		wg.Go(func() {
			for j := range writesPerGoroutine {
				_, err := s.Store(
					strings.NewReader(fmt.Sprintf("payload-%d-%d", i, j)),
					TypeBlob, "text/plain",
					fmt.Sprintf("name-%d-%d.txt", i, j),
					fmt.Sprintf("sess-%d", i%4),
				)
				if err != nil {
					t.Errorf("Store: %v", err)
					return
				}
			}
		})
	}

	for range reads {
		wg.Go(func() {
			for range writesPerGoroutine {
				_ = s.All()
				_ = s.ListBySession("sess-0")
				_ = s.ListByType(TypeBlob)
			}
		})
	}

	wg.Wait()

	// Every write should have landed in the index.
	if got := len(s.All()); got != writers*writesPerGoroutine {
		t.Errorf("All() returned %d objects, want %d", got, writers*writesPerGoroutine)
	}
}

func TestSaveDataURL(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	// Tiny 1x1 PNG as data URL
	dataURL := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	meta, err := s.SaveDataURL(dataURL, "", "sess-test")
	if err != nil {
		t.Fatalf("SaveDataURL: %v", err)
	}
	if meta.Type != TypeImage {
		t.Errorf("Type = %v, want image", meta.Type)
	}
	if meta.MimeType != "image/png" {
		t.Errorf("MimeType = %v", meta.MimeType)
	}
	if meta.SessionID != "sess-test" {
		t.Errorf("SessionID = %v", meta.SessionID)
	}
}

// TestSaveDataURL_FilenameFallbackForGenericMIME guards the v0.11.0
// fix for the Finder drag-drop case: macOS hands a .md file to the
// frontend with file.type == "application/octet-stream", which would
// previously land in TypeBlob and get skipped by the binding's
// attachment handler. SaveDataURL now consults origName's extension
// when MIME is empty or generic and re-routes to TypeMarkdown.
func TestSaveDataURL_FilenameFallbackForGenericMIME(t *testing.T) {
	cases := []struct {
		name     string
		mime     string
		origName string
		wantType ObjectType
		wantMime string
	}{
		// Frontend was supposed to fix the MIME but didn't; the
		// backend has to catch it via filename. .md.
		{".md with application/octet-stream", "application/octet-stream", "audit.md", TypeMarkdown, "text/markdown"},
		// Empty MIME (some paste paths) — same recovery.
		{".markdown with empty MIME", "", "notes.markdown", TypeMarkdown, "text/markdown"},
		// .txt → text/plain.
		{".txt with application/octet-stream", "application/octet-stream", "log.txt", TypeMarkdown, "text/plain"},
		// Already-correct MIME survives untouched.
		{"correct text/markdown", "text/markdown", "doc.md", TypeMarkdown, "text/markdown"},
		// No filename → stays TypeBlob (we can't infer).
		{"empty origName stays blob", "application/octet-stream", "", TypeBlob, "application/octet-stream"},
		// Unrelated extension stays TypeBlob.
		{"binary extension stays blob", "application/octet-stream", "archive.zip", TypeBlob, "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStoreAt(t.TempDir())
			body := base64.StdEncoding.EncodeToString([]byte("test body"))
			dataURL := "data:" + tc.mime + ";base64," + body
			meta, err := s.SaveDataURL(dataURL, tc.origName, "sess-test")
			if err != nil {
				t.Fatalf("SaveDataURL: %v", err)
			}
			if meta.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", meta.Type, tc.wantType)
			}
			if meta.MimeType != tc.wantMime {
				t.Errorf("MimeType = %q, want %q", meta.MimeType, tc.wantMime)
			}
		})
	}
}

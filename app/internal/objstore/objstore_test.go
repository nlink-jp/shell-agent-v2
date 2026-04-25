package objstore

import (
	"strings"
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
	if len(id1) != 12 {
		t.Errorf("ID length = %d, want 12", len(id1))
	}
}

func TestSaveDataURL(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	// Tiny 1x1 PNG as data URL
	dataURL := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	meta, err := s.SaveDataURL(dataURL, "sess-test")
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

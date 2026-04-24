package objstore

import (
	"strings"
	"testing"
)

func TestStoreAndGet(t *testing.T) {
	s := NewStoreAt(t.TempDir())

	meta, err := s.Store(strings.NewReader("hello world"), "text/plain", "test.txt")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if meta.ID == "" {
		t.Error("ID is empty")
	}
	if meta.Size != 11 {
		t.Errorf("Size = %d, want 11", meta.Size)
	}
	if meta.MimeType != "text/plain" {
		t.Errorf("MimeType = %v", meta.MimeType)
	}

	// Get
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
	meta, _ := s.Store(strings.NewReader("test data"), "text/plain", "data.txt")

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
	meta, _ := s.Store(strings.NewReader("delete me"), "text/plain", "del.txt")

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
	s.Store(strings.NewReader("a"), "text/plain", "a.txt")
	s.Store(strings.NewReader("b"), "text/plain", "b.txt")

	all := s.All()
	if len(all) != 2 {
		t.Errorf("All count = %d, want 2", len(all))
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()

	// Store
	s1 := NewStoreAt(dir)
	meta, _ := s1.Store(strings.NewReader("persist"), "text/plain", "p.txt")

	// Reload
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

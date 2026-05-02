package atomicio

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteFileAtomic_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := WriteFileAtomic(path, []byte("hello"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("contents = %q, want hello", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("mode = %v, want 0600", mode)
	}
}

func TestWriteFileAtomic_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("contents = %q, want new", got)
	}
}

// TestWriteFileAtomic_LeavesNoTempOnSuccess verifies the tempfile is
// renamed away rather than left behind cluttering the directory.
func TestWriteFileAtomic_LeavesNoTempOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := WriteFileAtomic(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir entries = %v, want exactly [out.json]", names)
	}
}

// TestWriteFileAtomic_PreviousFileReadableUnderLoad pins the
// crash-safety claim: under concurrent writes a reader pulling the
// file always sees a complete previous-or-new contents, never empty
// or torn (security-hardening-2.md C4 / H10).
func TestWriteFileAtomic_PreviousFileReadableUnderLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	// Seed an initial well-formed file.
	if err := WriteFileAtomic(path, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: alternate between two valid contents until reader signals stop.
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			payload := []byte("BBBB")
			if i%2 == 0 {
				payload = []byte("CCCC")
			}
			if err := WriteFileAtomic(path, payload, 0600); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	})

	// Reader: confirm we never observe empty or partial contents,
	// then signal the writer to exit before joining.
	for range 1000 {
		b, err := os.ReadFile(path)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("read: %v", err)
		}
		s := string(b)
		if s != "AAAA" && s != "BBBB" && s != "CCCC" {
			close(stop)
			wg.Wait()
			t.Fatalf("torn read: %q (len=%d)", s, len(b))
		}
	}
	close(stop)
	wg.Wait()
}

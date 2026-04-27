package bundled

import (
	"os"
	"path/filepath"
	"testing"
)

func TestList_HasExpectedTools(t *testing.T) {
	names := List()
	want := map[string]bool{
		"file-info.sh":    false,
		"preview-file.sh": false,
		"list-files.sh":   false,
		"weather.sh":      false,
		"get-location.sh": false,
		"write-note.sh":   false,
	}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("bundled tool list missing %q", n)
		}
	}
}

func TestInstall_FreshDir(t *testing.T) {
	dir := t.TempDir()
	installed, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(installed) == 0 {
		t.Fatal("expected at least one file installed")
	}
	for _, name := range installed {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("installed file %s missing: %v", name, err)
			continue
		}
		if info.Mode()&0100 == 0 {
			t.Errorf("installed file %s is not executable (mode %v)", name, info.Mode())
		}
	}
}

func TestInstall_PreservesUserEdits(t *testing.T) {
	dir := t.TempDir()
	preExisting := filepath.Join(dir, "file-info.sh")
	customContent := []byte("#!/bin/bash\n# user-edited stub\n")
	if err := os.WriteFile(preExisting, customContent, 0700); err != nil {
		t.Fatal(err)
	}

	installed, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, n := range installed {
		if n == "file-info.sh" {
			t.Error("Install must not overwrite an existing file")
		}
	}
	got, err := os.ReadFile(preExisting)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(customContent) {
		t.Errorf("user content was modified; want %q got %q", customContent, got)
	}
}

func TestInstall_SkipsExamplesDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "examples")); !os.IsNotExist(err) {
		t.Errorf("examples/ should not be auto-installed; stat err=%v", err)
	}
}

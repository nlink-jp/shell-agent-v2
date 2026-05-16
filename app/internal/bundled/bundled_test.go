package bundled

import (
	"os"
	"path/filepath"
	"strings"
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

// TestRepoRootExamples_HaveToolHeader scans the optional shell-tool
// examples shipped at examples/shell_tools/ in the repository root
// (out-of-binary on purpose; see package doc). Each script must
// carry the @tool: header that the agent's tool-loader recognises,
// otherwise a user who copies it into <dataDir>/tools/ would end up
// with a script the registry can't surface.
//
// Skipped when run outside a repo checkout (the binary itself
// doesn't carry these files).
func TestRepoRootExamples_HaveToolHeader(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// The test file lives at app/internal/bundled/; the repo root
	// is three levels up.
	exDir := filepath.Join(wd, "..", "..", "..", "examples", "shell_tools")
	if _, err := os.Stat(exDir); err != nil {
		t.Skipf("examples/shell_tools/ not found at %s — skipping (running outside repo?)", exDir)
	}
	entries, err := os.ReadDir(exDir)
	if err != nil {
		t.Fatalf("read examples dir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		count++
		data, err := os.ReadFile(filepath.Join(exDir, e.Name()))
		if err != nil {
			t.Errorf("read %s: %v", e.Name(), err)
			continue
		}
		if !strings.Contains(string(data), "@tool:") {
			t.Errorf("%s: missing @tool: header", e.Name())
		}
	}
	if count == 0 {
		t.Error("examples/shell_tools/ contained no .sh files — expected at least the four bundled examples")
	}
}

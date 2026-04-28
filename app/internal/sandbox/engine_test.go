package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// makeFakeBin creates an executable shell stub at <dir>/<name> that
// simply echoes a unique marker so we can detect that the right
// binary was picked up by Detect.
func makeFakeBin(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	src := "#!/bin/sh\necho " + name + " stub\n"
	if err := os.WriteFile(path, []byte(src), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDefaults(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Engine != "auto" {
		t.Errorf("Engine = %q, want 'auto'", c.Engine)
	}
	if c.Image != "python:3.12-slim" {
		t.Errorf("Image = %q", c.Image)
	}
	if c.TimeoutSeconds != 60 {
		t.Errorf("TimeoutSeconds = %d, want 60", c.TimeoutSeconds)
	}
	if c.CPULimit != "2" || c.MemoryLimit != "1g" {
		t.Errorf("limits = %+v", c)
	}
}

func TestApplyDefaults_KeepsExplicit(t *testing.T) {
	c := &Config{Engine: "docker", Image: "myimage", TimeoutSeconds: 5, CPULimit: "1", MemoryLimit: "512m"}
	c.applyDefaults()
	if c.Engine != "docker" || c.Image != "myimage" || c.TimeoutSeconds != 5 {
		t.Errorf("explicit fields overwritten: %+v", c)
	}
}

func TestResolveEngine_AutoPrefersPodman(t *testing.T) {
	dir := t.TempDir()
	makeFakeBin(t, dir, "podman")
	makeFakeBin(t, dir, "docker")
	t.Setenv("PATH", dir)
	bin, ok := resolveEngine("auto")
	if !ok {
		t.Fatal("expected auto-resolution to succeed")
	}
	if bin != "podman" {
		t.Errorf("auto picked %q, want 'podman'", bin)
	}
}

func TestResolveEngine_AutoFallsBackToDocker(t *testing.T) {
	dir := t.TempDir()
	makeFakeBin(t, dir, "docker")
	t.Setenv("PATH", dir)
	bin, ok := resolveEngine("auto")
	if !ok || bin != "docker" {
		t.Errorf("auto fallback: bin=%q ok=%v, want docker/true", bin, ok)
	}
}

func TestResolveEngine_AutoNoneAvailable(t *testing.T) {
	dir := t.TempDir() // empty
	t.Setenv("PATH", dir)
	if _, ok := resolveEngine("auto"); ok {
		t.Error("expected resolution to fail when neither binary present")
	}
}

func TestResolveEngine_ExplicitMissing(t *testing.T) {
	dir := t.TempDir()
	makeFakeBin(t, dir, "podman")
	t.Setenv("PATH", dir)
	if _, ok := resolveEngine("docker"); ok {
		t.Error("explicit 'docker' should fail when only podman is present")
	}
}

func TestNewCLI_FailsWhenNoEngine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if _, err := NewCLI(Config{Engine: "auto", SessionsDir: dir}); err == nil {
		t.Error("expected ErrEngineNotAvailable when no engine on PATH")
	}
}

func TestNewCLI_DetectCachesBinary(t *testing.T) {
	dir := t.TempDir()
	makeFakeBin(t, dir, "podman")
	t.Setenv("PATH", dir)
	e, err := NewCLI(Config{Engine: "auto", SessionsDir: dir})
	if err != nil {
		t.Fatalf("NewCLI: %v", err)
	}
	bin1, _ := e.Detect()
	// Remove podman from PATH; cached lookup should still hold.
	t.Setenv("PATH", t.TempDir())
	bin2, _ := e.Detect()
	if bin1 != "podman" || bin2 != "podman" {
		t.Errorf("Detect did not cache: bin1=%q bin2=%q", bin1, bin2)
	}
}

func TestCLIEngine_WorkDir(t *testing.T) {
	dir := t.TempDir()
	makeFakeBin(t, dir, "podman")
	t.Setenv("PATH", dir)
	root := filepath.Join(t.TempDir(), "sessions")
	e, err := NewCLI(Config{Engine: "podman", SessionsDir: root})
	if err != nil {
		t.Fatal(err)
	}
	got := e.WorkDir("sess-001")
	want := filepath.Join(root, "sess-001", "work")
	if got != want {
		t.Errorf("WorkDir = %q, want %q", got, want)
	}
}

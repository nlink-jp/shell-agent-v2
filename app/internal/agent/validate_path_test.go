package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateBinaryPath_Empty(t *testing.T) {
	if _, err := validateBinaryPath(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestValidateBinaryPath_NotFound(t *testing.T) {
	tmp := t.TempDir()
	if _, err := validateBinaryPath(filepath.Join(tmp, "nope")); err == nil {
		t.Fatal("expected error for missing file")
	} else if !strings.Contains(err.Error(), "binary not found") {
		t.Errorf("error = %v, want 'binary not found'", err)
	}
}

func TestValidateBinaryPath_Directory(t *testing.T) {
	tmp := t.TempDir()
	if _, err := validateBinaryPath(tmp); err == nil {
		t.Fatal("expected error when path is a directory")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %v, want 'not a regular file'", err)
	}
}

func TestValidateBinaryPath_NotExecutable(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "noexec")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateBinaryPath(p); err == nil {
		t.Fatal("expected error for non-executable")
	} else if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("error = %v, want 'not executable'", err)
	}
}

func TestValidateBinaryPath_Valid(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "exec")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	abs, err := validateBinaryPath(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(abs) {
		t.Errorf("returned path is not absolute: %s", abs)
	}
}

func TestValidateBinaryPath_FollowsSymlinkToValidExecutable(t *testing.T) {
	// A path that resolves through a symlink to a real executable should
	// be acceptable — os.Stat follows symlinks, which is the intended
	// behaviour for "is this a real binary I can run".
	tmp := t.TempDir()
	target := filepath.Join(tmp, "real")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlink unsupported")
	}
	if _, err := validateBinaryPath(link); err != nil {
		t.Errorf("symlink to executable should pass: %v", err)
	}
}

func TestValidateBinaryPath_DanglingSymlink(t *testing.T) {
	tmp := t.TempDir()
	link := filepath.Join(tmp, "dangling")
	if err := os.Symlink(filepath.Join(tmp, "missing-target"), link); err != nil {
		t.Skip("symlink unsupported")
	}
	if _, err := validateBinaryPath(link); err == nil {
		t.Fatal("expected error for dangling symlink")
	}
}

func TestValidateProfilePath_Empty(t *testing.T) {
	if _, err := validateProfilePath(""); err == nil {
		t.Fatal("expected error for empty profile")
	}
}

func TestValidateProfilePath_BareNameAllowed(t *testing.T) {
	got, err := validateProfilePath("atlassian")
	if err != nil {
		t.Fatalf("bare name should pass through: %v", err)
	}
	if got != "atlassian" {
		t.Errorf("bare name should pass through unchanged, got %q", got)
	}
}

func TestValidateProfilePath_RejectsControlChars(t *testing.T) {
	for _, bad := range []string{
		"name\nwith newline",
		"name\twith tab",
		"name\x00with null",
		"name\x7fwith del",
	} {
		if _, err := validateProfilePath(bad); err == nil {
			t.Errorf("control char should be rejected in %q", bad)
		} else if !strings.Contains(err.Error(), "control") {
			t.Errorf("error for %q = %v, want 'control'", bad, err)
		}
	}
}

func TestValidateProfilePath_PathFormValidatesFile(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "profile.json")
	if err := os.WriteFile(p, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := validateProfilePath(p)
	if err != nil {
		t.Fatalf("path-form: %v", err)
	}
	if got != p {
		t.Errorf("expected absolute path %s, got %s", p, got)
	}
}

func TestValidateProfilePath_PathFormRejectsMissing(t *testing.T) {
	tmp := t.TempDir()
	if _, err := validateProfilePath(filepath.Join(tmp, "no-such-profile")); err == nil {
		t.Fatal("expected error for missing path-form profile")
	}
}

func TestValidateProfilePath_PathFormRejectsDirectory(t *testing.T) {
	tmp := t.TempDir()
	if _, err := validateProfilePath(tmp); err == nil {
		t.Fatal("expected error when path-form profile is a directory")
	}
}

func TestValidateProfilePath_TildeFormResolves(t *testing.T) {
	// ~/ form must trigger path-validation. Use ~/non-existent to verify
	// the path branch executes (would error as "profile not found"
	// rather than passing through as a bare name).
	if _, err := validateProfilePath("~/this-path-must-not-exist-shell-agent-v2-test"); err == nil {
		t.Fatal("tilde-prefixed path should be validated, not passed through")
	} else if !strings.Contains(err.Error(), "profile not found") && !strings.Contains(err.Error(), "invalid path") {
		t.Errorf("error = %v, want 'profile not found' or 'invalid path'", err)
	}
}

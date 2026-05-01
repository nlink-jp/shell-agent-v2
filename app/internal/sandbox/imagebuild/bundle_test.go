package imagebuild

import (
	"io/fs"
	"strings"
	"testing"
)

// TestBundle_HasExpectedFiles ensures the embedded build
// context contains the Dockerfile and matplotlibrc the
// design relies on. A missing file would silently produce a
// failing podman build later — surface it at compile-test
// time instead.
func TestBundle_HasExpectedFiles(t *testing.T) {
	wantFiles := []string{
		"bundle/Dockerfile",
		"bundle/matplotlibrc",
	}
	for _, p := range wantFiles {
		data, err := Bundle.ReadFile(p)
		if err != nil {
			t.Errorf("missing %q: %v", p, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%q is empty", p)
		}
	}
}

func TestBundle_DockerfileContainsCJKFonts(t *testing.T) {
	data, err := Bundle.ReadFile("bundle/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "fonts-noto-cjk") {
		t.Errorf("Dockerfile does not install fonts-noto-cjk; matplotlib will produce mojibake")
	}
}

func TestBundle_MatplotlibrcReferencesCJKFont(t *testing.T) {
	data, err := Bundle.ReadFile("bundle/matplotlibrc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Noto Sans CJK JP") {
		t.Errorf("matplotlibrc does not include Noto Sans CJK JP in the font fallback chain")
	}
}

func TestCanonicalTag_IncludesBundleVersion(t *testing.T) {
	if !strings.HasSuffix(CanonicalTag, ":"+BundleVersion) {
		t.Errorf("CanonicalTag = %q, want suffix :%s", CanonicalTag, BundleVersion)
	}
	if !strings.HasPrefix(CanonicalTag, "shell-agent-v2-sandbox:") {
		t.Errorf("CanonicalTag = %q, want shell-agent-v2-sandbox: prefix", CanonicalTag)
	}
}

// TestBundle_WalksWithoutError catches breakage in the
// directory layout (e.g. an entry the embed.FS can't
// enumerate).
func TestBundle_WalksWithoutError(t *testing.T) {
	count := 0
	err := fs.WalkDir(Bundle, "bundle", func(_ string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if count < 2 {
		t.Errorf("walked %d entries, want at least 2 (Dockerfile + matplotlibrc)", count)
	}
}

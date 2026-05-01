package imagebuild

import (
	"strings"
	"testing"
)

func TestRecommendedDockerfile_HasCJKFontInstall(t *testing.T) {
	if !strings.Contains(RecommendedDockerfile, "fonts-noto-cjk") {
		t.Errorf("recommended Dockerfile does not install fonts-noto-cjk; matplotlib will produce mojibake")
	}
}

func TestRecommendedDockerfile_HasMatplotlibRcWithCJKFont(t *testing.T) {
	if !strings.Contains(RecommendedDockerfile, "Noto Sans CJK JP") {
		t.Errorf("recommended Dockerfile does not configure matplotlibrc with Noto Sans CJK JP")
	}
	if !strings.Contains(RecommendedDockerfile, "MATPLOTLIBRC") {
		t.Errorf("recommended Dockerfile does not set MATPLOTLIBRC env var")
	}
}

func TestTagFor_StableForSameInput(t *testing.T) {
	a := TagFor("FROM scratch\n")
	b := TagFor("FROM scratch\n")
	if a != b {
		t.Errorf("TagFor not deterministic: %q vs %q", a, b)
	}
}

func TestTagFor_DiffersOnEdit(t *testing.T) {
	a := TagFor("FROM scratch\n")
	b := TagFor("FROM scratch\n# extra\n")
	if a == b {
		t.Errorf("TagFor returned same tag for different bodies: %q", a)
	}
}

func TestTagFor_HasPrefixAndShortHash(t *testing.T) {
	tag := TagFor("FROM scratch\n")
	if !strings.HasPrefix(tag, TagPrefix+":") {
		t.Errorf("tag %q missing prefix %q", tag, TagPrefix+":")
	}
	suffix := strings.TrimPrefix(tag, TagPrefix+":")
	if len(suffix) != 12 {
		t.Errorf("tag suffix %q is not 12 hex chars", suffix)
	}
}

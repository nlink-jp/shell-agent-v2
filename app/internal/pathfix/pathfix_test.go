package pathfix

import (
	"strings"
	"testing"
)

func TestCandidates_Order(t *testing.T) {
	got := Candidates("/Users/x")
	want := []string{"/opt/homebrew/bin", "/usr/local/bin", "/Users/x/bin", "/Users/x/go/bin"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCandidates_NoHome(t *testing.T) {
	got := Candidates("")
	if len(got) != 2 {
		t.Errorf("with empty HOME, expected 2 system candidates, got %v", got)
	}
}

func TestAugment_PrependsExistingMissingDirs(t *testing.T) {
	exists := func(p string) bool {
		return p == "/opt/homebrew/bin" || p == "/Users/x/bin"
	}
	got := Augment("/usr/bin:/bin", []string{"/opt/homebrew/bin", "/missing", "/Users/x/bin"}, exists)
	want := "/opt/homebrew/bin:/Users/x/bin:/usr/bin:/bin"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAugment_SkipsAlreadyPresent(t *testing.T) {
	exists := func(p string) bool { return true }
	got := Augment("/opt/homebrew/bin:/usr/bin", []string{"/opt/homebrew/bin", "/usr/local/bin"}, exists)
	want := "/usr/local/bin:/opt/homebrew/bin:/usr/bin"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAugment_EmptyCurrent(t *testing.T) {
	exists := func(p string) bool { return true }
	got := Augment("", []string{"/opt/homebrew/bin"}, exists)
	if got != "/opt/homebrew/bin" {
		t.Errorf("got %q, want %q", got, "/opt/homebrew/bin")
	}
}

func TestAugment_NoCandidatesExistReturnsCurrent(t *testing.T) {
	exists := func(p string) bool { return false }
	got := Augment("/usr/bin", []string{"/opt/homebrew/bin", "/usr/local/bin"}, exists)
	if got != "/usr/bin" {
		t.Errorf("got %q, want %q (unchanged)", got, "/usr/bin")
	}
}

func TestAugment_DefaultExistsFunc(t *testing.T) {
	// Smoke-test with the default os.Stat exists func: /usr/bin
	// must always exist on macOS, so passing it as a candidate
	// when current omits it should prepend it.
	got := Augment("/bin", []string{"/usr/bin"}, nil)
	if !strings.HasPrefix(got, "/usr/bin:") {
		t.Errorf("expected /usr/bin to be prepended via default exists func, got %q", got)
	}
}

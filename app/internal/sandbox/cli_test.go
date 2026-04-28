package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildRunArgs_Defaults(t *testing.T) {
	cfg := Config{
		Engine:         "podman",
		Image:          "python:3.12-slim",
		Network:        false,
		CPULimit:       "2",
		MemoryLimit:    "1g",
		TimeoutSeconds: 60,
	}
	got := buildRunArgs(cfg, "shell-agent-v2-test", "/tmp/work")

	mustHaveSeq := [][]string{
		{"run", "-d"},
		{"--name", "shell-agent-v2-test"},
		{"--label", containerLabel},
		{"--workdir", "/work"},
		{"--volume", "/tmp/work:/work:Z"},
		{"--network", "none"},
		{"--cpus", "2"},
		{"--memory", "1g"},
	}
	for _, pair := range mustHaveSeq {
		if !containsSeq(got, pair) {
			t.Errorf("missing flag pair %v in %v", pair, got)
		}
	}
	// Image and tail command must be at the end.
	if got[len(got)-3] != "python:3.12-slim" {
		t.Errorf("image position wrong: %v", got)
	}
	if got[len(got)-2] != "sleep" || got[len(got)-1] != "infinity" {
		t.Errorf("expected trailing 'sleep infinity', got %v", got[len(got)-2:])
	}
}

func TestBuildRunArgs_NetworkOnOmitsNoneFlag(t *testing.T) {
	cfg := Config{Engine: "podman", Image: "i", Network: true, CPULimit: "1", MemoryLimit: "256m", TimeoutSeconds: 30}
	got := buildRunArgs(cfg, "n", "/w")
	if slices.Contains(got, "none") {
		t.Errorf("network=true must not append --network none: %v", got)
	}
}

func TestBuildRunArgs_OmitsEmptyLimits(t *testing.T) {
	cfg := Config{Engine: "podman", Image: "i", Network: false, TimeoutSeconds: 30}
	got := buildRunArgs(cfg, "n", "/w")
	if slices.Contains(got, "--cpus") {
		t.Errorf("empty CPULimit should omit flag; got %v", got)
	}
	if slices.Contains(got, "--memory") {
		t.Errorf("empty MemoryLimit should omit flag; got %v", got)
	}
}

func TestBuildExecArgs_Shell(t *testing.T) {
	got := buildExecArgs("c1", "shell")
	want := []string{"exec", "-i", "--workdir", "/work", "c1", "sh", "-s"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildExecArgs_Python(t *testing.T) {
	got := buildExecArgs("c1", "python")
	want := []string{"exec", "-i", "--workdir", "/work", "c1", "python3", "-"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildExecArgs_UnknownLanguageFallsBackToShell(t *testing.T) {
	got := buildExecArgs("c1", "ruby")
	if !slices.Contains(got, "sh") {
		t.Errorf("unknown language should fall back to shell; got %v", got)
	}
}

func TestParseLabelFilter(t *testing.T) {
	cases := map[string][]string{
		"abc\n":          {"abc"},
		"abc\ndef\n":     {"abc", "def"},
		"":               nil,
		"\n  \n":         nil,
		" abc \n  def ":  {"abc", "def"},
	}
	for in, want := range cases {
		got := parseLabelFilter(in)
		if !slices.Equal(got, want) {
			t.Errorf("parseLabelFilter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"sess-001":          "sess-001",
		"sess.123":          "sess.123",
		"sess/with/slash":   "sess_with_slash",
		"日本語":               "___",
		"abc def":           "abc_def",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContainerName(t *testing.T) {
	if got := containerName("sess-001"); got != "shell-agent-v2-sess-001" {
		t.Errorf("containerName = %q", got)
	}
}

// containsSeq reports whether b contains the contiguous sub-slice sub.
func containsSeq(b, sub []string) bool {
	for i := 0; i+len(sub) <= len(b); i++ {
		if slices.Equal(b[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// Helper used by other tests in the package.
var _ = strings.Contains

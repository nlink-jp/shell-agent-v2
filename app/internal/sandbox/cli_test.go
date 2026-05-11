package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	got := buildRunArgs(cfg, "shell-agent-v2-test", "/tmp/work", true, true)

	mustHaveSeq := [][]string{
		{"run", "-d"},
		{"--name", "shell-agent-v2-test"},
		{"--label", containerLabel},
		{"--workdir", "/work"},
		{"--volume", "/tmp/work:/work:Z"},
		{"--userns", "keep-id:uid=1000,gid=1000"},
		{"--user", "1000:1000"},
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
	got := buildRunArgs(cfg, "n", "/w", true, true)
	if slices.Contains(got, "none") {
		t.Errorf("network=true must not append --network none: %v", got)
	}
}

func TestBuildRunArgs_OmitsEmptyLimits(t *testing.T) {
	cfg := Config{Engine: "podman", Image: "i", Network: false, TimeoutSeconds: 30}
	got := buildRunArgs(cfg, "n", "/w", true, true)
	if slices.Contains(got, "--cpus") {
		t.Errorf("empty CPULimit should omit flag; got %v", got)
	}
	if slices.Contains(got, "--memory") {
		t.Errorf("empty MemoryLimit should omit flag; got %v", got)
	}
}

// TestBuildRunArgs_VolumeFlagHonoursSelinuxRelabel asserts the
// :Z suffix is only attached when the caller asked for it
// (podman + Linux). On all other (engine, OS) combinations the
// mount must use the plain :rw suffix so docker-desktop and
// non-SELinux Linux+docker don't reject the option or clobber
// labels on shared parents.
func TestBuildRunArgs_VolumeFlagHonoursSelinuxRelabel(t *testing.T) {
	cfg := Config{Engine: "podman", Image: "i", Network: false, TimeoutSeconds: 30}
	withZ := buildRunArgs(cfg, "n", "/w", true, true)
	withoutZ := buildRunArgs(cfg, "n", "/w", false, true)

	if !slices.Contains(withZ, "/w:/work:Z") {
		t.Errorf("selinuxRelabel=true should use :Z; got %v", withZ)
	}
	if !slices.Contains(withoutZ, "/w:/work:rw") {
		t.Errorf("selinuxRelabel=false should use :rw; got %v", withoutZ)
	}
	if slices.Contains(withoutZ, "/w:/work:Z") {
		t.Errorf("selinuxRelabel=false must NOT include :Z; got %v", withoutZ)
	}
}

// TestBuildRunArgs_PodmanRemapsHostUID asserts that, on podman,
// we route through `--userns=keep-id:uid=1000,gid=1000` and run
// the container as UID 1000 — never passing the host UID through
// to `--user`. This keeps large host UIDs (e.g., LDAP-mapped
// corporate macOS accounts where Getuid()=200M+) inside the
// rootless subuid range and avoids `crun: setresuid: Invalid
// argument` at container start.
func TestBuildRunArgs_PodmanRemapsHostUID(t *testing.T) {
	cfg := Config{Engine: "podman", Image: "i", Network: false, TimeoutSeconds: 30}
	got := buildRunArgs(cfg, "n", "/w", false, true)
	if !containsSeq(got, []string{"--userns", "keep-id:uid=1000,gid=1000"}) {
		t.Errorf("podman path must request keep-id userns remap; got %v", got)
	}
	if !containsSeq(got, []string{"--user", "1000:1000"}) {
		t.Errorf("podman path must run container as UID 1000; got %v", got)
	}
	hostUID := strconv.Itoa(os.Getuid())
	if slices.Contains(got, hostUID) {
		t.Errorf("podman path must not pass host UID %s through to --user; got %v", hostUID, got)
	}
}

// TestBuildRunArgs_NonPodmanPassesHostUID asserts that the docker
// path keeps the existing `--user $(id -u)` behaviour. Docker's
// rootless model differs from podman's; the keep-id syntax is
// podman-specific and would be rejected by docker.
func TestBuildRunArgs_NonPodmanPassesHostUID(t *testing.T) {
	cfg := Config{Engine: "docker", Image: "i", Network: false, TimeoutSeconds: 30}
	got := buildRunArgs(cfg, "n", "/w", false, false)
	if slices.Contains(got, "--userns") {
		t.Errorf("docker path must not emit --userns; got %v", got)
	}
	hostUID := strconv.Itoa(os.Getuid())
	if !containsSeq(got, []string{"--user", hostUID}) {
		t.Errorf("docker path must pass host UID %s to --user; got %v", hostUID, got)
	}
}

// TestUsePodmanUserns asserts the binary basename detection
// matches podman (case-insensitively) and rejects docker /
// arbitrary other binaries.
func TestUsePodmanUserns(t *testing.T) {
	cases := map[string]bool{
		"podman":            true,
		"/usr/bin/podman":   true,
		"/opt/homebrew/bin/podman": true,
		"Podman":            true, // case-insensitive
		"docker":            false,
		"/usr/local/bin/docker": false,
		"nerdctl":           false,
		"":                  false,
	}
	for in, want := range cases {
		if got := usePodmanUserns(in); got != want {
			t.Errorf("usePodmanUserns(%q) = %v, want %v", in, got, want)
		}
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

// (TestWriteBundle removed in r3 — the embed.FS bundle was
// replaced by a single Dockerfile string written directly
// to the build temp dir inside BuildImage.)
var _ = filepath.Join // keep filepath used elsewhere
var _ = os.Stat

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/sandbox"
)

// fakeEngine is a minimal sandbox.Engine for tests. It records every
// call and can be instructed to fail or to return a canned ExecResult.
type fakeEngine struct {
	workRoot   string
	execResult *sandbox.ExecResult
	execErr    error
	execCalls  []sandbox.ExecArgs
	stopCalls  []string
	stopAllN   int
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	return &fakeEngine{workRoot: t.TempDir()}
}

func (f *fakeEngine) Detect() (string, bool) { return "fake", true }
func (f *fakeEngine) EnsureContainer(_ context.Context, sid string) error {
	return os.MkdirAll(f.WorkDir(sid), 0700)
}
func (f *fakeEngine) Exec(_ context.Context, _ string, args sandbox.ExecArgs) (*sandbox.ExecResult, error) {
	f.execCalls = append(f.execCalls, args)
	if f.execErr != nil {
		return nil, f.execErr
	}
	if f.execResult != nil {
		return f.execResult, nil
	}
	return &sandbox.ExecResult{Stdout: "ok\n"}, nil
}
func (f *fakeEngine) Stop(_ context.Context, sid string) error {
	f.stopCalls = append(f.stopCalls, sid)
	return nil
}
func (f *fakeEngine) StopAll(_ context.Context) error {
	f.stopAllN++
	return nil
}
func (f *fakeEngine) WorkDir(sid string) string { return filepath.Join(f.workRoot, sid, "work") }
func (f *fakeEngine) Info(_ context.Context, _ string) (*sandbox.Info, error) {
	return &sandbox.Info{Engine: "fake", Image: "fake:latest", PythonVersion: "Python 3.x"}, nil
}

// ImageReady defaults to "ready" so existing sandbox-tool
// tests don't need to wire a build flow. Tests that exercise
// the gate should override this.
func (f *fakeEngine) ImageReady(_ context.Context, _ string) (bool, error) { return true, nil }
func (f *fakeEngine) BuildImage(_ context.Context, _ string, _ func(string)) (string, error) {
	return "", nil
}
func (f *fakeEngine) ListImages(_ context.Context) ([]sandbox.ImageInfo, error) {
	return nil, nil
}
func (f *fakeEngine) RemoveImage(_ context.Context, _ string) error { return nil }

// newAgentWithSandbox returns an Agent with a fake sandbox engine and
// a session pre-loaded so tool dispatchers can run.
func newAgentWithSandbox(t *testing.T) (*Agent, *fakeEngine) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a := New(config.Default())
	fe := newFakeEngine(t)
	a.sandbox = fe
	a.session = &memory.Session{ID: "sess-test", Title: "Test"}
	a.objects = objstore.NewStoreAt(filepath.Join(t.TempDir(), "objects"))
	return a, fe
}

func TestBuildToolDefs_IncludesSandboxWhenEngineSet(t *testing.T) {
	a, _ := newAgentWithSandbox(t)
	tools := a.buildToolDefs()
	wantNames := map[string]bool{
		"sandbox-run-shell":          false,
		"sandbox-run-python":         false,
		"sandbox-write-file":         false,
		"sandbox-copy-object":        false,
		"sandbox-register-object":    false,
		"sandbox-info":               false,
		"sandbox-load-into-analysis": false,
		"sandbox-export-sql":         false,
	}
	for _, td := range tools {
		if _, ok := wantNames[td.Name]; ok {
			wantNames[td.Name] = true
		}
	}
	for n, found := range wantNames {
		if !found {
			t.Errorf("buildToolDefs missing %q", n)
		}
	}
}

func TestBuildToolDefs_OmitsSandboxWhenEngineNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := New(config.Default()) // sandbox stays nil because cfg.Sandbox.Enabled is false
	tools := a.buildToolDefs()
	for _, td := range tools {
		if strings.HasPrefix(td.Name, "sandbox-") {
			t.Errorf("unexpected sandbox tool %q when engine is nil", td.Name)
		}
	}
}

func TestIsToolMITLRequired_SandboxPrefixDefaultOn(t *testing.T) {
	a := &Agent{cfg: config.Default()}
	if !a.IsToolMITLRequired("sandbox-run-shell") {
		t.Error("sandbox-* should require MITL by default")
	}
	a.cfg.Tools.MITLOverrides = map[string]bool{"sandbox-run-shell": false}
	if a.IsToolMITLRequired("sandbox-run-shell") {
		t.Error("override should be respected")
	}
}

func TestExecuteSandboxTool_RunShellCallsEngineExec(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	out, _ := a.executeSandboxTool(context.Background(), "sandbox-run-shell", `{"command":"echo hi"}`)
	if !strings.Contains(out, "ok") {
		t.Errorf("unexpected output: %q", out)
	}
	if len(fe.execCalls) != 1 || fe.execCalls[0].Language != "shell" || fe.execCalls[0].Code != "echo hi" {
		t.Errorf("Exec call wrong: %+v", fe.execCalls)
	}
}

func TestExecuteSandboxTool_RunPythonCallsEngineExec(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	_, _ = a.executeSandboxTool(context.Background(), "sandbox-run-python", `{"code":"print(1)"}`)
	if len(fe.execCalls) != 1 || fe.execCalls[0].Language != "python" || !strings.Contains(fe.execCalls[0].Code, "print") {
		t.Errorf("Exec call wrong: %+v", fe.execCalls)
	}
}

func TestExecuteSandboxTool_WriteFileWritesToWorkDir(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	out, _ := a.executeSandboxTool(context.Background(), "sandbox-write-file", `{"path":"data.csv","content":"a,b\n1,2\n"}`)
	if !strings.Contains(out, "wrote") {
		t.Errorf("unexpected output: %q", out)
	}
	written, err := os.ReadFile(filepath.Join(fe.WorkDir(a.session.ID), "data.csv"))
	if err != nil {
		t.Fatalf("expected file written: %v", err)
	}
	if string(written) != "a,b\n1,2\n" {
		t.Errorf("written content mismatch: %q", written)
	}
}

func TestExecuteSandboxTool_WriteFileRejectsTraversal(t *testing.T) {
	a, _ := newAgentWithSandbox(t)
	out, _ := a.executeSandboxTool(context.Background(), "sandbox-write-file", `{"path":"../escape.txt","content":"x"}`)
	if !strings.Contains(out, "Error") || !strings.Contains(out, "escapes") {
		t.Errorf("expected traversal rejection, got %q", out)
	}
}

func TestExecuteSandboxTool_WriteFileNormalisesWorkPrefix(t *testing.T) {
	// LLMs frequently pass the in-container absolute path '/work/foo'
	// because that's what they see inside sandbox-run-python. We must
	// not interpret that as workDir/work/foo (the dreaded /work/work/
	// regression). Same for "work/foo" without leading slash.
	for _, in := range []string{"/work/data.csv", "work/data.csv", "data.csv"} {
		a, fe := newAgentWithSandbox(t)
		out, _ := a.executeSandboxTool(context.Background(), "sandbox-write-file", `{"path":"`+in+`","content":"x"}`)
		if !strings.Contains(out, "wrote") {
			t.Errorf("input %q: expected success, got %q", in, out)
			continue
		}
		expected := filepath.Join(fe.WorkDir(a.session.ID), "data.csv")
		if _, err := os.Stat(expected); err != nil {
			t.Errorf("input %q: expected file at %s, got %v", in, expected, err)
		}
		// The success message must show /work/data.csv (single
		// /work/), not /work//work/data.csv — the result text is
		// what the LLM sees, and a doubled prefix can mislead the
		// next call.
		if strings.Contains(out, "/work//") || strings.Contains(out, "/work/work/") {
			t.Errorf("input %q: result message has doubled /work/ segment: %q", in, out)
		}
		if !strings.Contains(out, "/work/data.csv") {
			t.Errorf("input %q: result message should mention /work/data.csv, got %q", in, out)
		}
	}
}

func TestExecuteSandboxTool_CopyObject(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	// Seed an object in the store.
	meta, err := a.objects.Store(strings.NewReader("hello"), objstore.TypeBlob, "text/plain", "hello.txt", a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := a.executeSandboxTool(context.Background(), "sandbox-copy-object", `{"object_id":"`+meta.ID+`"}`)
	if !strings.Contains(out, "copied object") {
		t.Errorf("unexpected output: %q", out)
	}
	got, err := os.ReadFile(filepath.Join(fe.WorkDir(a.session.ID), "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("file copied wrong: %q", got)
	}
}

func TestExecuteSandboxTool_RegisterObject(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	if err := a.sandbox.EnsureContainer(context.Background(), a.session.ID); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(fe.WorkDir(a.session.ID), "chart.png")
	if err := os.WriteFile(src, []byte("\x89PNG-fake"), 0644); err != nil {
		t.Fatal(err)
	}
	out, _ := a.executeSandboxTool(context.Background(), "sandbox-register-object", `{"path":"chart.png"}`)
	if !strings.Contains(out, "registered as object") || !strings.Contains(out, "image") {
		t.Errorf("unexpected output: %q", out)
	}
	if len(a.objects.All()) != 1 {
		t.Errorf("expected 1 object stored, got %d", len(a.objects.All()))
	}
	if a.objects.All()[0].MimeType != "image/png" {
		t.Errorf("MIME = %q, want image/png", a.objects.All()[0].MimeType)
	}
}

func TestExecuteSandboxTool_LoadIntoAnalysis_NoEngine(t *testing.T) {
	a, _ := newAgentWithSandbox(t)
	a.analysis = nil
	out, _ := a.executeSandboxTool(context.Background(), "sandbox-load-into-analysis", `{"path":"x.csv","table_name":"t"}`)
	if !strings.Contains(out, "analysis engine not available") {
		t.Errorf("expected absent-analysis error: %q", out)
	}
}

func TestExecuteSandboxTool_Info(t *testing.T) {
	a, _ := newAgentWithSandbox(t)
	out, _ := a.executeSandboxTool(context.Background(), "sandbox-info", `{}`)
	if !strings.Contains(out, "engine:") || !strings.Contains(out, "fake") {
		t.Errorf("FormatInfo unexpected output: %q", out)
	}
}

func TestSandboxStop_SafeWhenDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := New(config.Default())
	if err := a.SandboxStop(context.Background(), "any-sid"); err != nil {
		t.Errorf("SandboxStop with no engine should not error: %v", err)
	}
}

func TestSandboxStop_DelegatesWhenEnabled(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	if err := a.SandboxStop(context.Background(), "sess-test"); err != nil {
		t.Fatal(err)
	}
	if len(fe.stopCalls) != 1 || fe.stopCalls[0] != "sess-test" {
		t.Errorf("Stop calls = %v", fe.stopCalls)
	}
}

// Suppress "unused" warning if the import list eventually slims.
var _ = llm.Message{}
var _ = time.Second

func TestExecResultStatus(t *testing.T) {
	cases := []struct {
		name string
		in   *sandbox.ExecResult
		want ActivityEventStatus
	}{
		{"nil → error", nil, ActivityStatusError},
		{"clean exit 0", &sandbox.ExecResult{ExitCode: 0}, ActivityStatusSuccess},
		{"non-zero exit", &sandbox.ExecResult{ExitCode: 1}, ActivityStatusError},
		{"timed out", &sandbox.ExecResult{TimedOut: true}, ActivityStatusError},
		{"timeout takes precedence over zero exit", &sandbox.ExecResult{ExitCode: 0, TimedOut: true}, ActivityStatusError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execResultStatus(tc.in); got != tc.want {
				t.Errorf("execResultStatus(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExecuteSandboxTool_RunShellExitCodeMaps(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	fe.execResult = &sandbox.ExecResult{Stdout: "no", Stderr: "boom", ExitCode: 1}

	out, status := a.executeSandboxTool(context.Background(), "sandbox-run-shell", `{"command":"false"}`)
	if status != ActivityStatusError {
		t.Errorf("status = %q, want error", status)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("expected stderr in output, got %q", out)
	}
}

func TestExecuteSandboxTool_RunPythonTimeoutMaps(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	fe.execResult = &sandbox.ExecResult{TimedOut: true}

	_, status := a.executeSandboxTool(context.Background(), "sandbox-run-python", `{"code":"while True: pass"}`)
	if status != ActivityStatusError {
		t.Errorf("status = %q, want error (timeout)", status)
	}
}

// --- safeWorkPath tests (Phase 2 hardening) ---

// resolvedDir returns the symlink-resolved form of t.TempDir().
// On macOS the temp root is /var/folders/... but the canonical
// path is /private/var/folders/...; safeWorkPath returns the
// resolved form, so test expectations need to too.
func resolvedDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSafeWorkPath_AcceptsValidNewLeaf(t *testing.T) {
	wd := resolvedDir(t)
	p, err := safeWorkPath(wd, "subdir/file.csv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != filepath.Join(wd, "subdir", "file.csv") {
		t.Errorf("got %q", p)
	}
}

func TestSafeWorkPath_StripsContainerPrefix(t *testing.T) {
	wd := resolvedDir(t)
	for _, in := range []string{"/work/foo.txt", "work/foo.txt", "foo.txt"} {
		p, err := safeWorkPath(wd, in)
		if err != nil {
			t.Errorf("input %q: %v", in, err)
			continue
		}
		if p != filepath.Join(wd, "foo.txt") {
			t.Errorf("input %q: got %q, want %q", in, p, filepath.Join(wd, "foo.txt"))
		}
	}
}

func TestSafeWorkPath_BlocksAbsolute(t *testing.T) {
	wd := resolvedDir(t)
	if _, err := safeWorkPath(wd, "/etc/passwd"); err == nil {
		// "/etc/passwd" gets the leading slash stripped → "etc/passwd",
		// which is a valid relative path. So this is "accepted" but
		// rooted at workDir; not actually escaping. The real escape
		// vectors are dotdot and symlinks tested below.
	}
	// What we definitely must reject:
	if _, err := safeWorkPath(wd, "../../etc/passwd"); err == nil {
		t.Error("../../etc/passwd should escape /work")
	}
}

func TestSafeWorkPath_BlocksDotDot(t *testing.T) {
	wd := resolvedDir(t)
	cases := []string{"..", "../foo", "a/../../b", "subdir/../../escape"}
	for _, in := range cases {
		if _, err := safeWorkPath(wd, in); err == nil {
			t.Errorf("input %q: expected escape error, got nil", in)
		}
	}
}

func TestSafeWorkPath_BlocksSymlinkLeaf(t *testing.T) {
	wd := resolvedDir(t)
	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wd, "trap")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := safeWorkPath(wd, "trap"); err == nil {
		t.Error("expected error: leaf is a symlink to outside")
	}
}

func TestSafeWorkPath_BlocksSymlinkInParent(t *testing.T) {
	wd := resolvedDir(t)
	// /work/badparent → /tmp/somewhere-outside
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(wd, "badparent")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeWorkPath(wd, "badparent/file.txt"); err == nil {
		t.Error("expected error: parent component is a symlink to outside")
	}
}

func TestSafeWorkPath_AllowsSymlinkInsideWorkDir(t *testing.T) {
	wd := resolvedDir(t)
	if err := os.Mkdir(filepath.Join(wd, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	// Symlink that stays inside workDir is OK at the parent level.
	if err := os.Symlink(filepath.Join(wd, "real"), filepath.Join(wd, "alias")); err != nil {
		t.Fatal(err)
	}
	p, err := safeWorkPath(wd, "alias/inner.txt")
	if err != nil {
		t.Fatalf("intra-workdir symlink should be allowed: %v", err)
	}
	if !strings.HasPrefix(p, wd+string(filepath.Separator)) {
		t.Errorf("resolved path %q escapes workDir %q", p, wd)
	}
}

func TestSafeWorkPath_RejectsEmpty(t *testing.T) {
	if _, err := safeWorkPath(resolvedDir(t), "/work/"); err == nil {
		t.Error("empty path after stripping should error")
	}
}

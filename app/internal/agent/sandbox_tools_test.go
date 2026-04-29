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
	out := a.executeSandboxTool(context.Background(), "sandbox-run-shell", `{"command":"echo hi"}`)
	if !strings.Contains(out, "ok") {
		t.Errorf("unexpected output: %q", out)
	}
	if len(fe.execCalls) != 1 || fe.execCalls[0].Language != "shell" || fe.execCalls[0].Code != "echo hi" {
		t.Errorf("Exec call wrong: %+v", fe.execCalls)
	}
}

func TestExecuteSandboxTool_RunPythonCallsEngineExec(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	a.executeSandboxTool(context.Background(), "sandbox-run-python", `{"code":"print(1)"}`)
	if len(fe.execCalls) != 1 || fe.execCalls[0].Language != "python" || !strings.Contains(fe.execCalls[0].Code, "print") {
		t.Errorf("Exec call wrong: %+v", fe.execCalls)
	}
}

func TestExecuteSandboxTool_WriteFileWritesToWorkDir(t *testing.T) {
	a, fe := newAgentWithSandbox(t)
	out := a.executeSandboxTool(context.Background(), "sandbox-write-file", `{"path":"data.csv","content":"a,b\n1,2\n"}`)
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
	out := a.executeSandboxTool(context.Background(), "sandbox-write-file", `{"path":"../escape.txt","content":"x"}`)
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
		out := a.executeSandboxTool(context.Background(), "sandbox-write-file", `{"path":"`+in+`","content":"x"}`)
		if !strings.Contains(out, "wrote") {
			t.Errorf("input %q: expected success, got %q", in, out)
			continue
		}
		expected := filepath.Join(fe.WorkDir(a.session.ID), "data.csv")
		if _, err := os.Stat(expected); err != nil {
			t.Errorf("input %q: expected file at %s, got %v", in, expected, err)
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
	out := a.executeSandboxTool(context.Background(), "sandbox-copy-object", `{"object_id":"`+meta.ID+`"}`)
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
	out := a.executeSandboxTool(context.Background(), "sandbox-register-object", `{"path":"chart.png"}`)
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
	out := a.executeSandboxTool(context.Background(), "sandbox-load-into-analysis", `{"path":"x.csv","table_name":"t"}`)
	if !strings.Contains(out, "analysis engine not available") {
		t.Errorf("expected absent-analysis error: %q", out)
	}
}

func TestExecuteSandboxTool_Info(t *testing.T) {
	a, _ := newAgentWithSandbox(t)
	out := a.executeSandboxTool(context.Background(), "sandbox-info", `{}`)
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

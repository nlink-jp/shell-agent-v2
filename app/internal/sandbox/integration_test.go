package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// integrationEngine spins up a real cliEngine if podman or docker is
// available; otherwise the calling test is skipped. Sessions are
// derived from t.Name() so concurrent / re-run safety is automatic.
func integrationEngine(t *testing.T) (Engine, string) {
	t.Helper()
	if _, ok := resolveEngine("auto"); !ok {
		t.Skip("podman/docker not on PATH; skipping integration test")
	}
	root := t.TempDir()
	e, err := NewCLI(Config{
		Engine:         "auto",
		Image:          imageOrFallback(),
		Network:        false,
		CPULimit:       "1",
		MemoryLimit:    "512m",
		TimeoutSeconds: 30,
		SessionsDir:    root,
	})
	if err != nil {
		t.Skipf("NewCLI: %v", err)
	}
	t.Cleanup(func() {
		_ = e.StopAll(context.Background())
	})
	return e, sessionIDFor(t)
}

// imageOrFallback returns the env override if set, or the default
// python image. Lets CI / local runs swap in something smaller.
func imageOrFallback() string {
	if v := getenv("SHELL_AGENT_TEST_IMAGE"); v != "" {
		return v
	}
	return "python:3.12-slim"
}

func getenv(k string) string {
	cmd := exec.Command("printenv", k)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func sessionIDFor(t *testing.T) string {
	h := sha256.Sum256([]byte(t.Name()))
	return "test-" + hex.EncodeToString(h[:6])
}

// --- Tests ----------------------------------------------------------

func TestIntegration_RoundTrip(t *testing.T) {
	e, sid := integrationEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := e.EnsureContainer(ctx, sid); err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}
	res, err := e.Exec(ctx, sid, ExecArgs{Language: "shell", Code: "echo hi"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hi" || res.ExitCode != 0 {
		t.Errorf("got %+v", res)
	}
}

func TestIntegration_FilePersistence(t *testing.T) {
	e, sid := integrationEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := e.EnsureContainer(ctx, sid); err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}
	if _, err := e.Exec(ctx, sid, ExecArgs{Language: "shell", Code: "echo persisted > /work/a"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Exec(ctx, sid, ExecArgs{Language: "shell", Code: "cat /work/a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "persisted" {
		t.Errorf("file did not persist; stdout=%q", res.Stdout)
	}
}

func TestIntegration_CrossSessionIsolation(t *testing.T) {
	e, sid := integrationEngine(t)
	other := sid + "-other"
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := e.EnsureContainer(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err := e.EnsureContainer(ctx, other); err != nil {
		t.Fatal(err)
	}
	defer e.Stop(ctx, other)

	if _, err := e.Exec(ctx, sid, ExecArgs{Language: "shell", Code: "echo mine > /work/secret"}); err != nil {
		t.Fatal(err)
	}
	res, _ := e.Exec(ctx, other, ExecArgs{Language: "shell", Code: "ls /work/secret 2>&1; true"})
	if strings.Contains(res.Stdout, "secret") && !strings.Contains(res.Stdout, "No such") {
		t.Errorf("session %q saw the other's file: %q", other, res.Stdout)
	}
}

func TestIntegration_StopRemovesContainer(t *testing.T) {
	e, sid := integrationEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.EnsureContainer(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Exec(ctx, sid, ExecArgs{Language: "shell", Code: "true"}); err == nil {
		t.Error("Exec after Stop should fail")
	}
}

func TestIntegration_Timeout(t *testing.T) {
	e, sid := integrationEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.EnsureContainer(ctx, sid); err != nil {
		t.Fatal(err)
	}
	res, err := e.Exec(ctx, sid, ExecArgs{Language: "shell", Code: "sleep 5", Timeout: time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("expected TimedOut=true, got %+v", res)
	}
	if res.ExitCode == 0 {
		t.Errorf("expected non-zero exit on timeout")
	}
}

func TestIntegration_Info(t *testing.T) {
	e, sid := integrationEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := e.EnsureContainer(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Exec(ctx, sid, ExecArgs{Language: "shell", Code: "echo x > /work/marker"}); err != nil {
		t.Fatal(err)
	}
	info, err := e.Info(ctx, sid)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.EngineVersion == "" {
		t.Error("EngineVersion empty")
	}
	if info.PythonVersion == "" {
		t.Error("PythonVersion empty (image should ship python3)")
	}
	foundMarker := false
	for _, f := range info.WorkFiles {
		if f.Path == "marker" {
			foundMarker = true
			break
		}
	}
	if !foundMarker {
		t.Errorf("expected /work listing to include 'marker'; got %+v", info.WorkFiles)
	}
}

// suppress unused-import warnings when filepath is only used
// transitively above.
var _ = filepath.Join

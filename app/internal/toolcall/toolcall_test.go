package toolcall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testScript = `#!/bin/bash
# @tool: test-echo
# @description: Echo back the input
# @param: message string "Message to echo"
# @category: read

cat
`

const testWriteScript = `#!/bin/bash
# @tool: test-write
# @description: Write something
# @param: path string "File path"
# @param: content string "Content to write"
# @category: write

echo "would write"
`

func TestParseToolHeader(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "test-echo.sh")
	os.WriteFile(scriptPath, []byte(testScript), 0755)

	tool, err := parseToolHeader(scriptPath)
	if err != nil {
		t.Fatalf("parseToolHeader: %v", err)
	}
	if tool == nil {
		t.Fatal("tool is nil")
	}
	if tool.Name != "test-echo" {
		t.Errorf("name = %v, want test-echo", tool.Name)
	}
	if tool.Description != "Echo back the input" {
		t.Errorf("description = %v", tool.Description)
	}
	if tool.Category != CategoryRead {
		t.Errorf("category = %v, want read", tool.Category)
	}
	if len(tool.Params) != 1 {
		t.Fatalf("params count = %d, want 1", len(tool.Params))
	}
	if tool.Params[0].Name != "message" {
		t.Errorf("param name = %v, want message", tool.Params[0].Name)
	}
}

func TestNeedsMITL(t *testing.T) {
	read := &Tool{Category: CategoryRead}
	write := &Tool{Category: CategoryWrite}
	execute := &Tool{Category: CategoryExecute}

	if read.NeedsMITL() {
		t.Error("read should not need MITL")
	}
	if !write.NeedsMITL() {
		t.Error("write should need MITL")
	}
	if !execute.NeedsMITL() {
		t.Error("execute should need MITL")
	}
}

func TestScanDir(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, "echo.sh"), []byte(testScript), 0755)
	os.WriteFile(filepath.Join(tmpDir, "write.sh"), []byte(testWriteScript), 0755)
	os.WriteFile(filepath.Join(tmpDir, "plain.txt"), []byte("not a tool"), 0644)

	r := NewRegistry()
	if err := r.ScanDir(tmpDir); err != nil {
		t.Fatalf("ScanDir: %v", err)
	}

	all := r.All()
	if len(all) != 2 {
		t.Errorf("tools count = %d, want 2", len(all))
	}

	tool, ok := r.Get("test-echo")
	if !ok {
		t.Fatal("test-echo not found")
	}
	if tool.Category != CategoryRead {
		t.Errorf("test-echo category = %v", tool.Category)
	}

	tool, ok = r.Get("test-write")
	if !ok {
		t.Fatal("test-write not found")
	}
	if tool.Category != CategoryWrite {
		t.Errorf("test-write category = %v", tool.Category)
	}
}

func TestScanDirNonExistent(t *testing.T) {
	r := NewRegistry()
	err := r.ScanDir("/nonexistent/path")
	if err != nil {
		t.Errorf("non-existent dir should not error: %v", err)
	}
}

func TestExecute(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "echo.sh")
	os.WriteFile(scriptPath, []byte(testScript), 0755)

	tool := &Tool{
		Name:       "test-echo",
		ScriptPath: scriptPath,
		Category:   CategoryRead,
	}

	result, err := Execute(context.Background(), tool, `{"message":"hello"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != `{"message":"hello"}` {
		t.Errorf("result = %q, want JSON input echoed back", result)
	}
}

// --- WithWorkDir env var injection (docs/en/work-dir-shell-bridge.md) ---

// TestExecute_WithWorkDir_SetsEnvVar: when WithWorkDir is passed,
// the spawned process sees SHELL_AGENT_WORK_DIR in its environment.
func TestExecute_WithWorkDir_SetsEnvVar(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "echo-env.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/bash
echo "$SHELL_AGENT_WORK_DIR"
`), 0755); err != nil {
		t.Fatal(err)
	}
	tool := &Tool{Name: "echo-env", ScriptPath: script}

	want := "/tmp/work-xyz"
	out, err := Execute(context.Background(), tool, "{}", WithWorkDir(want))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(out)
	if got != want {
		t.Errorf("SHELL_AGENT_WORK_DIR = %q, want %q", got, want)
	}
}

// TestExecute_NoWithWorkDir_NoEnvVar: callers that don't pass
// WithWorkDir get the same behaviour as before — env var absent
// (or whatever the parent set).
func TestExecute_NoWithWorkDir_NoEnvVar(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "echo-env.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/bash
echo "${SHELL_AGENT_WORK_DIR:-UNSET}"
`), 0755); err != nil {
		t.Fatal(err)
	}
	tool := &Tool{Name: "echo-env", ScriptPath: script}

	// Make sure the parent doesn't have the var set so we can
	// observe the absence.
	t.Setenv("SHELL_AGENT_WORK_DIR", "")
	os.Unsetenv("SHELL_AGENT_WORK_DIR")

	out, err := Execute(context.Background(), tool, "{}")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := strings.TrimSpace(out); got != "UNSET" {
		t.Errorf("env var leaked into child: %q (want UNSET)", got)
	}
}

// --- @timeout header (docs/en/tool-execution-timeout.md) ---

func writeScriptWithHeader(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tool.sh")
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParseToolHeader_TimeoutHonoured: a valid `@timeout: N` header
// produces tool.Timeout == N seconds.
func TestParseToolHeader_TimeoutHonoured(t *testing.T) {
	const src = `#!/bin/bash
# @tool: long-poll
# @description: Slow external poll
# @param: url string "URL"
# @category: read
# @timeout: 90

echo ok
`
	tool, err := parseToolHeader(writeScriptWithHeader(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if tool == nil {
		t.Fatal("tool nil")
	}
	if got, want := tool.Timeout, 90*time.Second; got != want {
		t.Errorf("Timeout = %v, want %v", got, want)
	}
}

// TestParseToolHeader_TimeoutMissing_LeavesZero: no @timeout line
// → Timeout == 0 (Execute will fall back to DefaultTimeout).
func TestParseToolHeader_TimeoutMissing_LeavesZero(t *testing.T) {
	tool, err := parseToolHeader(writeScriptWithHeader(t, testScript))
	if err != nil {
		t.Fatal(err)
	}
	if tool.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (Execute should default)", tool.Timeout)
	}
}

// TestParseToolHeader_TimeoutInvalid_FallsBack: malformed values
// (non-numeric, zero, negative, Go duration string) all leave
// Timeout == 0 — the script still loads, Execute uses
// DefaultTimeout. logger.Error is a no-op without logger.Init in
// unit tests.
func TestParseToolHeader_TimeoutInvalid_FallsBack(t *testing.T) {
	cases := []string{
		"# @timeout: abc",
		"# @timeout: 0",
		"# @timeout: -10",
		"# @timeout: 90s", // Go duration string not supported in v1
		"# @timeout:",     // empty value
	}
	for _, line := range cases {
		t.Run(line, func(t *testing.T) {
			src := "#!/bin/bash\n# @tool: t\n# @description: d\n# @category: read\n" + line + "\n\necho ok\n"
			tool, err := parseToolHeader(writeScriptWithHeader(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if tool == nil {
				t.Fatal("tool nil — invalid @timeout should not block registration")
			}
			if tool.Timeout != 0 {
				t.Errorf("Timeout = %v, want 0 (fall through to DefaultTimeout)", tool.Timeout)
			}
		})
	}
}

// TestExecute_HonoursToolTimeout: tool.Timeout > 0 actually wins
// over DefaultTimeout. Use a bash-builtin busy loop (no `sleep`
// child) so SIGKILL on cancellation actually terminates the
// process and CombinedOutput returns promptly — `sleep` would be
// orphaned by bash's death and keep the pipes open until it
// completes.
func TestExecute_HonoursToolTimeout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "spin.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nwhile true; do :; done\n"), 0755); err != nil {
		t.Fatal(err)
	}
	tool := &Tool{Name: "spin", ScriptPath: script, Timeout: 500 * time.Millisecond}

	start := time.Now()
	_, err := Execute(context.Background(), tool, "{}")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error from an infinite loop capped at 500ms")
	}
	// SIGKILL on the bash builtin loop kills it immediately;
	// CombinedOutput returns straight after.
	if elapsed >= 3*time.Second {
		t.Errorf("elapsed %v ≥ 3s — per-tool timeout did not preempt", elapsed)
	}
}

// TestExecute_FallsBackToDefaultTimeout: when tool.Timeout is 0,
// DefaultTimeout (30s) is used. A short sleep should complete
// normally without hitting the cap.
func TestExecute_FallsBackToDefaultTimeout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "quick.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	tool := &Tool{Name: "quick", ScriptPath: script /* Timeout: 0 */}

	out, err := Execute(context.Background(), tool, "{}")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Error("empty output")
	}
}

func TestToolDefParams(t *testing.T) {
	tool := &Tool{
		Params: []Param{
			{Name: "path", Type: "string", Description: "File path"},
			{Name: "force", Type: "boolean", Description: "Force overwrite"},
		},
	}

	def := tool.ToolDefParams()
	if def["type"] != "object" {
		t.Error("type should be object")
	}
	props, ok := def["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be map")
	}
	if len(props) != 2 {
		t.Errorf("properties count = %d, want 2", len(props))
	}
}

package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// makeStub writes a Python script that mimics an MCP guardian on stdio.
// It honours initialize / tools/list / tools/call (with a fail-tool branch
// that returns an RPC error) and rejects everything else with -32601.
func makeStub(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-guardian")
	src := `#!/usr/bin/env python3
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    try:
        req = json.loads(line)
    except Exception:
        continue
    method = req.get("method", "")
    rid = req.get("id", 0)
    if method == "initialize":
        resp = {"jsonrpc":"2.0","id":rid,"result":{}}
    elif method == "tools/list":
        resp = {"jsonrpc":"2.0","id":rid,"result":{"tools":[
            {"name":"hello","description":"say hi","inputSchema":{}},
            {"name":"fail-tool","description":"errors out","inputSchema":{}}
        ]}}
    elif method == "tools/call":
        name = req.get("params", {}).get("name", "")
        if name == "fail-tool":
            resp = {"jsonrpc":"2.0","id":rid,"error":{"code":-32000,"message":"tool failed"}}
        else:
            resp = {"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":"ok"}]}}
    else:
        resp = {"jsonrpc":"2.0","id":rid,"error":{"code":-32601,"message":"method not found"}}
    print(json.dumps(resp), flush=True)
`
	if err := os.WriteFile(path, []byte(src), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGuardian_StartTools(t *testing.T) {
	stub := makeStub(t)
	g := NewGuardian(stub)
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Stop()

	tools := g.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	names := []string{tools[0].Name, tools[1].Name}
	if !slices.Contains(names, "hello") || !slices.Contains(names, "fail-tool") {
		t.Errorf("tool names = %v", names)
	}
}

func TestGuardian_CallToolSuccess(t *testing.T) {
	stub := makeStub(t)
	g := NewGuardian(stub)
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Stop()

	args := json.RawMessage(`{}`)
	res, err := g.CallTool("hello", args)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(res), "ok") {
		t.Errorf("result = %s", res)
	}
}

func TestGuardian_CallToolRPCErrorSurfaces(t *testing.T) {
	stub := makeStub(t)
	g := NewGuardian(stub)
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Stop()

	_, err := g.CallTool("fail-tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected RPC error to surface as Go error")
	}
	if !strings.Contains(err.Error(), "tool failed") {
		t.Errorf("error = %v, want 'tool failed'", err)
	}
	if !strings.Contains(err.Error(), "-32000") {
		t.Errorf("error should include RPC code: %v", err)
	}
}

func TestGuardian_StopReleasesResources(t *testing.T) {
	stub := makeStub(t)
	g := NewGuardian(stub)
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := g.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Calls after Stop must be rejected.
	if _, err := g.CallTool("hello", json.RawMessage(`{}`)); err == nil {
		t.Error("CallTool after Stop should error")
	}
	// Stop is idempotent.
	if err := g.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

// TestGuardian_StartTimesOutOnSilentBinary verifies that a guardian
// binary which never produces stdout output is reaped within
// StartTimeout. Regression test for the original locking bug where
// call() held g.mu across stdout.Scan, deadlocking Stop when the
// timeout fired.
func TestGuardian_StartTimesOutOnSilentBinary(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "silent")
	if err := os.WriteFile(stub, []byte("#!/usr/bin/env bash\nsleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}
	g := NewGuardian(stub)
	done := make(chan error, 1)
	go func() { done <- g.Start() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Start against silent binary should not succeed")
		}
	case <-time.After(StartTimeout + 5*time.Second):
		t.Fatalf("Start did not return within %s — locking regression", StartTimeout+5*time.Second)
	}
}

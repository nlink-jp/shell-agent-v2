package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
        elif name == "tool-isError":
            # Tool-level failure: RPC succeeds but isError marks
            # the result as a logical failure.
            resp = {"jsonrpc":"2.0","id":rid,"result":{"isError":True,"content":[{"type":"text","text":"oops"}]}}
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

func TestGuardian_CallToolIsErrorSurfacesAsErrToolFailed(t *testing.T) {
	// MCP spec: a successful RPC response with result.isError:true
	// means the tool ran and reported a tool-level failure. CallTool
	// must surface this as ErrToolFailed so the agent can render the
	// chat bubble red while still passing the result body to the LLM
	// (the body has the diagnostic content).
	stub := makeStub(t)
	g := NewGuardian(stub)
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Stop()

	res, err := g.CallTool("tool-isError", json.RawMessage(`{}`))
	if !errors.Is(err, ErrToolFailed) {
		t.Fatalf("err = %v, want ErrToolFailed", err)
	}
	if !strings.Contains(string(res), "oops") {
		t.Errorf("result body should still be returned even on isError, got: %s", res)
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

// TestGuardian_DrainsStderrWithoutDeadlock writes ~1 MB to stderr from
// the guardian during initialize before responding on stdout. Without
// the stderr drain (security-hardening-2.md C2) the kernel pipe buffer
// fills around 64 KB, the child blocks on its next stderr write, and
// the parent's stdout.Scan never sees the response — the whole agent
// hangs until StartTimeout fires. The test asserts Start returns
// successfully well within timeout.
func TestGuardian_DrainsStderrWithoutDeadlock(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "noisy-guardian")
	src := `#!/usr/bin/env python3
import sys, json
# Flood stderr before the first stdout write. Larger than the
# kernel pipe buffer (~64 KB on Linux/macOS) so a non-draining
# parent will deadlock here.
for _ in range(2000):
    sys.stderr.write("noise " * 100 + "\n")
sys.stderr.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except Exception:
        continue
    method = req.get("method", "")
    rid = req.get("id", 0)
    if method == "initialize":
        resp = {"jsonrpc":"2.0","id":rid,"result":{}}
    elif method == "tools/list":
        resp = {"jsonrpc":"2.0","id":rid,"result":{"tools":[]}}
    else:
        resp = {"jsonrpc":"2.0","id":rid,"error":{"code":-32601,"message":"nope"}}
    print(json.dumps(resp), flush=True)
`
	if err := os.WriteFile(path, []byte(src), 0755); err != nil {
		t.Fatal(err)
	}

	g := NewGuardian(path)
	g.SetName("noisy")
	done := make(chan error, 1)
	go func() { done <- g.Start() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		g.Stop()
	case <-time.After(StartTimeout - 1*time.Second):
		g.Stop()
		t.Fatal("Start deadlocked — stderr drain regression")
	}
}

// TestGuardian_RejectsResponseIdMismatch verifies that a guardian
// returning an unexpected response id is treated as a transport error
// rather than silently routing one call's body to another caller
// (security-hardening-2.md H4).
func TestGuardian_RejectsResponseIdMismatch(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lying-guardian")
	src := `#!/usr/bin/env python3
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except Exception:
        continue
    method = req.get("method", "")
    # Always respond with id=999 regardless of what was sent.
    if method == "initialize":
        resp = {"jsonrpc":"2.0","id":999,"result":{}}
    else:
        resp = {"jsonrpc":"2.0","id":999,"result":{}}
    print(json.dumps(resp), flush=True)
`
	if err := os.WriteFile(path, []byte(src), 0755); err != nil {
		t.Fatal(err)
	}

	g := NewGuardian(path)
	// initialize itself sends id=1 and the stub answers id=999, so
	// Start should fail with the mismatch error.
	err := g.Start()
	if err == nil {
		g.Stop()
		t.Fatal("Start succeeded despite response id mismatch")
	}
	if !strings.Contains(err.Error(), "id mismatch") {
		t.Errorf("error = %v, want 'id mismatch'", err)
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

// makeSlowStub writes a guardian stub that responds to initialize /
// tools/list immediately but sleeps for `sleepSecs` seconds before
// answering tools/call. Used to exercise CallToolContext cancellation
// against a tool call that hangs on the upstream side.
func makeSlowStub(t *testing.T, sleepSecs int) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "slow-guardian")
	src := `#!/usr/bin/env python3
import sys, json, time
SLEEP = ` + fmt.Sprintf("%d", sleepSecs) + `
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
            {"name":"slow","description":"sleeps","inputSchema":{}}
        ]}}
    elif method == "tools/call":
        time.sleep(SLEEP)
        resp = {"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":"done"}]}}
    else:
        resp = {"jsonrpc":"2.0","id":rid,"error":{"code":-32601,"message":"nope"}}
    print(json.dumps(resp), flush=True)
`
	if err := os.WriteFile(path, []byte(src), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGuardian_CallToolContextRespectsCancel verifies that cancelling
// the context unblocks an in-flight tool call within bounded time by
// killing the child process. Regression for the v0.6.0 user report
// that MCP tool calls could not be Aborted from the chat UI.
func TestGuardian_CallToolContextRespectsCancel(t *testing.T) {
	stub := makeSlowStub(t, 10) // 10 s upstream sleep
	g := NewGuardian(stub)
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := g.CallToolContext(ctx, "slow", json.RawMessage(`{}`))
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Should return shortly after the 100 ms cancel fires — far
	// less than the 10 s upstream sleep. 2 s gives plenty of slack
	// for slow CI machines.
	if elapsed > 2*time.Second {
		t.Errorf("CallToolContext took %s after cancel; want < 2s", elapsed)
	}
}

// TestGuardian_CallToolContextSucceedsWhenFast verifies that a normal
// fast-returning call works through CallToolContext exactly like
// CallTool when the context is not cancelled.
func TestGuardian_CallToolContextSucceedsWhenFast(t *testing.T) {
	stub := makeStub(t)
	g := NewGuardian(stub)
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := g.CallToolContext(ctx, "hello", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallToolContext: %v", err)
	}
	if !strings.Contains(string(res), "ok") {
		t.Errorf("result = %s", res)
	}
}

// TestGuardian_CallToolContextDoesNotLeakGoroutine verifies that the
// orphan goroutine spawned inside CallToolContext exits after Stop
// unblocks its Scan. We assert by polling NumGoroutine post-cancel
// rather than tracking the buffered channel directly (which is
// internal to the method).
func TestGuardian_CallToolContextDoesNotLeakGoroutine(t *testing.T) {
	stub := makeSlowStub(t, 10)
	g := NewGuardian(stub)
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Stop()

	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := g.CallToolContext(ctx, "slow", json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// The orphan goroutine inside CallToolContext should exit
	// shortly after Stop unblocks the inner Scan. Poll for up to
	// 2 s — runtime.NumGoroutine is racy by nature so we accept
	// "back to baseline ± small drift" rather than equality.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines did not return to baseline: have %d, baseline %d", runtime.NumGoroutine(), baseline)
}

// writeExecStub writes an executable shell script and returns its path.
func writeExecStub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stub-guardian")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGuardian_StartReportsSignalKilled verifies ADR-0026: a guardian
// that is killed by a signal before answering initialize fails with an
// error that names the exit signal plus the Gatekeeper hint — instead of
// the opaque "read response: EOF". Models macOS SIGKILLing a
// quarantined / cloud-synced binary.
func TestGuardian_StartReportsSignalKilled(t *testing.T) {
	g := NewGuardian(writeExecStub(t, "kill -KILL $$"))
	err := g.Start()
	if err == nil {
		t.Fatal("expected Start to fail when the guardian is killed")
	}
	if !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("error should name the exit signal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "exited before initialising") {
		t.Errorf("error should mention the process exit, got: %v", err)
	}
	if !strings.Contains(err.Error(), "codesign") {
		t.Errorf("error should carry the Gatekeeper hint for signal: killed, got: %v", err)
	}
}

// TestGuardian_StartReportsExitStatus verifies a non-signal early exit
// surfaces the exit status and does NOT add the Gatekeeper hint.
func TestGuardian_StartReportsExitStatus(t *testing.T) {
	g := NewGuardian(writeExecStub(t, "exit 3"))
	err := g.Start()
	if err == nil {
		t.Fatal("expected Start to fail when the guardian exits non-zero")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error should name the exit status, got: %v", err)
	}
	if strings.Contains(err.Error(), "codesign") {
		t.Errorf("non-signal exit must not get the Gatekeeper hint, got: %v", err)
	}
}

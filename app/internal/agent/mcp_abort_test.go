package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/mcp"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// makeSlowMCPStub writes a Python guardian stub that sleeps for
// `sleepSecs` seconds before responding to tools/call. Mirrors
// internal/mcp/guardian_test.go's helper of the same shape — kept
// local to the agent package because integration tests can't reach
// across packages for unexported helpers and the upfront duplication
// (~30 lines) is cheaper than cross-package test plumbing.
func makeSlowMCPStub(t *testing.T, sleepSecs int) string {
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

// TestAgent_AbortDuringMCPTool exercises the v0.6.1 fix for the user-
// reported regression that the chat could not be aborted while an MCP
// tool call was in flight. Cancelling the context passed to executeTool
// must surface (Cancelled by user) and trigger a per-guardian restart
// so the next user turn sees a fresh process.
func TestAgent_AbortDuringMCPTool(t *testing.T) {
	stubPath := makeSlowMCPStub(t, 10) // 10 s upstream sleep

	cfg := config.Default()
	cfg.Tools.MCPProfiles = []config.MCPProfileConfig{{
		Name:        "test-mcp",
		Binary:      stubPath,
		ProfilePath: "noop",
		Enabled:     true,
	}}
	a := New(cfg)
	a.session = &memory.Session{ID: "test", Records: []memory.Record{}}

	// Bring up the guardian manually rather than via startGuardians
	// (which would require validateProfilePath to accept the stub
	// path — easier to inject directly).
	g := mcp.NewGuardian(stubPath)
	g.SetName("test-mcp")
	if err := g.Start(); err != nil {
		t.Fatalf("Guardian.Start: %v", err)
	}
	defer func() {
		// Cleanup whatever guardian (original or restarted) lives in
		// the map at the end of the test.
		a.guardiansMu.Lock()
		for _, gg := range a.guardians {
			gg.Stop()
		}
		a.guardiansMu.Unlock()
	}()

	a.guardiansMu.Lock()
	a.guardians["test-mcp"] = g
	a.mcpStatuses = []MCPStatus{{Name: "test-mcp", Status: "running", ToolCount: 1}}
	original := a.guardians["test-mcp"]
	a.guardiansMu.Unlock()

	// Cancel the context shortly after dispatching so executeTool
	// goes through the CallToolContext cancel path.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	tc := llm.ToolCall{
		ID:        "tc-1",
		Name:      "mcp__test-mcp__slow",
		Arguments: `{}`,
	}

	start := time.Now()
	result, status := a.executeTool(ctx, tc)
	elapsed := time.Since(start)

	if result != "(Cancelled by user)" {
		t.Errorf("result = %q, want %q", result, "(Cancelled by user)")
	}
	if status != ActivityStatusError {
		t.Errorf("status = %v, want ActivityStatusError", status)
	}
	if elapsed > 2*time.Second {
		t.Errorf("executeTool took %s after cancel; want < 2s", elapsed)
	}

	// Wait for the async restartGuardian goroutine to swap the map
	// entry. Guardian Start does initialize + tools/list against the
	// stub, which is fast (~50–500 ms in practice); 5 s is plenty.
	deadline := time.Now().Add(5 * time.Second)
	var replaced *mcp.Guardian
	for time.Now().Before(deadline) {
		a.guardiansMu.RLock()
		replaced = a.guardians["test-mcp"]
		a.guardiansMu.RUnlock()
		if replaced != nil && replaced != original {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if replaced == nil {
		t.Fatal("restartGuardian: map entry never repopulated")
	}
	if replaced == original {
		t.Error("restartGuardian: map entry still points at the killed guardian")
	}

	// MCPStatuses should reflect the live (restarted) guardian.
	a.guardiansMu.RLock()
	statuses := append([]MCPStatus(nil), a.mcpStatuses...)
	a.guardiansMu.RUnlock()
	if len(statuses) != 1 || statuses[0].Name != "test-mcp" {
		t.Fatalf("mcpStatuses = %#v, want one entry for test-mcp", statuses)
	}
	if statuses[0].Status != "running" {
		t.Errorf("mcpStatuses[0].Status = %q, want %q", statuses[0].Status, "running")
	}
}

// TestAgent_RestartGuardian_RemovesEntryWhenConfigGone covers the
// edge case where the config no longer contains the named profile
// by the time restartGuardian runs (race against a Settings save).
// The dead map entry should be dropped without spawning a replacement.
func TestAgent_RestartGuardian_RemovesEntryWhenConfigGone(t *testing.T) {
	cfg := config.Default()
	// No MCPProfiles in config.
	a := New(cfg)

	// Inject a sentinel guardian under a name the config doesn't know
	// about. Calling restartGuardian should delete it.
	g := mcp.NewGuardian("/nonexistent")
	a.guardiansMu.Lock()
	a.guardians["ghost"] = g
	a.mcpStatuses = []MCPStatus{{Name: "ghost", Status: "running", ToolCount: 0}}
	a.guardiansMu.Unlock()

	a.restartGuardian("ghost")

	a.guardiansMu.RLock()
	_, exists := a.guardians["ghost"]
	a.guardiansMu.RUnlock()
	if exists {
		t.Error("guardian map still has entry for 'ghost' after restartGuardian")
	}
}


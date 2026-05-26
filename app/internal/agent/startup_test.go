package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// TestNewDefersBackgroundInit verifies that agent.New does not perform
// the externally-blocking sandbox/guardian initialisation (ADR-0024
// Part B) and that StartBackground claims the sandbox boot-init guard
// (§3.2). With the default config (sandbox disabled, no MCP profiles)
// neither step touches an external system, so the test is hermetic.
func TestNewDefersBackgroundInit(t *testing.T) {
	a := New(config.Default())

	if a.getSandbox() != nil {
		t.Fatal("New must not initialise the sandbox; init is deferred to StartBackground")
	}
	a.sandboxMu.RLock()
	started := a.sandboxStarted
	a.sandboxMu.RUnlock()
	if started {
		t.Fatal("sandboxStarted must be false before StartBackground runs")
	}

	a.StartBackground(context.Background())

	a.sandboxMu.RLock()
	started = a.sandboxStarted
	a.sandboxMu.RUnlock()
	if !started {
		t.Fatal("StartBackground must claim the sandbox boot-init guard")
	}
}

// TestSandboxAccessRaceFree exercises concurrent reads and writes of
// a.sandbox to prove sandboxMu guards the field (ADR-0024 §3.1). It is
// the regression guard for moving sandbox init onto a goroutine; run it
// under `go test -race`.
func TestSandboxAccessRaceFree(t *testing.T) {
	a := New(config.Default())
	fe := newFakeEngine(t)

	const iters = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: mimics StartBackground / RestartSandbox flipping the
	// engine in and out.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i%2 == 0 {
				a.setSandbox(fe)
			} else {
				a.setSandbox(nil)
			}
		}
	}()

	// Reader: mimics the descriptor-view gate / tool handlers reading
	// the engine concurrently.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = a.getSandbox()
		}
	}()

	wg.Wait()
}

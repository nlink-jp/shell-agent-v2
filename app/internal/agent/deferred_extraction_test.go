package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// TestDeferredExtraction_UIUnlocksBeforeExtraction verifies the
// core ADR-0015 contract: after a turn completes, the agent
// transitions back to StateIdle *before* the memory-extraction
// goroutine returns, so the UI can unlock while extraction is
// still in flight.
//
// We invoke postResponseTasks directly (it's in the same package)
// rather than driving a full Send, because Send pulls in mock
// backend / chat / objstore setup that isn't relevant to this
// state-machine test. The session is left nil — generateTitleIfNeeded
// short-circuits on nil session and returns immediately, which
// is exactly what we want for the title side.
func TestDeferredExtraction_UIUnlocksBeforeExtraction(t *testing.T) {
	a := New(config.Default())

	// Hold the extraction in flight until the test releases the
	// channel. The override is invoked under trackBg, so the
	// goroutine respects ctx cancellation if the test ever
	// hits Abort (it doesn't here).
	release := make(chan struct{})
	a.extractMemoriesOverride = func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Drive the state machine into Busy as Send would do.
	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	a.postResponseTasks(context.Background())

	// State should drop to Idle once title completes (instant
	// here — nil session early-return). Extraction is still
	// held by `release`. Poll with a short timeout to absorb
	// goroutine scheduling.
	if !waitFor(50*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.state == StateIdle
	}) {
		t.Fatal("state did not transition to Idle within 2s — title may be wedged")
	}

	a.mu.Lock()
	inFlight := a.extractionInFlight
	state := a.state
	a.mu.Unlock()

	if state != StateIdle {
		t.Errorf("state = %q, want StateIdle (extraction must not gate UI)", state)
	}
	if !inFlight {
		t.Errorf("extractionInFlight = false, want true (we're still holding the extraction)")
	}

	// Release extraction. It should clear extractionInFlight.
	close(release)
	if !waitFor(50*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.extractionInFlight
	}) {
		t.Fatal("extractionInFlight stayed true after release")
	}
}

// TestDeferredExtraction_RealExtractorPathIsDefault is a
// regression guard: the production path must use the real
// extractMemories method when the override is nil. We can't
// run the real extractor in a unit test without a configured
// backend, but we can assert the override slot is nil after
// New() so we don't accidentally ship the test hook engaged.
func TestDeferredExtraction_RealExtractorPathIsDefault(t *testing.T) {
	a := New(config.Default())
	if a.extractMemoriesOverride != nil {
		t.Fatal("New() must not pre-populate extractMemoriesOverride")
	}
}

// waitFor polls fn at interval until it returns true or timeout
// elapses. Helper local to this test file; the existing test
// helpers in the package are build-tag-gated to integration
// builds (lmstudio / vertexai).
func waitFor(interval, timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(interval)
	}
	return fn()
}

// recordEventCounts wraps an atomic counter for use as a quick
// event-emission spy in deferred-extraction tests added in
// later commits in this series. Kept here so the helper is
// shared across the test file series without leaking into
// production.
type eventCounter struct {
	n atomic.Int64
}

func (e *eventCounter) Inc()          { e.n.Add(1) }
func (e *eventCounter) Count() int64  { return e.n.Load() }

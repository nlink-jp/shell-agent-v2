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

// TestQueuedSend_HeldDuringExtraction is the structural happy
// path for ADR-0015's queue: send a turn, hold the extraction,
// fire a second SEND, observe that the second SEND lands in the
// queue slot rather than starting (or being rejected with
// ErrBusy). Auto-dispatch is exercised by the next test.
func TestQueuedSend_HeldDuringExtraction(t *testing.T) {
	a := New(config.Default())
	a.baseCtx = context.Background()

	release := make(chan struct{})
	a.extractMemoriesOverride = func(ctx context.Context) error {
		<-release
		return nil
	}

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	a.postResponseTasks(context.Background())

	// Wait until extraction is in flight (state Idle + flag set).
	if !waitFor(20*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.state == StateIdle && a.extractionInFlight
	}) {
		t.Fatal("extraction never entered in-flight window")
	}

	// A SEND now should be queued, not rejected.
	result, err := a.SendWithAttachments(context.Background(), "queued message", nil, nil, nil)
	if err != nil {
		t.Fatalf("SendWithAttachments returned error during extraction: %v", err)
	}
	if result != "QUEUED" {
		t.Errorf("result = %q, want \"QUEUED\"", result)
	}

	a.mu.Lock()
	q := a.queuedSend
	a.mu.Unlock()
	if q == nil {
		t.Fatal("queuedSend is nil — SEND was not captured")
	}
	if q.Message != "queued message" {
		t.Errorf("queuedSend.Message = %q, want \"queued message\"", q.Message)
	}

	close(release)
}

// TestQueuedSend_OverwriteMostRecentWins fires three SENDs in
// rapid succession while extraction is held; only the most
// recent should end up in the queue slot.
func TestQueuedSend_OverwriteMostRecentWins(t *testing.T) {
	a := New(config.Default())
	a.baseCtx = context.Background()

	release := make(chan struct{})
	a.extractMemoriesOverride = func(ctx context.Context) error {
		<-release
		return nil
	}

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	a.postResponseTasks(context.Background())

	if !waitFor(20*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.state == StateIdle && a.extractionInFlight
	}) {
		t.Fatal("extraction never entered in-flight window")
	}

	for _, m := range []string{"first", "second", "third"} {
		if _, err := a.SendWithAttachments(context.Background(), m, nil, nil, nil); err != nil {
			t.Fatalf("SendWithAttachments(%q): %v", m, err)
		}
	}

	a.mu.Lock()
	q := a.queuedSend
	a.mu.Unlock()
	if q == nil {
		t.Fatal("queuedSend is nil")
	}
	if q.Message != "third" {
		t.Errorf("queuedSend.Message = %q, want \"third\" (most-recent-wins)", q.Message)
	}

	close(release)
}

// TestAbortClearsQueue verifies Abort drops both the in-flight
// extraction and any pending queued SEND (ADR-0015 §3.4).
func TestAbortClearsQueue(t *testing.T) {
	a := New(config.Default())
	a.baseCtx = context.Background()

	released := make(chan struct{})
	a.extractMemoriesOverride = func(ctx context.Context) error {
		<-ctx.Done() // wait for Abort to cancel us
		close(released)
		return ctx.Err()
	}

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	a.postResponseTasks(context.Background())

	// Wait for extraction in flight.
	if !waitFor(20*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.extractionInFlight
	}) {
		t.Fatal("extraction never entered in-flight window")
	}

	// Queue a SEND under the lock (production path is
	// SendWithAttachments, but that's exercised elsewhere; here
	// we want to verify Abort clears whatever's in the slot).
	a.mu.Lock()
	a.queuedSend = &queuedSend{Message: "should-be-cleared", QueuedAt: time.Now()}
	a.mu.Unlock()

	a.Abort()

	// Abort should:
	//   1. Cancel the extraction goroutine's ctx (released channel closes)
	//   2. Clear queuedSend immediately
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("extraction goroutine did not see ctx cancellation within 2s")
	}

	a.mu.Lock()
	q := a.queuedSend
	a.mu.Unlock()
	if q != nil {
		t.Errorf("queuedSend should be nil after Abort, got %+v", q)
	}
}

// TestIsBusyDuringExtraction verifies the agent reports busy
// (via IsExtractionInFlight / HasQueuedSend getters) while
// extraction is in flight even though state == StateIdle.
// Bindings.IsBusy ORs these together to gate app-quit, so this
// is the load-bearing invariant for the OnBeforeClose path.
func TestIsBusyDuringExtraction(t *testing.T) {
	a := New(config.Default())
	a.baseCtx = context.Background()

	release := make(chan struct{})
	a.extractMemoriesOverride = func(ctx context.Context) error {
		<-release
		return nil
	}

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	a.postResponseTasks(context.Background())

	// Wait until state has settled to Idle with extraction
	// in flight.
	if !waitFor(20*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.state == StateIdle && a.extractionInFlight
	}) {
		t.Fatal("extraction never entered in-flight window")
	}

	if !a.IsExtractionInFlight() {
		t.Error("IsExtractionInFlight reports false during the in-flight window")
	}
	if a.HasQueuedSend() {
		t.Error("HasQueuedSend reports true with empty queue")
	}

	// Add a queued SEND and confirm HasQueuedSend flips.
	a.mu.Lock()
	a.queuedSend = &queuedSend{Message: "x", QueuedAt: time.Now()}
	a.mu.Unlock()
	if !a.HasQueuedSend() {
		t.Error("HasQueuedSend should report true with non-nil queuedSend")
	}

	close(release)
}

// TestQueuedSend_ExtractionErrorStillDispatches asserts that a
// failure in extractMemories does not prevent the queued SEND
// from auto-dispatching once the extraction goroutine returns.
// The user's queued message must not be lost because the
// background bookkeeping ran into an LLM error.
//
// We can't observe the full dispatch end-to-end without driving
// the agentLoop (which requires a configured backend), but we
// can observe that queuedSend is cleared and that the agent
// transitions back through StateBusy briefly (which the
// dispatch goroutine triggers). Once a real backend isn't
// configured, the dispatched SendWithAttachments returns very
// quickly with an error of its own — that's fine for this
// test; we only care that the dispatch attempt happens.
func TestQueuedSend_ExtractionErrorStillDispatches(t *testing.T) {
	a := New(config.Default())
	a.baseCtx = context.Background()

	a.extractMemoriesOverride = func(ctx context.Context) error {
		return context.DeadlineExceeded // simulate extraction error
	}

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	a.postResponseTasks(context.Background())

	// Slip a queued SEND in before extraction finishes — easiest
	// way is to set it directly under the lock; the production
	// path goes via SendWithAttachments but the field semantics
	// are the same and racing with the goroutine is harder to
	// orchestrate in a test.
	if !waitFor(20*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.extractionInFlight
	}) {
		t.Fatal("extraction never entered in-flight window")
	}
	a.mu.Lock()
	a.queuedSend = &queuedSend{Message: "post-error", QueuedAt: time.Now()}
	a.mu.Unlock()

	// After extraction returns (with the simulated error), the
	// completion goroutine should clear queuedSend and dispatch.
	// Observe by waiting for queuedSend to become nil — the
	// dispatch goroutine clears it before invoking SendWithAttachments.
	if !waitFor(20*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.queuedSend == nil
	}) {
		t.Fatal("queuedSend was not consumed by auto-dispatch")
	}
}

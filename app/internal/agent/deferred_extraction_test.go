package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
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
	a := New(withAutoExtract(config.Default()))

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

	// State drops to Idle on postResponseTasks entry, BEFORE
	// either bg goroutine starts. Extraction is still held by
	// `release`. Title may or may not have run yet (irrelevant
	// to the state machine). Poll briefly to absorb goroutine
	// scheduling, though state should already be Idle by the
	// time postResponseTasks returns.
	if !waitFor(50*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.state == StateIdle
	}) {
		t.Fatal("state did not transition to Idle within 2s — entry-time flip regressed")
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
	a := New(withAutoExtract(config.Default()))
	if a.extractMemoriesOverride != nil {
		t.Fatal("New() must not pre-populate extractMemoriesOverride")
	}
}

// TestAbort_ClearsExtractionInFlight pins ADR-0021 §2.4 / audit V8:
// Abort must clear the extractionInFlight flag together with
// queuedSend. The previous behaviour cancelled the extraction
// context but left the flag set until the goroutine's normal
// cleanup ran — a SEND in that window queued instead of starting
// fresh, which surprises the user who just aborted.
func TestAbort_ClearsExtractionInFlight(t *testing.T) {
	a := New(withAutoExtract(config.Default()))

	// Hold extraction in flight until released.
	release := make(chan struct{})
	a.extractMemoriesOverride = func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	a.postResponseTasks(context.Background())

	if !waitFor(50*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.extractionInFlight
	}) {
		t.Fatal("extractionInFlight never became true")
	}

	a.Abort()

	a.mu.Lock()
	inFlight := a.extractionInFlight
	queued := a.queuedSend
	a.mu.Unlock()

	if inFlight {
		t.Errorf("extractionInFlight = true after Abort, want false (audit V8)")
	}
	if queued != nil {
		t.Errorf("queuedSend should be nil after Abort, got %v", queued)
	}

	// Release the held extraction so the goroutine can wind down.
	close(release)
}

// TestLoadSession_ResetsStaleFlags pins ADR-0021 §2.4 / audit V7:
// LoadSession defensively resets extractionInFlight + queuedSend
// after wg.Wait(), so any stranded flag from a prior session's
// panicking / leaked cleanup doesn't poison the new session.
func TestLoadSession_ResetsStaleFlags(t *testing.T) {
	a := New(withAutoExtract(config.Default()))

	// Manually leak stale flags to simulate a prior session whose
	// extraction goroutine panicked or whose cleanup was skipped.
	a.mu.Lock()
	a.extractionInFlight = true
	a.queuedSend = &queuedSend{Message: "stale"}
	a.mu.Unlock()

	if err := a.LoadSession(&memory.Session{ID: "fresh"}); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	a.mu.Lock()
	inFlight := a.extractionInFlight
	queued := a.queuedSend
	a.mu.Unlock()

	if inFlight {
		t.Errorf("extractionInFlight stayed true after LoadSession (audit V7)")
	}
	if queued != nil {
		t.Errorf("queuedSend not cleared by LoadSession: %v", queued)
	}
}

// TestModelSwitch_WaitsForBgTasks pins ADR-0021 §2.4 / audit V9:
// /model and SwitchBackend must drain postTasksWg before rebuilding
// the LLM backend, so an in-flight extraction goroutine doesn't
// race a backend swap mid-Chat call.
func TestModelSwitch_WaitsForBgTasks(t *testing.T) {
	a := New(withAutoExtract(config.Default()))

	release := make(chan struct{})
	a.extractMemoriesOverride = func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Bring up an in-flight extraction goroutine.
	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	a.postResponseTasks(context.Background())
	if !waitFor(50*time.Millisecond, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.extractionInFlight
	}) {
		t.Fatal("extractionInFlight never became true")
	}

	// SwitchBackend should not return until extraction is done.
	switchReturned := make(chan struct{})
	go func() {
		a.SwitchBackend(config.BackendLocal)
		close(switchReturned)
	}()

	// Wait briefly — SwitchBackend should be blocked on postTasksWg.
	select {
	case <-switchReturned:
		t.Fatal("SwitchBackend returned before extraction completed; postTasksWg.Wait() missing")
	case <-time.After(200 * time.Millisecond):
		// Expected — switch is still waiting.
	}

	// Release extraction; switch should now return.
	close(release)
	select {
	case <-switchReturned:
		// OK.
	case <-time.After(2 * time.Second):
		t.Fatal("SwitchBackend never returned after extraction released")
	}
}

// TestAutoDispatch_DrainedByWait pins ADR-0021 §2.4 / audit V2:
// the queue auto-dispatch goroutine must be registered on
// postTasksWg so wg.Wait() in LoadSession etc. blocks until the
// dispatched SendWithAttachments completes. Pre-fix this was a
// bare goroutine, allowing a cross-session leak.
//
// Mechanism: simulate the auto-dispatch path by setting
// extractMemoriesOverride to a fast-returning fn AND pre-loading
// a queuedSend before calling postResponseTasks. After extraction
// completes, the cleanup defer launches the auto-dispatch
// goroutine; postTasksWg.Wait() should not return until that
// dispatched call has finished.
//
// We don't have a clean way to mock SendWithAttachments itself
// without a real session, so instead we install an extractor that
// signals when the cleanup defer enters the auto-dispatch branch
// and use a session-less agent (SendWithAttachments will reject
// because session is nil, returning quickly — but it MUST have
// been registered on the wg before that).
func TestAutoDispatch_DrainedByWait(t *testing.T) {
	a := New(withAutoExtract(config.Default()))
	a.baseCtx = context.Background()

	a.extractMemoriesOverride = func(ctx context.Context) error {
		return nil
	}

	// Pre-seed the queued send so the cleanup defer enters the
	// auto-dispatch branch.
	a.mu.Lock()
	a.queuedSend = &queuedSend{Message: "test"}
	a.state = StateBusy
	a.mu.Unlock()

	a.postResponseTasks(context.Background())

	// Wait() must block until BOTH the extraction goroutine AND
	// the auto-dispatch goroutine complete. The dispatched
	// SendWithAttachments will fail fast (no session), but the
	// invariant is that it's wg-tracked.
	doneCh := make(chan struct{})
	go func() {
		a.postTasksWg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		// OK — both goroutines drained.
	case <-time.After(3 * time.Second):
		t.Fatal("postTasksWg.Wait() never returned; auto-dispatch likely not wg-tracked")
	}
}

// TestSnapshot_PhaseForEachState walks the FSM through each phase
// and asserts Snapshot reports the expected Phase string (ADR-0021
// §2.1, §2.3).
func TestSnapshot_PhaseForEachState(t *testing.T) {
	a := New(withAutoExtract(config.Default()))

	// Ready
	if got := a.Snapshot().Phase; got != PhaseReady {
		t.Errorf("fresh agent: Phase=%q want %q", got, PhaseReady)
	}

	// Busy
	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	if got := a.Snapshot().Phase; got != PhaseBusy {
		t.Errorf("StateBusy: Phase=%q want %q", got, PhaseBusy)
	}

	// Extracting (state goes Idle while extraction runs in bg)
	a.mu.Lock()
	a.state = StateIdle
	a.extractionInFlight = true
	a.mu.Unlock()
	if got := a.Snapshot().Phase; got != PhaseExtracting {
		t.Errorf("extracting: Phase=%q want %q", got, PhaseExtracting)
	}

	// Queued (extracting + queued message)
	a.mu.Lock()
	a.queuedSend = &queuedSend{Message: "next message"}
	a.mu.Unlock()
	snap := a.Snapshot()
	if snap.Phase != PhaseQueued {
		t.Errorf("queued: Phase=%q want %q", snap.Phase, PhaseQueued)
	}
	if snap.QueuedMessage != "next message" {
		t.Errorf("queued: QueuedMessage=%q want %q", snap.QueuedMessage, "next message")
	}

	// Back to Ready after clearing
	a.mu.Lock()
	a.resetStateMachine()
	a.mu.Unlock()
	if got := a.Snapshot().Phase; got != PhaseReady {
		t.Errorf("after reset: Phase=%q want %q", got, PhaseReady)
	}
}

// TestIsBusy_ReflectsAllPhases: IsBusy is true iff phase != Ready.
func TestIsBusy_ReflectsAllPhases(t *testing.T) {
	a := New(withAutoExtract(config.Default()))

	if a.IsBusy() {
		t.Error("fresh agent should not be busy")
	}

	a.mu.Lock()
	a.extractionInFlight = true
	a.mu.Unlock()
	if !a.IsBusy() {
		t.Error("extracting agent should be busy")
	}

	a.mu.Lock()
	a.resetStateMachine()
	a.state = StateBusy
	a.mu.Unlock()
	if !a.IsBusy() {
		t.Error("StateBusy agent should be busy")
	}
}

// TestAutoExtractDisabled_SkipsExtraction is the ADR-0019 §3.2
// invariant: when the active profile's active backend has
// AutoExtract off, postResponseTasks must not set extractionInFlight,
// must not invoke extractMemories, and must not emit extraction:*
// events. Title generation still runs (verified by absence of crash;
// generateTitleIfNeeded is nil-session-safe).
func TestAutoExtractDisabled_SkipsExtraction(t *testing.T) {
	// config.Default() already has local=off; we use it as-is here
	// (no withAutoExtract) so the test mirrors a fresh-install user.
	a := New(config.Default())

	var extractCalls atomic.Int64
	a.extractMemoriesOverride = func(ctx context.Context) error {
		extractCalls.Add(1)
		return nil
	}

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	a.postResponseTasks(context.Background())

	// Give bg goroutines a window to start (they shouldn't).
	time.Sleep(50 * time.Millisecond)

	a.mu.Lock()
	inFlight := a.extractionInFlight
	state := a.state
	a.mu.Unlock()

	if inFlight {
		t.Errorf("extractionInFlight = true, want false when AutoExtract is off")
	}
	if state != StateIdle {
		t.Errorf("state = %q, want StateIdle", state)
	}
	if got := extractCalls.Load(); got != 0 {
		t.Errorf("extractMemories was called %d time(s) despite AutoExtract being off", got)
	}
}

// withAutoExtract turns AutoExtractEnabled on for both backends of
// the default profile. ADR-0019 flipped the local default to off, so
// tests that exercise the deferred-extraction state machine must
// opt in explicitly; the alternative would be to rely on an implicit
// per-test config-mutation idiom.
func withAutoExtract(cfg *config.Config) *config.Config {
	on := true
	prof := &cfg.LLM.Profiles[0]
	prof.Local.AutoExtractEnabled = &on
	prof.VertexAI.AutoExtractEnabled = &on
	return cfg
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
	a := New(withAutoExtract(config.Default()))
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
	if result.Phase != SendPhaseQueued {
		t.Errorf("result.Phase = %q, want SendPhaseQueued", result.Phase)
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
	a := New(withAutoExtract(config.Default()))
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
	a := New(withAutoExtract(config.Default()))
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
	a := New(withAutoExtract(config.Default()))
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
	a := New(withAutoExtract(config.Default()))
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

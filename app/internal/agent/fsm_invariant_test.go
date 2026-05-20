package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// FSM invariants (ADR-0021 §2.1):
//
//   Phase           state    extractionInFlight   queuedSend
//   Ready           Idle     false                nil
//   Busy            Busy     false                nil
//   Extracting      Idle     true                 nil
//   Queued          Idle     true                 non-nil
//
// Invalid combinations (this file pins each as unreachable):
//   - (Busy, true, *)         — extraction overlapping with Busy
//   - (Idle, false, non-nil)  — queue without extracting
//   - (Busy, *, non-nil)      — queue during Busy
//
// The tests drive the agent through realistic sequences and assert
// the snapshot phase is one of the four valid values.

// TestFSM_BusyAndExtractingNeverCoexist asserts (Busy, true, *)
// never occurs under any sequence of public API calls.
//
// State==Busy enters via SendWithAttachments line 1002. By that
// point, the check at line 985 has ruled out extractionInFlight
// (or queued already), so the two cannot both be true at the
// moment state becomes Busy. Tests via snapshot polling under a
// Send goroutine.
func TestFSM_BusyAndExtractingNeverCoexist(t *testing.T) {
	a := newSnapshotTestAgent(t)

	// Snapshot polling in the background.
	stop := make(chan struct{})
	violations := make(chan AgentSnapshot, 16)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := a.Snapshot()
			a.mu.Lock()
			both := a.state == StateBusy && a.extractionInFlight
			a.mu.Unlock()
			if both {
				violations <- snap
			}
		}
	}()

	// Drive a Send + extraction + auto-dispatch cycle.
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
	time.Sleep(50 * time.Millisecond)
	close(release)
	a.postTasksWg.Wait()
	close(stop)

	select {
	case bad := <-violations:
		t.Fatalf("FSM invariant violation: state=Busy && extractionInFlight=true observed (phase=%q)", bad.Phase)
	default:
		// OK
	}
}

// TestFSM_QueuedRequiresExtracting asserts (Idle, false, non-nil)
// never occurs.
//
// queuedSend is written in two places: Send line 987 (under check
// `state==Idle && extractionInFlight`) and never elsewhere. It's
// cleared by the extraction cleanup (line ~2275) and by Abort
// (line 1051). Both clear under the same mu, so the invariant
// holds at every observable moment.
func TestFSM_QueuedRequiresExtracting(t *testing.T) {
	a := newSnapshotTestAgent(t)

	// Try to set up a queued send without extracting (should not
	// happen via the public API).
	a.mu.Lock()
	a.queuedSend = &queuedSend{Message: "test"}
	a.extractionInFlight = false
	a.state = StateIdle
	a.mu.Unlock()

	// Snapshot should still derive a valid phase even from this
	// manually-corrupted state — snapshotLocked's switch case order
	// gives "Idle && !extractingInFlight" → Ready, ignoring the
	// stale queue. That's the right defensive behaviour: phase
	// reflects what the FSM "really is", not the corrupted fields.
	snap := a.Snapshot()
	if snap.Phase != PhaseReady {
		t.Errorf("phase = %q, want %q for (Idle, false, non-nil) corruption", snap.Phase, PhaseReady)
	}

	// LoadSession should normalise this with resetStateMachine.
	if err := a.LoadSession(&memory.Session{ID: "x"}); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	a.mu.Lock()
	queued := a.queuedSend
	a.mu.Unlock()
	if queued != nil {
		t.Error("LoadSession should have cleared corrupted queuedSend")
	}
}

// TestFSM_QueuedNeverDuringBusy: (Busy, *, non-nil) is unreachable.
//
// queuedSend writes happen only in the StateIdle branch of Send
// (line 987 inside `if a.state == StateIdle && a.extractionInFlight`).
// State Busy + queuedSend non-nil could only arise by direct field
// mutation — same as above.
func TestFSM_QueuedNeverDuringBusy(t *testing.T) {
	a := newSnapshotTestAgent(t)

	// Send while busy — should reject.
	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	res, err := a.SendWithAttachments(context.Background(), "blocked", nil, nil, nil)
	if err == nil {
		t.Fatal("Send during StateBusy should return ErrBusy")
	}
	if res.Phase != SendPhaseError {
		t.Errorf("res.Phase = %q, want SendPhaseError", res.Phase)
	}
	a.mu.Lock()
	queued := a.queuedSend
	a.mu.Unlock()
	if queued != nil {
		t.Error("Send during StateBusy must not queue (invariant V11)")
	}
}

// TestExtractionPanic_RecoversAndClearsFlags pins the V4 fix:
// when extractMemories panics, trackBg's defer-recover converts
// it to an error and the extraction cleanup defer still runs,
// so extractionInFlight returns to false. wg.Wait can be relied on.
func TestExtractionPanic_RecoversAndClearsFlags(t *testing.T) {
	a := newSnapshotTestAgent(t)
	a.extractMemoriesOverride = func(ctx context.Context) error {
		panic("simulated extraction crash")
	}

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()
	a.postResponseTasks(context.Background())
	a.postTasksWg.Wait()

	snap := a.Snapshot()
	if snap.Phase != PhaseReady {
		t.Errorf("phase = %q after extraction panic, want %q", snap.Phase, PhaseReady)
	}
	a.mu.Lock()
	inFlight := a.extractionInFlight
	a.mu.Unlock()
	if inFlight {
		t.Error("extractionInFlight stayed true after panic (audit V4)")
	}
}

// TestSendDuringExtraction_ReturnsQueuedResult: per ADR-0021 §2.2,
// the response is authoritative — it must carry Phase=queued so
// the frontend doesn't depend on agent:extraction:started arriving
// before the response is processed (audit V1).
func TestSendDuringExtraction_ReturnsQueuedResult(t *testing.T) {
	a := newSnapshotTestAgent(t)

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
		t.Fatal("extraction never started")
	}

	res, err := a.SendWithAttachments(context.Background(), "during extraction", nil, nil, nil)
	if err != nil {
		t.Fatalf("Send during extraction: %v", err)
	}
	if res.Phase != SendPhaseQueued {
		t.Errorf("res.Phase = %q, want %q", res.Phase, SendPhaseQueued)
	}
	// QueuedAt must be set so the frontend can render an accurate
	// pill without waiting for the agent:queued event (audit V1).
	if res.QueuedAt == "" {
		t.Error("res.QueuedAt is empty; frontend can't render queue pill from the response alone")
	}
	// Should not have a Content field — the bug being prevented is
	// "QUEUED" being appended as an assistant bubble.
	if strings.Contains(res.Content, "QUEUED") {
		t.Errorf("res.Content carries QUEUED-as-text (audit V3): %q", res.Content)
	}

	close(release)
	a.postTasksWg.Wait()
}

// newSnapshotTestAgent builds an agent with auto-extract on and a
// non-nil baseCtx so postResponseTasks' auto-dispatch can fire if
// the test queues a send.
func newSnapshotTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := New(withAutoExtract(config.Default()))
	a.baseCtx = context.Background()
	return a
}

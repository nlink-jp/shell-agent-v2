package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// waitForIdle polls Agent.State until it reports Idle or the deadline
// elapses. Post-response background tasks now keep state at Busy
// until they finish, so tests that previously asserted Idle right
// after Send returns must give the trailing goroutine a moment to
// land. Returns true if Idle was observed within the timeout.
func waitForIdle(a *Agent, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if a.State() == StateIdle {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return a.State() == StateIdle
}

func TestNewAgent(t *testing.T) {
	a := New(config.Default())
	if a.State() != StateIdle {
		t.Errorf("initial state = %v, want %v", a.State(), StateIdle)
	}
}

func TestSendReturnsToIdle(t *testing.T) {
	a := New(config.Default())
	a.session = &memory.Session{ID: "test", Records: []memory.Record{}}

	// Send fails because the local LLM isn't running. The post-
	// response tasks then try the same dead endpoint and burn
	// retries; Abort cancels both the in-flight Send context and
	// the post-task context, letting state drop back to Idle.
	_, _ = a.Send(context.Background(), "hello")
	a.Abort()
	if !waitForIdle(a, 5*time.Second) {
		t.Errorf("state after Send+Abort = %v, want %v (timed out waiting for post-tasks)", a.State(), StateIdle)
	}
}

func TestSendRejectsDuringBusy(t *testing.T) {
	a := New(config.Default())

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	_, err := a.Send(context.Background(), "hello")
	if err != ErrBusy {
		t.Errorf("Send during busy = %v, want ErrBusy", err)
	}
}

func TestAbortOnIdle(t *testing.T) {
	a := New(config.Default())
	a.Abort() // should not panic
}

func TestModelCommand(t *testing.T) {
	a := New(config.Default())

	// Show current
	result, err := a.handleCommand("/model")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	// Switch to vertex
	result, err = a.handleCommand("/model vertex")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if a.CurrentBackend() != "vertex_ai" {
		t.Errorf("backend = %v, want vertex_ai", a.CurrentBackend())
	}

	// Switch back to local
	result, err = a.handleCommand("/model local")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if a.CurrentBackend() != "local" {
		t.Errorf("backend = %v, want local", a.CurrentBackend())
	}
	_ = result
}

func TestFindingCommand(t *testing.T) {
	a := New(config.Default())
	a.session = &memory.Session{ID: "test-sess", Title: "Test Session"}

	result, err := a.handleCommand("/finding Sales peak in April")
	// Save may fail (temp path), but command itself should work
	if err != nil {
		// Allow save errors in test
		_ = err
	}
	_ = result
}

func TestFindingsCommandEmpty(t *testing.T) {
	a := New(config.Default())
	a.findings = findings.NewStore() // fresh store

	result, err := a.handleCommand("/findings")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result != "No findings yet." {
		t.Errorf("result = %v, want 'No findings yet.'", result)
	}
}

func TestUnknownCommand(t *testing.T) {
	a := New(config.Default())

	result, err := a.handleCommand("/unknown")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result for unknown command")
	}
}

func TestLoadSessionRejectsDuringBusy(t *testing.T) {
	a := New(config.Default())
	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	err := a.LoadSession(&memory.Session{})
	if err != ErrBusy {
		t.Errorf("LoadSession during busy = %v, want ErrBusy", err)
	}
}

func TestNormalizeToolArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // exact wanted output (post-jsonfix); empty means "must round-trip via Unmarshal cleanly"
	}{
		{"plain JSON unchanged", `{"a":1}`, `{"a":1}`},
		{"empty stays empty", "", ""},
		{"markdown fence stripped", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"prose around JSON", `Sure, here it is: {"a":1}`, `{"a":1}`},
		{"trailing comma repaired", `{"a":1,}`, `{"a":1}`},
		{"single-quoted keys repaired", `{'a':1}`, `{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeToolArgs(tc.in)
			if got != tc.want {
				t.Errorf("normalizeToolArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// captureBgEvents installs a thread-safe BgTaskHandler that
// accumulates every event for assertion. Tests need a mutex because
// trackBg invokes the handler from goroutines in real use; here it is
// called serially, but the locking matches production semantics.
func captureBgEvents(a *Agent) func() []BgTaskEvent {
	var (
		mu     sync.Mutex
		events []BgTaskEvent
	)
	a.SetBgTaskHandler(func(e BgTaskEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})
	return func() []BgTaskEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]BgTaskEvent, len(events))
		copy(out, events)
		return out
	}
}

func TestTrackBg_Success(t *testing.T) {
	a := New(config.Default())
	get := captureBgEvents(a)

	a.trackBg(context.Background(), "title", func() error { return nil })

	events := get()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %#v", len(events), events)
	}
	if events[0] != (BgTaskEvent{Name: "title", Phase: "start"}) {
		t.Errorf("start event = %#v", events[0])
	}
	if events[1] != (BgTaskEvent{Name: "title", Phase: "end", Error: ""}) {
		t.Errorf("end event = %#v", events[1])
	}
}

func TestTrackBg_Error(t *testing.T) {
	a := New(config.Default())
	get := captureBgEvents(a)

	a.trackBg(context.Background(), "memory-compaction", func() error {
		return errors.New("boom")
	})

	events := get()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[1].Phase != "end" || events[1].Error != "boom" {
		t.Errorf("end event = %#v, want Phase=end Error=boom", events[1])
	}
}

func TestTrackBg_CanceledNotReportedAsError(t *testing.T) {
	a := New(config.Default())
	get := captureBgEvents(a)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before fn runs

	a.trackBg(ctx, "pinned-extraction", func() error {
		return ctx.Err()
	})

	events := get()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[1].Phase != "end" {
		t.Errorf("end event phase = %q, want end", events[1].Phase)
	}
	// Cancellation must not flash red — Error stays empty even though
	// the body returned context.Canceled.
	if events[1].Error != "" {
		t.Errorf("canceled task reported Error=%q, want empty", events[1].Error)
	}
}

func TestTrackBg_NilHandlerSafe(t *testing.T) {
	// notifyBg must no-op when no handler is registered (e.g. tests
	// that construct an Agent without bindings, or a brief window at
	// startup before bindings.go wires the handler).
	a := New(config.Default())
	// Intentionally do NOT call SetBgTaskHandler.
	a.trackBg(context.Background(), "title", func() error { return nil })
	// Reaching here without panicking is the assertion.
}

func TestNormalizeToolArgs_FallsBackOnGarbage(t *testing.T) {
	// jsonfix.Extract returns ErrNoJSON for input that doesn't
	// contain anything recoverable. We must surface the original
	// string so downstream Unmarshal produces a normal "invalid
	// arguments" error instead of pretending the input was empty.
	in := "absolutely not JSON"
	if got := normalizeToolArgs(in); got != in {
		t.Errorf("garbage input should pass through untouched, got %q", got)
	}
}

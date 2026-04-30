package llm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlink-jp/nlk/backoff"
)

// fakeBackend implements Backend with deterministic, programmable
// behavior so we can drive the retry layer without spinning real
// HTTP calls.
type fakeBackend struct {
	calls int32 // atomic
	// scenarios is a slice of "what attempt N returns": err first
	// (drives retry), nil last (drives success path). If exhausted,
	// fakeBackend keeps returning the last entry.
	scenarios []error
	// streamCalls counts ChatStream invocations separately so tests
	// can distinguish.
	streamCalls int32
	// onCall is called before each attempt for tests that need to
	// observe attemptCtx (e.g. assert that the per-attempt timeout
	// was applied).
	onCall func(ctx context.Context, attempt int)
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Chat(ctx context.Context, _ []Message, _ []ToolDef) (*Response, error) {
	n := atomic.AddInt32(&f.calls, 1) - 1
	if f.onCall != nil {
		f.onCall(ctx, int(n))
	}
	if int(n) < len(f.scenarios) {
		if err := f.scenarios[n]; err != nil {
			return nil, err
		}
	}
	return &Response{Content: "ok"}, nil
}

func (f *fakeBackend) ChatStream(ctx context.Context, _ []Message, _ []ToolDef, _ StreamCallback) (*Response, error) {
	atomic.AddInt32(&f.streamCalls, 1)
	return f.Chat(ctx, nil, nil)
}

// fastBackoff disables jitter and shortens base so retries don't
// slow tests down. Same shape as nlk/backoff defaults otherwise.
func fastBackoff() backoff.Backoff {
	return backoff.New(
		backoff.WithBase(1*time.Millisecond),
		backoff.WithMax(2*time.Millisecond),
		backoff.WithJitter(0),
	)
}

func TestWithRetry_SucceedsOnFirstAttempt(t *testing.T) {
	fb := &fakeBackend{}
	b := WithRetry(fb, RetryPolicy{MaxAttempts: 3, Backoff: fastBackoff()})

	resp, err := b.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want ok", resp.Content)
	}
	if fb.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry needed)", fb.calls)
	}
}

func TestWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	fb := &fakeBackend{
		scenarios: []error{
			errors.New("HTTP 503 service unavailable"),
			errors.New("connection reset by peer"),
			nil, // success on attempt 3
		},
	}
	b := WithRetry(fb, RetryPolicy{MaxAttempts: 3, Backoff: fastBackoff()})

	resp, err := b.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("expected eventual success, got: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q", resp.Content)
	}
	if fb.calls != 3 {
		t.Errorf("calls = %d, want 3", fb.calls)
	}
}

func TestWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	fb := &fakeBackend{
		scenarios: []error{
			errors.New("503"),
			errors.New("503"),
			errors.New("503"),
		},
	}
	b := WithRetry(fb, RetryPolicy{MaxAttempts: 3, Backoff: fastBackoff()})

	_, err := b.Chat(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error after max attempts")
	}
	if fb.calls != 3 {
		t.Errorf("calls = %d, want 3", fb.calls)
	}
}

func TestWithRetry_DoesNotRetryNonRetryableErrors(t *testing.T) {
	fb := &fakeBackend{
		scenarios: []error{errors.New("invalid auth token")},
	}
	b := WithRetry(fb, RetryPolicy{MaxAttempts: 5, Backoff: fastBackoff()})

	_, err := b.Chat(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if fb.calls != 1 {
		t.Errorf("calls = %d, want 1 (auth errors are not retryable)", fb.calls)
	}
}

func TestWithRetry_PerAttemptTimeoutFiresAndRetries(t *testing.T) {
	fb := &fakeBackend{
		// attempt 1 sleeps past the timeout; attempt 2 returns ok.
		onCall: func(ctx context.Context, attempt int) {
			if attempt == 0 {
				select {
				case <-ctx.Done():
				case <-time.After(50 * time.Millisecond):
				}
			}
		},
		scenarios: []error{
			context.DeadlineExceeded, // simulate the "expired" return
			nil,
		},
	}
	b := WithRetry(fb, RetryPolicy{
		MaxAttempts:       3,
		PerRequestTimeout: 5 * time.Millisecond,
		Backoff:           fastBackoff(),
	})

	resp, err := b.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("expected success after timeout retry: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q", resp.Content)
	}
	if fb.calls != 2 {
		t.Errorf("calls = %d, want 2", fb.calls)
	}
}

func TestWithRetry_HonoursCallerCancellation(t *testing.T) {
	fb := &fakeBackend{
		scenarios: []error{errors.New("503"), errors.New("503"), errors.New("503")},
	}
	b := WithRetry(fb, RetryPolicy{MaxAttempts: 5, Backoff: fastBackoff()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := b.Chat(ctx, nil, nil)
	if err == nil {
		t.Fatal("expected an error when caller cancelled")
	}
	// Should not have looped through all 5 attempts. fakeBackend
	// returns on attempt 1 with 503; the wrapper checks ctx.Err()
	// after the failure and bails.
	if fb.calls > 2 {
		t.Errorf("calls = %d, want ≤ 2 — should bail on caller cancel", fb.calls)
	}
}

func TestWithRetry_ChatStreamPathRetriesToo(t *testing.T) {
	fb := &fakeBackend{
		scenarios: []error{errors.New("RESOURCE_EXHAUSTED"), nil},
	}
	b := WithRetry(fb, RetryPolicy{MaxAttempts: 3, Backoff: fastBackoff()})

	_, err := b.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream eventual success expected: %v", err)
	}
	if fb.streamCalls != 2 {
		t.Errorf("streamCalls = %d, want 2", fb.streamCalls)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"http 429", fmt.Errorf("server returned 429 too many requests"), true},
		{"http 503", fmt.Errorf("server: 503 service unavailable"), true},
		{"vertex 429 quota", fmt.Errorf("Error 429, Message: Quota exceeded, Status: 429 Too Many Requests"), true},
		{"vertex unavailable text", fmt.Errorf("Error 503, Message: backend unavailable"), true},
		{"connection reset", fmt.Errorf("read tcp: connection reset"), true},
		{"i/o timeout", fmt.Errorf("dial: i/o timeout"), true},
		{"auth", fmt.Errorf("invalid token"), false},
		{"4xx other", fmt.Errorf("400 bad request: missing field"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithRetry_NameDelegates(t *testing.T) {
	fb := &fakeBackend{}
	b := WithRetry(fb, RetryPolicy{})
	if b.Name() != "fake" {
		t.Errorf("Name() = %q, want fake", b.Name())
	}
}

func TestDefaultRetryPolicy_HasNonZeroBackoff(t *testing.T) {
	p := DefaultRetryPolicy(120 * time.Second)
	if p.MaxAttempts < 2 {
		t.Errorf("MaxAttempts = %d, want ≥ 2", p.MaxAttempts)
	}
	if p.Backoff.Duration(0) <= 0 {
		t.Errorf("Backoff.Duration(0) = %v, want > 0", p.Backoff.Duration(0))
	}
	if p.PerRequestTimeout != 120*time.Second {
		t.Errorf("PerRequestTimeout = %v, want 120s", p.PerRequestTimeout)
	}
}

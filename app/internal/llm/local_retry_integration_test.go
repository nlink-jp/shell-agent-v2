package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlink-jp/nlk/backoff"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// flakyServer is a test HTTP server that returns the given sequence
// of statuses for successive requests. The last entry is held when
// the sequence is exhausted, so a single trailing 200 means
// "succeed forever after the listed failures".
func flakyServer(t *testing.T, statuses []int) (*httptest.Server, *int32) {
	t.Helper()
	var count int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		i := atomic.AddInt32(&count, 1) - 1
		s := statuses[len(statuses)-1]
		if int(i) < len(statuses) {
			s = statuses[i]
		}
		switch s {
		case 200:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		default:
			http.Error(w, fmt.Sprintf("simulated %d", s), s)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &count
}

// fastBackoffPolicy keeps total test runtime well under 1s even
// when 3 retries fire.
func fastBackoffPolicy(timeout time.Duration) RetryPolicy {
	return RetryPolicy{
		MaxAttempts:       3,
		PerRequestTimeout: timeout,
		Backoff: backoff.New(
			backoff.WithBase(2*time.Millisecond),
			backoff.WithMax(5*time.Millisecond),
			backoff.WithJitter(0),
		),
	}
}

func TestLocalWithRetry_503ThenSuccess(t *testing.T) {
	srv, calls := flakyServer(t, []int{503, 503, 200})
	inner := NewLocal(config.LocalConfig{Endpoint: srv.URL + "/v1", Model: "test"})
	b := WithRetry(inner, fastBackoffPolicy(2*time.Second))

	resp, err := b.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("expected eventual success, got: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("Content = %q, want hello", resp.Content)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("server saw %d calls, want 3 (2 failures + 1 success)", got)
	}
}

func TestLocalWithRetry_PersistentFailureGivesUp(t *testing.T) {
	srv, calls := flakyServer(t, []int{502, 502, 502, 502})
	inner := NewLocal(config.LocalConfig{Endpoint: srv.URL + "/v1", Model: "test"})
	b := WithRetry(inner, fastBackoffPolicy(2*time.Second))

	_, err := b.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected failure after 3 attempts")
	}
	if !strings.Contains(err.Error(), "gave up after 3 attempts") {
		t.Errorf("error should reference giveup, got: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("server saw %d calls, want 3 (max attempts)", got)
	}
}

func TestLocalWithRetry_4xxAuthDoesNotRetry(t *testing.T) {
	srv, calls := flakyServer(t, []int{401, 401, 401})
	inner := NewLocal(config.LocalConfig{Endpoint: srv.URL + "/v1", Model: "test"})
	b := WithRetry(inner, fastBackoffPolicy(2*time.Second))

	_, err := b.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	// 401 should NOT be retryable — auth failures don't fix
	// themselves on a redo.
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("server saw %d calls, want 1 (no retry on 401)", got)
	}
}

func TestLocalWithRetry_PerRequestTimeoutFires(t *testing.T) {
	// Server delays each response past the per-attempt timeout so
	// the client cancels and the wrapper retries. After
	// MaxAttempts the wrapper gives up and we verify the count.
	//
	// We don't block server-side on r.Context().Done() because
	// httptest.Server.Close() waiting for outstanding handlers can
	// keep that channel un-fired in some Go versions, deadlocking
	// the test. A bounded delay with t.Cleanup is robust.
	const handlerDelay = 200 * time.Millisecond
	const perAttemptTimeout = 30 * time.Millisecond
	mux := http.NewServeMux()
	var calls int32
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-time.After(handlerDelay):
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"late"}}]}`)
		case <-r.Context().Done():
			// client gave up; nothing more to do.
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	inner := NewLocal(config.LocalConfig{Endpoint: srv.URL + "/v1", Model: "test"})
	b := WithRetry(inner, fastBackoffPolicy(perAttemptTimeout))

	start := time.Now()
	_, err := b.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// 3 attempts × ~30ms timeout + small backoff between them.
	// Allow generous headroom for CI but bound it so a hang is
	// caught.
	if elapsed > 3*time.Second {
		t.Errorf("elapsed = %v, want < 3s (suggesting per-attempt timeout did fire)", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server saw %d calls, want 3 (timeout retried)", got)
	}
}

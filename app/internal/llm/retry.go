// retry.go — backend wrapper that adds per-request timeout, retry
// with backoff, and start/done logging around an underlying Backend.
//
// Why a wrapper instead of touching Local / Vertex directly: each
// backend already does enough work mapping roles, tools, and
// streaming. Layering retry + timeout + logging in one place keeps
// the underlying clients single-call and testable, and gives us a
// single seam to change retry policy in future without touching
// either backend.
//
// Per shell-agent-v2-rfp.md §3 ("nlk: …backoff…"), exponential
// backoff is supplied by github.com/nlink-jp/nlk/backoff.

package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/nlink-jp/nlk/backoff"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
)

// RetryPolicy controls the retry/timeout/backoff behaviour around
// an LLM call. Zero values produce a "no retries, no timeout"
// policy that simply delegates straight through (useful in tests).
type RetryPolicy struct {
	// PerRequestTimeout caps a single attempt. 0 = no per-attempt
	// timeout (the caller's ctx still applies).
	PerRequestTimeout time.Duration

	// MaxAttempts is the total number of attempts including the
	// first. 1 = no retries. 0 also disables retries.
	MaxAttempts int

	// Backoff is consulted between attempts via Backoff.Duration(i)
	// where i is the just-failed attempt index. The zero value
	// triggers the package-default backoff schedule (base 5s, 2×
	// growth capped at 60s, jitter ±10%).
	Backoff backoff.Backoff
}

// DefaultRetryPolicy returns the policy used when no explicit one
// is configured. base 5s, 2× growth capped at 60s, jitter ±10%,
// 3 attempts total. The constants live here so tests and Settings
// UI agree on what "default" means.
func DefaultRetryPolicy(perRequestTimeout time.Duration) RetryPolicy {
	return RetryPolicy{
		PerRequestTimeout: perRequestTimeout,
		MaxAttempts:       3,
		Backoff:           backoff.New(),
	}
}

// retryBackend wraps another Backend with retry+timeout+logging.
type retryBackend struct {
	inner  Backend
	policy RetryPolicy
}

// WithRetry wraps b so that Chat / ChatStream are subject to the
// given retry policy. Pass DefaultRetryPolicy(...) for the
// shell-agent-v2 default. The returned Backend reports the same
// Name() as the underlying backend so logs and tool-result
// attribution are unaffected.
func WithRetry(b Backend, p RetryPolicy) Backend {
	return &retryBackend{inner: b, policy: p}
}

func (r *retryBackend) Name() string { return r.inner.Name() }

func (r *retryBackend) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	op := func(c context.Context) (*Response, error) {
		return r.inner.Chat(c, messages, tools)
	}
	return r.do(ctx, op, "Chat")
}

func (r *retryBackend) ChatStream(ctx context.Context, messages []Message, tools []ToolDef, cb StreamCallback) (*Response, error) {
	op := func(c context.Context) (*Response, error) {
		return r.inner.ChatStream(c, messages, tools, cb)
	}
	return r.do(ctx, op, "ChatStream")
}

// do is the shared retry+timeout+logging loop. opName is used in
// log messages ("Chat" / "ChatStream").
func (r *retryBackend) do(ctx context.Context, op func(context.Context) (*Response, error), opName string) (*Response, error) {
	maxAttempts := r.policy.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	bo := r.policy.Backoff
	if bo.Duration(0) == 0 {
		// Zero-value Backoff produces zero durations on every
		// attempt — that turns a retry loop into a tight loop.
		// Use the package default as a safety net.
		bo = backoff.New()
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Per-attempt context with the configured timeout, derived
		// from the caller's ctx so cancellation still propagates.
		attemptCtx := ctx
		var cancel context.CancelFunc
		if r.policy.PerRequestTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, r.policy.PerRequestTimeout)
		}

		start := time.Now()
		logger.Info("llm: %s.%s start attempt=%d/%d timeout=%s",
			r.inner.Name(), opName, attempt+1, maxAttempts, r.policy.PerRequestTimeout)

		resp, err := op(attemptCtx)
		// If our per-attempt timeout fired, the underlying SDK
		// surfaces it in many shapes (context.DeadlineExceeded
		// directly, or a 499 / "cancelled" / "context canceled"
		// wrapping). Capture the authoritative signal before we
		// release the cancel func.
		attemptTimedOut := attemptCtx.Err() == context.DeadlineExceeded
		if cancel != nil {
			cancel()
		}
		dur := time.Since(start)

		if err == nil {
			logger.Info("llm: %s.%s done attempt=%d duration=%s tokens=%d/%d",
				r.inner.Name(), opName, attempt+1, dur, resp.PromptTokens, resp.OutputTokens)
			return resp, nil
		}

		retryable := attemptTimedOut || IsRetryable(err)
		logger.Info("llm: %s.%s err attempt=%d duration=%s retryable=%v timed_out=%v err=%v",
			r.inner.Name(), opName, attempt+1, dur, retryable, attemptTimedOut, err)

		lastErr = err

		// Caller-side abort: don't retry, surface immediately.
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(ctxErr, context.DeadlineExceeded) {
			return nil, err
		}
		if !retryable {
			return nil, err
		}
		if attempt+1 >= maxAttempts {
			break
		}

		// Wait per backoff, but bail early if the caller's ctx
		// fires while we're sleeping.
		wait := bo.Duration(attempt)
		logger.Info("llm: %s.%s backoff attempt=%d wait=%s",
			r.inner.Name(), opName, attempt+1, wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("llm: %s %s: gave up after %d attempts: %w",
		r.inner.Name(), opName, maxAttempts, lastErr)
}

// IsRetryable reports whether an error from a Backend call should
// be retried. Conservative: only well-known transient signals
// qualify. Unknown errors are NOT retried, on the grounds that an
// unknown failure is likely a bug or a permanent client error
// (auth, malformed request) where retry just spins.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Per-attempt timeout fired — retry with backoff.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Caller-cancelled — never retry.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Network-level transient failures (connection reset, timeout
	// at TCP layer, DNS hiccup mid-request etc.).
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// Fall back to substring matching against the error chain text.
	// SDK error types differ between Local (http) and Vertex
	// (google.golang.org/genai apiError) and are not exported, so
	// inspecting the message is the only portable path. Keep this
	// list tight to avoid false positives.
	msg := strings.ToLower(err.Error())
	for _, hint := range retryableHints {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// retryableHints lists substrings (lower-case) that indicate the
// failure was transient. Order doesn't matter; first match wins.
var retryableHints = []string{
	"429",                // raw HTTP status from local
	"499",                // client-closed-request — Vertex echoes this when our
	                      // own per-attempt cancellation lands as the response
	"500", "502", "503", "504", // server-side, often transient
	"resource_exhausted", // gRPC code 8 / Vertex quota
	"unavailable",        // gRPC code 14
	"deadline_exceeded",  // gRPC code 4
	"deadline exceeded",  // also "context deadline exceeded"
	"cancelled",          // Vertex APIError.Status when client closed
	"connection reset",
	"connection refused",
	"i/o timeout",
	"eof", // mid-stream tear-down on the network side
}

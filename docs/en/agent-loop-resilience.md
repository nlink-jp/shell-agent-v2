# Agent Loop Resilience — Design Document

> Date: 2026-05-01
> Status: Shipped in v0.1.16 (Feature 2 in 8ea5db1, Feature 1 in 180911e)
> Scope: Two adjacent observability / self-correction
> features that share the agent-loop seam

## 1. Problem

The TODO file already records two patterns observed during
real Vertex sessions but not yet handled:

1. **Stuck loop on the same error.** Vertex (gemini-2.5-flash)
   produced Python with a multi-line string literal that
   wasn't escaped, hit `SyntaxError: unterminated string
   literal`, then called `sandbox-run-python` six rounds in a
   row with the same broken pattern before `max rounds (10)
   reached` killed the session. Each retry was a trivial
   variation; the model didn't recognise the actual cause.

2. **Silent 429 backoffs.** The same session produced two
   `RESOURCE_EXHAUSTED` errors mid-conversation. The retry
   layer handled them correctly (backoff 4.8s and 5.7s,
   succeeded on attempt 2 in both cases), but the UI showed
   nothing — the user only saw a "Thinking…" indicator that
   was slower than usual, with no way to know a retry was
   in progress.

Both are observability gaps at the agent-loop / LLM-call seam.
Neither changes the core control flow; both add a feedback
hook in one well-defined spot.

## 2. Goals & Non-goals

### Goals

1. When the LLM calls the same tool with `status=error` three
   rounds in a row, inject a transient corrective hint into
   the next LLM message list so the model sees it's stuck.
2. When the retry layer enters a backoff (transient failure
   detected, waiting before the next attempt), surface a
   short user-visible status badge so the user knows a slow
   round is a backoff, not a hang.
3. Neither feature changes any persisted state. Both are
   observability hooks layered on top of existing flow.

### Non-goals

- **No new retry policy.** Existing exponential backoff +
  3-attempt cap stays as-is.
- **No new max-round handling.** The hardcoded 10-round cap
  is a separate TODO, not addressed here.
- **No rollback / state mutation.** The corrective hint is
  a one-time advisory; the model still drives the next call.
- **No new UI panel.** Just a small footer-status badge for
  the backoff signal.

## 3. Feature 1 — Loop detection & corrective hint

### 3.1 Detection

`Agent` gains a small ring buffer:

```go
type toolCallTrace struct {
    Name   string
    Status ActivityEventStatus
}

// recentToolCalls keeps the last RecentToolWindow entries
// (3) so we can detect "same tool, all error" stretches.
recentToolCalls []toolCallTrace
```

Push a `toolCallTrace{tc.Name, status}` after each call's
status is computed. Cap the slice at `RecentToolWindow = 3`;
older entries roll off.

A loop is suspected when:
- `len(recentToolCalls) == RecentToolWindow`, AND
- All entries share the same `Name`, AND
- All entries have `Status == ActivityStatusError`

### 3.2 Hint injection

When the loop condition is hit at the start of the *next*
round (i.e., before assembling the next LLM call), prepend
a transient system message to the `messages` slice that
`buildMessagesV2` produced. **The hint is NOT added to
`session.Records`** — it's a one-shot nudge, not memory.

Hint text (English; the LLM is multilingual enough that this
works regardless of the user's UI locale):

> System note: you have called `<tool>` three times in a row
> and each call returned an error. Stop retrying with minor
> variations. Try a substantively different approach — for
> example, write the input to a `/work` file first and
> inspect it with `sandbox-run-shell` before re-running, or
> abandon this branch and ask the user for clarification.

Variants per tool family can be added later; the generic
hint is the v1.

The hint fires **at most once** per consecutive-error stretch.
After firing, the buffer is reset, so a third call still
identical to the previous two won't trigger again on the
fourth — the next trigger requires a fresh stretch.

### 3.3 Telemetry

Emit a log line at INFO whenever the hint is injected:

```
[INFO] agentLoop: loop-detection: <tool> hit error 3× in a row, injected corrective hint
```

Visible in `app.log` for postmortem and a hint that the
model is misbehaving even when it eventually recovers.

### 3.4 Edge cases

| Case | Behaviour |
|---|---|
| MITL rejection counts as error today | Yes — but rejecting 3× is itself a strong signal worth a hint |
| Different tools alternating (A, B, A) | No hint — the ring requires same `Name` for all 3 |
| User aborts mid-loop | Buffer resets when agent loop exits |
| Tool succeeds on round 4 | Buffer clears as soon as a non-matching entry pushes the old ones out |

## 4. Feature 2 — Backoff status surfaced to UI

### 4.1 Backend hook

`RetryPolicy` gains a callback:

```go
type RetryPolicy struct {
    PerRequestTimeout time.Duration
    MaxAttempts       int
    Backoff           backoff.Backoff
    // OnBackoff is called once per backoff period — i.e.
    // after attempt N failed with a retryable error, before
    // sleeping for `wait`. Used to surface "rate-limited,
    // retrying" state to the UI. Optional; nil is fine.
    OnBackoff func(attempt int, wait time.Duration, err error)
}
```

Inside `retry.go`'s `do` loop, after determining `retryable`
and computing `wait`, call `r.policy.OnBackoff` if set.

### 4.2 Plumbing through agent

`agent.setBackend` builds the policy via
`DefaultRetryPolicy(...)` and now also wires:

```go
policy.OnBackoff = func(attempt int, wait time.Duration, err error) {
    a.emitActivity(ActivityEvent{
        Type:   "retry_backoff",
        Detail: fmt.Sprintf("attempt %d: %s (waiting %s)", attempt, classifyErr(err), wait.Round(100*time.Millisecond)),
    })
}
```

`classifyErr` produces a short user-friendly label:
- `Error 429` → `"rate limit"`
- `503` / `unavailable` → `"server unavailable"`
- `context deadline exceeded` → `"timeout"`
- otherwise → `"transient error"`

### 4.3 Frontend handling

`App.tsx`'s existing `agent:activity` listener gains a case
for `'retry_backoff'`. When fired, it:

1. Sets a transient `retryStatus` state (the detail string).
2. Renders that state as a small badge in the footer
   `input-status-bar`, between the backend badge and the
   message-counts span.
3. Clears the badge when the agent reports `tool_end` (the
   call eventually returned, success or error) or when a
   new `tool_start` arrives.

CSS: a single rule `.retry-badge` with a subtle warning tone
(`var(--text-error)` text on `var(--bg-msg-error)` background,
muted), reusing existing tokens. No new colours.

### 4.4 Edge cases

| Case | Behaviour |
|---|---|
| Multiple retries within one Chat | `OnBackoff` fires per attempt; UI label updates each time |
| Retry succeeds | The successful Chat returns; the next `tool_end` clears the badge |
| Retry exhausts attempts | Final Chat returns an error; agent loop's normal error path; badge cleared on next `tool_start` or session abort |
| Multiple sessions / users | Activity events are scoped to the active session as today; no change |

## 5. Test Plan

### Feature 1

- Unit test: feed the agent a synthetic tool-execution loop
  (3× same name, all error) and assert that the next
  message-build receives the hint as the first system entry.
  Assert the hint fires once even if the loop continues.
- Manual: induce a Python `SyntaxError` 3× in a sandbox
  session, confirm `app.log` has the loop-detection line and
  the LLM's next reply acknowledges the hint (Vertex usually
  does — gemma may not).

### Feature 2

- Unit test: drive `WithRetry` with a fake backend that
  returns `Error 429` on the first call, then succeeds.
  Assert `OnBackoff` is called exactly once with
  `attempt=1`, classifies the error as `"rate limit"`, and
  surfaces a non-empty wait duration.
- Manual: hit a real Vertex 429 (or simulate via a paused
  test server), confirm the badge appears within ~half a
  second of the failure and disappears when the call
  completes.

## 6. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Hint injection makes LLM start every reply with "I see I'm in a loop…" prose | Inject only when triggered, only as a system note (not user message), and clear after one firing. Vertex tends to acknowledge briefly and move on. |
| `OnBackoff` callback fires on a goroutine that races with `tool_end` | The backoff happens inside `Chat()` which is synchronous to the agent loop; `tool_end` only fires after `Chat()` returns. No race. |
| Footer badge flicker on fast retries | The badge stays mounted but the label updates; CSS no transition flicker. |

## 7. Out of Scope

- A configurable max-rounds cap (separate TODO, mentioned
  alongside loop detection in TODO.md).
- Per-tool-family hint variants (future iteration; v1 ships
  the generic message).
- Pause/resume controls on the retry layer.
- A persistent "errors over time" panel in the UI.

## 8. Phasing

Two commits, in order:

1. **Feature 2 (smaller / observability-only).**
   `OnBackoff` callback + activity event + footer badge.
   No agent-control change. Lower risk, useful diagnostic.
2. **Feature 1 (LLM-facing).**
   Ring buffer + hint injection + telemetry log.
   Higher risk because it actively talks to the LLM, but
   the hint is one-shot and gated by `RecentToolWindow=3`.

Each phase verified manually against a real session before
the next is started.

# ADR-0021: State machine consistency — formal FSM + authoritative Send response

- Status: Implemented in v0.14.0 (2026-05-20)
- Deciders: magi
- Related: ADR-0015 (deferred extraction, the design this revises)
- Supersedes (partially): ADR-0015 §3.5 (event-based UI gating) — events become
  supplementary; the Send response carries the authoritative phase

## 1. Context

Production reports a state-machine drift in v0.13.3: UI shows Idle, user sends
a message, backend returns "QUEUED", UI displays inconsistent status,
subsequent operations cascade into further drift.

A code audit traced this to twelve distinct invariant violations, all
rooted in the original ADR-0015 design choice to treat Wails events as
the authoritative source of frontend UI state.

The violations, by class:

**Race conditions (4)**
- V1: `extractionInFlight = true` written under lock, `extraction:started`
  event emitted after unlock. Send arriving in this window returns "QUEUED"
  while frontend still sees Idle.
- V5: Same race in the other direction on `extraction:done`.
- V6: `IsBusy()` reads three mu-protected fields in three separate lock
  acquisitions, not atomic w.r.t. each other.
- (Wails event ordering across the Go→JS boundary is async; the response
  channel and the event channel are not synchronised.)

**Leaks / unmanaged goroutines (3)**
- V2: Auto-dispatch goroutine (`go a.SendWithAttachments(...)` in
  postResponseTasks) not registered to `postTasksWg`. `LoadSession.Wait()`
  returns before it completes.
- V7: `LoadSession` / `DeleteSession` / `Export` / `Import` do not reset
  `extractionInFlight` or `queuedSend`. Stale flags from a prior session
  carry over.
- V8: `Abort` clears `queuedSend` but leaves `extractionInFlight=true`
  even though the extraction context is cancelled.

**Robustness (2)**
- V4: `extractMemories` panic bypasses the post-trackBg cleanup that
  clears `extractionInFlight`. `defer wg.Done()` still fires, so
  `wg.Wait()` returns prematurely.
- V10: `trackBg` has no `defer recover()`. Title-gen panic produces
  the same wg-drain-without-cleanup pattern.

**Frontend bugs (2)**
- V3: `"QUEUED"` string falls through to `else if (response && response.trim())`
  and is appended as an `assistant` chat bubble. Visible chat pollution.
- V9: `/model` backend switch doesn't `postTasksWg.Wait()`. Title /
  extraction in flight may reference an about-to-be-rebuilt backend.

**Specification gaps (2)**
- V11: The state machine is specified only in ADR-0015 prose + scattered
  code comments. No FSM diagram, no enumerated invariants.
- V12: ADR-0015 doesn't mandate event-emit ordering relative to the
  flag write that the event signals. The current "set then unlock then
  emit" order is the source of V1/V5.

## 2. Decision

Replace the event-as-authoritative-source model with **`SendResult` as
the authoritative source**, formalise the FSM, and harden every cleanup
path against panic / cross-session leakage.

### 2.1 Formal FSM

The agent has four observable phases:

```
       ┌─────────┐  user Send  ┌──────┐
       │  Ready  │ ───────────▶│ Busy │
       │         │             │      │
       └─────────┘◀────────────┴──┬───┘
            ▲                     │ agentLoop returns,
            │   extraction        │ autoExtract = false
            │   completes,        │
            │   no queue          ▼
            │              ┌────────────┐
            │              │ Extracting │
            │              └─────┬──────┘
            │      auto-dispatch │ ▲ user Send
            │  ┌─────────────────┘ │
            │  │                   ▼
            │  │              ┌──────────┐
            │  │              │  Queued  │
            │  └──────────────┴──────────┘
            │      extraction completes,
            └──────  queue dispatch creates
                     new turn ─▶ Busy
```

**States** encoded as the triple `(state, extractionInFlight, queuedSend!=nil)`:

| Phase | state | extractionInFlight | queuedSend |
|-------|-------|---------------------|------------|
| Ready | Idle | false | nil |
| Busy | Busy | false | nil |
| Extracting | Idle | true | nil |
| Queued | Idle | true | non-nil |

**Invalid combinations** (must never be observable):
- `(Busy, true, *)` — extraction cannot overlap with agentLoop
- `(Idle, false, non-nil)` — queue without extracting is meaningless
- `(Busy, *, non-nil)` — queue should only exist during Extracting

### 2.2 Authoritative Send response

`Send` / `SendWithImages` return a structured `SendResult`:

```go
type SendResult struct {
    // Phase is the agent phase the caller should now display.
    // One of: "completed" | "queued" | "command" | "error".
    Phase string `json:"phase"`

    // Content is the assistant's text response when Phase=="completed".
    Content string `json:"content,omitempty"`

    // CmdResult is the slash-command output when Phase=="command".
    CmdResult string `json:"cmd_result,omitempty"`

    // QueuedAt is the timestamp the SEND was queued, when Phase=="queued".
    QueuedAt string `json:"queued_at,omitempty"`

    // ErrorMessage is the human-readable error when Phase=="error".
    ErrorMessage string `json:"error_message,omitempty"`
}
```

The frontend's `handleSend` reads `Phase` and routes accordingly. No
event waiting, no string sniffing of magic prefixes.

`extraction:*` / `queued` / `queue_cleared` events stay, but become
supplementary signals for state changes the frontend didn't initiate
(e.g. an auto-dispatch firing while the user is mid-typing). The
post-Send UI updates no longer depend on them.

### 2.3 Snapshot API

```go
type AgentSnapshot struct {
    Phase string // "ready" | "busy" | "extracting" | "queued"
    QueuedMessage string // empty unless Phase=="queued"
}

func (a *Agent) Snapshot() AgentSnapshot {
    a.mu.Lock()
    defer a.mu.Unlock()
    return a.snapshotLocked()
}
```

`Snapshot()` reads all three state fields under one lock, deriving the
phase from the triple. `IsBusy()` is rewritten as a thin wrapper over
this. Frontend can call it on mount to recover state after a refresh /
reconnect without depending on event replay.

### 2.4 Cleanup paths

**Backend hardening:**

1. `trackBg` wraps the inner `fn()` in `defer recover()`. Panics are
   logged and converted to errors; cleanup code after trackBg always
   runs.
2. The extraction goroutine's flag-clear runs inside a deferred block,
   not as straight-line code after trackBg. Order: defer flag-clear
   first, then defer wg.Done(). Defers run in LIFO so flag-clear
   precedes wg.Done(), and both run on every exit path (panic or
   normal).
3. Auto-dispatch is registered to `postTasksWg` (or to a dedicated
   `queueDispatchWg` drained alongside it by LoadSession). The
   lifecycle drains it before swapping session pointers.
4. `Abort` clears `extractionInFlight` and `queuedSend` together,
   not just the queue.
5. `LoadSession` / `DeleteSession` / `Export` / `Import` / `NewSession`
   each call a single `resetStateMachine()` helper after `wg.Wait()`
   to reset `extractionInFlight = false`, `queuedSend = nil`, `state =
   StateIdle` defensively under mu. This is a no-op when cleanup ran
   correctly, but recovers from panic / leak paths.
6. `/model` (backend switch) calls `postTasksWg.Wait()` before
   rebuilding the backend, so title / extraction never reference a
   freed client.

**Frontend changes:**

1. `handleSend` consumes `SendResult.Phase`; remove the magic-string
   sniffing of `[CMD]` and `QUEUED`.
2. On mount, call `Snapshot()` and seed `state` / `extractionPending`
   / `queuedMessage` from it.
3. Unify `isBusy` to a single derivation:
   `isBusy = phase !== "ready"`. Drop the dual definition (top-level
   `state==='busy' || postBusy` vs. ChatInput's `state==='busy' ||
   extractionPending`).
4. Treat `agent:extraction:*` / `agent:queued` / `agent:queue_cleared`
   as supplementary refreshers — when received, re-read state via
   `Snapshot()` rather than mutate React state piecewise. This is
   simpler and avoids the per-event mutation bugs.

### 2.5 Event ordering

Events emit **inside** the lock that wrote the flag, in the same
critical section. The Wails runtime's `EventsEmit` is non-blocking
(buffered channel send), so this doesn't introduce contention. The
backend's causal order is now "flag write → event emit → unlock", and
any concurrent reader who acquires the lock next sees the post-emit
state. The async JS→handler hop is still possible, but the response
is now authoritative so the frontend doesn't rely on event ordering
for correctness.

## 3. Implementation plan

Twelve commits across two phases.

**Phase A: Backend FSM hardening (no protocol change)**

1. `refactor(agent): resetStateMachine() helper + extraction cleanup via defer`
2. `feat(agent): trackBg panic recovery`
3. `fix(agent): Abort clears extractionInFlight`
4. `fix(agent): LoadSession / Delete / Export / Import reset FSM`
5. `fix(agent): /model waits postTasksWg before backend rebuild`
6. `fix(agent): auto-dispatch goroutine on postTasksWg`
7. `feat(agent): Snapshot() + atomic IsBusy`

**Phase B: Protocol + frontend (breaking internal API)**

8. `feat(agent): SendResult return type (Send / SendWithImages)`
9. `feat(bindings): SendResult DTO + Snapshot binding`
10. `feat(frontend): handleSend consumes SendResult; remove magic-string sniffing`
11. `feat(frontend): Snapshot-driven isBusy / state seeding on mount`
12. `test(agent): FSM invariant tests (each invalid state assert-unreachable)`

**Phase C: Release**

13. `docs: CHANGELOG v0.14.0 + ADR-0021 Implemented`
14. `chore: release v0.14.0`

Minor bump (not patch) because:
- Send response shape changes (internal but observable across the
  Go/JS boundary).
- Behaviour changes: Abort now clears extraction flag; restart from
  cancelled extraction is now immediate, not delayed until next turn.

## 4. Test strategy

### 4.1 FSM invariant tests

For each invalid state combination, a unit test asserts the agent
cannot reach it under any sequence of public-API calls:

```go
TestFSM_BusyAndExtractingNeverCoexist
TestFSM_QueuedRequiresExtracting
TestFSM_QueuedNeverDuringBusy
```

Driven via the existing `extractMemoriesOverride` test hook plus
goroutine-pause channels.

### 4.2 Race regression tests

- `TestSend_DuringExtraction_ReturnsQueuedResult` — assert `Phase ==
  "queued"`, not the string "QUEUED".
- `TestExtractionPanic_RecoversAndClearsFlags` — install a panicking
  extractFn override; assert post-call snapshot is `ready`.
- `TestAbort_ClearsExtractionInFlight` — start extraction, abort,
  snapshot → ready.
- `TestLoadSession_ResetsStateMachine` — leak extractionInFlight=true
  manually; LoadSession should normalise.

### 4.3 Frontend manual smoke

1. Vertex profile with auto-extract on. Send → wait for response →
   send again quickly. Expect: second send shows queued pill, no
   "QUEUED" assistant bubble, ChatInput correctly disabled.
2. Abort during extraction. Expect: ChatInput re-enables immediately,
   no stale extracting indicator.
3. Refresh / dev-tools reload mid-extraction. Expect: snapshot on
   mount restores the correct UI.

## 5. Migration / compatibility

- **Settings file**: no schema changes.
- **Session files**: no schema changes.
- **Wails binding signatures**: `Send` / `SendWithImages` change from
  `(string, error)` to `(SendResult, error)`. Frontend updated in the
  same release; no external consumers.
- **Event payloads**: unchanged. Frontend still listens but doesn't
  drive primary state from them.
- **Existing tests**: updated to match new return types. The
  `extractMemoriesOverride` hook still works.

## 6. Out of scope

- Replacing Wails events with a poll-only model. Events remain useful
  for spontaneous state changes (auto-dispatch firing).
- Restructuring the per-session memory stores or the agentLoop itself.
  This ADR is purely about FSM consistency.
- Per-session FSM isolation (multiple agents in one process). v2 is
  single-agent; revisit if multi-session-active becomes a feature.

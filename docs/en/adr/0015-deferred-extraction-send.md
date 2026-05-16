# Deferred extraction + single-slot send queue — Design Note

**Status:** Approved (2026-05-16); implementing in v0.11.0.
**Target:** v0.11.0 (minor bump — user-visible state-machine change,
new Wails events, no breaking API changes).
**Reported by:** User — "chat response is very slow, even with
Vertex AI". Time from SEND to next-message-enabled regularly
exceeds 10 s on substantive turns, because the UI stays locked
through the post-response memory-extraction LLM call.

This note specifies a **deferred-extraction model**: the UI
unlocks as soon as the visible response is delivered, while
`extractMemories` continues to run in the background. A new
SEND issued during the background extraction is **queued in a
single slot**; the queued SEND fires only after the in-flight
extraction completes, so the next turn's `BuildSystemPrompt`
always reads memory that includes the prior turn's extracted
facts. No facts are dropped, no extraction is aborted — only
the *UI gating* changes.

---

## 1. Problem

Today's turn lifecycle (`agent.go` + `bindings.go`):

```
[1] User input
[2] SEND  ─→  state = StateBusy  ─→  UI locked
[3] LLM round(s) via agentLoop
[4] Response delivered (visible in chat)
[5] postResponseTasks fires title + extractMemories goroutines
[6] postTasksWg.Wait() — waits for BOTH to finish
[7] state = StateIdle  ─→  UI unlocked
```

`extractMemories` (`agent.go:2258`) is a separate LLM call that
analyses the recent conversation tail and routes
`preference`/`decision`/`fact`/`context` items into Global
Memory and Session Memory. It costs **3-8 s** of dead time on
every turn. Combined with title generation (first turn only,
3-5 s) and the agent loop itself (5-15 s with tool calls),
typical end-to-end latency from SEND to next-input-enabled
runs 10-20+ s on substantive turns.

The pain is felt by the composer: the user has read the
response, knows what they want to ask next, and is staring at
a locked input. The remaining wait is *all* memory-bookkeeping
the user doesn't directly observe.

### Why simpler fixes were rejected

- **Abort extraction on new SEND** — drops facts; for data-
  analysis sessions where extractMemories captures user
  preferences ("exclude outliers"), partial decisions, and
  derived context ("this log is from team X"), the loss is
  material. Memory extraction is one of the system's
  selling points.
- **Pure fire-and-forget extraction** — if the user SENDs fast
  enough, turn N+1's `BuildSystemPrompt` reads memory before
  turn N's extraction wrote to it. Eventually consistent in
  records, but turn N+1's LLM doesn't see turn N's facts.
- **Streaming the response** — disabled deliberately because
  Gemini 2.5 Flash leaks `"THOUGHT\n…"`/`"思考\n…"` text Parts
  with `Thought=false` (`vertex.go:74-83`); these would flow
  into the chat bubble before the post-stream `CleanResponse`
  pass can strip them. Revisitable when Gemini 3 stabilises
  (per-Part `Thought=true` is reliable there) but out of scope
  for this ADR.
- **Skip extraction on trivial turns** — viable as an
  orthogonal optimisation, but doesn't help substantive turns
  which are exactly the slow ones the user is complaining about.

---

## 2. Goals

1. **UI unlocks at step [4] (response visible)**, not at step
   [7] (extraction complete). The composer can start typing
   the next message immediately.
2. **Zero fact loss.** Extraction always runs to completion.
   The new turn's `BuildSystemPrompt` always sees the prior
   turn's extracted facts.
3. **Single-slot SEND queue.** A SEND during background
   extraction is held until extraction finishes, then fires
   automatically. A second SEND while one is already queued
   *replaces* the prior queued message (the user can type and
   correct themselves — most-recent wins).
4. **No breaking changes** to existing Wails bindings'
   signatures. Frontend behaviour changes only.
5. **Cancellable**. The user can abort the current turn, the
   in-flight extraction, and a queued SEND, all via the
   existing Abort affordance.

Non-goals:

- **Multi-slot queue.** Chat semantics: one pending message
  per user. If they SEND twice, only the latest matters.
  Sequentially firing both would be confusing (the user
  would see their first message disappear into history with
  no response yet, then their second message also disappear).
- **Speedup of `extractMemories` itself.** Out of scope; this
  ADR is about UI gating, not LLM cost reduction.
- **Streaming.** See §1's rejection note. Tracked separately.
- **Cross-turn extraction batching.** Each turn still triggers
  its own extraction. Batching (every Nth turn) is a
  different ADR if needed.

---

## 3. Design

### 3.1 State machine

The visible `agent.State` enum gains nothing — the existing
`StateIdle` / `StateBusy` values stay. The change is a new
flag inside the agent struct:

```go
type Agent struct {
    // existing fields...
    state State // StateIdle / StateBusy

    // extractionInFlight is true between the moment a turn's
    // response has been delivered and the moment that turn's
    // extractMemories goroutine returns. While set, the UI is
    // unlocked (state == StateIdle) but a SEND is held in
    // queuedSend rather than starting immediately.
    extractionInFlight bool

    // queuedSend, if non-nil, is the most recent SEND received
    // while extractionInFlight was true. It fires as soon as
    // the in-flight extraction completes. A second SEND
    // overwrites this field (single-slot, most-recent-wins).
    queuedSend *queuedSend
}

type queuedSend struct {
    Message            string
    ImageObjectIDs     []string
    ImageDataURLs      []string
    DocumentObjectIDs  []string
    QueuedAt           time.Time
}
```

Both fields are guarded by `a.mu` (existing mutex).

### 3.2 Send entry point changes

`Agent.SendWithAttachments` (`agent.go:682`) gains one branch:

```go
a.mu.Lock()
switch {
case a.state == StateBusy && !a.extractionInFlight:
    // True busy — agent loop in progress. Reject as before.
    a.mu.Unlock()
    return "", ErrBusy
case a.state == StateIdle && a.extractionInFlight:
    // Response delivered, extraction running — queue it.
    a.queuedSend = &queuedSend{
        Message: message, ImageObjectIDs: imageObjectIDs,
        ImageDataURLs: imageDataURLs,
        DocumentObjectIDs: documentObjectIDs,
        QueuedAt: time.Now(),
    }
    a.mu.Unlock()
    a.emitQueued()  // see §3.4 for the wails event
    return "QUEUED", nil
case a.state == StateIdle:
    // Normal case — proceed.
    a.state = StateBusy
    ctx, a.cancel = context.WithCancel(ctx)
    a.mu.Unlock()
    return a.agentLoop(ctx, message, imageObjectIDs, imageDataURLs, documentObjectIDs)
}
```

The frontend treats `"QUEUED"` identically to a successful
SEND for input-clearing purposes: the message has been
accepted into the system, just not yet processed.

### 3.3 postResponseTasks changes

Current shape (`agent.go:1740`):

```go
a.postTasksWg.Add(2)
go { a.trackBg(ctx, "title", generateTitle) }
go { a.trackBg(ctx, "memory-extraction", extractMemories) }
go {
    a.postTasksWg.Wait()
    // state = StateIdle
}
```

New shape:

1. **Title generation stays blocking** (still tracked by wg
   and contributes to the StateIdle transition). It only runs
   on the first turn and completes fast.
2. **Memory extraction moves off the wg**. A new goroutine
   runs extraction in the background:

```go
// Title — gates the StateIdle transition (short, first turn only).
a.postTasksWg.Add(1)
go func() {
    defer a.postTasksWg.Done()
    a.trackBg(ctx, "title", generateTitle)
}()

// State flip back to Idle now happens as soon as title
// completes (extraction is no longer gating).
go func() {
    a.postTasksWg.Wait()
    a.mu.Lock()
    a.state = StateIdle
    a.cancel = nil
    a.mu.Unlock()
    a.emitState() // ← UI unlocks here, even if extraction is still running
}()

// Memory extraction — runs concurrently with the user's next
// composition window. When it finishes, dispatch any queued SEND.
a.mu.Lock()
a.extractionInFlight = true
a.mu.Unlock()
a.emitExtractionStarted()
go func() {
    a.trackBg(ctx, "memory-extraction", extractMemories)

    a.mu.Lock()
    a.extractionInFlight = false
    queued := a.queuedSend
    a.queuedSend = nil
    a.mu.Unlock()
    a.emitExtractionDone()

    if queued != nil {
        // Auto-dispatch the queued SEND. We deliberately call
        // SendWithAttachments rather than agentLoop directly so
        // the normal state machine, MITL hooks, and event
        // emitters run.
        go a.SendWithAttachments(
            a.baseCtx,
            queued.Message,
            queued.ImageObjectIDs,
            queued.ImageDataURLs,
            queued.DocumentObjectIDs,
        )
    }
}()
```

`a.baseCtx` is the long-lived context captured at startup
(`Agent.Start`), not the per-turn cancellable ctx. The queued
SEND must outlive the turn that queued it.

### 3.4 Abort

`Agent.Abort` (`agent.go:725`) gains queue-clearing
responsibilities. The existing behaviour (cancel current turn
+ cancel post-response goroutines) is preserved; the new
behaviour is:

```go
func (a *Agent) Abort() {
    a.mu.Lock()
    cancel := a.cancel
    postCancel := a.postCancel
    queued := a.queuedSend
    a.queuedSend = nil
    state := a.state
    a.mu.Unlock()

    if cancel != nil { cancel() }
    if postCancel != nil { postCancel() }
    if queued != nil {
        a.emitQueueCleared() // see §3.5
    }
    _ = state
}
```

Aborting during extraction:
- The extraction goroutine receives ctx cancellation, returns
  early. Whatever facts it had partially extracted are
  discarded. **This is acceptable**: explicit Abort is a
  stronger user intent than implicit "I want speed".
- The queued SEND (if any) is cleared, since the user is
  abandoning the conversation flow.

### 3.5 Wails events for the frontend

Three new events emitted to the frontend so the UI can
display the deferred-extraction states distinctly:

| Event | Payload | When |
|-------|---------|------|
| `agent:extraction:started` | `{turn: int}` | After response delivered, before extraction goroutine starts |
| `agent:extraction:done` | `{turn: int, success: bool}` | After extraction goroutine returns (success or error) |
| `agent:queued` | `{at: ISO8601}` | After a SEND is accepted into the queue slot |
| `agent:queue_cleared` | `{}` | After Abort drains the queue, or after auto-dispatch consumes it |

The existing `agent:activity` events for `tool_start`/
`tool_end` continue to fire for the new turn the queue
dispatches. The frontend treats them identically to a normal
SEND.

### 3.6 Frontend UX

#### Input bar states

The input field has three meaningful states post-ADR:

1. **Ready** — `state == StateIdle && !extractionInFlight`.
   SEND button green, hint text empty. The agent is fully
   ready.
2. **Extraction in flight** — `state == StateIdle &&
   extractionInFlight && queuedSend == nil`. SEND button
   still active but tinted (e.g. amber), with a small inline
   hint:
   > "⏳ Extracting memory — your next message will send when
   > done."
3. **Queued** — `state == StateIdle && extractionInFlight &&
   queuedSend != nil`. The composer area is cleared (message
   sent into queue), and a status pill above the input shows:
   > "Queued: '<first 60 chars>' — sends when memory extraction
   > completes. ✕ to cancel."
   The ✕ calls Abort.

Visual mock for state (3):
```
┌──────────────────────────────────────────────┐
│  [agent's last response]                     │
│                                              │
│  ⏳ Queued: "How about the time series view…" │
│      Will send when extraction completes ✕   │
│  ┌──────────────────────────────────────────┐│
│  │ (input cleared — ready for next message) ││
│  └──────────────────────────────────────────┘│
└──────────────────────────────────────────────┘
```

The pill disappears on `agent:queue_cleared` or when the
turn auto-dispatches.

#### Status bar

The existing `input-status-bar` gains one optional indicator
(small, right-aligned, beside the backend badge):

> `⚙ extracting…`

shown while `extractionInFlight && !queuedSend`, swapped
for the queue pill above when a SEND is queued.

---

## 4. Edge cases

1. **Multiple SENDs during extraction.** Single-slot, most-
   recent-wins. The second SEND overwrites `queuedSend`; the
   first is silently dropped. The UI updates the queue pill
   to show the new message. *Rationale*: the user is
   correcting themselves. Showing both would clutter; firing
   both sequentially would surprise.

2. **SEND during `StateBusy` (response generation).** Unchanged
   from today: rejected with `ErrBusy`. The frontend already
   guards on `state === 'busy'`; this stays.

3. **Abort during extraction.** Per §3.4: extraction cancelled,
   queue cleared. The partially-extracted facts are dropped.
   Aborted turns are an explicit user choice; it's OK to lose
   memory bookkeeping there.

4. **Extraction fails (timeout / LLM error).** The error is
   logged (existing `trackBg` does this). The `agent:extraction:done`
   event fires with `success: false`. The queued SEND, if any,
   still dispatches — extraction failure is not the user's
   fault. Mid-extraction memory state is whatever the LLM
   reply parser managed before the error.

5. **Session-management ops (switch / delete / export /
   import / rename / new) while extraction in flight.** Block
   them. The extraction goroutine references agent fields
   (`a.session.Records`, `a.sessionMemory`, `a.globalMemory`,
   `a.findings`) that get repurposed on session change.
   Allowing a switch would mean facts derived from session A's
   tail would land in session B's memory file, or the
   shared `global_memory.json` would see two extractions
   racing (the one from A's tail and the one B's first turn
   would trigger). Delete is worse: the extraction could be
   mid-write to a file the deletion is removing.

   Implementation: the existing frontend gate
   (`handleLoadSession` in `App.tsx:510`:
   `state === 'idle' && bgTasks.length === 0`) already covers
   this *if* the extraction goroutine continues to register
   via `trackBg`. The new design keeps that registration —
   extraction is still in `bgTasks` for the duration; it just
   doesn't gate the `state` transition. The session-management
   bindings (`LoadSession`, `DeleteSession`, `ExportSession`,
   `ImportSession`, `RenameSession`, `NewSession`,
   `NewPrivateSession`) all gate on the same idiom and inherit
   the block automatically. No per-binding changes needed.

   Visual: sidebar session entries get a `cursor: not-allowed`
   + tooltip "Extracting memory…" while
   `extractionInFlight === true && state === 'idle'`. The
   chat-pane `⏳ Extracting memory…` indicator is the primary
   signal; the sidebar disablement is a secondary affordance
   that prevents accidental clicks.

6. **App quit while extraction in flight.** `OnBeforeClose`
   (`main.go:56`) already gates on `IsBusy()`. We extend
   `IsBusy()` to return `true` if `extractionInFlight` is
   true *or* `queuedSend != nil`. Quitting mid-extraction
   could lose facts mid-write — block until clean.

7. **Queued SEND auto-dispatch races with manual Abort.** The
   queue-dispatch goroutine reads `queuedSend` under `a.mu`,
   then releases the lock before calling SendWithAttachments.
   If Abort fires between the lock release and the actual
   send, `queuedSend` is already cleared (by the dispatch
   goroutine's own clear) so Abort's clear is a no-op. The
   in-flight SendWithAttachments grabs the lock, finds state
   = Idle (or Busy if it raced with another path), and
   proceeds. No deadlock; the worst case is the queued SEND
   fires once when the user thought they had cancelled — but
   the user can immediately Abort the new turn. Acceptable.

8. **Trivial turn (no tool calls, short response).** Total
   turn-to-next-input-enabled latency in the old model was
   dominated by `extractMemories`. In the new model the UI
   unlocks at step [4]. If the user composes faster than
   extraction completes, the queue lights up; otherwise the
   turn feels instant. This is the primary intended win.

9. **Long-running turn (many tool calls).** No change in
   feel — `state == StateBusy` for the duration of agentLoop,
   UI locked, same as before. The new mechanism only changes
   the *post-response* gate, not the in-loop gate.

10. **Title generation still gates StateIdle.** Title gen is
    short (3-5 s) and only runs once per session, so this is
    a minor first-turn-only cost. Not worth complicating the
    state machine further by moving title off the wg too.

---

## 5. Rejected alternatives

### 5.1 Abort extraction on new SEND (variant B-1 from chat)

Rejected (§1): data-analysis sessions value extracted facts
(user preferences, derived context, decisions). Throwing them
away when the user simply wants to type faster is the wrong
trade.

### 5.2 Pure fire-and-forget without queue (variant B-2)

Rejected: causes turn N+1's `BuildSystemPrompt` to read memory
before turn N's extraction wrote to it. The records are
preserved, so the information is *technically* available via
context-build, but the compressed Global Memory entries are
the load-bearing carrier for cross-session value. Eventually
consistent != consistent within a session.

### 5.3 Multi-slot SEND queue (FIFO)

Rejected (§2 Non-goals): chat semantics. Composing message B
while message A is queued is rare and confusing. Single-slot
most-recent-wins matches how every other chat UI behaves when
the user re-types before send.

### 5.4 Batch extraction every N turns

Considered. Reduces total extraction load to ~1/N. But:
- Adds complexity to memory-extraction trigger logic.
- Risks "fact rot" — facts surface in memory N-1 turns later
  than they could.
- Orthogonal to the UI-responsiveness goal; doesn't help the
  Nth turn (which still pays the full extraction cost).
- Tracked as a separate ADR if the LLM-cost angle ever
  matters enough to justify it.

### 5.5 Replace extraction with streaming summarisation

Considered. Would interleave extraction work with the agent
loop itself, amortising the cost. But:
- Requires re-architecting `extractMemories` to consume
  partial conversation tail.
- Touches the response pipeline, which is high-risk to mess
  with.
- This ADR's deferred-extraction model achieves the user-
  visible goal (UI responsiveness) without touching the
  extraction pipeline at all. Lower risk, simpler change.

---

## 6. Tests / invariants

### 6.1 Backend (Go)

- **`TestAgent_DeferredExtraction_UIUnlocksBeforeExtraction`**
  — drive `Send` → wait for response → assert `State() ==
  StateIdle` *before* the mock extractor returns.
- **`TestAgent_QueuedSend_FiresAfterExtraction`** — Send,
  wait for response, second Send while extraction in
  progress, assert second Send is queued; complete mock
  extraction; assert second Send fires automatically and
  produces a new response.
- **`TestAgent_QueueOverwrite`** — three Sends in
  quick succession during extraction; assert only the third
  fires; assert `queue_cleared` event is NOT emitted in
  between (the overwrites are silent).
- **`TestAgent_AbortClearsQueue`** — Send, second Send while
  extracting, Abort; assert queued SEND is cleared, neither
  fires.
- **`TestAgent_ExtractionErrorStillDispatchesQueue`** — mock
  extractor returns error; queued SEND still fires.
- **`TestAgent_IsBusyDuringExtraction`** — extraction in
  flight → `IsBusy()` returns true (so OnBeforeClose gates
  quit).

### 6.2 Frontend (manual smoke + seeder)

Extend `cmd/seed-objlink-smoke` to optionally pre-position an
extractionInFlight state? Probably overkill. Instead:

- Manual smoke step: open the app, send a substantive message
  (one that exercises the agent loop, e.g. an `analyze-data`
  call), observe that:
  - Response appears, input bar becomes active immediately
  - `⏳ Extracting memory…` indicator visible
  - Send a second message during that window → message pill
    appears showing "Queued: …", input clears
  - Wait → queue pill disappears, new turn begins
  - Verify the queued turn's BuildSystemPrompt actually saw
    the prior turn's facts (e.g. via debug log of system
    prompt contents)

### 6.3 Structural

- No new structural invariants. The state-machine extension
  is encapsulated under `a.mu` so the existing race-free
  guarantees hold.

---

## 7. Compatibility

No breaking changes.

- **Existing API**: `Send` / `SendWithImages` / `Abort` /
  `IsBusy` signatures unchanged. `Send` may now return the
  string `"QUEUED"` as a *success* value rather than the
  agent's response text. Frontend handling already treats
  the input-clear as the success signal; the response itself
  arrives via `agent:activity` and message-list events.
- **Existing events**: `agent:stream`, `agent:activity`,
  `session:title`, etc., unchanged. New events
  (`agent:extraction:started/done`, `agent:queued`,
  `agent:queue_cleared`) are additive.
- **Existing sessions**: no schema changes; chat.json,
  global_memory.json, session_memory.json all unchanged.
- **Existing tests**: extending; not replacing.

---

## 8. Phasing

Single PR. Commits:

1. `feat(agent): extractionInFlight + queuedSend state + tests`
2. `refactor(agent): move extractMemories off postTasksWg`
3. `feat(agent): dispatch queued SEND when extraction completes`
4. `feat(bindings): IsBusy reflects extractionInFlight; Abort clears queue`
5. `feat(events): agent:extraction:started/done + agent:queued/queue_cleared`
6. `feat(frontend): input-bar tinted state + queue pill + Abort
   wiring + status-bar indicator`
7. `docs: ADR-0015 + CHANGELOG v0.11.0 + system-rules /
   architecture reference notes`

---

## 9. Out of scope

- Streaming response output (deferred until Gemini 3 stabilises
  per-Part Thought signalling).
- Skipping extraction on trivial turns (separate optimisation,
  orthogonal).
- Batched extraction every N turns (separate ADR if needed).
- `extractMemories` LLM model/prompt tuning (separate concern).

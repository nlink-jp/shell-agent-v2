# Background Task Indicator

## Status

Shipped in v0.1.19. The footer badge, the post-task Busy gate
(input field + Sidebar New / Load / Delete all locked while
post-tasks run), and the symmetric INFO/ERROR logging
(`done` / `canceled` / `<err>`) are all live. The auto-cancel
variant described in earlier drafts of this doc was tried and
reverted; see the §"State stays Busy until post-tasks finish"
section below for why.

## Problem

After a chat turn completes, the agent launches three background
goroutines under `postResponseTasks`:

1. `generateTitleIfNeeded`  — names new sessions.
2. `compactMemoryIfNeeded`  — folds the warm tail into a summary.
3. `extractPinnedMemories`  — promotes user-facing facts.

All three may call the LLM, so each can take seconds to minutes
(longer under 429 retries). Today they run **invisibly**:

- The UI returns to Idle as soon as the visible reply lands.
- No indication that anything is still in flight.
- A 429 retry can hold the next `Send` hostage at the
  `postTasksWg.Wait()` barrier with no UX signal of why.
- Failures are silent unless the user opens the log.

The ad-hoc `postCancel` shipped in `b8ebb2f` lets `Abort` cut these
off, but doesn't address the underlying invisibility.

## Goals

- Show the user **what** is running in the background and **for
  how long**.
- Make the next `Send` automatically cancel any still-running
  background tasks (no manual abort needed).
- Surface failures briefly without nagging.
- Stay consistent with the existing Wails-event-driven UI plumbing
  (`agent:activity`, `sandbox:build:line`, etc.).

## Non-goals

- Fine-grained progress (no percentage bar — these tasks have no
  natural progress signal).
- A history view of past background tasks.
- User-facing retry / resume controls.

## Design

### Backend — task-state stream

Add to `internal/agent`:

```go
// BgTaskEvent is emitted at start/end of each post-response task.
type BgTaskEvent struct {
    Name  string // "title" | "memory-compaction" | "pinned-extraction"
    Phase string // "start" | "end"
    Error string // populated on Phase=="end" if the task failed
}

type BgTaskHandler func(BgTaskEvent)
```

`Agent` gains an optional `bgTaskHandler BgTaskHandler` field, set
once at construction time by `bindings.go` (same pattern as the
existing stream/activity callbacks). `postResponseTasks` wraps each
goroutine in a small helper:

```go
func (a *Agent) trackBg(ctx context.Context, name string, fn func() error) {
    logger.Info("bg-task %s: start", name)
    a.notifyBg(BgTaskEvent{Name: name, Phase: "start"})
    err := fn()
    msg := ""
    switch {
    case err == nil:
        logger.Info("bg-task %s: done", name)
    case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
        // Auto-cancel from the next Send. Not a failure, but we
        // still log so the operator can correlate "task didn't
        // finish" with "user typed again before it could complete".
        logger.Info("bg-task %s: canceled (next Send)", name)
    default:
        logger.Error("bg-task %s: %v", name, err)
        msg = err.Error()
    }
    a.notifyBg(BgTaskEvent{Name: name, Phase: "end", Error: msg})
}
```

Tasks return their error so `trackBg` can log and forward it.
Logging is symmetric — `start`, then exactly one of `done`,
`canceled`, or the error line. The `canceled` branch suppresses the
red footer flash but still leaves a breadcrumb in the log so an
operator investigating "why didn't this session ever get a title"
can see the cancellation rather than just an unexplained absence.

### State stays Busy until post-tasks finish

A previous iteration tried auto-cancelling lingering post-tasks at
the start of the next `Send`. That broke down for two reasons:

1. **Local LLMs are slow.** A well-behaved post-task can easily
   take longer than the user takes to type the next message, so
   under rapid conversation the user would auto-cancel every
   pinned-fact extraction in the session and the pinned store
   would silently lose facts.
2. **Tasks are not all idempotent.** Pinned-fact extraction looks
   only at the most recent four hot records, so if it never gets
   to run before being cancelled by the next turn, those facts
   are gone — not deferred, gone.

The fix is to keep the agent in `StateBusy` from the moment the
user message arrives until **all three post-tasks have completed**.
The existing Wails frontend already disables the input field on
Busy, so the user physically cannot type during this window —
neither a chat message nor a slash command will fire.

```go
// SendWithImages — Busy gate, then agentLoop with a defer-fired
// postResponseTasks; the trailing goroutine drops state to Idle.

a.mu.Lock()
if a.state != StateIdle {
    a.mu.Unlock()
    return "", ErrBusy
}
a.state = StateBusy
ctx, a.cancel = context.WithCancel(ctx)
a.mu.Unlock()

return a.agentLoop(ctx, message, objectIDs, dataURLs)
// agentLoop's `defer a.postResponseTasks(ctx)` covers every
// return path; postResponseTasks's trailing goroutine returns
// state to Idle once the WaitGroup completes.
```

Slash commands run inside the Busy window so two of them can't
race, but they do not call `agentLoop` and therefore do not trigger
post-tasks — they release state directly before returning.

`Abort` remains the user's escape hatch: it fires both `cancel`
(in-flight agent loop) and `postCancel` (post-task ctx). The
trailing goroutine in `postResponseTasks` then drops state to
Idle, just as in the success path.

### Bindings — Wails event bridge

In `bindings.go::onAgentReady` (or wherever the agent is wired):

```go
agent.SetBgTaskHandler(func(e BgTaskEvent) {
    wailsRuntime.EventsEmit(b.ctx, "bg-task:" + e.Phase, map[string]any{
        "name":  e.Name,
        "error": e.Error,
    })
})
```

Two event names — `bg-task:start` and `bg-task:end` — match the
existing colon-prefixed convention.

### Frontend — Footer indicator

Add a thin status row at the bottom of the main column (above any
existing input area). It is empty by default and rendered only when
either an active task exists or a recent failure is being flashed.

State (held in `App.tsx` or a small dedicated context):

```ts
type BgTask = { name: BgTaskName; startedAt: number };
type BgTaskFailure = { name: BgTaskName; error: string; at: number };

const [active, setActive] = useState<BgTask[]>([]);
const [failure, setFailure] = useState<BgTaskFailure | null>(null);
```

`bg-task:start` pushes onto `active`. `bg-task:end` removes from
`active`; if `error !== ''`, sets `failure` and clears it 5 s later.

Rendering rules:

- Empty `active` and no `failure` → footer hidden (no DOM).
- Non-empty `active` → grey row, `"処理中: タイトル生成, メモリ圧縮"`
  (label table localised in the frontend).
- `failure` set → red row, `"失敗: メモリ圧縮 (timeout)"`, auto-fades
  after 5 s. If a new task starts in the meantime, normal grey row
  takes precedence; the failure stays in place beneath only if both
  conditions hold simultaneously (in practice a new task starting
  within 5 s of a failure is rare; we collapse to the active row).

Label table:

| code              | ja            | en                       |
|-------------------|---------------|--------------------------|
| title             | タイトル生成   | Title generation         |
| memory-compaction | メモリ圧縮     | Memory compaction        |
| pinned-extraction | 注目情報抽出   | Pinned-memory extraction |

### Failure semantics

| outcome           | log                              | UI                              |
|-------------------|----------------------------------|---------------------------------|
| success           | `INFO bg-task <name>: done`      | task quietly disappears         |
| auto-canceled     | `INFO bg-task <name>: canceled`  | task quietly disappears         |
| error             | `ERROR bg-task <name>: <err>`    | red flash for 5 s + name + msg  |

`canceled` is logged at INFO so it doesn't pollute error rates, but
it is logged — that's what lets an operator correlate "the session
never got a title" with "user typed the next message before
title-gen finished."

## Test strategy

- `agent_test.go`:
  - `postResponseTasks` calls the handler with `Phase: "start"` and
    `Phase: "end"` for each of the 3 tasks.
  - When `parentCtx` is cancelled mid-task, the resulting `end`
    event has empty `Error` (canceled is not a failure).
  - When a task returns a non-cancel error, the `end` event carries
    the message.
- Frontend (jest/Vitest if present, otherwise manual):
  - Reducer collapses start/end correctly with overlapping tasks.
  - Failure clears after 5 s timer.

## Out of scope (explicitly)

- Showing the LLM token cost of each background task.
- Per-task abort buttons.
- Historical log of past background tasks (use the existing log).

## Affected files

- `app/internal/agent/agent.go`  — handler field, `trackBg`, wire
  into `postResponseTasks`, return errors from helpers.
- `app/bindings.go`  — register handler, emit events.
- `app/frontend/src/App.tsx`  — subscribe to `bg-task:*`, manage
  state.
- `app/frontend/src/components/BgTaskFooter.tsx` (new)  — render.
- Tests under `app/internal/agent/`.

# `tool_progress` Activity Events — Design Note

**Status:** Design draft (2026-05-07); pending approval.
**Targets:** v0.4.1 (post-v0.4.0)
**Issue:** [#5](https://github.com/nlink-jp/shell-agent-v2/issues/5) — analyze-data progress bubbles stuck "running"

This note specifies a small, targeted change to the agent's
activity-event protocol so long-running tools can update a
single chat bubble in place rather than spamming a fresh
"running" bubble per progress tick.

The immediate motivator is `analyze-data`: its sliding-window
summarizer emits one `tool_start` per window, and because no
matching `tool_end` ever fires for those sub-bubbles, every
window leaves a permanent "running" pill in the chat pane (#5).

---

## 1. Goals

- **Fix #5** — sub-progress bubbles for `analyze-data` no longer
  stay stuck after the tool finishes.
- **Reduce visual noise** — a multi-window analyse-data emits one
  bubble that updates in place, not N bubbles that pile up.
- **Reusable mechanism** — any future long-running tool (large
  `query-sql`, multi-file sandbox jobs, etc.) can use the same
  protocol without bespoke UI code.
- **Backwards compatible at the wire level** — the new event
  type is additive; old frontends seeing it just ignore it,
  old backends emit nothing new.

---

## 2. Wire format

### 2.1 New event Type

```
ActivityEvent.Type = "tool_progress"
```

Fields used:

| Field | Required | Meaning |
|-------|----------|---------|
| `Type` | yes | Literal `"tool_progress"` |
| `ToolCallID` | yes | The **parent** tool call's ID — the same value the parent's `tool_start` / `tool_end` carry. Lets the frontend find the bubble. |
| `Detail` | yes | New display text for the bubble. Replaces the previous `Detail` in place. |
| `Status` | n/a | Always empty. Status transitions still happen via `tool_end`. |

### 2.2 Lifecycle

For an `analyze-data` invocation that runs three windows:

```
tool_start   detail="analyze-data"                tool_call_id=tc-1
tool_progress detail="analyze-data — window 1/3"  tool_call_id=tc-1
tool_progress detail="analyze-data — window 2/3"  tool_call_id=tc-1
tool_progress detail="analyze-data — window 3/3"  tool_call_id=tc-1
tool_end     detail="analyze-data"  status=success  tool_call_id=tc-1
```

The bubble lifecycle: `running ("analyze-data")` → updated three
times to show the current window → `success ("analyze-data — window 3/3")`.
Final text is whatever the last `tool_progress` set; the
`tool_end` does not roll the text back to the parent name.

(If the user prefers the bubble to read just `"analyze-data"`
once finished, the agent can issue one final `tool_progress` with
`Detail: "analyze-data"` before `tool_end`. Out of scope here;
the visual default is "show what was last reported".)

---

## 3. Backend changes

### 3.1 `internal/agent/agent.go`

- Document `ActivityEvent.Type == "tool_progress"` in the
  comment above the struct.
- No code change to `emitActivity` — it forwards every event
  type opaquely.

### 3.2 `internal/agent/tools.go` — `toolAnalyzeData`

Replace the per-window `tool_start` callback (line 827-831)
with a per-window `tool_progress` carrying the parent's
`tool_call_id`:

```go
// Capture the parent ID so the progress events can target the
// existing "analyze-data" bubble instead of spawning new ones.
parentToolCallID := /* … see §3.3 below … */

result, err := summarizer.Analyze(ctx, args.Prompt, rows, func(idx, total int) {
    a.emitActivity(ActivityEvent{
        Type:       "tool_progress",
        Detail:     fmt.Sprintf("analyze-data — window %d/%d", idx+1, total),
        ToolCallID: parentToolCallID,
    })
})
```

### 3.3 Parent `ToolCallID` propagation

The current parent `tool_start` is fired in
`internal/agent/agent.go:1498` from inside `agentLoop`, where
`tc.ID` is in scope. The tool function itself
(`toolAnalyzeData`) does not currently receive the call ID.

Two options:

- **A.** Pass the `tc.ID` through to every tool function as a
  fresh argument. Cleanest API, but touches every tool signature
  in `tools.go` (~30 functions).
- **B.** Store the active tool call ID on the agent struct
  before invoking the tool, clear after. Tool functions
  read it via `a.activeToolCallID()` accessor. Localised
  change to `agentLoop` + a small accessor; tool signatures
  stay stable.

**Recommendation: B.** The narrow goal here is to fix #5 and
introduce a reusable progress mechanism, not to refactor 30 tool
signatures. The "active tool call" concept is real (only one
tool runs at a time per agent — Idle/Busy guarantees it), so
storing it on the struct is honest. If a future need arises to
make tool-call context first-class, the accessor is a clean
seam to evolve into a proper context-passed value.

Implementation sketch:

```go
// agent.go (Agent struct)
activeToolCallID string  // set by agentLoop, read by tool funcs

// agentLoop, just before invoking the tool dispatch:
a.mu.Lock()
a.activeToolCallID = tc.ID
a.mu.Unlock()
defer func() { a.mu.Lock(); a.activeToolCallID = ""; a.mu.Unlock() }()
```

Note: `a.mu` is the same lock that already guards the state
machine. Holding it for a tiny field-write is fine.

---

## 4. Frontend changes

### 4.1 `frontend/src/App.tsx` — activity event handler

Add a third `else if` branch (between `tool_end` and `tool_start`,
order-insensitive):

```typescript
} else if (data.type === 'tool_progress') {
    // Update the matching running tool-event in place. Match by
    // tool_call_id so multiple parallel tools (future: not today)
    // can't cross-contaminate each other.
    if (!data.tool_call_id) return
    setProgressTool(data.detail || '')
    setMessages(prev => {
        let idx = -1
        for (let i = prev.length - 1; i >= 0; i--) {
            const m = prev[i]
            if (m.role === 'tool-event' && m.status === 'running' && m.toolCallId === data.tool_call_id) {
                idx = i
                break
            }
        }
        if (idx === -1) return prev
        const next = prev.slice()
        next[idx] = {...next[idx], content: data.detail || ''}
        return next
    })
}
```

### 4.2 Defensive notes

- If `tool_call_id` is missing on the event (legacy backend),
  the branch is a no-op. No regression on older bundles.
- If no running bubble matches the ID (event arrived after the
  bubble already transitioned to success/error), the branch is
  a no-op. Race-safe by construction.
- The footer `progressTool` indicator is updated alongside the
  bubble so the status-bar text reflects the latest window too.

---

## 5. What's not changing

- `tool_start` / `tool_end` semantics are untouched.
- `tool_call_id` persistence in `chat.json` records — `tool_progress`
  is purely transient; nothing is recorded.
- Other tools that don't use the new event continue to work
  exactly as before.

---

## 6. Verification

### Unit
- No new agent unit tests are strictly required (the change is a
  field assignment + an event emission). A small assertion in
  `agent_test.go` could verify that `activeToolCallID` is set
  during a tool call and cleared after, but this is bookkeeping
  and the cost of adding tests outweighs the benefit.

### Manual smoke
1. Load enough rows to trigger a multi-window `analyze-data`
   (sliding-window summarizer needs the table large enough that
   one window doesn't fit).
2. Send a prompt that calls `analyze-data`.
3. **Before fix:** N "analyze-data (window k/N)" bubbles all
   stuck "running" forever.
4. **After fix:** one "analyze-data" bubble that updates its
   text to "analyze-data — window k/N" as progress advances,
   ending in success state with the last window's text.
5. Other tools (`load-data`, `query-sql`, etc.) still show
   their normal start/end bubble lifecycle unchanged.

---

## 7. Rejected alternatives

- **Emit paired `tool_end` for each sub-bubble** (Option A in
  the issue triage). Smaller backend change, but leaves N
  separate bubbles in the chat pane after a multi-window run —
  still noisy, still wasteful. Doesn't generalise to other
  long-running tools.
- **Only update the footer status-bar, no chat bubble update.**
  Loses the in-bubble visibility that this change is partly
  about (the user wants to see the window count in the chat
  history, not just in the transient footer).
- **A new persisted record type for progress.** Overkill;
  progress is transient by definition. The chat history should
  carry the final tool result, not its progress trace.

---

## 8. Out of scope

- Cross-tool concurrency (multiple tools running in parallel) —
  the agent is single-tool-at-a-time today; if that ever
  changes, `activeToolCallID` becomes a stack rather than a
  scalar, but the wire format (per-event `tool_call_id`)
  already accommodates it.
- Persisting progress for replay on session reload — out of
  scope; the persisted tool-event bubble carries only the
  final status, which is the right level of detail.
- Generalising the same protocol to non-tool agent activities
  (memory extraction progress, summarisation progress, etc.) —
  could be added later under the same event type.

# Session Delete — UX & Concurrency Hardening

**Status:** Design draft (2026-05-07); pending approval.
**Targets:** v0.4.2 (post-v0.4.1)
**Issue:** [#6](https://github.com/nlink-jp/shell-agent-v2/issues/6) — session delete: no confirmation, no in-flight feedback, no state-machine integration

This note specifies a three-part fix for session deletion. The
delete path is the only destructive operation in the sidebar
that doesn't ask for confirmation, gives no progress feedback,
and isn't gated by the agent state machine — the same machine
that already serialises Send / Load / Export / Import. Any one
of those gaps would be a defect on its own; together they
allow real data corruption (active session deleted mid-Send →
chat.json reanimates as a partial file).

---

## 1. Goals

- **No accidental deletion** — a stray click on the X icon
  should not destroy a session's chat, memory, findings,
  sandbox files, and DuckDB.
- **Visible work** — multi-second deletes (large objstore,
  large `work/` tree) must show that something is happening,
  not look like the click was lost.
- **No concurrent operations during delete** — the same
  busy-gate that protects Export / Import / Send / Load also
  applies to Delete. Deleting the active session leaves the
  agent in a clean post-delete state (no dangling pointer to a
  removed session that a follow-up Save would resurrect).

---

## 2. Concurrency story today (the bug)

`bindings.DeleteSession` only checks `IsBusy()` at entry; it
does **not** transition the agent state to Busy for the
duration of the delete. So while the delete is in flight:

- `agent.State == StateIdle`
- `IsBusy()` returns false
- Other entry points (`Send`, `LoadSession`, `NewSession`,
  `ExportSession`, `ImportSession`) are not blocked
- The frontend has no idea anything is happening

Concrete failure modes:

| # | Sequence | Result |
|---|----------|--------|
| F1 | Delete A → user clicks LoadSession(A) before delete finishes | Race on `chat.json`: read may succeed (partial) or fail mid-RemoveAll |
| F2 | Delete the active session A → user types in chat input → Send | `Send` passes the `IsBusy` gate; `agentLoop` runs; `a.session.AddUserMessage` mutates the still-pointed `*Session`; the trailing `a.session.Save()` writes to `sessions/A/chat.json`. `os.MkdirAll` in `Save()` recreates the directory. Net effect: session A "comes back" as a partially populated dir, separate from the user's intent and disconnected from sidebar listings. |
| F3 | Delete A → ExportSession(A) | Similar race on the per-session files; export bundle may be incomplete or fail mid-stream |
| F4 | Two simultaneous deletes of the same session | Both call objstore.DeleteBySession + sandbox.Stop + RemoveAll concurrently — usually no harm, but sandbox container teardown can return spurious errors. |

The fix is the same shape as the Export / Import work in
v0.4.0: hold the agent state in Busy for the operation's
lifetime. All the failure modes evaporate because every
competing entry point fails fast at the `state != Idle` check.

---

## 3. Design

### 3.1 Backend: `agent.DeleteSession`

A new method on `*Agent` that owns the orchestration:

```go
// DeleteSession removes a session's per-session files, its
// objstore objects, and its sandbox container. Held under the
// agent state-machine slot for the duration so concurrent
// Send / Load / Export / Import calls return ErrBusy. If
// sessionID names the active session, the agent's per-session
// pointers (session, sessionMemory, findings, analysis Engine)
// are cleared / closed before the dir is removed; the binding
// layer is responsible for switching to a different session
// (or creating a fresh one) after this returns.
func (a *Agent) DeleteSession(ctx context.Context, sessionID string) error {
    a.postTasksWg.Wait()
    a.mu.Lock()
    if a.state != StateIdle { a.mu.Unlock(); return ErrBusy }
    a.state = StateBusy
    a.mu.Unlock()
    defer func() { a.mu.Lock(); a.state = StateIdle; a.mu.Unlock() }()

    isActive := a.session != nil && a.session.ID == sessionID
    if isActive {
        if a.analysis != nil { _ = a.analysis.Close() }
        a.session = nil
        a.sessionMemory = nil
        a.findings = nil
    }
    if a.objects != nil { _ = a.objects.DeleteBySession(sessionID) }
    if a.sandbox != nil { _ = a.SandboxStop(ctx, sessionID) }
    return memory.DeleteSessionDir(sessionID)
}
```

Notes:
- The state guard is the same `Agent.mu` that already serialises
  Send / Load / Export / Import. No new lock is introduced.
- Closing the analysis Engine before deleting the directory
  releases the DuckDB file handle so RemoveAll on the
  containing dir doesn't fight the open `*sql.DB`.
- Clearing `a.session`/`a.sessionMemory`/`a.findings` to nil
  prevents F2 (the post-Send Save reanimating the dir): even
  if a Send somehow slipped through (it can't, because state
  is Busy), it would fail safely on the nil session.
- The error from `memory.DeleteSessionDir` is the only one
  surfaced to the caller; the others (objstore index save,
  sandbox stop) are best-effort, mirroring the current
  bindings layer's behaviour.

### 3.2 Bindings layer

`bindings.DeleteSession` becomes a thin pass-through:

```go
func (b *Bindings) DeleteSession(sessionID string) error {
    return b.agent.DeleteSession(b.ctx, sessionID)
}
```

The pre-existing `IsBusy` early-exit is gone — the agent
method's own state-machine gate is the single source of truth,
matching the post-v0.4.0 ExportSession / ImportSession
shape.

### 3.3 Frontend: 2-click confirm + Deleting state

Two new pieces of per-row state in `Sidebar.tsx`:

```typescript
// Per-row: the session whose X is currently in the "confirm?"
// state. Only one row can be in confirm at a time.
const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null)
// Per-row: the session whose delete request is in flight.
const [deletingSession, setDeletingSession] = useState<string | null>(null)
```

Click flow on the X:

1. **Idle** → user clicks X → state goes to `confirmingDelete = id`.
   The X icon swaps to ✓?, with `aria-label="Confirm delete"`.
2. A 3-second timer arms; on expiry, `confirmingDelete = null`
   (clears back to X). Clicking outside the row also clears.
3. **Confirming** → user clicks ✓? again →
   `deletingSession = id`, `confirmingDelete = null`. The row
   becomes greyed; the X / ✎ buttons are disabled; the title
   is replaced (or accompanied) by "Deleting…".
4. The handler awaits `Bindings.DeleteSession(id)`. On
   resolve, the parent (`App.tsx`) refreshes the session list
   and (if the active session was deleted) auto-switches —
   the existing logic in `handleDeleteSession` already covers
   both branches.
5. **Deleting** → handler completes →
   `deletingSession = null` (the row is gone from the list
   anyway because `sessions` was refreshed).

If `Bindings.DeleteSession` rejects (e.g. `ErrBusy` because
the agent is doing post-tasks), the row's `Deleting…` state
clears and an error toast / inline message surfaces. The
remaining sessions list is re-fetched defensively.

### 3.4 Visual treatment

- **Confirm state** — X glyph swapped for ✓ with a small
  question-mark superscript or a `?` suffix; same width so the
  row doesn't reflow. Tooltip changes to "Click again to
  confirm; or click elsewhere to cancel".
- **Deleting state** — row text colour drops to `--text-dim`;
  a small inline spinner (CSS `@keyframes spin` on a single
  Unicode char like `↻`) prepends the title, replacing the
  date. All buttons in the row use `disabled` + `pointer-events: none`.

### 3.5 Activity event for global awareness

Optional: emit an `agent:activity` event of type `tool_start`
+ `tool_end` around the delete (Detail = `delete-session
<short-id>`). This would let the existing footer
"progress tool" indicator show the delete too, free of charge.

**Decision: skip for now.** The per-row spinner is sufficient
and more localized. Adding a global event for delete blurs the
"tool" terminology and would require frontend filtering to
avoid showing it as a chat-pane bubble.

---

## 4. Edge cases

| Case | Behaviour |
|------|-----------|
| Confirm timer expires (3 s) without second click | Row reverts to X, no action |
| User clicks elsewhere in the sidebar during confirm | Confirm clears (event listener on document for click-outside) |
| Confirm on session A, then start confirming on B | A's confirm clears, B is now confirming (single-row invariant) |
| Confirm + click rename | Rename takes priority; confirm clears |
| Delete the only session | Same as today: handler auto-creates a new "New Session" after delete (existing App.tsx logic) |
| Delete the active session | Same as today: handler auto-switches to the first remaining session (existing App.tsx logic). Now safe because `agent.session` is nil-cleared inside DeleteSession before the LoadSession runs. |
| Two delete clicks in rapid succession (double-click race) | First click enters confirm; second click confirms; treated as intentional. Acceptable risk vs. the ergonomics of single-click-then-single-click. |
| Delete of session A while ExportSession of session B is running | Both go through `Agent.mu`'s state gate. The second to arrive returns ErrBusy. Frontend surfaces the error and clears the deleting state. |
| App crash mid-delete | Partial deletion possible (objstore index updated but dir not removed, etc.). Same as today; not addressed by this change. The atomicio writes mean the index/dir won't be torn at byte level. |

---

## 5. Implementation phases

Single phase — the change is small and the three parts (Q3 / Q4
/ Q5 in the project tasks) are tightly coupled.

1. **Backend** (Q3): `agent.DeleteSession`, `bindings.DeleteSession`
   pass-through. Build green.
2. **Tests** (Q4): three agent tests — RejectsWhenBusy,
   Inactive, Active (clears session pointer).
3. **Frontend** (Q5): per-row state, click handler, visual
   states. Frontend build + manual smoke.

---

## 6. Verification

### Unit
- `TestAgent_DeleteSession_RejectsWhenBusy` — state Busy →
  ErrBusy.
- `TestAgent_DeleteSession_Inactive` — different active session,
  no interference; deleted session's dir gone, active session
  intact.
- `TestAgent_DeleteSession_Active` — active session is the one
  deleted; `a.session`, `a.sessionMemory`, `a.findings` are nil
  after; dir is gone; `a.analysis.Close()` called.

### Manual smoke
1. Click X on a non-active session → row shows ✓? for ~3s →
   click again → row greys, "Deleting…" appears, then the row
   disappears.
2. Click X, wait 3s without clicking again → row reverts to X.
3. Click X on session A, click elsewhere in the sidebar → row
   reverts to X.
4. Click ✓? on the active session → row greys → after delete,
   sidebar auto-switches to another session (or auto-creates
   one if none remain).
5. Start a Send (long-running LLM call) → click X → confirm →
   binding rejects with ErrBusy → row's deleting state clears
   and an error message surfaces.
6. (Race attempt) Click ✓? on session A, then immediately try
   to LoadSession(B) while delete is in flight → LoadSession
   returns ErrBusy from the busy-gate; UI ignores it (button
   should already be disabled by the same Busy state).

---

## 7. Rejected alternatives

- **Modal "Are you sure?" dialog** — works, but breaks the
  flow more than 2-click confirm and is inconsistent with the
  existing in-row 2-click bulk-delete UX for Findings /
  Global Memory / Session Memory.
- **Trash / soft-delete with N-day retention** — overkill for
  a single-user local app. Adds restore UI, retention policy,
  and a cleanup job that nobody asked for.
- **Lock the WHOLE sidebar during delete** (e.g. an overlay
  spinner) — global-level feedback for a per-row operation.
  The per-row state handles it more precisely.
- **Add the per-session export lock to delete** (the lock
  v0.4.0 dropped) — redundant; the agent state machine already
  serialises every entry point.

---

## 8. Out of scope

- **Bulk session delete** (select multiple sessions, delete
  all). Today's UX is one-by-one; bulk could be added later
  with the same per-row state pattern.
- **Undo within a session of deleting another session** — not
  worth the storage cost; the user retains responsibility for
  the export/backup workflow if they want recoverability
  (v0.4.0's `.shellagent` bundles).
- **Confirmation timeout configurability** — 3 s is a
  reasonable default; users who want a longer or shorter
  window can ask.

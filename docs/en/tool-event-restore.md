# Tool-event restore on session load

## Status

Design — to be implemented after sign-off.

## Problem

When a chat turn runs tool calls, the live UI shows each call as a
`tool-event` bubble: a small inline row with the tool name and a
status (running → success / error). On session restore those
bubbles disappear:

```go
// bindings.go LoadSession
case "tool":
    continue
```

Restored sessions therefore look like a thread of plain
user/assistant text with all activity context erased. For a
conversation that produced charts, ran SQL, or registered objects
in the sandbox, that is a meaningful loss of provenance — the user
can no longer tell which tool produced which artifact, in what
order, or whether any of them errored.

The information needed to reconstruct the bubbles is *almost* in
the session already. `memory.Record` for a tool turn carries:

- `Role: "tool"`
- `ToolName` — the tool that ran
- `ToolCallID` — Vertex/OpenAI tool-call linkage (unused at restore)
- `Content` — the tool result text
- `Timestamp`

What's missing is the **status** (success / error). Live runs
classify each tool result via `executeTool`'s
`ActivityEventStatus` return — that classification is fed to the
activity event but is not persisted with the tool record.

## Goals

- Restored sessions show the same `tool-event` bubbles as the live
  view, in the same order, with correct success / error styling.
- New sessions persist enough information to make this exact.
- Old sessions (already on disk, no status field) still load
  cleanly — they default to `success`, which is the common case.
- The `Cancel` "Cancelled" tool-result variant continues to render
  reasonably; we do not invent a third status for it.

## Non-goals

- Replaying the *transient* `running` state during restore — the
  tool isn't actually in flight, so a frozen "running" pulse would
  be misleading. Restored bubbles always end at `success` or
  `error`.
- Surfacing tool **arguments** in the restored bubble. Live bubbles
  only show the tool name (`data.detail`); restore mirrors that.
- Re-rendering the `progressTool` "thinking" banner that was
  attached to a tool-call assistant turn. That banner is by design
  ephemeral and tied to the in-flight phase.
- Attaching the tool **result** to the bubble. Today the result
  goes back to the LLM but not to the chat (the user only ever
  sees the bubble). Same on restore.

## Design

### 1. Persist tool-result status

Add a `Status` field on `memory.Record`:

```go
// memory/memory.go
type Record struct {
    // … existing fields …
    Status string `json:"status,omitempty"` // populated only when Role == "tool"
}
```

Allowed values: `"success"`, `"error"`. Empty string is a legacy
record (pre-this-change) and is treated as `success` at the
read-side.

Update `AddToolResult` to accept the status:

```go
// memory/memory.go (signature change)
func (s *Session) AddToolResult(callID, toolName, result, status string)
```

Update the agent's loop to pass the value it already has:

```go
// agent/agent.go (existing line)
result, status := a.executeTool(ctx, tc)
a.session.AddToolResult(tc.ID, tc.Name, result, string(status))
```

`status` here is the `ActivityEventStatus` already used for the
`tool_end` activity event — same source of truth, no risk of drift.

### 2. Surface status to the frontend

`MessageData` carries the bubble shape across the Wails boundary.
Add an optional status field:

```go
// bindings.go
type MessageData struct {
    Role      string `json:"role"`
    Content   string `json:"content"`
    Timestamp string `json:"timestamp"`
    Status    string `json:"status,omitempty"`
}
```

Mirror on the TS side:

```ts
// frontend/src/types.ts
export interface MessageData {
    role: string;
    content: string;
    timestamp: string;
    status?: 'success' | 'error';
}
```

### 3. Map tool records to bubbles in LoadSession

Replace the `continue` with a constructed `tool-event` row:

```go
// bindings.go LoadSession
case "tool":
    status := r.Status
    if status == "" {
        // Pre-existing sessions that predate this change; we
        // never fail-flag silently, so default to success.
        status = "success"
    }
    msgs = append(msgs, MessageData{
        Role:      "tool-event",
        Content:   r.ToolName,
        Status:    status,
        Timestamp: r.Timestamp.Format("15:04:05"),
    })
```

Records with empty `ToolName` (shouldn't happen but old debug data
might) skip with `continue` so we don't render an empty bubble.

### 4. Frontend mapping

`App.tsx` already converts `MessageData` → `ChatMessage` when it
fills `messages` from `LoadSession`. Update that mapping to copy
`status` through. The existing `MessageItem` already renders
`tool-event` rows from `ChatMessage.status`, so no component
changes are needed downstream.

## Backward compatibility

- `Record.Status` is `omitempty`: existing chat.json files load
  with `Status == ""` and read as `success`.
- `AddToolResult` signature change: callers are limited to the
  agent loop. Tests that build `Record` directly (without going
  through `AddToolResult`) need a one-field tweak; surveyed below.
- Live behaviour is unchanged — the same status that drives the
  `tool_end` activity event now also lands on the persisted
  record.

## File-level changes

| File | Change |
|------|--------|
| `app/internal/memory/memory.go` | Add `Status` field; update `AddToolResult` signature. |
| `app/internal/memory/memory_test.go` (and any callers) | Adjust test setups. |
| `app/internal/agent/agent.go` | Pass `string(status)` from `executeTool` into `AddToolResult`. |
| `app/bindings.go` | Add `Status` to `MessageData`; replace `case "tool": continue` with bubble construction. |
| `app/frontend/src/types.ts` | Add optional `status` to `MessageData`. |
| `app/frontend/src/App.tsx` | Copy `status` when mapping `MessageData[]` → `ChatMessage[]` after `LoadSession`. |

No new files; no new dialogs.

## Test strategy

### Backend (Go)

- **`memory_test.go`**:
  - `AddToolResult(_, _, _, "success")` → record has `Status="success"`
    and round-trips through `MarshalJSON` / `UnmarshalJSON`.
  - Loading an older session JSON (status field absent) decodes
    cleanly with `Status == ""`.
- **`bindings_test.go`** (or extension of an existing
  LoadSession-coverage test):
  - Session with `Role:"tool", Status:"success"` → restored
    `MessageData` has `Role:"tool-event"`, `Content:<toolName>`,
    `Status:"success"`.
  - Same with `Status:""` → `Status:"success"` after restore.
  - Same with `Status:"error"` → propagates.

### Manual

1. Run a turn that exercises a successful sandbox tool plus a
   deliberate failure (e.g. `pip install nonexistent`).
2. Reload the app, switch to that session.
3. Confirm both bubbles re-appear in the same order with the
   correct success / error styling — and that the failed tool
   shows its red marker.
4. Re-load an *old* session created before this change. Tool
   bubbles re-appear with success styling (legacy default).

### Verification

```sh
cd app
go test -tags no_duckdb_arrow ./internal/memory/... ./...
make build
```

End-to-end: launch the .app, load a session that contains tool
calls, confirm the chat thread now shows the `tool-event` bubbles.

## Out of scope (explicitly)

- Persisting tool **arguments** for a richer restored bubble.
- Replaying live `running` state on restore.
- Surfacing the tool's textual result to the user (live or
  restored — by design only the LLM sees it).
- Cancelled-state handling — the existing `(Cancelled)` content
  string on cancelled turns still flows through; status remains
  `success` (the tool didn't error, the user aborted).

## Risks

- **Test sweep**: callers of `AddToolResult` must pass the new
  parameter. Compile-time safety catches everything; the risk is
  cosmetic (slightly larger PR diff).
- **Old data**: `Status==""` defaults to `success`. A historical
  failed tool will now render as a green bubble instead of red.
  Acceptable — the alternative (defaulting to `error`) would
  mislabel the much-more-common success case.
- **Schema growth**: `memory.Record` gains another field. The
  struct already carries Vertex-specific (`ToolCalls`),
  legacy-specific (`SummaryRange`), and image-specific fields, so
  one more `omitempty` string is consistent with what's there.

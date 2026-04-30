# TODO

Tracked items that aren't ready for an implementation pass yet —
either because the design needs more thought, or because the work
is too large to fit into the current release stream.

## Tool-bubble success/failure indication

**Status as of 2026-04-30**: Phase A (event schema) and Phase B-1
(sandbox classification) shipped. The agent-side wiring is in
place: `executeTool` returns `(string, ActivityEventStatus)` and
each branch sets the status explicitly. The chat tool-event
bubble now renders red on `error` and green on `success`.

What's actually classified today:

| Tool family            | Classification source                                                |
|------------------------|----------------------------------------------------------------------|
| sandbox-run-shell/-py  | `ExecResult.ExitCode` / `TimedOut`                                   |
| sandbox-{write,copy,…} | typed `(string, ActivityEventStatus)` return per branch              |
| analysis-*             | `executeAnalysisTool` Go-side `error` (incl. MITL rejection)         |
| MCP (`mcp__*`)         | RPC error → Go err, *or* `result.isError:true` → `ErrToolFailed`     |
| shell-script tools     | `toolcall.Execute` Go-side `error`                                   |
| list-objects, get-object, resolve-date | per-branch `err`                                     |

### Possible later cleanups

- Drop the soft-fallback `'done'` status in the frontend tool-
  event union once we're confident no in-flight events still
  arrive without a `status` field.

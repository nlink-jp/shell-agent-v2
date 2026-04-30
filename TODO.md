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

| Tool family            | Classification source                    |
|------------------------|------------------------------------------|
| sandbox-run-shell/-py  | `ExecResult.ExitCode` / `TimedOut`       |
| sandbox-{write,copy,…} | `"Error:"` prefix in result string       |
| analysis-*             | `executeAnalysisTool` Go-side `error`    |
| MCP (`mcp__*`)         | `Guardian.CallTool` Go-side `error`      |
| shell-script tools     | `toolcall.Execute` Go-side `error`       |
| list-objects, get-object, resolve-date | per-branch `err`         |

### Outstanding refinement: MCP `result.isError`

The MCP spec lets a tool succeed at the RPC layer but fail at the
tool layer by setting `result.isError: true`. Our `Guardian.
CallTool` returns `(json.RawMessage, error)`; it surfaces RPC
errors as Go errors, but ignores `isError` inside a successful
RPC response. So an MCP tool that explicitly reports a tool-level
failure currently shows a green check.

Fix when this becomes a real problem: parse the MCP response
inside `CallTool`, look at `isError` on the result, and either
return a Go error or extend the return tuple with a bool. Then
the agent branch maps that to `ActivityStatusError`. Low priority
for now — none of the MCP servers we ship use this pattern.

### Possible later cleanups

- Drop the soft-fallback `'done'` status in the frontend tool-
  event union once we're confident no in-flight events still
  arrive without a `status` field.
- Drop `wrapErrorPrefix` once `sandbox-write-file` etc. are
  refactored to return `(string, error)` directly. The string-
  prefix sniffing is fragile — moving the dispatchers to typed
  errors makes this disappear.

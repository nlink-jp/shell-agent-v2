# Aborting in-flight MCP tool calls — Design Note

**Status:** Design draft (2026-05-12); pending approval.
**Target:** v0.6.1 (point release on top of v0.6.0)
**Reported by:** User — "MCP を利用した Tool Calling 実行中に Chat を Abort できない"

This note specifies how the agent regains the ability to abort a
chat while an MCP-guardian tool call is in flight. The current
v0.6.x agent honours `Abort()` for every other tool source (LLM
streaming, analysis tools, sandbox tools, shell scripts) but
silently waits for any MCP tool call to complete before the abort
takes effect.

---

## 1. Problem

The MCP dispatch site in `internal/agent/agent.go` (the
`mcp__` branch of `executeTool`, ~line 1792 as of v0.6.0) reads:

```go
result, err := g.CallTool(toolName, json.RawMessage(tc.Arguments))
```

Two compounding issues:

1. **`mcp.Guardian.CallTool` has no `context.Context` parameter.**
   Its signature is `(name string, arguments json.RawMessage)`
   (see `internal/mcp/mcp.go`, the comment block above line 208).
2. **The underlying `Guardian.call()` blocks on
   `g.stdout.Scan()`** which has no cancellation hook. The
   package-level comment (`mcp.go` lines 50–62) acknowledges
   that the only way to unblock a hanging `Scan` is for
   `Stop()` to close stdin and kill the child process.

`Agent.Abort()` cancels the Send context, but the MCP call never
observes it; it keeps blocking inside `Scan` until the upstream
guardian responds. From the user's perspective, the Abort button
appears broken whenever a remote MCP tool is slow (rate-limited
API calls, long-running searches, etc.).

The MCP wire protocol (2024-11-05) itself has no
"tool call cancel" notification, so there is no graceful way to
ask the upstream server to stop work. Killing the child process
is the only reliable interruption mechanism.

---

## 2. Goals

- **Make `Abort()` interrupt MCP tool calls** in bounded time
  (≤ 100 ms of overhead vs. the existing immediate Abort for
  other tool sources).
- **Per-guardian blast radius.** Aborting one MCP tool call must
  not knock out unrelated guardians.
- **No surprise re-runs.** A cancelled tool call surfaces a
  clear `(Cancelled by user)` result string and ends the agent's
  current Send, exactly like the other Abort paths.
- **Self-healing.** The next user message that targets the same
  guardian must work — i.e. the guardian process must be
  re-spawned before the next call would otherwise hit
  `"guardian is stopped"`.
- **Process-tree hygiene.** No orphaned child processes after an
  Abort; no goroutine leaks beyond the in-flight call's natural
  unwind.

Non-goals:

- Graceful in-protocol cancellation (impossible with MCP
  2024-11-05).
- Cancelling work the upstream MCP server has already kicked
  off (external API requests, etc.) — those run to completion
  on the server side; we just stop waiting for the result.

---

## 3. Design

### 3.1 New `Guardian.CallToolContext`

Add a context-aware wrapper around the existing `CallTool` so
the cancellation path is concentrated in the `mcp` package
where the goroutine + `Stop()` interaction is already
understood:

```go
// CallToolContext is CallTool plus context-driven cancellation.
// On ctx.Done() before the upstream replies, the guardian process
// is killed via Stop() to unblock the in-flight stdout.Scan, and
// ctx.Err() is returned. The orphan goroutine then exits with a
// read error; the result it eventually writes to the buffered
// channel is harmless.
//
// IMPORTANT: a cancelled CallToolContext renders the guardian
// unusable (Stop sets g.stopped = true permanently). Callers must
// re-spawn the guardian before issuing the next CallTool — see
// Agent.restartGuardian.
func (g *Guardian) CallToolContext(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error)
```

Implementation sketch:

```go
func (g *Guardian) CallToolContext(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
    type res struct {
        body json.RawMessage
        err  error
    }
    ch := make(chan res, 1) // buffered so the goroutine never blocks
    go func() {
        b, e := g.CallTool(name, arguments)
        ch <- res{b, e}
    }()
    select {
    case r := <-ch:
        return r.body, r.err
    case <-ctx.Done():
        _ = g.Stop() // unblocks the goroutine's stdout.Scan
        return nil, ctx.Err()
    }
}
```

Why a goroutine rather than rewriting `call()` to use ctx-aware
I/O? The `bufio.Scanner` API has no cancellation, and lifting
the entire JSON-RPC machinery to context-aware net I/O would be
a much larger refactor for marginal gain. The
"goroutine + Stop"  pattern is consistent with the package's
existing concurrency model documented at `mcp.go:50–62`.

### 3.2 Per-guardian restart helper

Refactor `startGuardians` to extract a single-profile spawn
helper:

```go
// spawnGuardian builds and starts a guardian for one profile.
// Returns the live guardian (or nil) plus the MCPStatus to
// record. Caller holds guardiansMu. Pure helper — used by both
// startGuardians (boot) and restartGuardian (post-abort).
func (a *Agent) spawnGuardian(p config.MCPProfile) (*mcp.Guardian, MCPStatus)
```

Then add a public-to-agent (private-to-package) restart:

```go
// restartGuardian re-spawns the named guardian using the current
// config. Safe to call from a goroutine — takes guardiansMu and
// swaps the map entry atomically. Used after CallToolContext
// reports context.Canceled: the cancellation killed the child
// process to unblock Scan, so the map entry is now a dead handle.
func (a *Agent) restartGuardian(name string)
```

`restartGuardian` updates `a.mcpStatuses` in place so the
Settings UI reflects the transient "restarting" then "running"
states.

### 3.3 Dispatcher change

The MCP branch of `executeTool` becomes:

```go
result, err := g.CallToolContext(ctx, toolName, json.RawMessage(tc.Arguments))
if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
    // CallToolContext killed the child process to break out of
    // the blocked Scan. Re-spawn asynchronously so the next user
    // turn can use this guardian again. The MCP protocol has no
    // tool-call cancellation notification, so this kill-and-respawn
    // is the only way to make Abort responsive (see docs/en/mcp-abort.md).
    go a.restartGuardian(guardianName)
    return "(Cancelled by user)", ActivityStatusError
}
```

The rest of the branch (`ErrToolFailed` handling, generic error
mapping) is unchanged.

### 3.4 Wire-level visibility

No new activity-event types or wire-format changes. The
cancelled tool's bubble flips to `error` status via the existing
`ActivityStatusError` path and shows the `(Cancelled by user)`
detail — identical to how analyze-data / sandbox tools surface
their own abort cases. Frontend changes: none.

---

## 4. Testing

### 4.1 `mcp` package — unit

Add to `internal/mcp/guardian_test.go`:

- **`TestGuardian_CallToolContextRespectsCancel`** — start a
  Python stub that sleeps 10 s before responding. Issue
  `CallToolContext` with a context cancelled after 100 ms.
  Assert: returns `context.Canceled` within ~200 ms, and the
  guardian's `cmd.Process` is no longer running.
- **`TestGuardian_CallToolContextSucceedsWhenFast`** — issue
  `CallToolContext` against the existing `hello` tool with a
  context that has plenty of time. Assert: returns the body
  successfully, equivalent to `CallTool`.
- **`TestGuardian_CallToolContextDoesNotLeakGoroutine`** — after
  a cancelled call, give the orphan goroutine a beat (`time.Sleep
  50ms`) and assert the in-flight call exited (no goroutine leak
  via `runtime.NumGoroutine` delta or via the result channel
  being drained — the latter is easier and sufficient).

### 4.2 `agent` package — integration

Add to `internal/agent/integration_test.go` or a new
`mcp_abort_test.go`:

- **`TestAgent_AbortDuringMCPTool`** — wire a mock guardian that
  blocks indefinitely, drive a Send through, call `Abort()`,
  assert the Send returns `(Cancelled)` within ~500 ms and the
  guardian map entry has been replaced (different `*Guardian`
  pointer, or `MCPStatuses()[i].Status == "running"` with a
  refreshed PID).

The agent test will need a way to inject a mock guardian into
the map. Simplest: a test-only helper `setGuardianForTest(name,
g)` gated by `//go:build testing` or just exported under the
package's existing internal-only access. (`a.guardians` is
already package-private and the test lives in the same package,
so no build tag is needed — the test can write to the map
directly via a test helper.)

---

## 5. Rejected alternatives

### 5.1 Restart all guardians on Abort

The existing `RestartGuardians()` already exists and would be a
one-line change at the dispatch site. Rejected because aborting
one tool would knock out every other (potentially long-lived,
authenticated) guardian session, including ones unrelated to
the in-flight call. The per-guardian helper is ~30 LOC of
refactor and is strictly better behaviour.

### 5.2 Keep the guardian, drain the orphan response in the background

Instead of killing the process, run the original call in a
goroutine that pushes its eventual response into a "discard
bin" channel. The guardian's `callMu` would still be held
until the upstream replies, so subsequent calls block — same
UX bug, just relocated. Rejected.

### 5.3 Rewrite `call()` over context-aware net I/O

Replace `bufio.Scanner` with a manual `os.Pipe` + `SetReadDeadline`
loop or a goroutine pumping bytes into a channel that `call()`
selects on. Would let ctx cancellation propagate cleanly without
killing the child. Rejected for v0.6.1 — large change, churns
well-audited code (`security-hardening-2.md` references), and
the kill-and-respawn approach already gives the user the abort
semantics they asked for. Revisit if MCP gains protocol-level
cancellation (post-2024-11-05).

### 5.4 Add a `cancel` notification on top of MCP

Some MCP servers will adopt cancellation conventions ad hoc.
Rejected because (a) it's not in the spec we target, (b) it
would only help cooperating servers, (c) it doesn't fix the
client-side blocked-Scan problem anyway.

---

## 6. Risks & open questions

- **Orphan upstream work.** Killing the child process means any
  external request the guardian had in flight (HTTP call, DB
  query) is abandoned client-side but completes server-side.
  This is acceptable — and unavoidable without protocol-level
  cancel — but worth a one-line CHANGELOG note so users don't
  expect rate-limit refunds on abort.
- **Restart latency.** `mcp.Guardian.Start()` includes an
  `initialize` + `tools/list` round-trip with a 15 s timeout.
  In practice this is typically ~50–500 ms but a slow guardian
  could leave that profile in "restarting" state longer than the
  user expects. The restart is asynchronous, so it doesn't block
  the agent returning to idle; only the next call against
  *that specific guardian* would wait. Acceptable.
- **`a.cfg.Tools.MCPProfiles` race.** `restartGuardian` reads
  the profile list from config. Config mutation happens via
  separate Settings paths that already take their own locks;
  the read-only access here under `guardiansMu` is fine.
  Worth a comment in the helper.
- **Goroutine leak on hung Stop.** If `cmd.Process.Kill()`
  itself somehow blocks (it shouldn't on macOS/Linux for a
  running process), the cancel path would never return. No
  mitigation planned — if `Kill` is unreliable we have larger
  problems. The `time.After`-bounded Start timeout (15 s) is
  the implicit backstop on the restart path.
- **MITL prompt during MCP call + Abort.** If the user aborts
  while the MITL approval dialog is open (before `g.CallTool`
  fires), `requestMITL` already wraps the prompt in the same
  context, so the path returns rejection text and never enters
  `CallToolContext`. No change needed.

---

## 7. Compatibility

- LLM-observable: cancelled MCP tool calls now return the
  literal string `(Cancelled by user)` instead of hanging. The
  bubble status flips to `error`. Existing prompts shouldn't
  care; no system-prompt change.
- API: `Guardian.CallTool` keeps its signature. The new
  `CallToolContext` is purely additive.
- Bindings / frontend: no changes.
- Session export/import: no changes (cancelled tool calls
  already serialise correctly as error-status tool results).

---

## 8. Rollout

Single commit per phase, all bisect-friendly:

1. `feat(mcp): add CallToolContext for cancellable tool calls`
   — mcp.go + guardian_test.go additions.
2. `refactor(agent): extract spawnGuardian helper from startGuardians`
   — pure refactor, no behaviour change.
3. `feat(agent): restart MCP guardian after aborted tool call`
   — `restartGuardian` + dispatcher change + agent test.
4. `docs: describe MCP abort + guardian restart` — this note +
   architecture roll-up + CHANGELOG draft.
5. `chore: release v0.6.1` — release commit + tag.

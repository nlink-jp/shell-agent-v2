# ADR-0024: Non-blocking startup and deterministic session restore

- Status: Accepted
- Deciders: magi
- Related: startup regression report (slow window restore + no session selected on launch); ADR-0021 (state machine consistency), ADR-0022 (agent file decomposition)

## 1. Context

Two startup regressions are reported on recent builds:

1. **Slow window restore + late input-readiness.** After launch the
   window stays at the default 1024×768 and the UI is not interactive
   for a long time before snapping to the saved size.
2. **No session selected.** After launch the sidebar lists sessions but
   none is selected — an indeterminate state where the last-active
   session is not restored.

Both trace to a single cause: **`bindings.startup()` (the Wails
`OnStartup` callback) performs heavy, externally-blocking work
synchronously, and the UI-critical steps sit behind it.**

### 1.1. The blocking path

`bindings.startup()` (`app/bindings.go:68`) calls
`agent.New(cfg)` (`app/internal/agent/agent.go:220`) synchronously at
`bindings.go:91`. Inside `New`, two calls reach external systems with
no timeout:

- `a.startGuardians()` (`agent.go:248` → `agent_mcp.go:37`) spawns each
  configured MCP guardian **process** and blocks on its
  `initialize` + `tools/list` handshake (`g.Start()`).
- `a.maybeStartSandbox()` (`agent.go:249`, defined at `agent.go:331`)
  probes the container engine: `eng.ImageReady(context.Background(), …)`
  and `eng.StopAll(context.Background())` shell out to podman/docker
  **with `context.Background()` — no timeout.** A stopped podman
  machine or an unresponsive Docker daemon makes this hang.

Window restore (`bindings.go:226-231`,
`wailsRuntime.WindowSetSize` / `WindowSetPosition`) is the **last**
thing `startup()` does, so it cannot run until the blocking calls
return. Hence symptom 1.

### 1.2. The session-restore race

In Wails v2 the webview loads and JS binding calls are dispatched on
their own goroutines while `OnStartup` is still running. The frontend's
one-shot init effect (`app/frontend/src/App.tsx:707-724`, deps `[]`)
runs during that window:

```ts
const s = await window.go.main.Bindings.ListSessions()   // works
setSessions(s || [])
if (!s || s.length === 0) { /* create */ }
else {
    const msgs = await window.go.main.Bindings.LoadSession(s[0].id) // FAILS
    setCurrentSessionId(s[0].id)                                    // never reached
    setMessages(restoredMessages(msgs))
}
```

- `ListSessions()` → `memory.ListSessions()` (`bindings.go:619`) reads
  session metadata straight from disk and does **not** touch `b.agent`,
  so the sidebar populates.
- `LoadSession(s[0].id)` (`bindings.go:465`) starts with
  `if b.agent == nil { return …"agent not initialised yet" }`
  (`bindings.go:466-468`). While `agent.New` is still blocked, `b.agent`
  is nil, so the call rejects, the async IIFE throws, and
  `setCurrentSessionId` never runs.
- The effect has `[]` deps and **no retry**, so the app is stuck with
  no session selected. Hence symptom 2.

The slower the startup (§1.1), the wider the race window, which is why
the two symptoms appear together and why the onset correlates with
anything that makes guardian/sandbox init slower (a newly added MCP
profile, an enabled sandbox, a stopped podman machine) even without a
code change to the startup ordering itself.

## 2. Decision

Make startup non-blocking for everything the user needs to see a sized,
interactive window with their last session, defer the slow external
work to the background, and gate message input until that background
work is done so no user action can race a half-initialised agent. Five
parts, shipped together.

### Part A — Size the window at creation, not after startup

Read the saved window geometry in `main()` **before** `wails.Run` and
pass it as the initial `Width`/`Height` (and position) in
`options.App`, replacing the hardcoded `1024`/`768`
(`main.go:76-77`). Remove the post-startup `WindowSetSize` /
`WindowSetPosition` (`bindings.go:226-231`).

The window then opens at the correct size with no resize jump, and it
no longer depends on `startup()` finishing. This mirrors how csv-editor
restores window size (config read in `main()`, passed to `wails.Run`).
A sane default and a minimum-dimension floor are kept for first launch
and absurd persisted values.

### Part B — Return the agent fast; defer external init to the background

`agent.New` keeps the cheap, local construction it already does
(registry scan, chat engine, memory/sysrules load, descriptor
registration) and returns the fully-usable `*Agent` **without** the two
blocking calls. They move into a new
`(*Agent).StartBackground(ctx context.Context)` that `bindings.startup`
launches in a goroutine after assigning `b.agent`:

```go
b.agent = agent.New(cfg)        // fast: b.agent is non-nil immediately
b.agent.SetObjects(b.objects)
b.agent.SetHandlers(...)
wailsRuntime.EventsEmit(b.ctx, "app:ready", nil)   // session restore may proceed
go func() {
    b.agent.StartBackground(b.ctx)                 // startGuardians + maybeStartSandbox
    wailsRuntime.EventsEmit(b.ctx, "tools:ready", nil)  // composer may enable
}()
```

### Part C — Bound the sandbox probes with a timeout

Replace the `context.Background()` in `maybeStartSandbox`'s
`ImageReady` and `StopAll` probes (`agent.go:367`, `agent.go:383`) with
a `context.WithTimeout` (proposed 5s). Even in the background, an
unresponsive engine must not leave a goroutine hung forever; on timeout
we log and leave sandbox tools hidden, exactly as the existing
error-return paths already do. This also bounds the worst-case composer
gate in Part D.

### Part D — Two readiness signals; gate the composer until tools are ready

Two distinct readiness levels, two events, two consequences:

| Signal | Emitted when | Frontend consequence |
|---|---|---|
| `app:ready` | right after `b.agent` is assigned (Part B), before the goroutine | session restore may run; history + sidebar + session switching usable |
| `tools:ready` | when `StartBackground` returns (guardians + sandbox done/timed-out) | the message composer (SEND) is enabled |

**Session restore** (the `LoadSession(s[0])` path) keys off `app:ready`,
which fires essentially immediately because `agent.New` is now fast.
Robustness against lost events / mount ordering: the frontend also calls
a small `Ready() bool` binding on mount (covers the case where
`app:ready` was emitted before the listener attached), and `LoadSession`
rejection with "agent not initialised yet" triggers one bounded retry.

**Message input** is disabled with an "initializing…" affordance until
`tools:ready`. This is the decision for the "user input races init"
concern (§3.2): rather than reason about a SEND landing mid-init, we
forbid it. The rest of the UI is fully usable meanwhile (read history,
switch sessions, open settings). In the common case `tools:ready` fires
sub-second; with sandbox enabled and the engine down it is bounded by
Part C's 5s timeout. The frontend also queries a `ToolsReady() bool`
binding on mount for the missed-event case.

### Part E — Remove the dead `LastSession` field; restore via `s[0]`

`config.Config.LastSession` (`internal/config/config.go:340`) is the
**only** reference to itself in the entire codebase — no reader, writer,
binding, or frontend use. It is a lone orphaned field (added
speculatively in the window-state commit, never wired), not the tip of a
half-built feature. **Delete it.** `omitempty` + zero writers means no
config.json in the wild carries `last_session`, so removal is safe.

Session-restore correctness needs no new machinery: once the race is
fixed (Parts B + D), the frontend loads `s[0]` — the most-recently-
updated session per `memory.ListSessions` sort (`store.go:62-64`) —
which is the last-active session in normal use. Per the "don't build
difficult machinery if it already works" guidance, we keep the `s[0]`
heuristic and do not introduce a persisted last-active pointer.

## 3. Safety during the init window

### 3.1. The `a.sandbox` data race Part B introduces

Today `maybeStartSandbox` runs synchronously inside `agent.New`, before
any concurrent access, so the unguarded field `a.sandbox` is safe.
There is **no `sandboxMu`** — `a.sandbox` is read without a lock at
`agent.go:1449`, `agent.go:2448`, and throughout `sandbox_tools.go`
(`Exec`, `WorkDir`, `RestartSandbox`, …).

Moving `maybeStartSandbox` to a goroutine means `a.sandbox = eng`
(`agent.go:376`) can run concurrently with those reads — a data race.
Part B is therefore **not complete without synchronizing `a.sandbox`.**
Decision: add `sandboxMu sync.RWMutex` and route every `a.sandbox`
read/write (and the companion `a.chat.SetSandboxEnabled` call) through
it. This matches the existing `guardiansMu` pattern, is the least
surprising option (vs `atomic.Pointer` over an interface), and fixes a
latent unguarded write that already exists today: `RestartSandbox`
mutates `a.sandbox` at runtime (`sandbox_tools.go:262-265`) with no lock.

`a.guardians` already has `guardiansMu` (`agent.go:142`); `startGuardians`
holds it for the whole spawn loop (`agent_mcp.go:42`) and the readers
`buildToolDefs` (`agent.go:2339`), `ListTools` (`agent.go:2455`) take
`RLock`. The MCP half of Part B needs no new lock.

### 3.2. What a user action during the init window can hit

Even with input gated (Part D), session switching, settings, and tool
listing remain live during init. Enumerated, each is safe:

- **MCP still spawning + any tool-def build / ListTools.** Readers take
  `guardiansMu.RLock` and block until `startGuardians` releases the
  write lock, then see the complete map. `spawnGuardian` only adds a
  guardian to the map **after** `g.Start()` succeeds, so a half-spawned
  guardian is never dispatchable. No race, no partial dispatch — at
  worst a brief lock wait.
- **Sandbox not yet initialised + a sandbox tool referenced.** Sandbox
  descriptors register unconditionally and gate on `a.sandbox != nil` at
  view/exec time (`agent.go:1449`, `agent.go:2448`). With §3.1's
  `sandboxMu`, the read is consistent and returns either nil (tool
  hidden / "sandbox unavailable") or the live engine. Graceful, no crash.
- **State machine.** The agent starts `StateIdle` (`agent.go:239`); SEND
  transitions Idle→Busy and is independent of background init. Nothing in
  `StartBackground` touches `a.state`.
- **`RestartSandbox` from Settings during boot init.** Both it and
  `StartBackground`'s `maybeStartSandbox` write `a.sandbox`; `sandboxMu`
  serialises them so there is no race, but a double-init is logically
  possible (rare: user opens Settings and toggles within the gate
  window). Guard `StartBackground`'s sandbox step with a "boot sandbox
  init done" flag so a user-triggered restart wins cleanly.

Conclusion: with §3.1 in place, the only observable effects of activity
during init are a bounded lock wait and "a tool not yet available" —
both benign and consistent with the existing dynamic-tool design. The
Part D composer gate removes even those from the SEND path.

## 4. What does NOT change

- The agent's Idle/Busy state machine, the execution loop, MITL, and the
  tool dispatch contract.
- Session file format and `memory.ListSessions` ordering / the `s[0]`
  restore heuristic.
- The set of tools eventually available; only their *availability
  timing* shifts from "before first paint" to "by `tools:ready`".
- `RestartGuardians` / `RestartSandbox` user-triggered paths (still
  synchronous on user action; only boot moves to background).
- The MCP `mcp__<guardian>__<tool>` envelope and dispatch.

## 5. Rejected alternatives

### 5.1. Just move `WindowSetSize` to the top of `startup()`

Restores the window earlier but still inside `OnStartup`, and does not
address the session-restore race or the long input-unready period (the
agent is still built synchronously). Part A (size at creation) is
strictly better and removes the resize jump entirely.

### 5.2. Allow a "degraded" SEND during init (no composer gate)

Let SEND fire at `app:ready`; the first turn simply lacks MCP/sandbox
tools if they are not yet registered. Safe on the locks (§3.2), but it
means the model's tool set silently varies for the first turn — a
"sometimes the tool is there, sometimes not" behaviour that is hard to
explain and hard to test. Rejected in favour of the explicit gate (Part
D): a brief, visible "initializing…" is easier to reason about than a
probabilistically-degraded first turn.

### 5.3. Two-stage UI (SEND enabled at `app:ready`, a "tools loading" pill)

Same exposure as 5.2 to the degraded-first-turn case, just with a pill.
Rejected for the same reason; the gate is simpler and removes the class
of concern outright.

### 5.4. Lazy-init guardians/sandbox on first use

Defer until the first tool call that needs them. Rejected: MCP tool
*definitions* must be known before the first LLM turn so the model can
call them, and Settings → Tools expects them listed shortly after
launch. A boot-time background goroutine gives "available within a
second" without a first-call latency spike.

### 5.5. Skip the `a.sandbox` lock and hope the goroutine finishes first

A data race by definition; `go test -race` would flag it and it would
manifest as intermittent corruption. Rejected outright.

### 5.6. Wire `LastSession` to restore the exact last-active session

Persist `LastSession` on each switch, read on launch. Rejected as
unnecessary: `s[0]` already restores the last-active session in normal
use, and the field has no supporting machinery. Building it would be
effort for no behavioural gain. We delete the field instead (Part E).

## 6. Implications and risks

- **Composer gate latency.** SEND is disabled until `tools:ready`:
  sub-second normally, ≤5s in the sandbox-enabled-but-engine-down edge
  case (bounded by Part C). The rest of the UI stays responsive
  throughout, so this is a far better experience than today's unsized
  window + no session.
- **Tool-availability visibility.** Settings → Tools should refresh on
  `tools:ready` (or an existing `agent:*` signal) so a user watching that
  pane at launch sees MCP/sandbox tools appear rather than reopening it.
- **`go test -race`** must pass with the new `sandboxMu`; add a test
  exercising a concurrent `a.sandbox` read during a simulated
  `StartBackground`.
- **Lost-event robustness.** `app:ready` / `tools:ready` are paired with
  `Ready()` / `ToolsReady()` bindings so a listener that attaches after
  the emit still resolves. Without this the gate could stick.
- **No data migration**, no session-format change. The only config-schema
  change is the *removal* of an unused field (Part E).

## 7. Implementation plan

Single PR; parts are interdependent (Part B is unsafe without §3.1; Part
D depends on the events added in Part B).

1. **§3.1 first — add `sandboxMu sync.RWMutex`** and route every
   `a.sandbox` read/write (`agent.go:1449`, `2448`, `sandbox_tools.go`,
   `maybeStartSandbox`, `RestartSandbox`) through it, plus the boot-init
   guard flag for §3.2. Everything is still synchronous here;
   `go test -race ./internal/... -tags no_duckdb_arrow` stays green.
2. **Part C** — timeouts on the two sandbox probes.
3. **Part B** — split the two blocking calls into
   `(*Agent).StartBackground(ctx)`; launch from `bindings.startup` after
   `b.agent` is assigned; emit `app:ready` then `tools:ready`. Add
   `Ready()` / `ToolsReady()` bindings.
4. **Part A** — load window geometry in `main()`, pass to `wails.Run`,
   delete the post-startup `WindowSetSize`/`WindowSetPosition`.
5. **Part D** — frontend: session-init keyed on `app:ready` / `Ready()`
   with one bounded `LoadSession` retry; composer disabled until
   `tools:ready` / `ToolsReady()` with an "initializing…" affordance.
6. **Part E** — delete the `LastSession` field.
7. **Tests** — `-race` concurrency test for `a.sandbox`; a unit test
   asserting `agent.New` no longer performs the external probes (fake
   that records calls); keep the existing suite green.
8. **Docs / CHANGELOG** — README/README.ja need nothing beyond "faster
   startup"; CHANGELOG framed as `fix: non-blocking startup — restore
   window + last session without waiting on MCP/sandbox init`.

`go test ./internal/... -tags no_duckdb_arrow` (and a `-race` pass) must
be green on each commit.

## 8. Out of scope

- Parallelizing multiple MCP guardian spawns against each other.
- A general async-init framework for the agent; this ADR moves exactly
  the two known-blocking boot calls.
- Reworking the Idle/Busy state machine (ADR-0021 territory).
- Auto-starting a stopped sandbox engine on the user's behalf — we only
  stop *probing* it from blocking the boot.

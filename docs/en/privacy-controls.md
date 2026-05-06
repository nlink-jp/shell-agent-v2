# Privacy Controls — Design Note

**Status:** Design confirmed (open questions resolved 2026-05-06); ready to implement.
**Targets:** v0.3.0 (post-v0.2.5)

This document specifies two related privacy features:

1. **Private sessions** — opt-in per-session mode that suppresses
   cross-session memory promotion (Global Memory) while keeping
   the in-session experience normal.
2. **Log privacy controls** — tighten the default verbosity of
   `app.log` so user prompts, LLM responses, and tool arguments
   stop leaking to disk by default; provide an opt-in DEBUG mode
   for diagnosis.

Both share a common thread: **prevent persistent off-the-fly
storage of conversation content the user did not explicitly
intend to retain.**

---

## 1. Threat model

shell-agent-v2 is a single-user local-first app. Network exposure
is minimal. The realistic exposure paths for sensitive
conversation content are:

- **`global_memory.json`** — a long-lived JSON file; once a fact
  lands here it re-injects into every future session and is
  visible in any memory inspector / sidebar screenshot.
- **`app.log`** — file-based log written for the lifetime of the
  install; persists across restarts. By default contains user
  message snippets, LLM response heads, tool arguments, and
  extraction LLM replies (which embed the verbatim "memorable
  fact" text being considered).
- **Session JSON** (`chat.json`, `session_memory.json`,
  `findings.json`) — per-session, deletable as a unit via the UI.
  Acceptable for in-session content.

In-scope: reduce the first two surfaces.
Out-of-scope: device-level filesystem encryption, cloud sync,
external LLM provider data retention (Vertex AI logs requests
on the GCP side independently of this app).

---

## 2. Private sessions

### 2.1 User-visible model

Two ways to start a chat:

- **+ New Chat** (existing) — normal session, auto-extraction
  routes preference / decision into Global Memory as today.
- **+ New Private Session** (new) — same as above, except the
  session is marked private and the Global-Memory route is
  suppressed. Session Memory and Findings still work normally
  (they're per-session and deleted with the session).

The UI surfaces the private state in **two places**:
- Sidebar session list — **🔒 lock indicator** next to the title.
- Chat pane — **🔒 indicator** near the top of the active
  conversation, so the user sees it without leaving focus.

The double-indication is intentional: privacy is a
trust-critical state and one indicator can be missed
(collapsed sidebar, scroll, etc.).

A session's privacy is fixed at creation. There is **no toggle
mid-session** — the boundary needs to be unambiguous so the user
can rely on "I started this private, nothing leaked".

### 2.2 Data model

```go
// memory/Record.go
type Session struct {
    ID      string
    Title   string
    Private bool          // NEW; defaults false; persisted in chat.json
    Records []Record
}
```

`Private` is persisted on `chat.json` so a restored session
retains its privacy setting.

### 2.3 Routing change

`agent.extractMemories` already routes by category:

- `preference` / `decision` → GlobalMemoryStore
- `fact` / `context` → SessionMemoryStore

Privacy gate:

```go
isGlobal := category == "preference" || category == "decision"
if isGlobal && a.session.Private {
    logger.Debug("extractMemories: dropping global-route fact in private session: %q", fact)
    continue
}
```

Findings:

- **Auto-promote** from `analyze-data` → goes to per-session
  findings. Unaffected (already per-session; deleted with the
  session).
- **Pin to Global Memory** (UI action) on a Finding or Session
  Memory entry → user-initiated promotion. Blocked in private
  sessions: the UI hides the ★ Pin button, and the binding
  also rejects the call server-side so a stale UI / direct
  binding call can't bypass it. Without this the "private"
  label would be misleading.

### 2.4 Bindings + frontend

- `Bindings.NewSession() string` (existing) creates non-private.
- `Bindings.NewPrivateSession() string` (new) creates with
  `Private: true`.
- `SessionInfo` exposed to the frontend gains `Private bool` so
  the sidebar can render the lock icon.
- Sidebar bottom-nav: existing **+ New Chat** + new **+ New
  Private Session** (lock-icon button).

### 2.5 Edge cases

- **Session switch during Busy**: existing Idle/Busy gate
  applies. No change.
- **Promote on a stale ID**: Pin handlers must check
  `a.session.Private` server-side too — UI hiding alone isn't
  enough (binding can still be called).
- **Legacy sessions** written before this release have no
  `Private` field; JSON unmarshaling defaults to `false` (non-
  private). No migration needed.

### 2.6 Audit log

INFO-level log entries are emitted on creation and load of a
private session so the user can verify in `app.log`:

```
[INFO] session created: id=<id> private=true
[INFO] session loaded:  id=<id> private=true
```

Non-private session creation is also logged at INFO with
`private=false` for symmetry. The lifecycle event itself does
not leak conversation content; the trade-off (an attacker
reading the log can infer "this session was private") is
acceptable because (a) this is a single-user local app, (b) the
log is on the same disk as the chat data anyway, and (c)
verifiability outweighs the speculative leak.

---

## 3. Log privacy controls

### 3.1 Current leak surface

Audit of `app.log` outputs that include user-visible content:

| Site | Current level | Content |
|------|--------------|---------|
| `extractMemories: LLM reply (N chars): "..."` | INFO | Verbatim extraction LLM reply, embeds memorable fact text |
| `agentLoop: response content=...` (truncate 200) | DEBUG | Assistant reply head |
| `agentLoop: message=...` (truncate 90) | DEBUG | User message head |
| `agentLoop: tool_call args=...` (truncate 200) | DEBUG | Tool arguments (may include paths, queries, prompts) |
| `agentLoop: tool_result name=... result=...` (truncate 200) | DEBUG | Tool result head |
| `vertex parseResponse part[0]: ... textHead=...` | DEBUG | Vertex response head |

DEBUG output is currently always written to disk — there is no
level filter. INFO leaks via `extractMemories: LLM reply`.

### 3.2 Design

**(a) Add a level filter to `internal/logger`.** Logger gains a
configurable threshold (Debug / Info / Warn / Error). Output
below the threshold is dropped before writing.

**(b) Default level = Info.** DEBUG output (which contains the
bulk of the conversation content) is suppressed unless the user
opts in.

**(c) Demote leaky INFO calls to Debug.** Specifically:

- `extractMemories: LLM reply` → Debug

The lifecycle / event INFO calls (`bg-task ...: start/done`,
`agentLoop: tool_call name=... args_len=...` (length only),
`Bindings.Abort: invoked`, `Agent.Abort: ...`) stay at INFO —
they're useful for "what happened?" without leaking content.

**(d) Configuration surface.**

- `cfg.Logger.Level: "debug" | "info" | "warn" | "error"`
  (default `"info"`).
- Settings UI: under General, a select dropdown labelled
  "Log verbosity" with a help text "Default `info` keeps user
  messages, LLM responses, and tool arguments out of `app.log`.
  Switch to `debug` only when reproducing an issue."
- A startup INFO log line announces the active level so the
  user can confirm.

**(e) Out of scope (future work).**

- Log file size cap / rotation. Currently `app.log` grows
  forever. This is real but tangential — even a 0-byte log
  with leaks is bad. Address in a separate change.
- Redaction of file paths, IPs, etc. inside the lifecycle INFO
  calls. The existing INFO surface is already minimal; revisit
  if a specific leak is found.

### 3.3 Compatibility

- Existing logger `Init(dir string) error` API unchanged; level
  defaults to Info.
- New `SetLevel(Level)` exported so `main.go` can apply config
  after `Init`.
- All existing `logger.Info` / `logger.Debug` / `logger.Error`
  call sites work as-is; they just respect the threshold.

---

## 4. Implementation phases

Two independent slices, can land in either order.

### Phase A — Log privacy

1. `internal/logger`: add `Level`, `SetLevel`, threshold check
   in `Info` / `Debug`.
2. `internal/config`: add `LoggerConfig{ Level string }` (default
   `"info"`).
3. `main.go`: parse + apply `cfg.Logger.Level` after `Init`.
4. Audit existing INFO calls; demote `extractMemories: LLM reply`
   to Debug. Verify tool-call args are length-only at INFO.
5. Settings UI: add "Log verbosity" select.
6. Test: unit test on logger threshold; manual smoke on Settings
   change.

### Phase B — Private sessions

1. `memory.Session`: add `Private bool`; persist in chat.json.
2. `Bindings.NewPrivateSession() string`; expose `Private` on
   `SessionInfo` returned by `ListSessions`.
3. `agent.extractMemories`: add the privacy gate.
4. `Bindings.PinSessionMemory` / `PinFinding`: reject when active
   session is private (return error so the UI can surface it).
5. Audit log: emit INFO on session create / load with the
   `private=` flag (§2.6).
6. Frontend `Sidebar.tsx`: add **+ New Private Session** button
   in bottom-nav (next to **+ New Chat**, lock icon).
7. Frontend session list: show 🔒 indicator on private rows.
8. Frontend chat pane: 🔒 indicator near the top of the
   conversation (active-session header area).
9. Frontend Memory tab + Findings panel: hide ★ Pin buttons on
   private-session rows (Session Memory section + FindingsPanel).
10. Test: extraction routing under private flag + Pin handler
    rejection (unit), UI smoke (manual).

---

## 5. Resolved questions

(Originally "open questions" — resolved 2026-05-06.)

1. **Pin button on private sessions** — Hide it AND reject in
   the binding (server-side check). §2.3 / §2.4 updated.
2. **Visual identification** — Both sidebar 🔒 indicator AND a
   chat-pane 🔒 indicator near the top of the active
   conversation. §2.1 updated.
3. **Default log level** — `info`. Simple, conservative, keeps
   conversation content out of `app.log` by default. §3.2 (b)
   stays as-is.
4. **Settings UI for log level** — Yes, add a "Log verbosity"
   select to the General tab. §3.2 (d) stays as-is.
5. **Audit log for private sessions** — Yes, log session
   creation and load at INFO (`private=true|false`). §2.6
   added.

---

## 6. Non-goals

- Encrypting `chat.json` / `session_memory.json` / `findings.json`
  on disk. The OS-level data dir permissions (`0700`) plus
  delete-on-session-removal are the storage protections. Adding
  encryption is a separate, larger discussion (key management,
  recovery, performance).
- Per-message privacy markers. The session-level boundary is
  simpler to reason about and matches user expectation
  ("this whole chat is private").
- Removing existing `global_memory.json` content on enabling
  privacy mode. Privacy mode controls future behaviour only;
  retroactive purging is a separate user action (sidebar Global
  Memory bulk delete already exists).

---

## 7. Resolution plan

After review on §5 open questions, write up unit tests
(extraction routing, Pin handler rejection, logger threshold)
and mark this doc `Status: 実装中` then `Status: 実装完了 v0.3.0`
when shipped. History entry stays in `docs/en/history/` only
when superseded; for now it lives at `docs/en/privacy-controls.md`
as current-state documentation.

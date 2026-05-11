# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.4.5] - 2026-05-11

Session-rename persistence fix — addresses a user report that
renaming a session (active or freshly-created) appeared to
work in the UI but reverted to the original title on the next
launch.

### Fixed

- **Renaming the active session now persists.** Pre-fix,
  `bindings.RenameSession` called `memory.RenameSession`
  directly, which read `chat.json` from disk, mutated the
  `Title` field, and wrote it back. The agent's in-memory
  `a.session.Title` was never updated. Any subsequent
  `a.session.Save()` (after a Send at `agent.go:1367`,
  inside the agent loop at `:1470`, after a tool at `:1538`,
  or from `generateTitleIfNeeded` at `:2065`) silently
  overwrote the disk copy with the stale in-memory title,
  and on next launch the user saw the original name.
  (**Mode A**)
- **Renaming a fresh "New Session" before the first message
  no longer gets clobbered by auto-title generation.**
  `generateTitleIfNeeded`'s `if a.session.Title != "New
  Session"` guard reads the in-memory title, so a fresh
  session that the user renamed before sending a message
  still passed the guard with the stale `"New Session"`
  value. The LLM-generated auto-title then overwrote the
  user's choice. The fix updates `a.session.Title` in-memory
  before the disk save, so the guard observes the new value
  and skips. (**Mode B**)

### Added

- **`agent.RenameSession(sessionID, title) error`** — agent-
  level wrapper that updates `a.session.Title` in-memory under
  `a.mu` when `sessionID` names the active session, then calls
  `a.session.Save()`. For non-active sessions (no in-memory
  copy held) it delegates to `memory.RenameSession` as before.
  No Busy gate — rename should work even during a long
  `analyze-data` run, and `a.mu`'s brief hold is enough
  because the agent loop never holds it across LLM calls.
- **`bindings.RenameSession`** is now a thin pass-through to
  `agent.RenameSession`, mirroring the v0.4.0+ pattern where
  any operation that touches per-session state routes through
  the agent layer (parallels `Export` / `Import` / `Delete`).
- **Tests:**
  - `TestRenameActiveSession_SurvivesSubsequentSave` —
    Mode A regression: rename, append a record, call
    `a.session.Save()`, reload from disk, assert renamed.
  - `TestRenameActiveSession_GuardsAutoTitleGen` — Mode B
    regression: rename a "New Session", call
    `generateTitleIfNeeded`, assert it returns nil
    (guard fires) and the on-disk title is the user's choice.
  - `TestRenameNonActiveSession_StillWorks` — no-regression:
    rename a session the agent has not loaded, assert disk
    holds the new title and the active session's in-memory
    title is undisturbed.

### Compatibility

- Public API: purely additive (one new method). No removals
  or signature changes. The frontend binding signature is
  unchanged.
- On-disk format: unchanged.
- Concurrency: rename now holds `a.mu` briefly; this matches
  the existing discipline in `agent.DeleteSession` (v0.4.2)
  and `agent.ExportSession` (v0.4.0). Long-running tools
  like `analyze-data` are not affected because the agent
  loop releases `a.mu` for every LLM call.

## [0.4.4] - 2026-05-11

`analyze-data` row-cap fix — addresses a user report that
`analyze-data` failed up front with `query result exceeds
10000 rows` on a 27,000-row table, which made the
sliding-window summarizer unreachable in exactly the regime
where the feature is interesting.

### Fixed

- **`analyze-data` no longer trips the interactive 10k row
  cap.** `Engine.QuerySQL` is hard-capped at `MaxQueryRows =
  10000` to protect the chat from unbounded `SELECT` output —
  correct for the three callers whose results land in the
  LLM tool result (`query-sql`, `query-preview`,
  `quick-summary`). Pre-fix, `analyze-data` shared that path
  and inherited the cap, even though its rows never enter the
  chat (they are chunked into per-window LLM calls). The fix
  adds a dedicated `Engine.QuerySQLForAnalyze` method backed
  by a separate `MaxAnalyzeRows = 1_000_000` constant and
  switches the single `toolAnalyzeData` call site to it. The
  three interactive callers are unchanged.

### Added

- **`MaxAnalyzeRows` constant** (default 1,000,000) — a pure
  memory-safety backstop, not a query-shape suggestion.
  Hitting it returns an explicit error suggesting
  pre-aggregation via `query-sql` (NOT `LIMIT` — adding
  `LIMIT` to `analyze-data` would defeat the sliding window's
  whole purpose by silently truncating the analysis to the
  first N rows).
- **`Engine.QuerySQLForAnalyze(query) ([]map[string]any,
  error)`** — public read-only-enforced helper, parallel to
  `QuerySQL`. Internally both share a `querySQLBounded`
  helper so the read-only check, statement preparation,
  scanning loop, and value coercion stay in one place.
- **Test seam `setMaxAnalyzeRowsForTesting(t, n)`** — lets
  the cap-overflow test verify backstop behaviour without
  materialising a million rows in CI.
- **Engine tests:**
  - `TestQuerySQLForAnalyze_AllowsBeyond10k` — pins the
    regression: 12k-row table now returns 12,000 rows.
  - `TestQuerySQL_StillCapsAt10k` — symmetric guard that the
    interactive cap is unchanged and still suggests `LIMIT`.
  - `TestQuerySQLForAnalyze_RespectsMaxAnalyzeRows` — uses
    the test seam to verify the analyze cap fires with the
    correct error wording (suggests pre-aggregation, **does
    not** suggest `LIMIT`).
  - `TestQuerySQLForAnalyze_RejectsWrite` — read-only gate
    still applies on the new path.

### Documentation

- New design note: [docs/en/analyze-data-row-cap.md](docs/en/analyze-data-row-cap.md)
  / [docs/ja/analyze-data-row-cap.ja.md](docs/ja/analyze-data-row-cap.ja.md)
  with symptom, root-cause analysis, the per-row memory math
  table that justifies the 1 M ceiling, the LLM-call latency
  table that explains why the practical ceiling is much lower
  anyway, the explicit "why a dedicated method, not a
  parameter" reasoning, and the explicit out-of-scope list
  (config knob, streaming, auto-sampling). README + README.ja
  and AGENTS.md "Recent design notes" sections updated.

### Compatibility

- **Public API:** purely additive — adds one new method
  (`QuerySQLForAnalyze`) and one new exported constant
  (`MaxAnalyzeRows`). Nothing removed or changed in
  signature; existing callers compile unchanged.
- **On-disk format:** unchanged.
- **Settings UI:** no new knobs. If field reports prove the
  fixed `MaxAnalyzeRows = 1_000_000` value insufficient, a
  `Settings → Tools → analyze-data max rows` knob is the
  obvious follow-up; the Engine method's signature is
  already shaped to accept a per-call max if we later want
  to plumb config through.

## [0.4.3] - 2026-05-11

Sandbox UID-mapping fix — addresses a user report that
`podman` container start fails on corporate-managed macOS
accounts whose UID is mapped from Active Directory / LDAP
to a value outside the rootless subuid range
(e.g., `crun: setresuid to 202594884: Invalid argument`).
Image build was already succeeding; only the container
`run` step was affected.

### Fixed

- **`podman` engine path now uses keep-id userns remap.**
  `internal/sandbox/cli.go::buildRunArgs` previously emitted
  `--user $(id -u)` regardless of host UID magnitude. Large
  host UIDs from LDAP/AD-mapped corporate macOS accounts fall
  outside the rootless subuid range and `crun` fails its
  `setresuid()` syscall with `EINVAL`. The fix splits the
  flag emission per engine via a new `usePodmanUserns(binary)`
  helper (parallel to the existing `useSELinuxRelabel`):
  podman gets `--userns=keep-id:uid=1000,gid=1000` plus
  `--user 1000:1000` (small in-namespace UID, host UID still
  mapped through `keep-id` so `/work` file ownership is
  preserved both directions); docker keeps the historical
  `--user $(id -u)` behaviour. The defence-in-depth posture
  from the v0.2.0 sandbox design (non-root container) is
  preserved — the container process still runs as a non-root
  user, just at UID 1000 inside its namespace instead of the
  host UID directly.

### Added

- **Test coverage for both engine paths.** New unit tests in
  `internal/sandbox/cli_test.go`:
  - `TestBuildRunArgs_PodmanRemapsHostUID` asserts the
    podman path emits both userns + user-1000 flags **and
    never** passes the host UID through. Guards against a
    silent regression to the old form.
  - `TestBuildRunArgs_NonPodmanPassesHostUID` asserts the
    docker path keeps the historical `--user $(id -u)`
    behaviour and never emits `--userns`.
  - `TestUsePodmanUserns` — basename-match table for the
    engine detection helper (case-insensitive, full path
    tolerant).

### Documentation

- New design note: [docs/en/sandbox-uid-mapping.md](docs/en/sandbox-uid-mapping.md)
  / [docs/ja/sandbox-uid-mapping.ja.md](docs/ja/sandbox-uid-mapping.ja.md)
  documenting the symptom, root cause, fix, podman version
  requirement (4.3+, Nov 2022), and what is explicitly out of
  scope. README + README.ja and AGENTS.md "Recent design notes"
  sections updated.

### Compatibility

- **Podman ≥ 4.3** (Nov 2022) is now required for the
  sandbox feature. Older Podman releases reject
  `--userns=keep-id:uid=N,gid=N` and the agent surfaces the
  podman error verbatim through the existing `sandbox: start
  container:` wrap.
- File ownership in `/work` is observably unchanged thanks
  to `keep-id`'s symmetric mapping.
- Docker users see no behaviour change at all.

## [0.4.2] - 2026-05-07

Session deletion UX & safety release ([#6](https://github.com/nlink-jp/shell-agent-v2/issues/6)),
plus a cross-document audit pass that brings README,
architecture, memory-model, data-analysis, and AGENTS docs
back in sync with everything that shipped in v0.3.0–v0.4.1.

### Added

- **Session delete confirmation** — the row's ✕ button now
  arms a "Confirm" state (red-emphasis text matching the
  existing `BulkActions` confirm pattern from Findings /
  Global Memory / Session Memory bulk-delete) before the
  destructive call fires. 6-second auto-cancel; clicking
  outside the row also cancels. Tooltip while armed:
  `Click again to delete "<title>"`.
- **In-flight deleting feedback** — while the binding
  promise is in flight, the row greys with a `↻ Deleting…`
  spinner and all action buttons disable. The user sees
  that something is happening rather than wondering whether
  the click was lost.

### Fixed

- **Session delete is now state-machine-gated** — the
  pre-v0.4.2 `bindings.DeleteSession` only checked
  `IsBusy()` at entry; concurrent `Send` / `LoadSession` /
  `Export` / `Import` could race a half-deleted session
  directory. The most striking failure: deleting the active
  session while the user typed a new message would let the
  trailing `a.session.Save()` recreate the directory as a
  partial file. The fix moves orchestration into a new
  `agent.DeleteSession` method that holds Busy for the
  operation's duration (mirroring `ExportSession` /
  `ImportSession`), nil-clears `a.session` /
  `a.sessionMemory` / `a.findings` and `Close()`s the
  analysis Engine before `RemoveAll` runs so a stray
  Save / Engine call cannot resurrect the directory.

### Documentation

- New design note: [docs/en/session-delete-ux.md](docs/en/session-delete-ux.md)
  / [docs/ja/session-delete-ux.ja.md](docs/ja/session-delete-ux.ja.md)
  (full parity, ~280 lines each). Covers the four real
  failure modes the looser pre-v0.4.2 path allowed (F1–F4),
  the 2-click confirm + Deleting state visual treatment,
  edge cases, and four rejected alternatives.
- **Cross-document audit**: README + architecture + memory-
  model + data-analysis + AGENTS.md were still framed
  around v0.2.0 and silent on the cross-cutting features
  shipped in v0.3.0–v0.4.1. They now cover Session.Private,
  the expanded Busy-gate operation set, the
  `internal/sessionio` package, the `tool_progress` event,
  and cross-link to the per-feature design notes. EN/JA
  brought to full parity throughout.

## [0.4.1] - 2026-05-07

Bug-fix release for [#5](https://github.com/nlink-jp/shell-agent-v2/issues/5):
`analyze-data`'s sliding-window progress bubbles stayed stuck
in "running" state in the chat pane.

### Fixed

- **`analyze-data` progress bubbles** now update one bubble in
  place ("analyze-data — window N/M") rather than spawning a
  fresh "running" pill per window that never transitioned to
  success. The completed bubble reverts to plain "analyze-data"
  to match the visual convention of every other tool.
- **`tool_end` matcher in the chat pane** switched to
  tool_call_id-primary (with content-equality fallback for
  genuinely legacy events). The old content-equality matcher
  would silently miss any bubble whose text had been mutated
  by a progress update.

### Added

- **`ActivityEvent.Type == "tool_progress"`** — new event type
  that lets long-running tools update a single chat bubble in
  place. The frontend matches by `tool_call_id` (carried on
  the event) and overwrites the bubble's displayed text.
  Backwards compatible at the wire level: missing
  `tool_call_id` or no matching running bubble is a no-op.
- **`Agent.activeToolCallID`** — internal field set by
  `agentLoop` before each `executeTool` call so progress-
  emitting tools can target the matching UI bubble without
  threading a new arg through ~30 tool functions. The
  Idle/Busy state machine guarantees only one tool runs at a
  time per agent, so a scalar field is sufficient.

### Documentation

- New design note: [docs/en/tool-progress-events.md](docs/en/tool-progress-events.md)
  / [docs/ja/tool-progress-events.ja.md](docs/ja/tool-progress-events.ja.md)
  (full parity, ~280 lines each). Covers wire format, the
  active-tool-call-ID propagation choice (struct field vs
  threaded arg), the matcher change, and three rejected
  alternatives.

## [0.4.0] - 2026-05-07

Session import / export release. A whole session — chat,
session memory, findings, summaries, sandbox `work/`, the
session-scoped DuckDB, and every objstore object the session
owns — can now be packaged into a single `.shellagent` bundle
and re-imported on the same or a different machine. The
privacy flag is preserved so a private session that travels in
a bundle stays private after import.

### Added

- **`.shellagent` bundle format** — internally a ZIP with a
  manifest at the root + every per-session artifact + an
  `objects/` subtree (index + raw blobs). Schema version 1
  is gated strictly: any other version is rejected. See
  [docs/en/session-import-export.md §3](docs/en/session-import-export.md).
- **Sidebar `Export` icon** (⬇) on each session row's hover
  actions, alongside Rename / Delete. Disabled while the agent
  is busy with explanatory tooltip.
- **Sidebar `Import Chat` button** (⬆) below `+ New Private
  Chat` in the bottom-nav. Opens a native open dialog filtered
  to `*.shellagent` and auto-switches to the imported session
  on success.
- **`/export` and `/import` slash commands** for keyboard-first
  use. `/export` exports the current session; `/import` opens
  the open dialog. `/help` lists both.
- **Audit log entries** for export and import:
  `session exported: id=... private=... bytes=N objects=K dest=...`
  and `session imported: original_id=... new_id=... private=... bytes=N objects=K`.
  Neither entry contains chat content or fact text. Both obey
  the v0.3.0 log-level filter.

### Behaviour notes

- **Object IDs are always regenerated on import.** Bundled
  objects are re-stored under fresh IDs in the local objstore,
  and every reference — `Record.ObjectIDs[]`, markdown
  `![alt](object:ID)` in `Record.Content`, and any
  `SummaryEntry.Summary` text — is rewritten to point at the
  new IDs. This makes re-importing the same bundle on the
  machine that produced it (e.g. as a backup-restore drill)
  trivially safe instead of a deterministic ID collision.
  `session_memory.json` and `findings.json` are intentionally
  not swept; the audit in
  [§5.3](docs/en/session-import-export.md#53-object-id-strategy)
  explains why their write paths cannot embed object refs.
- **Active-session export** drains the post-task work group,
  flushes per-session stores, and closes the analysis Engine
  before the bundle copy so the on-disk DuckDB file is
  consistent. The Engine is re-created via `switchAnalysis`
  after the export returns; subsequent analysis tool calls
  open it lazily.
- **Title-collision suffixing** — if the imported session's
  title matches an existing one, ` (imported)` is appended,
  then ` (imported 2)`, `3`, …
- **Default export filename** — `<safe-title>-<YYYYMMDD-HHMMSS>.shellagent`,
  with FS-disallowed characters replaced by `_` and the title
  truncated to 64 characters; user can override in the save
  dialog.

### Documentation

- New design note: [docs/en/session-import-export.md](docs/en/session-import-export.md)
  / [docs/ja/session-import-export.ja.md](docs/ja/session-import-export.ja.md)
  (full parity, ~550 lines each). Covers bundle format, race
  conditions catalogued by the Idle/Busy state machine, ID
  regeneration rationale, edge cases, and the manual smoke
  checklist that gated this release.

## [0.3.0] - 2026-05-06

Privacy controls release. Two related features tighten what
gets persisted on disk without an explicit user action.

### Added

- **Private sessions** — new `+ New Private Chat` entry point
  in the sidebar creates a session marked `Private`. While
  active:
  - `extractMemories` drops `preference` / `decision` facts
    instead of routing them to Global Memory; `fact` /
    `context` still populate per-session Session Memory and
    are deleted with the session.
  - `Pin to Global Memory` is hidden in the UI (Sidebar Session
    Memory section + chat-pane Findings panel) AND rejected
    server-side — the binding is the source of truth.
  - Sidebar session list shows a 🔒 indicator on private rows.
  - Chat pane shows a 🔒 banner above DataDisclosure as a
    persistent privacy reminder.
  Privacy is fixed at session creation (no mid-session toggle)
  so the boundary stays unambiguous. `Session.Private` is
  persisted in `chat.json` with `omitempty`, so legacy sessions
  load as non-private without migration. See
  [docs/en/privacy-controls.md §2](docs/en/privacy-controls.md).
- **Audit log entries** for session creation and load
  (`session created: id=... private=true|false` and
  `session loaded: ...`) so the user can verify privacy state
  in `app.log`.

### Changed

- **`app.log` defaults to `info` level** — DEBUG output (which
  contained user message snippets, LLM response heads, tool
  arguments, vertex response heads) is now suppressed unless
  the operator opts in. See
  [docs/en/privacy-controls.md §3](docs/en/privacy-controls.md).
- **Settings → Privacy → Log verbosity** select added (debug
  / info / warn / error). Changes apply live without an app
  restart.
- **`extractMemories` content-leaking INFO calls demoted to
  Debug** — `LLM reply (...)`, `dropped unparseable line`,
  `dropped fact with invalid category`, `dropped self-
  referential fact`, `globalMemory.Add returned false (dedup)`,
  `sessionMemory.Add returned false (dedup)`. The aggregate
  `added N facts to ...` lines stay at INFO (no content).

### Documentation

- **`docs/en/privacy-controls.md`** + Japanese mirror — full
  design note covering the threat model, both features, the
  resolved open questions, implementation phases, and
  non-goals. The §1 threat model + §3.1 leak-source audit
  table are useful even outside of this release.

## [0.2.5] - 2026-05-05

### Fixed

- **LLM settings changes (model name, endpoint, retry, context
  budget) didn't take effect until the next app restart.**
  `SaveSettings` persisted the updated config to disk but never
  called `RestartLLMBackend`, so the running agent kept using
  the previous Local / Vertex client. The Settings dialog now
  detects any change in `LLMConfig` (mirrors the existing
  `prevSandbox` pattern) and rebuilds the backend live.

## [0.2.4] - 2026-05-04

### Fixed

- **`resolve-date` rejected expressions when the conversation
  was in Japanese.** The resolver does pure-English keyword
  matching, but the parameter description didn't say so. The
  LLM mirrored the conversation language into the tool
  argument (e.g. `"先週の木曜日"`), which the resolver
  immediately rejected with `"unrecognized expression"`. The
  description now explicitly says ENGLISH ONLY, lists the
  supported forms verbatim, and tells the LLM to translate the
  date concept to English before calling the tool.

## [0.2.3] - 2026-05-04

### Fixed

- **`create-report` bubble appeared after the report bubble on
  session restore.** v0.2.2 only fixed the live-event ordering;
  records on disk were still written in the wrong order
  (`AddReportMessage` ran before `AddToolResult`), so cycling
  sessions reintroduced the swap. `toolCreateReport` now buffers
  into `Agent.pendingReport` and the agent loop's
  `flushPendingReport` appends the report record AFTER
  `AddToolResult`. Legacy sessions saved before this release
  are auto-corrected at `LoadSession` time by swapping any
  adjacent `(report, tool-event=create-report)` pair in the
  rendered view (the on-disk records aren't rewritten).
- **Local LLM emitted broken token output (e.g. `<|"|>`) after
  `create-report`.** The new tool→report record order put two
  assistant turns back-to-back across a tool boundary (`RoleReport`
  maps to `"assistant"` in LM Studio's OpenAI-compat layer),
  which trips some gemma-style chat templates' role-transition
  logic. Report records are now excluded from the LLM context
  the contextbuild assembles — the matching tool result already
  carries the "Report has been created and displayed" signal,
  and the full markdown was redundant kilobytes the model wrote
  itself moments ago. The chat-pane report bubble is unaffected.

### Refactored

- **`App.css` split into per-concern files under `frontend/src/styles/`.**
  The 3300-line monolithic stylesheet became 21 files (28-552
  lines each) plus a 47-line `App.css` import manifest. Vite
  inlines `@import` at build time so the bundled CSS is
  byte-identical to v0.2.2 — purely a maintenance reorganisation.

## [0.2.2] - 2026-05-04

### Added

- **Click-to-inspect tool-event bubbles.** Completed tool-event
  bubbles in the chat pane (✓ / ✕) are now clickable and open an
  overlay showing the tool's arguments (JSON pretty-printed when
  applicable), the result it returned, status, and call/return
  timestamps. Running bubbles (●) stay non-clickable since the
  result isn't recorded yet. Works for legacy sessions too —
  Vertex Gemini sessions written before this release stored
  empty `ToolCallID` (Gemini's FunctionCall has no first-class
  id), so `LoadSession` backfills them with `idx:N` synthetic
  ids and `GetToolCallDetails` resolves them via record-index
  lookup with run-order assistant pairing.
- **`vertex.go` synthesises FunctionCall IDs** as `vc-<hex>`
  when the API returns none, so all new Vertex sessions carry
  real ids end-to-end.

### Fixed

- **`create-report` bubble appeared after the report bubble.**
  The tool used to invoke `reportHandler` while still running,
  which raced the `tool_end` activity event in the Wails
  outbound queue and produced a rendering order of "report
  bubble → create-report tool-event bubble". Reports are now
  buffered in `Agent.pendingReport` and flushed by the agent
  loop after `tool_end` emission, so the chat pane sees them in
  source order ("create-report bubble → report bubble").

## [0.2.1] - 2026-05-04

### Fixed

- **Abort responsiveness during shell-tool execution.** Clicking
  Abort while a shell tool was in flight had no effect — the
  tool's child processes (curl, sleep, etc.) held the script's
  stdout/stderr pipes after `exec.CommandContext` SIGKILLed only
  the parent, and `CombinedOutput` blocked indefinitely waiting
  for the pipes to close. `toolcall.Execute` now puts each
  script in its own process group (`Setpgid`), overrides
  `Cmd.Cancel` to `kill(-pid, SIGKILL)` so the entire tree gets
  signalled at once, and sets `Cmd.WaitDelay = 2s` so a
  pipe-hoarding child can't keep `Wait` from returning past the
  cancel.
- **Guard envelope tags leaking into assistant responses.**
  Vertex Gemini, when quoting data from a wrapped user / tool
  record, sometimes reproduced the `<user_data_NONCE>...
  </user_data_NONCE>` envelope verbatim. The envelope is an
  internal prompt-injection defence marker, not user-visible
  content. Two-layer fix: the system prompt now explicitly tells
  the LLM never to reproduce the envelope, and the agent loop
  scrubs any leaked envelope tags using the *current turn's*
  guard nonce (so unrelated user prose mentioning a similar
  placeholder isn't mangled).

### Added

- Trace logging on the Abort path (`Bindings.Abort` /
  `Agent.Abort` / `toolcall.Execute` ctx-cancelled error) so
  future hangs are immediately diagnosable from `app.log`.

## [0.2.0] - 2026-05-03

### Breaking changes — memory architecture rewrite

The pinned + findings memory model from v0.1.x is replaced with a
4-facility design. **No data migration**: legacy `pinned.json` and
`findings.json` are ignored on first launch. See `docs/en/memory-model.md`.

The new facilities:

- **Records** — per-session conversation history (unchanged).
- **Session Memory** — *new*. Auto-extracted `fact` / `context`
  entries scoped to the current session. Lives at
  `sessions/<id>/session_memory.json`. Deleted with the session.
- **Findings** — narrowed to data-analysis discoveries. Now
  per-session at `sessions/<id>/findings.json` (was global).
  `/finding` and `/findings` slash commands are removed.
- **Global Memory** — renamed from Pinned. Holds `preference` /
  `decision` only. Cross-session at `<dataDir>/global_memory.json`.

Auto-extraction routes by category: `preference` / `decision` →
Global Memory, `fact` / `context` → Session Memory.
"Pin to Global Memory" is the explicit user action that promotes
a Session Memory entry or a Finding into the cross-session pool.

### Added

- **Pin to Global Memory dialog** — category picker (preference
  / decision) shown when promoting a Session Memory row from the
  sidebar or a Finding from the chat-pane panel.
- **FindingsDisclosure panel** in the chat pane — severity filter,
  free-text search, bulk delete, real-time refresh on
  `findings:updated`.
- **Sidebar Memory tab** restructured into Global Memory / Session
  Memory sections with independent bulk-select.
- **Findings dedup** — three layers (exact / normalised /
  word-set Jaccard ≥ 0.5) keep the same observation in slightly
  different wording from filling the store.
- **Auto-extraction window** walks back past tool records to keep
  the last 4 user / assistant turns in scope. Earlier the trailing
  4 records flat became all-tool when the assistant did 2-3 tool
  calls in a row, and extraction silently stopped landing facts.
- **Language-hint plumbing for analyze-data** — agent peeks at the
  user's recent turn (CJK ratio ≥30% → "Japanese") and forces the
  summarizer to keep finding descriptions in that language even
  when the assistant LLM translated the perspective string to
  English en route to the tool call.

### Removed

- **v1 destructive compaction**: `compaction.go`, `Tier` field on
  records, `Memory.UseV2` toggle, `HotTokenLimit`. Context-budget
  enforcement now lives entirely in the non-destructive
  `contextbuild` summary cache.
- **`/finding` and `/findings` slash commands**: replaced by the
  Findings panel and the `promote-finding` tool.
- **PinnedStore**: replaced by GlobalMemoryStore + SessionMemoryStore.

### Renamed

- Wails events: `pinned:updated` → `global_memory:updated` (plus
  new `session_memory:updated`).
- Bindings: `GetPinnedMemories` → `GetGlobalMemories`,
  `DeletePinnedMemory(ies)` → `DeleteGlobalMemory(ies)`,
  `UpdatePinnedMemory` → `UpdateGlobalMemory` (now takes
  `fact`, `native`, `category`).

### Fixed (sweep-up while testing v0.2.0)

- Pressing ArrowDown at end of input no longer wipes typed text.
  The history-down handler now only consumes the keypress while
  history navigation is active.
- `create-report` no longer emits the report's object ID into
  the LLM tool result. The LLM was using it to write a redundant
  `[link](object:ID)` into chat next to the rendered report.
- Report bubble in the chat pane is opaque again. WebView-level
  translucency turned off (`WebviewIsTransparent: false`) and the
  surface theme tokens (`--bg-primary`, `--bg-sidebar`,
  `--bg-input`) are now opaque rgb across all four themes.
- v0.2.0 styles ( `findings-disclosure`, `pin-dialog`, etc.)
  reference theme-defined tokens (`--bg-btn-primary`,
  `--border-accent`, `--text-accent`) instead of undefined
  `--accent-primary` / `--bg-secondary`. Light theme no longer
  loses selected filter buttons against the background.

## [0.1.28] - 2026-05-03

### Added

- **Pinned Memory rows show `learned YYYY-MM-DD`** in the
  sidebar — the date each fact was first pinned. The date was
  already embedded in the system-prompt injection (the LLM
  could see it via `(learned …)`), but the user couldn't see
  it in the UI. Helps audit how stale a pinned fact is when
  reviewing the list.
- **Findings sidebar refreshes in real time** after a finding
  is promoted via `promote-finding` (LLM tool call), the
  `/finding` slash command, or the `analyze-data` auto-promote
  loop. Previously findings only appeared after a session
  switch — Pinned Memory had this reactive update path
  (`pinned:updated` event) since v0.1.x but Findings was
  missing the symmetric `findings:updated` event.

### Notes

- No data migration. Legacy pinned entries with no `created_at`
  field continue to render without the date row.

## [0.1.27] - 2026-05-03

### Fixed

- Bump nlk to v0.5.2 to pick up the strip fix: think-tag handling
  no longer truncates LLM responses that explain the literal
  `<think>` tag inside a markdown inline-code span. The symptom
  surfaced during v0.1.26 verification when the user asked
  "THINK というタグについて教えて" — gemma's reply got truncated
  mid-explanation as soon as it tried to write `` `<think>` ``.

## [0.1.26] - 2026-05-03

### Added

- **Trust badge tooltips on memory entries.** Sidebar Pinned
  Memory and Findings rows now show a CSS pseudo-element
  tooltip on hover (replacing the unreliable native `title=`
  attribute that the Wails wkwebview embedding does not render
  consistently). The tooltip explains the v0.1.26 provenance
  contract in plain Japanese: "user-stated はユーザー発話
  または手動 pin、derived は LLM 経由の content（攻撃者影響下
  のバイトを含みうる）" — recovers the badge's meaning at
  glance.
- **Restore user-attached images on session reload.** When a
  past session is opened, user messages that originally
  included image attachments now show those images in their
  bubble. Previously the backend persisted the image
  `ObjectIDs` on the user record but never surfaced them in
  `LoadSession`, so restored conversations lost all visual
  context (the assistant's reply would reference "this image"
  with no referent). `MessageData` gained `ObjectIDs`,
  `LoadSession`'s user case populates it, and the frontend
  renders `object:<id>` URLs through the existing
  `ObjectImage` resolver.

### Fixed

- **Tool-call rounds now produce a chat bubble for explanation
  text.** When the LLM included a `"what I'm about to do and
  why"` preamble alongside a tool call (which the system prompt
  explicitly asks for), that text was being emitted only as a
  transient `Type: "thinking"` activity event — surfaced as a
  small status line, never as a chat message. The same content
  was already persisted in `session.Records` and reappeared on
  session reload, so the live UX was inconsistent with the
  restored UX. Now emitted as `Type: "assistant_text"` and
  appended to the chat as a proper assistant bubble.
- **Session-switch scroll position lands at the latest
  message.** Switching sessions used to leave the chat scrolled
  near the top of the restored history. Two issues compounded:
  `behavior: 'smooth'` scrolling was being interrupted by
  markdown / image layout settling; and `scrollIntoView` on a
  zero-height anchor below the last bubble stopped about one
  visual line short due to the `.messages` container's
  padding-bottom plus the last bubble's margin-bottom. Replaced
  with direct `scrollTop = scrollHeight` on the container
  element, with a delayed retry to catch late-rendering content.
- **Provenance source attribution survives the gemma "old
  3-part" extraction format.** When the local extraction LLM
  emitted the legacy `category|fact|native` shape instead of
  the v0.1.26 `category|turn-N|fact|native` shape, the parser
  put `parts[1]` (the english fact) into the turn-token slot
  and `parts[2]` (the native expression) into the fact slot,
  silently corrupting both content and source. New
  `parseExtractionLine` detects format by checking whether
  `parts[1]` looks like `turn-N` and falls back to 3-part
  parsing if not.
- **Source attribution for facts stated in Japanese.** Even
  when the LLM provided `turn-N` correctly, attribution was
  often `assistant_turn` (rendered `[derived]`) when the user
  had clearly stated the fact in Japanese — the extraction LLM
  picked the canonical English fact from the assistant's
  echoing turn rather than from the user's original Japanese.
  Added `extractCJKNgrams` (3-character overlapping windows
  over kanji/katakana runs) so the Japanese `native` field is
  cross-checked against the user's Japanese turn; a hit
  promotes attribution to `user_turn` and the badge correctly
  reads `[user-stated]`.
- **System-prompt clarification: never emit the input-anchor
  shape in output.** The LLM occasionally echoed the
  `Image (object ID: <hex>):` form (which is the shape the
  system prompt uses to anchor user-attached images in input
  context) when referencing a freshly produced image. The
  markdown renderer treats only `![alt](object:<hex>)` as an
  inline image, so the user saw the literal anchor as plain
  text and no image appeared. The system prompt now
  explicitly forbids the anchor shape in output and reminds
  the model to always use `![alt](object:<hex>)` for any
  image reference (user-attached, tool-produced, or
  retrieved). Defense by prompt only — no auto-rewrite, since
  the candidate regex would have mangled legitimate prose
  that talks ABOUT an ID (e.g. "Image (object ID: abc) is
  missing", documentation explaining the anchor format, code
  blocks).



### Security

- **Memory injection hardening (Security Round 3, 5 phases).**
  Triggered by a v0.1.25 regression where `THINK\n` started
  leaking into chat output across brand-new sessions before any
  user input. Root cause: two earlier auto-extracted pinned
  facts that *described* the THINK marker had been re-injected
  as authoritative system-prompt content into every subsequent
  session, paradoxically teaching the model to emit it. The
  same mechanism is a general indirect-prompt-injection
  vector — anything an assistant turn ever quotes (CSV cells,
  MCP responses, image OCR, web fetches) can be auto-extracted
  and pinned, then re-injected indefinitely. Design:
  [docs/en/memory-injection-hardening.md](docs/en/memory-injection-hardening.md).
  - **Phase A — provenance.** `PinnedFact` and `Finding` now
    carry source attribution (`user_turn` / `assistant_turn` /
    `manual` / `llm_promoted`), `SessionID`, and a
    `ToolOriginated` flag. `FormatForPrompt` for both stores
    prefixes each line with `[user-stated]` (high trust) or
    `[derived]` (lower trust — content traces through the LLM).
    Legacy entries with no source render as `[derived]` — the
    safer default.
  - **Phase B — pin-time defenses.** `extractPinnedMemories`
    now drops self-referential facts (anything mentioning the
    assistant, the model, system prompt, internal thought,
    THINK as a marker, tool call/output, …); enforces a
    category allowlist (`preference|decision|fact|context`);
    and wraps both the conversation tail and the existing-facts
    list with `nlk/guard` so the extraction LLM treats them as
    data, not instructions. `promote-finding` MITL default
    confirmed ON (already shipped as part of v0.1.20 H1+H2
    rework).
  - **Phase C — retention caps.** `MaxPinnedFacts` (default
    100) and `MaxFindings` (default 200) bound store growth via
    FIFO eviction, so a noisy or hostile session cannot inflate
    either store indefinitely. `FormatForPrompt` for both is
    bounded at 16 KiB total with newest-first inclusion and an
    elision marker.
  - **Phase D — UI source badges.** Sidebar Findings and Pinned
    Memory rows now show a trust badge (user-stated vs derived)
    next to each entry, with a tooltip explaining the
    provenance contract. Existing bulk-select + delete is the
    recovery path; no separate Settings tab needed.
  - **Phase E — docs.** Design document, README "Cross-session
    memory trust" subsection, and `memory-architecture-v2.md`
    threat-model section.
  - No data migration required. Existing `pinned.json` /
    `findings.json` files keep working; missing source fields
    default to the lower-trust label.

### Notes

- Auto-extraction itself remains on; only attempted defense is
  pin-time filters + provenance tagging. MITL on every
  extracted fact would destroy the chat UX (extraction runs
  most turns); the residual risk on auto-extraction is recovered
  via the audit + delete path in the sidebar.
- Self-referential filter is intentionally over-broad — false
  positives (a benign user fact about "the model T" never
  pinning) are cheaper than false negatives (a behaviour-
  overriding fact slipping through).

## [0.1.25] - 2026-05-03

### Added

- **Shell tool ↔ /work bridge.** Host-side shell tools now learn
  the per-session work directory via the
  `SHELL_AGENT_WORK_DIR` environment variable. The directory is
  the same physical path the sandbox bind-mounts at `/work`, so a
  file written by a shell tool on the host is immediately visible
  to sandbox tools (and vice versa) and shows up in the chat-pane
  Data → /work section. Existing tools that ignore the env var
  are unaffected.
- **`register-object` built-in tool.** No-sandbox equivalent of
  `sandbox-register-object`. Reads a file from the session work
  directory and registers it into objstore, returning an
  `object:<ID>` reference the chat can render. Use this to
  surface artefacts produced by shell tools (e.g. the rewritten
  `examples/generate-image.sh` writes to `$SHELL_AGENT_WORK_DIR`
  and the agent follows up with `register-object` to make the
  image appear inline). Path validation reuses the existing
  sandbox-side traversal / symlink rejection logic. MITL default:
  `false` (same trust level as a chat drag-and-drop). Design:
  [docs/en/work-dir-shell-bridge.md](docs/en/work-dir-shell-bridge.md).

### Fixed

- `examples/generate-image.sh` produced an image but it never
  reached objstore — the file was written under `/tmp/...` and
  the only thing the LLM saw back was a JSON status string with
  no rendering hint, so the user's chat stayed empty even after
  a successful generation. Rewritten to use the new
  `SHELL_AGENT_WORK_DIR` + `register-object` flow; the image
  now appears in the Data panel immediately and inline in chat
  after the follow-up call. Output is `{status, filename}` only
  — no `next_step` instruction (would derail multi-step plans)
  and no absolute host path (would be exfiltration material;
  see `work-dir-shell-bridge.md` §6).
- `bundled/tools/write-note.sh` now writes to
  `$SHELL_AGENT_WORK_DIR` instead of `/tmp/`. The note is
  immediately visible in the Data → /work panel and can be
  promoted to objstore via `register-object` if the user wants
  it to show up in chat. Same output contract as
  `generate-image.sh` (filename only, no instructions, no
  absolute paths). **Note for existing users**: the bundled
  installer doesn't overwrite a script that already exists in
  your `tools/` dir; delete `~/Library/Application Support/shell-agent-v2/tools/write-note.sh`
  before the next launch to pick up the new version, or copy
  the change in manually.

## [0.1.24] - 2026-05-02

### Added

- **Per-tool execution timeout (`@timeout: N` script header).**
  Shell-tool scripts can now declare their own timeout in the
  header, overriding the package default of 30 seconds for that
  tool only. Useful for legitimately long-running tools such as
  `web-search` (which calls `gem-search` via Vertex AI Gemini
  grounded search) and `generate-image` (image generation
  round-trip) — both example scripts ship with `@timeout: 120`.
  The bundled six (`weather`, `get-location`, `list-files`,
  `file-info`, `preview-file`, `write-note`) get an explicit
  `@timeout: 30` declaration too, matching the package default
  but making the option discoverable. Invalid values
  (non-numeric, zero, negative, Go duration string like `90s`)
  surface as `[ERROR]` in `app.log` and the script falls back to
  the default — registration still succeeds. Design:
  [docs/en/tool-execution-timeout.md](docs/en/tool-execution-timeout.md).

## [0.1.23] - 2026-05-02

Hygiene + small-feature batch.

### Added

- **Per-backend retry policy in Settings.** New per-backend
  `Retry max attempts` field in Settings → Local LLM and
  Settings → Vertex AI (default 3, range 1–10). Backoff timing
  knobs (`retry_backoff_base_seconds`, `retry_backoff_max_seconds`,
  `retry_jitter_seconds`) are config-only on `cfg.LLM.{local,vertex_ai}.*`
  for power users; default to nlk/backoff's defaults (5s base,
  120s max, 1s jitter). Lets users on a slower-quota GCP project
  shorten or lengthen the retry sequence without rebuilding.
- **`internal/frontendlint` package.** A Go test
  (`TestNoRehypeRaw` / `TestPackageJSONDoesNotDependOnRehypeRaw`)
  scans `frontend/src/` and `frontend/package.json` for the
  forbidden `rehype-raw` / `rehype-sanitize` imports and the
  `dangerouslySetInnerHTML` escape hatch. The Markdown pipeline
  is XSS-safe by construction (security-hardening-2.md §8); this
  test catches the regression vector if someone re-enables raw
  HTML in the future. Runs with `make test`, no separate ESLint
  pipeline needed.

### Changed

- **Internal: Go-1.22+/1.25+ idiom sweep across the codebase.**
  ~30 C-style `for i := 0; i < N; i++` loops converted to
  `for i := range N` (or `for range N` where the index isn't
  used); 3 `wg.Add(1) + go func() { defer wg.Done() … }()`
  patterns converted to `wg.Go(...)`; `strings.Split` → `SplitSeq`
  in two streaming-style consumers; one
  `HasPrefix + TrimPrefix` pair → `CutPrefix`; two unnecessary
  `fmt.Sprintf` calls inlined to string literals. Pure hygiene,
  no behaviour change. All tests pass under `-race`.

## [0.1.22] - 2026-05-02

### Changed

- **Analysis-tool descriptions clarified.** The LLM-facing
  descriptions for `query-sql`, `query-preview`, `quick-summary`,
  `suggest-analysis`, and `analyze-data` now spell out (a) what
  each tool actually does (LLM in the loop or not, executes or
  not, returns rows or narrative), and (b) when to prefer one
  over the others. Previously e.g. `quick-summary` was just
  "Execute a SQL query and generate a natural language summary"
  — accurate but didn't tell the model when to choose it over
  `query-sql` or `analyze-data`. The Settings → Tools list got
  matching one-line summaries that are descriptive sentences
  rather than 3-word labels.

### Fixed

- **Settings → Tools now lists `analyze-data`, `list-objects`,
  and `get-object`.** Three tools that were exposed to the LLM
  (via the dispatcher) but missing from the Settings UI's tool
  list, so users had no way to inspect their MITL state or
  disable them per-tool. Particularly noticeable for
  `analyze-data` since it's the headline sliding-window analysis
  tool of this app. Now consistently surfaced — same
  descriptions, same per-tool toggle controls as every other
  analysis tool.

## [0.1.21] - 2026-05-02

UX-driven release: drop the `hasData`-based dynamic filter on the
analysis-tool set so the LLM can plan multi-step "load → query →
analyse → report" workflows up front. Design in
[docs/en/agent-tool-visibility.md](docs/en/agent-tool-visibility.md).

### Changed

- **Analysis tools are now exposed every round**, regardless of
  whether the active session has data loaded. Previously
  `query-sql`, `describe-data`, `list-tables`, `query-preview`,
  `suggest-analysis`, `quick-summary`, `analyze-data`,
  `promote-finding` were hidden until a successful `load-data`,
  which forced the LLM into round-by-round discovery and broke
  up-front planning. The original rationale ("keep tool count
  low for local LLMs") was for the early gemma2 / gemma3 era;
  the standard local model (gemma-4-26b-a4b, MoE) and current
  gpt-oss / qwen3 generation handle 30+ tools without
  selection-accuracy regression.
- **Visible result for users**: when you ask "monthly sales
  totals" without attaching data, the model now proposes
  `load-data` (asking for the file path) instead of declining.
  When you attach data and ask in the same message, the model
  can plan the load + query in one response instead of two
  round-trips.

### Added

- New config flag `tools.hide_analysis_tools_until_data_loaded`
  (default `false`) restores the pre-v0.1.21 behaviour for users
  on weaker local backends where exposing 30+ tools measurably
  hurts selection accuracy. Power-user knob, config-only — not
  exposed in the Settings UI.
- `load-data`'s tool description now advertises the downstream
  pipeline (`query-sql`, `describe-data`, `analyze-data`, etc.)
  so the LLM doesn't have to guess what becomes available after
  a successful load.

## [0.1.20] - 2026-05-02

Second-round security hardening on top of v0.1.18 / v0.1.19. Phased
into five commits — see [docs/en/security-hardening-2.md](docs/en/security-hardening-2.md)
for the full design and finding inventory. Two additional
verification follow-ups (Settings UI MITL default surfacing; `~/`
expansion in `load-data`) are documented under §10 of the same
design doc.

### Security

- **MCP / sandbox / local-LLM I/O bounded.** A misbehaving MCP
  guardian, sandbox command, or local LLM endpoint can no longer
  hang or OOM the app:
  - MCP guardian stderr is now drained concurrently into the app
    log; previously, more than ~64 KB of stderr deadlocked the
    parent's stdout scan (security-hardening-2.md C2).
  - Sandbox `Exec` stdout/stderr capped per call at
    `Sandbox.MaxOutputBytes` (default 8 MiB). Excess bytes are
    discarded with a trailing `[output truncated at N bytes]`
    marker so the LLM still sees what happened (C3).
  - Local-backend `Chat` / `ChatStream` reject success-path bodies
    larger than 16 MiB (H12).
- **MCP response IDs are now validated.** A guardian that returns a
  response with the wrong `id` is rejected as a transport error
  rather than silently routing one call's body back to a different
  caller (H4).
- **DuckDB metadata lookup parameterised.** `refreshTableMeta` no
  longer string-concatenates the table name into the
  `duckdb_tables()` `WHERE` clause; LLM-supplied names with quote
  characters or SQL meta-syntax can no longer perturb the query
  (C1).
- **Robust MCP tool-name parsing.** The dispatcher used to do a
  naive `SplitN("__", 2)` on the part after `mcp__`, which
  mis-routed when a guardian or upstream tool name contained
  `__`. New `splitMCPName` helper falls back to a longest-prefix
  match against the registered guardian set. Guardian names are
  also validated against `^[a-zA-Z0-9-]+$` at startup so the
  separator collision can't be planted in fresh configs (H3).
- **Crash-safe JSON writes across the data path.** New
  `internal/atomicio.WriteFileAtomic` helper writes via
  tmp+rename so a power-fail or kill -9 mid-write leaves the
  previous file intact rather than a torn / empty file the next
  Load would mis-parse. Applied to: objstore index, findings
  store, pinned-memory store, contextbuild summary cache, and
  per-session chat.json (security-hardening-2.md C4 / H10).
- **Findings ID race fixed.** `findings.Store.Add` used to derive
  IDs from `len(s.findings)` without a lock; concurrent Adds
  produced duplicate IDs and a later DeleteByIDs would silently
  remove the wrong record. The store now serialises via
  `sync.Mutex`, and the >999-per-day overflow path picks up a
  6-hex random suffix so the ID stays unique without colliding
  with the legacy fixed-width format (H9).
- **Sandbox image pin advisory.** Settings → Sandbox surfaces a
  warning banner when the active image uses a mutable upstream
  tag (e.g. `python:3.12-slim`). Locally-built images and
  `@sha256:` digest pins are treated as safe. The warning is
  advisory — we do not refuse a mutable tag, since some users
  legitimately want to track upstream patch updates
  (security-hardening-2.md H5).
- **Local-LLM ToolCall.Arguments validated.** Each tool call's
  Arguments string is checked for valid JSON and capped at 1 MiB
  (configurable via `cfg.LLM.{local,vertex_ai}.max_tool_call_args_bytes`).
  This is a garbage / attack detection threshold — real workloads
  (sandbox-write-file with HTML/CSV/Python, create-report with
  long markdown) sit well below the cap. Empty Arguments strings
  remain accepted for no-parameter tools (H6).
- **Wider object-store IDs.** New objects use 16-byte (32 hex
  char) IDs, up from 12-hex (48-bit). Birthday-bound collisions
  are now astronomically improbable. Legacy 12-hex IDs continue
  to load — the read path is length-tolerant (H11).
- **`load-data` rejects symlinks.** `validateFilePath` now uses
  `os.Lstat` and refuses any path that is itself a symlink. An
  attacker who could plant a symlink in a path the LLM might
  pass would otherwise be able to redirect ingest to a host
  file the analysis layer is meant to refuse (H14).
- **`guard.Wrap` is now fail-closed.** `chat.BuildMessages`,
  `chat.BuildMessagesWithBudget`, `chat.WrapUserToolContent`,
  and `contextbuild.Build` all return an error when the
  underlying guard wrap fails (essentially crypto/rand
  catastrophe). Previously they silently fell back to the
  unwrapped content, giving the LLM untrusted input under our
  system-prompt trust level. The agent loop surfaces the error
  to the user instead of proceeding (security-hardening-2.md L1).

### Changed (API)

- `chat.Engine.BuildMessages`, `BuildMessagesWithBudget`, and
  `WrapUserToolContent` gained a trailing `error` return. Same
  for `contextbuild.Build` and the
  `BuildOptions.WrapUserToolContent` callback type. Internal
  packages only — no JSON / Wails-binding surface change.

### Fixed

- **Settings → Tools MITL toggles now actually work for analysis
  tools** (security-hardening-2.md H1+H2). The toggle for
  `load-data`, `reset-analysis`, `promote-finding`,
  `describe-data`, `list-tables`, `query-preview`,
  `suggest-analysis`, `quick-summary`, `create-report` was a
  no-op — the dispatcher's analysis branch never consulted
  `MITLOverrides`. Conversely, `query-sql` and `analyze-data`
  ignored the toggle's OFF state because their MITL was
  hard-coded inside the tool handler. Both directions are now
  honoured: turn the toggle ON to gate a previously-ungated tool,
  OFF to silence a previously-forced prompt. New defaults match
  what the UI used to imply (`load-data`, `reset-analysis`,
  `promote-finding`, `query-sql`, `analyze-data` default ON;
  metadata reads default OFF).
- **Settings → Tools toggle reflects the dispatcher's actual
  default.** Discovered during v0.1.20 verification: the toggle's
  rendered "default" state was computed locally in the frontend
  from `category`/`source`, which silently went out of sync with
  Phase B's `analysisToolMITLDefault` table. Result: the user
  saw `load-data` MITL toggle as OFF, toggling it had no effect
  (the value already equalled the locally-computed default so no
  override was persisted), and the dispatcher prompted anyway
  because its true default was ON. Backend now exposes
  `mitl_default` per tool via `GetTools` and the frontend uses
  it directly. `IsToolMITLRequired` is also the single source of
  truth across all tool sources (analysis / shell / sandbox /
  mcp) — the dispatcher's shell branch used to call
  `tool.NeedsMITL()` directly. New regression test
  `TestListTools_MITLDefaultMatchesGate` pins the contract.
- **`load-data` now expands `~/` in file paths.** Discovered
  during v0.1.20 verification: `filepath.Abs` alone leaves the
  literal `~` in place, so an LLM passing `~/Desktop/foo.csv`
  (because the user typed it that way) would always 404. The
  validator now expands `~` via `config.ExpandPath` before
  resolving — mirrors what MCP profile paths already do.
- `TestSandboxDefaults` was asserting that the default
  `Sandbox.Image` is populated, but the actual default is empty
  on purpose (the readiness gate hides sandbox tools until the
  user picks an image in Settings → Sandbox). Test updated to
  match the documented intent.

## [0.1.19] - 2026-05-02

User-experience and protocol-correctness release on top of v0.1.18's
security hardening. Two visible new pieces (background task
indicator, CSV / text-blob preview), one structural fix (tool-event
bubble restoration on session reload), and a sweep of LLM-pipeline
and macOS-build issues exposed during data-analysis runs.

Designs in
[docs/en/background-task-indicator.md](docs/en/background-task-indicator.md),
[docs/en/tool-event-restore.md](docs/en/tool-event-restore.md), and
[docs/en/tool-call-roundtrip.md](docs/en/tool-call-roundtrip.md).

### Added

- **Background task indicator.** When the agent kicks off
  post-response work (title generation, memory compaction,
  pinned-fact extraction), a small badge appears in the
  input-status-bar naming what's running. The agent stays Busy on
  the backend until those tasks complete; the input field stays
  disabled and the Sidebar's New / Load / Delete actions are
  greyed so a quickly-typed second message can't race them. Abort
  still cancels post-tasks. Logs are symmetric — every task
  records `start` and exactly one of `done` / `canceled` / error
  at INFO/ERROR level so an operator can correlate "session
  never got a title" with "user typed before title-gen finished".
- **CSV / text-blob preview in the Data pane.** Clicking a
  `text/csv` or `text/tab-separated-values` blob opens a real
  HTML table (first 200 rows, 30 columns shown, RFC-4180-shaped
  quoting); other text MIMEs (JSON, plain text, Markdown, HTML,
  XML, NDJSON, JS) drop to a fixed-width `<pre>` view. Source is
  clipped to 100 KB before reaching the modal. Binary blobs stay a
  click no-op, with Export still available.
- **Tool-event bubbles persist across session reload.** Tool
  calls used to be invisible in restored sessions because
  `LoadSession` dropped every `tool` record on the floor. They
  now come back as compact name + status bubbles, matching the
  live look. Required adding a `Status` field to `memory.Record`
  (omitempty, populated from the `executeTool` return value
  already used for `tool_end` activity events). Legacy records
  without the field default to `success` so older chats stay
  readable.

### Changed

- **Post-response tasks now hold Busy until they finish.** An
  earlier iteration tried auto-cancelling them at the start of
  the next `Send`, but local LLMs are slow enough that
  pinned-fact extraction never completed during rapid
  conversation, and the extractor only sees the latest 4 hot
  records — cancellation drops those facts permanently rather
  than deferring. Live behaviour now matches the design: state
  stays Busy from the moment a user message arrives until all
  three post-tasks finish, and the frontend (input field, Sidebar
  session ops) keys off that.
- **Sticky table headers in the Data pane.** Both the new
  BlobPreview CSV table and the existing Tables-tab DB preview
  used `--bg-hover` (≤0.1 alpha in every theme) for the sticky
  `<th>` background, so scrolled rows bled through. Added a
  per-theme opaque `--bg-sticky-header` and routed both rules
  through it.
- **Build artifact integrity on macOS.** `make build` used `cp
  -r` to publish `dist/shell-agent-v2.app`. macOS strips
  extended attributes (and with them the ad-hoc code-sign
  resource fork) on `cp -r`, producing a bundle that crashed at
  launch with `SIGKILL "Code Signature Invalid"` until manually
  re-signed. Switched to `ditto` to preserve the signing
  resources end-to-end.
- **Tool-call assistant turns are no longer restored as chat
  bubbles.** Live chat surfaces tool-call narration through the
  activity stream (the transient progressTool "thinking" banner),
  not as a chat bubble. Restore was inconsistent — any
  non-empty `Content` came back as a bubble regardless of
  attached `ToolCalls`, exposing thought-style preambles that the
  live view had hidden. Skip those records on restore so the two
  views match.

### Fixed

- **Vertex parallel function calls returned HTTP 400.** When the
  assistant emitted multiple FunctionCall parts in a single turn
  (parallel tool calls), the matching FunctionResponse parts were
  emitted as separate user Content blocks; Gemini requires them
  packed into one. `vertex.go buildContents` now coalesces
  consecutive `RoleTool` messages into a single Content block.
- **`/model` could appear frozen during a 429 retry.** The
  slash-command parse used to live behind `postTasksWg.Wait()`,
  so a `/model local` typed while a previous turn's
  pinned-fact-extraction was sleeping in retry-backoff blocked
  for minutes. Slash commands now parse before the wait, and
  Abort also fires the post-task cancel func — previously Abort
  only cancelled the in-flight Send, leaving the post-task
  goroutines unkillable.
- **Bindings could panic on startup.** `GetLLMStatus`,
  `LoadSession`, and `Send` had no nil-guard for the brief
  window between Wails' frontend mount and the backend
  finishing `agent.New`. The frontend's first poll could race
  in and trigger a nil-pointer panic in every `GetLLMStatus`
  tick. Added defensive checks.
- **Gemini 2.5 Flash thoughts leaked into the assistant text.**
  Some responses arrived as `THOUGHT\n…\n\nactual reply` or
  `思考\n…\n\n本文` or `シンクタイム: 3秒\n\n本文` — sometimes
  in their own Part with `Thought=true`, sometimes inline. Set
  `ThinkingConfig.IncludeThoughts=false` explicitly (default
  behaviour, but worth being explicit), filter Parts with
  `Thought=true` in `parseResponse`, and log per-Part shape at
  debug level so the rare inline-text case can be diagnosed
  without guesswork.

### Notes

The remaining inline-text-thought case (model writes `思考\n…`
into a regular text Part on a fraction of replies) is **not**
addressed in this release. The Part-shape filter doesn't reach
it, and a heuristic strip risks over-deleting legitimate user
content. The follow-up option is to set
`ThinkingConfig.ThinkingBudget = 0`, which trades reasoning
quality for an end to inline preambles; deferred to a future
release once the trade-off is evaluated against representative
analysis tasks.

## [0.1.18] - 2026-05-01

Security-hardening release. Addresses three HIGH-severity and
four MEDIUM-severity findings from the 2026-05-01 audit. Plan
in [docs/en/security-hardening.md](docs/en/security-hardening.md).

### Fixed

- **(HIGH) Symlink traversal through `/work`.** `safeWorkPath`
  now resolves symlinks on the parent directory and rejects
  any symlink leaf, including ones pointing inside `/work`.
  Combined with `--user $UID`, the previous lexical-only
  check let an attacker LLM `ln -s ~/.ssh/authorized_keys
  /work/foo` from inside the container, then write through
  the symlink via `sandbox-write-file path=foo` — host file
  modified. `sandbox-register-object` and
  `sandbox-load-into-analysis` had the symmetric read
  vulnerability. Both closed.
- **(HIGH) `objstore.Store` concurrent map writes.** The map
  was accessed without a mutex from the agent's
  post-response goroutines and from the next tool call
  on the main path; the Go runtime panics with
  `concurrent map writes` under realistic agent activity.
  Added `sync.RWMutex`.
- **(HIGH) `:Z` SELinux mount label always appended.** Only
  correct on podman + Linux. Docker-desktop on macOS rejects
  it as invalid; Linux + docker without SELinux can clobber
  labels on shared parents. `buildRunArgs` now takes a
  `selinuxRelabel bool`; `useSELinuxRelabel(binary)` returns
  true only when `runtime.GOOS == "linux"` and the binary
  basename is `podman`.
- **(MED) MITL channel could silently auto-approve a future
  prompt.** A click while no request was pending pushed a
  value into a long-lived buffered channel; the next request
  consumed that buffered value instead of waiting for fresh
  input. Replaced with a per-request slot — stray clicks
  no-op, double-clicks are idempotent.
- **(MED) `agent.guardians` map written without
  `guardiansMu.Lock()`** in `startGuardians`. Lock now held
  for the whole start sequence.
- **(MED) Sandbox containers leaked across launches.**
  `maybeStartSandbox` now sweeps stray containers (label-
  scoped) at startup. `main.go` installs a SIGINT/SIGTERM
  handler that runs the shutdown hook before `os.Exit`.
- **(MED) Settings change to sandbox config didn't restart a
  running container.** `SaveSettings` snapshots
  `cfg.Sandbox` before applying changes; if the new struct
  differs, it calls `agent.RestartSandbox` so the next
  sandbox-* tool recreates with the new settings.
- **(MED) `LoadSession` reassigned `a.session` while
  post-response goroutines were still reading it.** Drains
  `postTasksWg` before the swap, mirroring `Send`.

### Hardening

- `MockBackend` now uses a mutex around `Calls()` /
  `nextResponse()` so test inspection during in-flight
  background calls doesn't race with `-race`.

### Notes

`M4` (DuckDB `LoadFile` SQL string-concat with single-quote
doubling) is not addressed in this release. The current
defence works; parameterising would require an
analysis-package restructuring better tackled when DuckDB's
bind API is more battle-tested in our stack.

## [0.1.17] - 2026-05-01

Settings surface improvements and a round of dead-code cleanup
on top of v0.1.16's resilience and multimodal work.

### Added

- **Configurable max tool rounds.** `agent.max_tool_rounds`
  (default 10) now appears in the Settings dialog under "Agent
  loop". Loop detection (Feature 1, v0.1.16) catches stuck
  same-error stretches early, so raising this is reasonably
  safe when a long, legitimate analysis legitimately needs more
  rounds.
- **Configurable output reserve.** New `output_reserve` field
  on each per-backend context budget (default 4096). Tokens
  reserved for the model's reply, subtracted from
  `max_context_tokens` before context packing so the request
  stays under the model's window. Was previously hardcoded
  inside `agentLoop.buildMessagesV2`.
- **Settings reference in the README.** Both `README.md` and
  `README.ja.md` gained a full table covering agent-loop knobs,
  per-backend context budgets (5 fields), per-request
  timeouts, and the seven sandbox knobs.

### Changed

- **Tool description wording.** "Central object repository"
  rephrased to "session object store" in
  `sandbox-copy-object` / `sandbox-register-object`. Less
  internal-sounding for the LLM and humans.
- **Multi-image scaling closed.** v0.1.16's per-image-turn fix
  for the local backend has now been manually verified at N=3,
  N=5, and N=8 — the upper bound of Gemma 3's multi-image
  training. No additional mitigation needed.

### Removed

- **Dead `'done'` status.** The `ChatMessage.status` union
  dropped the legacy `'done'` member; the backend has only
  emitted `'success'` / `'error'` since v0.1.13.
- **Dead `.object-item` CSS family.** ~130 lines orphaned by
  the info-display redesign Phase 3 (replaced by
  `.data-object-*`). No remaining className users in the
  codebase.
- **Unused `ObjectReferences` Wails binding.** Go function +
  TypeScript declaration + two tests + auto-generated wailsjs
  entries. The frontend stopped calling it after the Objects
  panel was removed.

## [0.1.16] - 2026-05-01

Five resilience and multimodal improvements after v0.1.15. Plans
in
[docs/en/agent-loop-resilience.md](docs/en/agent-loop-resilience.md)
and
[docs/en/multi-image-handling.md](docs/en/multi-image-handling.md).

### Added

- **Loop detection with corrective hint** — when the LLM calls
  the same tool with `status=error` three rounds in a row, a
  one-shot system note is prepended to the next LLM call asking
  the model to try a substantively different approach. Detection
  uses a small ring buffer scoped to one agent turn; firing
  resets it so each consecutive-error stretch fires at most
  once.
- **Empty-response wrap-up retry** — when Vertex returns
  content="" with no tool calls right after a successful tool
  call (observed: tokens=N/0 silent exits), the agent gives it
  one chance to wrap up by injecting a system nudge asking for
  a brief summary. Falls through to the existing silent exit if
  the retry also returns empty.
- **Retry-backoff badge in the footer** — when the LLM backend
  hits a transient failure (429, 503, timeout) and is sleeping
  before the next attempt, a small badge appears in the
  input-status-bar (e.g. "attempt 1: rate limit (waiting
  4.8s)") so the user knows the slow round is a backoff, not a
  hang. The badge clears on the next `tool_start` /
  `tool_end` / turn end.

### Changed

- **Multimodal user messages now anchor each image to its
  persistent object ID.** Each image is preceded by a one-line
  prefix `Image (object ID: x):` so the model can correlate
  visible image content with the ID it should reference in
  reports. Previously the LLM could see the image data but had
  no reliable way to map "image at position N" → object ID, and
  reports could end up referencing swapped IDs.
- **Per-image user turns on the local backend.** With ≥2
  images attached, llama.cpp's mmproj cache reuses slots across
  one prompt and the positional binding between
  `<start_of_image>` markers and embedding tensors gets
  reordered, causing image-1 / image-3 swap on Gemma 3
  multimodal. Workaround: split a multimodal user `Message`
  into N image-bearing user turns plus one trailing turn with
  the original text, so each image gets its own prompt region.
  Vertex (which has no such bug) keeps a single Content block
  with the same one-line ID prefix.
- **Data disclosure refreshes immediately after each tool.**
  Previously the panel only re-fetched at turn end, so a freshly
  registered object wouldn't appear until the agent fully
  finished. The frontend now bumps the refresh tick on every
  `tool_end` activity event.

### Fixed

- **Image-attach order race.** `ChatInput.addImages` read each
  selected file with its own `FileReader` and appended the
  result inside `onload`. Bigger files finished later, so
  `pendingImages` could end up in a different order than the
  user actually attached. Now reads all files in parallel with
  `Promise.all` and appends in original order.
- **Retry badge could linger after the response finished.** If
  the final round of a turn didn't carry a `tool_start` /
  `tool_end` pair, the "rate limit (waiting Xs)" badge stayed
  on screen. Cleared on `state==='idle'` and gated on the
  busy state.

## [0.1.15] - 2026-05-01

### Added

- **Tabbed Data sub-sections** (information-display redesign
  §6). The chat-pane Data disclosure no longer stacks Objects
  / Tables / /work vertically; a tab strip at the top of the
  body switches between them, with only the active section's
  content rendered below. Tabs only appear for sub-sections
  that actually have data, and the active tab falls back to
  whichever still has content if the current one empties.

### Changed

- **Frontend code organisation.** `App.tsx` was 1457 lines
  with ten-plus interfaces, the Wails binding declaration,
  three subcomponents, the entire App component, the sidebar
  tree, and four overlay dialogs all in one file. Decomposed
  into a coordinating shell + dedicated modules:
  - `types.ts` — shared TypeScript interfaces
  - `bindings.ts` — `window.go.main.Bindings` global
    declaration
  - `components/` — `MessageItem`, `BulkActions`,
    `BackendBudgetEditor`
  - `sidebar/Sidebar.tsx` — accordion + bottom-nav + resize
    handle (sidebar-local state moved here)
  - `dialogs/SettingsDialog.tsx`, `MITLDialog.tsx`,
    `Lightbox.tsx`, `ReportViewer.tsx`
  Final `App.tsx`: 587 lines (~60% reduction). DOM, CSS
  classes, Wails binding surface all unchanged. Plan
  documented in
  [docs/en/frontend-decomposition.md](docs/en/frontend-decomposition.md).

## [0.1.14] - 2026-04-30

A round of UI fixes from the GitHub issue tracker, covering
#1–#4. The sidebar reorganization that v0.1.11 introduced had
several rough edges that this release smooths out.

### Fixed

- **#1 — Sidebar title showed "Status" instead of "Memory".**
  The variable rename from v0.1.11 missed the literal label at
  the top of the panel. Memory's icon also matched Sessions'
  triple-bar (≡), making them indistinguishable. Memory is
  now ★ and the section is labelled correctly. The whole
  sidebar was reworked into a single DOM tree that adapts to
  collapsed mode via a CSS class — collapsed and expanded
  sidebars now share an identical layout source-of-truth, so
  icon Y-positions and section dividers match between modes
  by construction.
- **#2 — Empty Data disclosure took chat-pane real estate.**
  When a session has no Objects, no DuckDB tables, and no
  `/work` files, `DataDisclosure` returns null instead of
  rendering the muted "Data — empty" strip.
- **#3 — Sidebar icon and label vertical alignment.** The icon
  glyph (font-size 18px on a 13px line) was rendering above
  the label baseline. `.sidebar-nav-btn` now uses flex with
  align-items: center, and the icon span is an inline-flex
  with a fixed 22px basis. Plus button horizontal padding
  bumped to 10px so the 22px icon centers in the 42px
  collapsed sidebar (10 + 22 + 10 = 42).
- **#4 — Sidebar width and collapsed state were ephemeral.**
  Saved to `UIConfig.SidebarWidth` / `SidebarCollapsed` via
  new `Bindings.GetSidebarPrefs` / `SaveSidebarPrefs`
  bindings; the frontend reads them on mount and writes on
  resize-end / collapse toggle.

## [0.1.13] - 2026-04-30

### Changed

- **All sandbox tool dispatchers return typed status.** The
  Phase B-1 wiring relied on a `wrapErrorPrefix` helper that
  inferred success / failure from whether the result string
  started with `"Error:"`. Each `toolSandbox*` function now
  returns `(string, ActivityEventStatus)` directly, the same
  shape `run-shell` / `run-python` already used. A
  successful tool whose output happened to begin with the
  word "Error" can no longer be misclassified.

### Added

- **MCP `result.isError` is now classified as failure.** The
  MCP spec lets a server succeed at the RPC layer but mark
  the result as a logical failure via `result.isError: true`.
  `Guardian.CallTool` now returns `ErrToolFailed` on that
  path while preserving the response body so the LLM still
  sees the diagnostic. The agent branch maps `ErrToolFailed`
  to an error tool-event bubble.

### Coverage

- `TestGuardian_CallToolIsErrorSurfacesAsErrToolFailed` covers
  the new path against the existing python stub guardian.
- All `executeSandboxTool` dispatch tests pass with the new
  signature; no behaviour change for previously-classified
  paths.

## [0.1.12] - 2026-04-30

### Added

- **Tool-event success / failure indicator in the chat.** The
  inline tool-event bubble now renders red ✗ when a tool failed
  and green ✓ when it succeeded; running stays muted with the
  existing pulse. Classification sources:
  - `sandbox-run-shell` / `sandbox-run-python`: container
    `ExitCode != 0` or `TimedOut` → error.
  - Other sandbox tools, analysis tools, MCP, shell-script:
    Go-side `error` from the dispatcher → error.
  - MITL rejections → error (no more green check next to "Tool
    execution rejected by user.").
  Plumbed through a new `ActivityEvent{Type, Detail, Status}`
  struct on the agent ↔ bindings boundary; the `'done'` event
  status is kept as a soft-fallback for older message records.

- **`nlk/jsonfix` at the tool-call boundary** (RFP §3 reuse
  target). When a model surrounds JSON tool arguments with
  ```json fences, surrounding prose, single quotes, or
  trailing commas, the agent now repairs them transparently
  before dispatching. Lazy: well-formed JSON is fast-pathed via
  a direct `json.Unmarshal` probe and never sees jsonfix, so
  Vertex's pristine output passes through completely untouched.

- **`nlk/jsonfix` in the analysis summarizer.**
  `parseWindowResponse` was a hand-rolled "try direct →
  ```json fence → first balanced { ... }" cascade — exactly
  what `jsonfix.Extract` does, plus jsonfix also repairs single
  quotes / unquoted keys / unbalanced braces. Replaced with one
  Extract call.

### Fixed

- **Inner-bubble visual redundancy.** The chat tool-event row
  was rendering "frame inside a frame" because both the outer
  `.message.tool-event` wrapper and the inner status bubble
  shared the `.tool-event` class — every CSS rule landed twice.
  Renamed the inner element to `.tool-bubble` so the bubble
  styles only apply once.

## [0.1.11] - 2026-04-30

### Added

- **LLM call control: per-request timeout, retry, backoff, and
  call logging.** Closes the `nlk: …backoff…` gap in
  `docs/en/shell-agent-v2-rfp.md` §3 — until now the Vertex
  backend had *no* timeout (the SDK's default `http.Client.
  Timeout` is zero) and the Local backend had a hardcoded
  5-minute one, neither retried, and `app.log` had zero
  visibility into the LLM call layer. A thinking-mode call
  could hang the UI indefinitely with no sign of life.

  New `internal/llm/retry.go` wraps any `Backend` with
  `context.WithTimeout` per attempt, conservative retry on
  transient signals (HTTP 429 / 5xx, gRPC `RESOURCE_EXHAUSTED`
  / `UNAVAILABLE` / `DEADLINE_EXCEEDED`, network resets, plus
  the per-attempt timeout firing — including the
  Vertex-side echo as `Error 499 CANCELLED`), exponential
  backoff via `nlk/backoff` (base 5s, ×2, cap 60s, ±10%
  jitter), 3 attempts total, and `start / done / err / backoff`
  log lines so app.log finally shows what happened.

  Configurable via Settings:
  - `LLM.Local.RequestTimeoutSeconds` (default 300)
  - `LLM.VertexAI.RequestTimeoutSeconds` (default 180 — gives
    gemini-2.5-flash thinking mode headroom while still
    bounding silent hangs)

  `Bindings.RestartLLMBackend` lets the Settings UI rebuild the
  wrapper live without an app restart. Local backend's
  hardcoded `http.Client.Timeout` was removed (one timeout
  source only).
- **Information display redesign — sidebar / chat pane /
  footer reorganisation** (docs/en/information-display-redesign.md).
  Six-phase plan; phases 1–5 ship in this release.
  - Sidebar shrinks to two panels: **Sessions** and **Memory**
    (Findings + Pinned, both global). The mixed-scope `Status`
    panel and the standalone `Objects` panel both go away.
  - Chat pane gains a collapsible **Data** disclosure at the
    top, scoped to the currently-selected session. Three
    sub-sections: **Objects** (card grid with image
    thumbnails / typed icons; click to preview, hover-revealed
    export + delete), **Tables** (row list; click for a
    20-row preview modal), **/work** (light card grid with
    extension badges; only when sandbox is on). Marker
    triangle ▶ / ▼ in the disclosure summary, count
    indicators on collapsed view.
  - Per-session DuckDB tables and sandbox `/work` files were
    previously LLM-only; now visible in the UI as a sanity
    check after `load-data` or session restore.
  - Footer strip below the chat shows `backend · Messages: N
    (+M summarized) · Tokens: X in / Y out`. Two-line wrap on
    narrow windows is the accepted degradation.
  - Delete UX: every destructive action (single-card delete,
    bulk delete) now goes through an inline Yes / No confirm
    with *separate* buttons. The previous "click the same
    button twice" pattern was replaced because a misclick
    landed on the now-confirming button and proceeded
    unintended.
  - New backend bindings: `GetSessionObjects`,
    `GetSessionTables`, `PreviewTable`, `GetWorkFiles`. The
    LLM-side `list-objects` was already per-session-filtered;
    only the UI catches up with this release.
- **Engine-level table preview** —
  `analysis.Engine.PreviewTable(name, limit)` runs `SELECT *
  FROM <name> LIMIT N` with identifier sanitisation, `[]byte` →
  string conversion for clean JSON, and a `[1, 1000]` clamp.
  Used by `Bindings.PreviewTable`; LLM still goes through
  `query-sql`.

### Fixed

- **Session reopen lost DuckDB tables in the UI.**
  `analysis.New(sessionID)` was lazy — `Open()` only ran on
  the first `LoadCSV` / `Query` call. After app restart and
  session selection, `HasData()` returned false because the
  tables map was empty, so `buildToolDefs` hid `query-sql` /
  `describe-data` / `suggest-analysis` from the LLM. The
  DuckDB file with the loaded tables was sitting on disk
  untouched.
  New `Engine.OpenIfExists()` opens the DB only if the file is
  already present (sessions that never used analysis still
  avoid creating an empty `.duckdb`). `bindings.switchAnalysis`
  calls it on every session load.
- **Local LLM looped `load-data` on sandbox-produced files.**
  After generating CSVs in the sandbox, gemma kept calling
  `load-data` with bare filenames and retrying for several
  rounds before discovering `sandbox-load-into-analysis`.
  `load-data`'s description and parameter doc now state
  explicitly that it's host-only and point at
  `sandbox-load-into-analysis` for `/work` files. The system
  prompt's sandbox guidance gains a decision rule.

### Coverage

- 9 unit + 4 integration tests for `internal/llm/retry.go`
  (transient retry, persistent giveup, non-retryable bail,
  per-attempt timeout, caller-cancel, ChatStream parity, and
  the `IsRetryable` truth table).
- `analysis.Engine.PreviewTable` covered (rows + Total +
  Truncated accounting, unknown-table error, limit clamping,
  cross-restart metadata restore).
- `Bindings.GetSessionObjects` / `GetSessionTables` /
  `PreviewTable` / `GetWorkFiles` unit-tested for filtering,
  empty-engine fallbacks, and slash-form path output.

## [0.1.10] - 2026-04-30

### Fixed

- **Local LLM looped `load-data` on sandbox-produced files.**
  After generating CSVs in the sandbox, gemma (and similar
  smaller local models) would call `load-data` with bare
  filenames and retry the same args several rounds before
  switching to `sandbox-load-into-analysis`. The `load-data`
  description and parameter doc now explicitly say it's for
  the host filesystem only and point at
  `sandbox-load-into-analysis` for `/work` files. The system
  prompt's sandbox guidance gains a decision rule: if the file
  lives under `/work`, use `sandbox-load-into-analysis` —
  `load-data` will never see `/work`, don't retry it.
- **`sandbox-write-file` and `sandbox-export-sql` result
  messages echoed the LLM's raw input.** When the model passed
  `/work/foo.csv`, the result said `wrote ... to /work//work/
  foo.csv` — a misleading double `/work/` segment that looks
  like the very path-doubling regression we fixed in
  `safeWorkPath`. Both messages now derive the relative path
  from the resolved destination so the displayed path is the
  canonical `/work/<rel>`.
- **Findings card didn't follow the active theme.** The CSS
  used `var(--bg-secondary, #1a2a3a)`, but `--bg-secondary` is
  not defined in any theme — so the hardcoded fallback always
  won. Same for hardcoded text colours. Findings now use the
  existing theme tokens (`--bg-hover`, `--text-primary`,
  `--text-muted`, `--text-link`, `--bg-inline-code`).
  Severity tag colours stay hardcoded — they encode meaning,
  not theme.
- **Findings checkbox left an empty column after mouse-leave.**
  `.finding-card` was `display: flex` with a fixed gap, so an
  `opacity: 0` bulk-check still reserved its column and made
  the body look indented. Pinned memory wasn't affected
  because it uses normal block flow. Findings now matches:
  the checkbox floats inline so when invisible it's truly out
  of layout.

## [0.1.9] - 2026-04-30

### Fixed

- **Sandbox tools invisible when the `.app` was launched from
  Finder.** The Finder/launchd inherited PATH is the system
  default (`/usr/bin:/bin:/usr/sbin:/sbin`), which excludes
  Homebrew (`/opt/homebrew/bin`, `/usr/local/bin`) and
  `~/bin`. `resolveEngine()` calls `exec.LookPath("podman")`
  against that PATH, gets nothing, and `maybeStartSandbox()`
  bails — leaving `a.sandbox == nil`, so `buildToolDefs()`
  hides every `sandbox-*` tool. Symptom: enable Sandbox in
  Settings, name an image, but no sandbox tools appear and no
  image is ever pulled. Launching the binary directly from a
  shell worked because that PATH was inherited from the user's
  shell.

  Fix: a new `internal/pathfix` helper prepends the macOS-typical
  user bin directories at `main()` startup, before any
  subprocess work begins. Already-present and non-existent
  entries are skipped. Same fix benefits any other subprocess
  the app shells out to (MCP servers, future tooling).

### Coverage

- `internal/pathfix/pathfix_test.go` — 7 cases covering
  candidate ordering, dedup against current PATH, no-existing
  candidates fallback, and the default `os.Stat`-backed
  exists hook.

## [0.1.8] - 2026-04-29

### Added

- **`sandbox-load-into-analysis` tool.** Bridges a CSV/JSON/JSONL
  file under `/work` into the DuckDB analysis engine as a table,
  so a file produced by `sandbox-run-python` can be queried with
  `query-sql` without an explicit `load-data` round-trip. Reads
  through the host-side mount; no container hop. Accepts both
  `path` and `file_path` because both showed up in real LLM output.
- **`sandbox-export-sql` tool.** The inverse direction: runs a
  SELECT against the analysis engine and writes the result CSV
  straight to `/work/<file_path>`. Closes a wasteful round-trip
  where query results were pasted into chat as text and then
  handed to `sandbox-run-python`. Backed by a new
  `analysis.QuerySQLToCSV(query, writer)` that streams rows.
- **Sandbox guidance in the system prompt.** When sandbox is
  enabled, `chat.BuildSystemPrompt` / `BuildMessages` append a
  guidance block that tells the model when to reach for which
  `sandbox-*` tool, and explicitly to *emit* a function call
  rather than describe what it would do — gemma in particular
  tended to narrate the next step instead of taking it.
- **MITL dialog renders code-bearing tool args as multi-line
  blocks.** `sandbox-run-shell` (`command`), `sandbox-run-python`
  (`code`), `sandbox-write-file` (`content`), and
  `sandbox-export-sql` (`sql`) are shown in a pre-formatted
  block, mirroring the existing SQL display. A 50-line single-line
  `print(...)` block is now actually readable.

### Fixed

- **MCP profile fields are now editable.** Settings → MCP profile
  inputs were rendered as text, not `<input>` elements, so edits
  appeared to take but never round-tripped to the config.
- **`safeWorkPath` no longer doubles `/work/` segments.** When the
  LLM passes the in-container absolute path it sees inside
  `sandbox-run-python` (`/work/foo.png`), the helper now strips
  repeated `/work/` and leading `/` prefixes before joining with
  the host work dir, so it doesn't produce
  `<sessions>/<sid>/work/work/foo.png` and fail at file-write or
  register-object time.
- **`sandbox-load-into-analysis` accepts both `path` and
  `file_path`.** LLMs split on which key name to use; the handler
  now takes either.

### Coverage

- New unit test
  `TestExecuteSandboxTool_WriteFileNormalisesWorkPrefix` — covers
  `/work/foo`, `work/foo`, and bare `foo` resolving to the same
  host path.
- `analysis.QuerySQLToCSV` covered by the existing analysis test
  suite; sandbox tool dispatch tests extended to include
  `sandbox-export-sql` in the expected tool name set.

## [0.1.7] - 2026-04-29

### Added

- **Container sandbox** (Settings → Sandbox, opt-in). When enabled
  the LLM gets six new `sandbox-*` tools that execute inside a
  per-session container managed via `podman` or `docker`:
  - `sandbox-run-shell` / `sandbox-run-python` — execute code
  - `sandbox-write-file` — LLM → sandbox text drop-off
  - `sandbox-copy-object` — central object store → sandbox
  - `sandbox-register-object` — sandbox-produced file → object
    store, returns an ID the LLM can embed in reports as
    `![alt](object:ID)`
  - `sandbox-info` — engine, image, Python version, installed pip
    packages, `/work` listing
  Files written under `/work` persist within the session and are
  isolated between sessions. Side effects do not touch the host.
  MITL-gated, network-off by default, resource-bounded
  (`--memory`, `--cpus`, per-call timeout).
  Design: `docs/{en,ja}/sandbox-execution{,.ja}.md` including a
  macOS setup guide that covers the `podman machine` /
  `applehv` / `krunkit` pitfalls hit during development.
- **Settings live reload for the sandbox.** Toggling
  `Enabled / Engine / Image / Network / cpu_limit / memory_limit /
  timeout` in Settings tears down existing containers and
  reconstructs the engine in place — no app restart required.
- **Automatic image pull.** On the first `sandbox-*` tool call
  after an image change, the engine runs `podman pull` (or
  `docker pull`) automatically. The user no longer needs to
  pre-pull from a separate terminal.
- **`Bindings.RestartSandbox()`** — exposed to the frontend, used
  by the Settings save handler.

### Coverage

- New `internal/sandbox` package: 24 unit + 6 integration tests
  (skipped when no engine is on PATH).
- New agent dispatch tests: `sandbox_tools_test.go`, 12 cases
  with a fake engine — covers each tool, MITL default, traversal
  rejection, lifecycle hooks.
- `config.SandboxDefaults` / `ResolvedSandbox` cases.

## [0.1.6] - 2026-04-28

### Fixed

- **Vertex AI stopped dispatching tool calls.** Memory v2 prepended
  a `[YYYY-MM-DD HH:MM TZ]` marker to every record, including the
  very first user turn. gemini-2.5-flash interpreted that as a
  logged / historical event and described tool actions in prose
  instead of emitting `function_call` parts. The system block
  already injects "now" via temporal context, so the leading marker
  on the first record was redundant; it is now skipped. Tool /
  report records and any record after a >30-minute gap are still
  annotated.
- **`get-location` returned no city.** Asia/Tokyo, Asia/Shanghai,
  Asia/Seoul and Europe/* entries had empty `admin_area` and
  `locality` — the LLM knew the country but had no city to feed
  into the `weather` tool, so it had to fall back to asking the
  user. Locality is now populated with the timezone's namesake
  city; US entries have city in `locality` and state in
  `admin_area`.

  Note: `bundled.Install` only writes files that don't already
  exist in the user's tool dir, so existing installs keep the old
  version. A future bundled-tool version / force-update mechanism
  would let this fix reach all users automatically; for now,
  delete `~/Library/Application Support/shell-agent-v2/tools/
  get-location.sh` to pick up the new version on next launch.

## [0.1.5] - 2026-04-28

### Fixed

- **mcp.Guardian deadlock when initializing a misbehaved binary.**
  `call()` previously held `g.mu` across the blocking `stdout.Scan`,
  so when the StartTimeout fired and `Start` invoked `g.Stop()`,
  Stop deadlocked waiting for the same mutex and the agent hung
  indefinitely. Split into two mutexes (`callMu` / `stateMu`); Stop
  now reliably preempts a blocked call by closing stdin and killing
  the process. Regression test included.

### Tests

- Coverage audit follow-up. New tests across security validation,
  memory v2 build path, LM Studio HTTP/SSE behaviour, mcp guardian
  RPC round-trip, config persistence, bindings object-panel
  operations, memory/pinned/findings disk I/O, and report
  rendering. Per-package coverage now:

  | package          | before | after |
  |------------------|-------:|------:|
  | bindings (main)  |   0.0% | 19.4% |
  | internal/agent   |  41.9% | 48.1% |
  | internal/llm     |   8.1% | 39.4% |
  | internal/mcp     |  11.7% | 79.3% |
  | internal/config  |  55.8% | 90.4% |
  | internal/findings|  66.7% | 94.7% |
  | internal/memory  |  64.2% | 87.2% |

### Docs

- `agent-data-flow.{md,ja.md}` — §4 rewritten to cover both v1
  destructive compaction and v2 non-destructive `contextbuild`
  paths, with the `Memory.UseV2` gate and the v0.1.1 Vertex 400
  fix.
- `object-storage.{md,ja.md}` — §7.4 documents the Objects
  sidebar panel: reference-aware bulk delete, per-row export
  with TypeReport inline expansion, cascade caveats.
- `shell-agent-v2-architecture.{md,ja.md}` — config tree showing
  per-backend `HotTokenLimit` / `ContextBudget` and the
  `Memory.UseV2` flag; bundled-tools embed + auto-install
  section.

## [0.1.4] - 2026-04-28

### Added

- Report objects in the Objects panel can now be previewed in the
  existing fullscreen report viewer. A clickable document icon
  replaces the earlier paragraph mark; clicking it loads the
  markdown via the new `GetObjectText` binding.

## [0.1.3] - 2026-04-28

### Added

- **Memory architecture v2** (opt-in, Settings → General → Memory).
  Records become immutable and full-fidelity; LLM context is built
  per-call from a new `contextbuild` package, sized to the active
  backend's budget, with older portions condensed via cached
  summaries that are content-keyed. Every information channel into
  the prompt — raw records, summaries, pinned, findings — carries a
  temporal marker so the model can reason about *when* each piece
  happened. Existing v1 sessions remain readable; legacy
  `Role=summary` records are surfaced as a "Summarized earlier
  turns" block in the chat instead of being silently filtered.
  Design: `docs/{en,ja}/memory-architecture-v2{.ja,}.md`.
- **Object repository panel** (sidebar → Objects). Lists every
  entry in the central object store with thumbnail or icon,
  metadata, originating session, and per-row Export and Delete.
  Reference-aware delete: scans all sessions for `Record.ObjectIDs`
  and `object:ID` markdown refs; objects still in use require a
  second-click confirmation. Report exports are passed through
  `resolveObjectRefsForExport` so image refs are inlined as data
  URLs in the saved markdown.
- **Bulk select / delete** for Findings and Pinned Memory.
  Per-item checkboxes appear on hover or while the section has any
  selection; section toolbar offers Select all / Delete (two-click
  confirm) / Clear.
- **`file-info` shell tool** — mime type, kind, size, modified,
  line count for text files.
- **`preview-file` shell tool** — head N lines (cap 1000) and bytes
  (cap 64KB) of a text file with non-text MIME refusal.
- **Pinned facts include `(learned YYYY-MM-DD)`** so the model can
  weigh fact recency.
- **Bundled tools auto-install** — default scripts ship inside the
  binary via `go:embed` and are copied to the user's tool dir on
  first launch when missing. User-edited files are never overwritten.

### Changed

- Repository `tools/` directory relocated to
  `app/internal/bundled/tools/` (Go embed must reach the data from
  inside the module tree).

## [0.1.2] - 2026-04-27

### Added

- Per-backend HotTokenLimit and ContextBudget — local and Vertex have very
  different context windows (~16K vs ~1M+); a single global limit forced
  one to over-compact or starve. Settings UI exposes Hot Token Limit /
  Max Context / Max Warm / Max Tool-Result per backend. Existing configs
  with only the legacy top-level fields keep working via inheritance.
- Tool-call timeline in chat — tool starts/ends now appear as inline
  pill entries (running pulse → done check) alongside the existing
  status-bar indicator. Ephemeral; not persisted across session reload.
- Chat input auto-grow (3-row min, 280px max with internal scroll).
- Attach button moved inside textarea bottom-left (Slack/Claude.ai style).

### Fixed

- Memory compaction preserves at least one recent message. A single
  huge tool result (e.g. 278KB MCP response) previously moved every
  hot record into the warm summary, so Vertex AI rejected the request
  with "Error 400: at least one contents field is required".
- Long single-line code blocks no longer collapse the chat-bubble
  layout (min-width: 0 on flex chain; pre overflow-x: auto on reports).
- Chat input area now follows the active theme (was hardcoded dark blue).
- Backend indicator no longer stuck on "..." after slow Wails startup
  (poll until window.go and the agent are ready).
- MCP `--profile` accepts bare profile names again (validation was
  forcing a stat that fails for non-path profile keys).

### Performance

- MessageItem memoized — pushing tool-event entries or streaming
  tokens no longer re-parses the entire ReactMarkdown history.
- Plugin arrays moved to module scope so ReactMarkdown sees stable
  prop references.

### Chore

- Disabled WindowIsTranslucent to drop the macOS private-API warning;
  the vibrancy effect wasn't a deliberate design feature.

## [0.1.1] - 2026-04-27

### Security

- weather.sh: pass region/XML via env vars instead of shell-expanded Python heredoc
  (eliminates code injection if LLM is induced to supply a malicious region string)
- chat: sanitize location string before embedding in system prompt
  (strips control chars and newlines, caps length — blocks prompt-injection via geolocation)
- findings: sanitize stored content/title before embedding in system prompt
- memory/pinned: sanitize fact/native/category before embedding in system prompt
- analysis: validate file paths and escape SQL strings in LoadCSV/LoadJSON/LoadJSONL
  (eliminates SQL injection via filename)
- analysis: enforce MaxQueryRows (10000) on QuerySQL results to bound memory use
- agent: propagate cancellation context into shell tool execution
- agent: validate MCP guardian binary executable bit and profile path before launch
- agent: separate Info/Debug logging — message bodies no longer logged at Info level
- objstore: tighten new-object file permissions to 0600

### Tests

- chat/sanitize_test, findings/sanitize_test, analysis/security_test

## [0.1.0] - 2026-04-27

Initial release. Full rewrite of shell-agent v1.

### Core

- Agent state machine with Idle/Busy execution model
- Tool chaining: tools passed every round (get-location → weather etc.)
- Context budget control: [Calling:] exclusion prevents pattern contamination
- Synchronous compaction before BuildMessages (HotTokenLimit=4096)
- Post-response tasks via async WaitGroup (title, compaction, pinned extraction)

### LLM

- Dual backend: Local (LM Studio) + Vertex AI (Gemini) with runtime switching
- LLM abstraction layer: role mapping, tool format conversion, multimodal
- System prompt language matching (responds in user's language)

### Data Analysis

- Session-scoped DuckDB engine with lazy initialization
- Tools: load-data, query-sql, query-preview, suggest-analysis, quick-summary,
  describe-data, list-tables, reset-analysis, promote-finding
- analyze-data: sliding window analysis with severity-based findings
- Dynamic tool filtering based on data presence
- CSV/JSON/JSONL loading support

### MITL (Man-In-The-Loop)

- query-sql: SQL preview before execution
- analyze-data: analysis plan confirmation
- Shell tools: category-based (read=auto, write/execute=approval)
- MCP tools: default approval required
- Reject + Feedback: user guidance returned to LLM for revision
- Per-tool Enabled/MITL override in Settings

### MCP

- mcp-guardian integration via stdio JSON-RPC 2.0
- Multiple guardian profiles (name, binary, profile_path, enabled)
- Tool names prefixed: mcp__guardianName__toolName
- Path expansion (~/ supported)
- Settings: profile management, status display, restart

### Shell Tools

- Shell script auto-discovery with header-based registration
- Bundled: list-files, weather, write-note, get-location
- Examples: web-search, generate-image

### Object Storage

- Central repository with session affinity (TypeImage, TypeBlob, TypeReport)
- list-objects / get-object LLM tools
- Frontend object:ID URL resolution via ObjectImage component
- Report save: object:ID → base64 data URL expansion

### Memory

- Hot/Warm/Cold compaction with LLM summarization
- Autonomous Pinned Memory extraction (bilingual)
- Global Findings store with origin session provenance
- Cascade delete: session deletion removes findings and objects

### UI

- Sidebar: v1-style icon navigation, collapse/expand, resize (180-500px)
- Settings: tabbed overlay (General/Tools/MCP)
- Tools tab: unified list with Enabled + MITL toggles
- MITL dialog: SQL/analysis preview, feedback input
- Command popup (/help, /model, /findings)
- Chat status bar: backend badge + token counts
- 4 themes: Dark, Light, Warm, Midnight
- Markdown: syntax highlighting, GFM, Math/LaTeX (KaTeX)
- Reports: dedicated container, fullscreen expand, save with image embedding
- Image: drag/drop, paste, lightbox, inline in reports
- IME composition handling, input history, auto-focus

### Testing

- LM Studio agent integration tests (tool chaining, context budget, limits)
- Vertex AI agent integration tests (multi-turn, heavy analysis)
- [Calling:] pattern contamination before/after comparison
- Unit tests across all packages

### Design Documents

- `docs/en/agent-data-flow.md` — agent loop, context budget, MITL, events
- `docs/en/object-storage.md` — central object storage design
- `docs/en/llm-abstraction.md` — LLM backend abstraction layer

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased] - v0.1.20 in progress

Second-round security hardening on top of v0.1.18 / v0.1.19. Phased
into five commits — see [docs/en/security-hardening-2.md](docs/en/security-hardening-2.md)
for the full design and finding inventory.

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
- **`load-data` expands `~/`.** Discovered during v0.1.20
  verification: `filepath.Abs` alone leaves the literal `~`
  in place, so an LLM passing `~/Desktop/foo.csv` (because
  the user typed it that way) would always 404. The validator
  now expands `~` via `config.ExpandPath` before resolving.
  Mirrors what MCP profile paths already do.
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

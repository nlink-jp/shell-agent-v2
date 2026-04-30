# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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

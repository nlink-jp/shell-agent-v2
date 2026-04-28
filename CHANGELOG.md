# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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

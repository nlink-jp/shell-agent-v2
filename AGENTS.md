# AGENTS.md — shell-agent-v2

## Summary

macOS local-first chat & agent tool with interactive data analysis.
Wails v2 (Go + React) desktop application. Redesign of shell-agent v1
with session-scoped DuckDB, Idle/Busy agent model, global Findings,
hybrid LLM backend (Local + Vertex AI), central object storage,
MCP integration, and MITL (Man-In-The-Loop) approval system.

## Build & Test

```bash
cd app
make build      # Build .app bundle → dist/shell-agent-v2.app
make dev        # Wails dev server with hot reload
make test       # go test ./... (add -tags no_duckdb_arrow for CGO builds)
make clean      # Remove build artifacts

# Integration tests (require running services):
go test ./internal/llm/ -tags lmstudio -v    # LM Studio LLM backend tests
go test ./internal/agent/ -tags "lmstudio no_duckdb_arrow" -v -timeout 300s  # Agent loop + tool calling
go test ./internal/agent/ -tags "lmstudio no_duckdb_arrow" -v -timeout 600s -run "Limit|Heavy|Chain"  # Limit/stress tests
VERTEX_PROJECT=xxx go test ./internal/agent/ -tags "vertexai no_duckdb_arrow" -v -timeout 600s  # Vertex AI
```

## Module

`github.com/nlink-jp/shell-agent-v2`

## Key Structure

```
shell-agent-v2/
├── app/
│   ├── main.go              # Entry point, Wails app setup
│   ├── bindings.go          # Wails bindings (thin delegation)
│   ├── internal/
│   │   ├── agent/           # State machine, execution loop, tool dispatch, MCP guardians
│   │   ├── chat/            # Message building, temporal context, context budget
│   │   ├── llm/             # Backend abstraction (Local + Vertex AI)
│   │   │   ├── backend.go   # Role types, Backend interface
│   │   │   ├── local.go     # LM Studio (OpenAI compat, tool→user mapping)
│   │   │   ├── vertex.go    # Vertex AI (genai SDK, FunctionCall/Response)
│   │   │   └── mock.go      # Mock backend for testing
│   │   ├── analysis/        # Session-scoped DuckDB engine, sliding window summarizer
│   │   ├── memory/          # Sessions + GlobalMemory + SessionMemory (v0.2.0 4-facility model)
│   │   ├── findings/        # Per-session data-analysis findings (v0.2.0)
│   │   ├── contextbuild/    # Non-destructive context assembly + summary cache
│   │   ├── objstore/        # Central object repository (image/blob/report)
│   │   ├── sessionio/       # .shellagent bundle pack/unpack + reference rewriter (v0.4.0)
│   │   ├── toolcall/        # Shell script registry, MITL categories
│   │   ├── mcp/             # mcp-guardian stdio JSON-RPC 2.0
│   │   ├── config/          # JSON config, path expansion
│   │   └── logger/          # File-based logging
│   ├── frontend/src/
│   │   ├── App.tsx          # Main UI (sidebar, chat, settings, MITL)
│   │   ├── ChatInput.tsx    # Input with IME, history, image drop
│   │   ├── ObjectImage.tsx  # Lazy object:ID URL resolver
│   │   └── themes.css       # Dark, Light, Warm, Midnight themes
│   ├── build/               # macOS app assets
│   ├── wails.json
│   └── Makefile
├── tools/                   # Shell tool scripts (list-files, weather, write-note, etc.)
│   └── examples/            # Optional tools (web-search, generate-image)
├── docs/
│   ├── en/                  # Design documents (authoritative)
│   │   ├── agent-data-flow.md   # Agent loop, context budget, MITL, events
│   │   ├── object-storage.md    # Central object storage design
│   │   ├── llm-abstraction.md   # Backend role mapping, tool format
│   │   └── shell-agent-v2-rfp.md
│   └── ja/                  # Japanese translations
└── CHANGELOG.md
```

## Architecture Overview

### Agent Loop
- Tools passed every round (enables tool chaining, e.g. get-location → weather)
- No streaming — Chat() used for all rounds (tool chaining precludes knowing final round)
- [Calling:] messages excluded from LLM context (prevents gemma pattern contamination)
- Post-response tasks (title generation, memory extraction) via async WaitGroup

### Context Budget Control (v0.2.0: non-destructive only)
- Per-backend `ContextBudget` (`Local`, `VertexAI`) resolved by `cfg.ContextBudgetFor(backend)` so the same session adapts to the active model's window.
- Records stay immutable; `internal/contextbuild` builds the LLM message list per call. Older portions condense via a content-keyed summary cache at `sessions/<id>/summaries.json`. Time-range markers are added to summaries, raw records (after a >30-min gap or for tool/report rows), Global Memory entries (`(learned …)`), Session Memory entries, and Findings.
- The v0.1.x destructive Hot→Warm compaction path was removed in v0.2.0 along with `Tier` / `HotTokenLimit` / `Memory.UseV2`.

### MITL (Man-In-The-Loop)
- Shell tools: category-based (read=auto, write/execute=MITL)
- MCP tools: default MITL=on (external service operations)
- query-sql: SQL preview before execution
- analyze-data: analysis plan confirmation
- Per-tool MITL override in config (DisabledTools + MITLOverrides)
- Reject + Feedback: user feedback returned to LLM for revision

### Events (Backend → Frontend)
- `agent:stream` — token streaming (Vertex AI only, disabled for local)
- `agent:activity` — tool_start/tool_end/thinking (unified activity)
- `session:title` — auto-generated session title
- `global_memory:updated` — Global Memory (cross-session pool) changes
- `session_memory:updated` — Session Memory (per-session pool) changes
- `findings:updated` — per-session Findings store changes
- `report:created` — report content for display
- `mitl:request` — MITL approval dialog

### MCP Integration
- Multiple guardian profiles (name, binary, profile_path, enabled)
- Tool names prefixed: `mcp__guardianName__toolName`
- Guardian lifecycle: start on app launch, restart from Settings
- Path expansion (~/ supported)

### Sandbox (opt-in, v0.1.7+)
- Per-session container managed via `podman` or `docker`, mounting
  `sessions/<id>/work/` at `/work`. Engine selected by Settings →
  Sandbox (auto / podman / docker).
- Eight tools, all prefixed `sandbox-`: `run-shell`, `run-python`,
  `write-file` (LLM → /work), `copy-object` (objstore → /work),
  `register-object` (/work → objstore), `info` (engine, image,
  Python version, pip list, /work listing), `load-into-analysis`
  (/work CSV/JSON → DuckDB), `export-sql` (analysis SELECT → /work
  CSV). All MITL by default.
- Lifecycle: lazy `EnsureContainer` on first tool use (auto-pulls
  the image if missing), `Stop(sessionID)` on session delete,
  `StopAll` on app shutdown. `RestartSandbox` reloads config
  without an app restart so Settings changes take effect live.
- `safeWorkPath` rejects absolute paths and `..` traversal for
  the file-touching tools.

### Bundled Shell Tools
- Source lives under `app/internal/bundled/tools/` and is
  embedded via `go:embed`. `bundled.Install` copies any missing
  file into `cfg.Tools.ScriptDir` on startup so new bundled
  scripts ship to existing users automatically; user-edited
  files are never overwritten. `examples/` is intentionally
  excluded.
- Defaults: `file-info`, `preview-file`, `list-files`, `weather`,
  `get-location`, `write-note`.
- Each script declares its execution timeout via `@timeout: N`
  (positive integer of seconds). Default is 30 if omitted; bundled
  scripts spell out `30` for discoverability and `examples/web-search`
  + `examples/generate-image` use `120` because `gem-search` /
  `gem-image` round-trips routinely exceed 30s. See
  [docs/en/history/tool-execution-timeout.md](docs/en/history/tool-execution-timeout.md).
- Scripts can write artefacts to `$SHELL_AGENT_WORK_DIR` (the host
  path of the per-session work directory; same physical location
  the sandbox bind-mounts at `/work`). Files there appear in the
  Data → /work panel and can be promoted to objstore via the
  built-in `register-object` tool. See
  [docs/en/history/work-dir-shell-bridge.md](docs/en/history/work-dir-shell-bridge.md).

### UI
- Sidebar: icon navigation with two panels — **Sessions**
  (session list) and **Memory** (Global Memory + Session Memory
  sections, v0.2.0). Collapsible / resizable.
- Chat-pane top: collapsible **Data** disclosure scoped to the
  selected session, with three sub-sections — Objects (card
  grid, image thumbnails / typed icons, hover-revealed export +
  delete with separate Yes / No confirm overlay), Tables
  (row-list, click → 20-row preview modal), and `/work` (light
  card grid; only when sandbox is on).
- Chat-pane: **Findings** disclosure (v0.2.0 Phase 8) — severity
  filter, search, bulk delete, real-time refresh, Pin-to-Global-
  Memory ★ button per row. Replaces the old sidebar Findings
  section.
- Chat-pane bottom: status footer strip — backend badge,
  message counts, prompt / output token totals from the last
  call. Wraps to two lines on narrow windows.
- Settings: tabbed (General / Tools / MCP) near-fullscreen overlay.
  General has Sandbox (Enabled, engine, image, network, limits)
  and per-backend budget editors. The v0.1.x "Use v2 context
  builder" toggle was removed in v0.2.0.
- Tools tab: unified list with Enabled + MITL toggles per tool;
  sandbox-* tools surface here when the engine is up.
- Tool-call timeline: every tool start/end appears inline in chat
  as a transient pill, in addition to the status-bar indicator.
- Bulk select / delete for Findings, Global Memory, Session Memory.
- Pin to Global Memory dialog (v0.2.0 Phase 9): category picker
  shown when promoting a Session Memory entry or a Finding into
  the cross-session pool.
- Commands (/help, /model): popup panel, not chat messages.
  /finding and /findings were removed in v0.2.0.
- MITL dialog: SQL preview, analysis plan, feedback input.

## Design Documents

All implementation must follow these design documents.

**Current top-level (post-v0.2.0):**
- **docs/en/architecture.md** — system-wide architecture, package layout, state machine (now gates Send/Load/Export/Import/Delete), events (now incl. tool_progress), storage (now incl. .shellagent bundle layout). The canonical reference.
- **docs/en/memory-model.md** — 4-facility design + Session.Private flag (v0.3.0).
- **docs/en/data-analysis.md** — DuckDB engine, sliding-window analyze-data, Findings lifecycle.

**Recent design notes (v0.3.0+):**
- **docs/en/privacy-controls.md** — private sessions, log-level filter, audit log entries
- **docs/en/session-import-export.md** — `.shellagent` bundle format, ID regeneration on import, race-condition catalogue
- **docs/en/tool-progress-events.md** — `tool_progress` activity event for in-place bubble updates
- **docs/en/session-delete-ux.md** — 2-click confirm + per-row Deleting state + state-machine integration

**History (audit trail behind v0.2.0):**
- **agent-data-flow.md** — agent loop, context budget, MITL, events, tool confirmation
- **memory-architecture-v2.md** — non-destructive contextbuild, summary cache, time markers across every channel
- **object-storage.md** — content-addressed object store (physical layout), lifecycle, LLM tools. UI surface lives in the chat-pane Data disclosure since the information-display redesign.
- **information-display-redesign.md** — sidebar / chat-pane / footer reorganisation that retired the standalone Objects panel and the mixed-scope Status panel.
- **sandbox-execution.md** — per-session container sandbox, eight `sandbox-*` tools, macOS prerequisites
- **llm-abstraction.md** — backend role mapping, tool format conversion, multimodal
- **shell-agent-v2-architecture.md** — top-level architecture, per-backend budget tree, bundled tool embed

## Environment

- Config: `~/Library/Application Support/shell-agent-v2/config.json`
- Sessions: `~/Library/Application Support/shell-agent-v2/sessions/{id}/`
- Objects: `~/Library/Application Support/shell-agent-v2/objects/`
- Tools: `~/Library/Application Support/shell-agent-v2/tools/`
- Log: `~/Library/Application Support/shell-agent-v2/app.log`

## Gotchas

- DuckDB requires CGO — use `-tags no_duckdb_arrow` for builds
- `wails build` outputs to `build/bin/`, Makefile copies to `dist/`
- Local backend maps tool→user role (gemma-4 workaround)
- Vertex AI uses native FunctionCall/FunctionResponse
- Agent loop uses Chat() for all rounds (no streaming — tool chaining requires it)
- [Calling:] messages stored in session but excluded from LLM context
- BuildMessages passes application-level roles — backends map internally
- Reports have dedicated UI (report-container), not regular chat bubbles
- MCP paths support ~ expansion via config.ExpandPath()
- Tool chaining verified: gemma-4 does not loop with tools always available
- MCP guardian stderr is drained into the app log so a noisy guardian can't deadlock the parent's stdout scan (security-hardening-2.md C2)
- MCP `call()` validates the response `id` matches the request `id` — a mismatched response is rejected as a transport error (H4)
- Sandbox `Exec` caps each of stdout/stderr at `Sandbox.MaxOutputBytes` (default 8 MiB); excess is dropped with a trailing marker (C3)
- Local-backend `Chat` / `ChatStream` reject response bodies above 16 MiB (`MaxLocalResponseBytes`) (H12)
- `analysis.refreshTableMeta` uses parameter binding for the `duckdb_tables()` lookup — never string-concatenate LLM-supplied table names into SQL (C1)
- Analysis tools route through `IsToolMITLRequired` like every other source — `analysisToolMITLDefault` (`tools.go`) is the single source of truth for per-tool defaults; `executeAnalysisTool` no longer does its own MITL gating (security-hardening-2.md H1+H2)
- MCP tool name parsing uses `splitMCPName` with longest-prefix fallback for the rare guardian / tool name containing `__`. Guardian names must match `validGuardianName` (`^[a-zA-Z0-9-]+$`) at startup (H3)
- JSON stores on the data path go through `internal/atomicio.WriteFileAtomic` (tmp+rename + parent-dir fsync) so a crash mid-save leaves the previous file intact. Applies to objstore index, per-session findings, per-session session_memory, global_memory, summaries cache, and per-session chat.json (security-hardening-2.md C4 / H10)
- `findings.Store.Add` is mutex-protected; ID generation reads the per-day count under the same lock so concurrent Adds can't collide (H9). The >999-per-day overflow uses a 6-hex random suffix.
- Settings → Sandbox surfaces a mutable-tag warning banner via `SandboxImageStatus.ActivePinnedByDigest` / `imagebuild.IsImageDigestPinned` — locally-built `<TagPrefix>:<sha>` and `@sha256:` upstream pins are safe (security-hardening-2.md H5)
- `llm.validateToolCallArgs` caps `ToolCall.Arguments` at 1 MiB (configurable via `LocalConfig.MaxToolCallArgsBytes` / `VertexAIConfig.MaxToolCallArgsBytes`) and requires valid JSON (H6)
- `objstore.generateID` produces 16-byte (32 hex) IDs; legacy 12-hex IDs continue to load via the length-tolerant read path (H11)
- `analysis.validateFilePath` uses `os.Lstat` and rejects symlinks outright — applies to `load-data` and any other host-path entry point (H14)
- `guard.Wrap` is fail-closed: `chat.BuildMessages` / `BuildMessagesWithBudget` / `WrapUserToolContent` and `contextbuild.Build` return an error rather than silently falling back to unwrapped content. The agent loop surfaces the error to the user (security-hardening-2.md L1)
- Analysis tools are exposed every round regardless of `hasData` since v0.1.21 (LLM can plan load-then-query workflows up front). Legacy hide-until-data-loaded behaviour is preserved behind `cfg.Tools.HideAnalysisToolsUntilDataLoaded` for users on weaker local backends. See `docs/en/history/agent-tool-visibility.md`.
- `extractMemories` (v0.2.0, was `extractPinnedMemories`) rejects self-referential facts (`memory.IsSelfReferential`) and unknown categories (`memory.ValidExtractionCategories`), wraps both the conversation tail and the existing-facts list with `nlk/guard` so the extraction LLM treats them as data, and routes by category: `preference` / `decision` → GlobalMemoryStore, `fact` / `context` → SessionMemoryStore. The window walks back past tool records to keep at least 4 user/assistant turns in scope. `findings.Add` runs a 3-tier dedup (exact / normalised / Jaccard ≥ 0.5) and takes a `source` argument (`SourceLLMPromoted` for `promote-finding`, `SourceAnalyzeData` for the analyze-data auto-promote). `FormatForPrompt` for all three stores prefixes lines with `[user-stated]` (high trust) or `[derived]` (lower trust — content traces through the LLM and may carry attacker-influenced bytes). Retention caps via `MemoryConfig.MaxPinnedFacts` (default 100, applies to GlobalMemory) and per-session `MaxFindings` / `MaxSessionMemory` (defaults 100 / 50) prevent unbounded store growth (FIFO eviction). See `docs/en/memory-model.md` (v0.2.0) and `docs/en/history/memory-injection-hardening.md` (v0.1.26).

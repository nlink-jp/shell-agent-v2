# shell-agent-v2 Architecture

This document is the canonical wider-system reference. It describes
**how the system fits together right now** — package boundaries,
state machines, dispatch flows. The bones were laid down at v0.2.0;
subsequent releases (v0.3.0 through v0.6.2) added cross-cutting
features that are described in-line below as the corresponding
subsystems evolved.

The **why** behind those changes — design rationale, rejected
alternatives, threat models for individual decisions — lives in the
ADR catalogue: see [`../INDEX.md`](../INDEX.md) for the indexed list.

Companion reference documents in this directory:

- [`memory-model.md`](memory-model.md) — canonical reference for
  the 4-facility memory design.
- [`data-analysis.md`](data-analysis.md) — the per-session DuckDB
  engine, the sliding-window analyze-data summarizer, and the
  Findings lifecycle.
- [`privacy-controls.md`](privacy-controls.md) — private sessions,
  log-level filter, audit entries.

For the pre-v0.2.0 audit trail see [`../history/`](../history/).

## 1. What it is

shell-agent-v2 is a macOS-native chat-and-agent application built
with [Wails v2](https://wails.io) (Go backend + React + TypeScript
frontend). It hosts a single user conversation against either a
local LLM (LM Studio, OpenAI-compatible) or Vertex AI (Gemini),
with first-class support for:

- **Data analysis** — load CSV / JSON / JSONL into a per-session
  DuckDB engine and let the agent run SELECTs, sliding-window
  summaries, and promote findings to the chat-pane Findings
  panel.
- **Shell-script tool calling** — register scripts as tools with
  per-tool MITL gates, headers for category / timeout / mitl
  hints, and a shared `/work` directory.
- **Container sandbox (opt-in)** — Python / shell execution in a
  per-session `podman` / `docker` container.
- **MCP (Model Context Protocol)** — stdio JSON-RPC 2.0 via the
  external `mcp-guardian` binary.
- **Cross-session memory** — Global Memory (preferences /
  decisions across sessions) plus per-session Session Memory and
  Findings; explicit user-controlled "Pin to Global Memory"
  promotion. See [`memory-model.md`](memory-model.md).

The project is a successor to shell-agent v1 (a Slack-driven
agent) but shares no code. v1's heuristics about Hot/Warm/Cold
memory tiers, /finding slash commands, and the global Pinned
store have all been replaced — see the v0.2.0 entry in
[`../../CHANGELOG.md`](../../CHANGELOG.md) for the breaking-change
summary.

## 2. Process model

### Idle / Busy state machine

The agent occupies the active session exclusively while it works:

- **Idle** — accepts user input. May still have an in-flight
  memory extraction (`a.extractionInFlight == true`); see below.
- **Busy** — agentLoop in progress. Input field disabled, session
  switch requires explicit abort.

**v0.11.0 deferred extraction** (ADR-0015): memory extraction no
longer gates the Busy→Idle transition. Title generation still
does, but it's first-turn-only and fast. While extraction runs in
the background:

- `state == Idle` — the input bar re-enables so the user can
  compose the next message.
- `a.extractionInFlight == true` — visible via `IsExtractionInFlight()`.
- A SEND received during this window lands in `a.queuedSend`
  (single-slot, most-recent-wins) and auto-fires when extraction
  completes. The frontend surfaces this as a "Queued" pill above
  the input bar.
- Session-management operations (`LoadSession`, `DeleteSession`,
  `ExportSession`, `ImportSession`, `RenameSession`, `NewSession`)
  remain blocked through the extraction window because the
  extraction goroutine continues to register via `trackBg` and the
  existing frontend gate checks `bgTasks.length === 0`. This
  prevents the extraction from writing into a session it no
  longer owns.

State transitions are owned by `internal/agent.Agent` and surfaced
to the frontend via the `agent:state` Wails event plus the
`GetState` binding. The Busy guard is also enforced backend-side:
binding entry-points that would mutate session state during Busy
return an error rather than queuing.

`Bindings.IsBusy()` (used by `OnBeforeClose` in `main.go` to
gate app quit) returns true for `state == Busy`, for
`IsExtractionInFlight()`, and for `HasQueuedSend()`. Quitting
mid-extraction could lose facts mid-write, so the OS confirmation
fires until extraction completes.

**Operations gated by the Busy guard** (by introduction date):

- v0.2.0: `Send` / `SendWithImages`, `LoadSession`
- v0.4.0: `ExportSession`, `ImportSession`
- v0.4.2: `DeleteSession` — previously bound directly in the
  binding layer with only an entry-time `IsBusy()` check; now
  routed through `agent.DeleteSession` which holds the slot for
  the operation's full duration. See
  [`ADR-0003`](../adr/0003-session-delete-ux.md) §2 for the
  failure modes the looser pre-v0.4.2 path allowed (active-
  session-deleted Send racing the dir RemoveAll, etc.).

Active-session deletes additionally nil-clear `a.session`,
`a.sessionMemory`, `a.findings` and `Close()` the analysis Engine
before `RemoveAll` runs, so a stray Save / Engine call cannot
resurrect the session directory.

### Agent loop

`Agent.Send(ctx, message)` runs a synchronous tool-calling loop:

```
buildMessages → backend.Chat → parse tool_calls
  ↓ if no tool calls
  return reply
  ↓ if tool calls
  for each call: dispatch → record result
  ↓ next round (max 10)
```

Hard cap: `cfg.MaxToolRounds` (default 10). Loop-detection logic
(repeated same-error stretches) trips earlier — see
`../history/agent-loop-resilience.md`.

The loop is **not** ReAct: tool results feed back into the next
round verbatim, with no separate "Observation/Thought" framing.
This keeps it compatible with weaker local LLMs that don't
reliably follow the ReAct grammar.

### Post-response background tasks

After the loop returns a reply, `Agent` kicks off two async
WaitGroup tasks (state stays Busy until both complete):

- **Title generation** (only on the first user turn of a session)
- **Memory extraction** — see §4.

Both surface as `bg-task:*` Wails events so the input bar can
show a per-task badge.

## 3. Package layout (Go)

```
app/
├── main.go                  # Wails App config + lifecycle
├── bindings.go              # Wails binding surface (thin delegation)
├── internal/
│   ├── agent/               # Idle/Busy state machine, tool dispatch, extractMemories
│   ├── chat/                # System prompt assembly, BuildMessages, temporal context
│   ├── llm/                 # Backend abstraction
│   │   ├── backend.go         # Backend interface, Message / ToolCall types
│   │   ├── local.go           # LM Studio (OpenAI compat, tool→user mapping)
│   │   ├── vertex.go          # Vertex AI (genai SDK, FunctionCall/Response)
│   │   └── mock.go            # Mock backend for testing
│   ├── analysis/            # Per-session DuckDB engine + sliding-window summarizer
│   ├── memory/              # Records, GlobalMemoryStore, SessionMemoryStore, sessions
│   ├── findings/            # Per-session findings store + Jaccard dedup
│   ├── contextbuild/        # Non-destructive context assembly + summary cache
│   ├── objstore/            # Central object repository (image/blob/report; 32-hex IDs)
│   ├── sessionio/           # .shellagent bundle pack/unpack + reference rewriter (v0.4.0)
│   ├── toolcall/            # Shell script registry, header parsing, MITL categories
│   ├── mcp/                 # mcp-guardian stdio JSON-RPC 2.0 client
│   ├── sandbox/             # Per-session podman/docker container
│   │   └── imagebuild/        # Sandbox image build + digest pinning checks
│   ├── bundled/             # First-run scaffold of bundled shell-tool scripts
│   ├── pathfix/             # macOS app-launch PATH normalisation (Homebrew)
│   ├── atomicio/            # WriteFileAtomic (tmp+rename + parent-dir fsync)
│   ├── config/              # JSON config, path expansion
│   ├── logger/              # File-based structured logging
│   └── frontendlint/        # CI guard against forbidden CSS / JS patterns
├── frontend/src/            # React + TypeScript UI
└── cmd/                     # Test helper binaries (tooltest-local, tooltest-vertex)
```

## 4. Memory model (summary)

Four facilities, three storage scopes, no v1 compaction:

| Facility | Categories | Scope | File |
|----------|-----------|-------|------|
| Records | — | per-session | `sessions/<id>/chat.json` |
| Session Memory | fact / context | per-session | `sessions/<id>/session_memory.json` |
| Findings | (data-analysis discoveries) | per-session | `sessions/<id>/findings.json` |
| Global Memory | preference / decision | cross-session | `<dataDir>/global_memory.json` |

**v0.3.0: privacy flag on `Session`.** A `Session.Private bool`
(persisted in `chat.json` with `omitempty` for legacy compat)
opts the session out of cross-session promotion: extraction
drops `preference` / `decision` facts, the Pin handlers
(`PromoteSessionMemoryToGlobal`, `PromoteFindingToGlobal`)
reject server-side, and the frontend hides the ★ Pin UI plus
shows a 🔒 indicator. Privacy is fixed at session creation.
Full design: [`privacy-controls.md`](privacy-controls.md).

Auto-extraction (`agent.extractMemories`) runs after each response,
takes the last 4 user/assistant turns (skipping past tool records
to avoid the tool-flood blind spot), asks the extraction LLM for
`category|turn-N|fact|native` lines, then routes by category:

- `preference` / `decision` → GlobalMemoryStore
- `fact` / `context` → SessionMemoryStore

Findings come from two paths and dedup at insert:

- **Auto-promote** from `analyze-data` sliding-window analysis.
- **Explicit** from `promote-finding` tool calls.

3-tier dedup (exact / normalised / word-set Jaccard ≥ 0.5) keeps
the same observation in slightly different wording from filling
the store.

User can promote a Session Memory entry or a Finding into Global
Memory via the **Pin to Global Memory** UI (sidebar ★ button
or chat-pane panel ★ button → category-picker dialog). The
original entry stays in place; promotion is additive.

Context is built non-destructively per call by `contextbuild`:
records stay full-fidelity, older portions condense via a
content-keyed summary cache at `sessions/<id>/summaries.json`.
Time-range markers are added to summaries and to raw records
after a >30-min gap so the LLM can reason about *when* events
happened.

Full design + threat model: [`memory-model.md`](memory-model.md).

## 5. Tool system

All tools — analysis built-ins, shell scripts, sandbox
counterparts, MCP — flow through one dispatcher
(`agent.executeTool`) and one MITL gate
(`agent.IsToolMITLRequired`). Since v0.6 the analysis +
builtin + sandbox sources additionally share a single
metadata source: `ToolDescriptor` (per-Agent
`a.toolDescriptors` slice, indexed by name in
`a.toolDescriptorIndex`).

### Sources

- **Builtin tools** (`internal/agent/tool_descriptors_builtin.go`):
  `resolve-date`, `list-objects`, `get-object`, `register-object`.
  Don't depend on the analysis engine.
- **Analysis tools** (`internal/agent/tool_descriptors_analysis.go`):
  `load-data`, `describe-data`, `query-sql`, `query-preview`,
  `quick-summary`, `analyze-data`, `promote-finding`,
  `create-report`, `list-tables`, `suggest-analysis`,
  `reset-analysis`, `analyze-text`, `grep-text`, `get-text`.
  Filtered out of the LLM tool list when `a.analysis == nil`;
  data-gated subset hidden when legacy mode is on and no table
  is loaded.
- **Sandbox tools** (`internal/agent/tool_descriptors_sandbox.go`):
  eight `sandbox-*` tools that execute in a per-session
  container with `/work` mounted from the session data dir.
  Filtered out of the LLM tool list and Settings UI when
  `a.sandbox == nil` (the engine's lifecycle is dynamic via
  `RestartSandbox`).
- **Shell scripts** (`internal/toolcall/`): user-registered
  scripts with header-driven metadata. Bundled scripts
  (`file-info`, `preview-file`, `list-files`, `weather`,
  `get-location`, `write-note`) are scaffolded on first launch
  via `go:embed`. Not in the descriptor registry — registered
  separately from the toolcall.Registry and joined into the LLM
  tool list at `buildToolDefs` time.
- **MCP tools** — discovered at startup from the configured
  guardian profiles and namespaced as `<guardian>__<tool>`. Not
  in the descriptor registry — joined dynamically at
  `buildToolDefs` time.

### Descriptor registry

Each `ToolDescriptor` carries Name, Description, Parameters
(JSON Schema), Category (`read`/`write`/`execute`), Source
(`analysis`/`builtin`/`sandbox`), MITLDefault,
MITLCategoryOverride (for the specialised SQL-preview /
analysis-plan dialogs), HideUntilDataLoaded (legacy hide-
until-table-loaded gate), and Handle (the closure that
captures `*Agent` and dispatches to the underlying tool
method). The same descriptor backs the LLM tool def
(`descriptorToolDefs`), the Settings → Tools entry
(`ListTools`), the MITL default (`IsToolMITLRequired` /
`toolMITLDefault`), and the dispatch
(`dispatchDescriptor`) — adding a new analysis / builtin /
sandbox tool requires editing exactly one file.

The pre-v0.6 design maintained those four surfaces as
parallel lists; the v0.5.0 → v0.5.1 manual smoke caught two
drift bugs (Settings tab missing a tool, stale MITL map
entry). Structural tests in `tool_descriptor_structural_test.go`
now enforce the invariants mechanically. Full design rationale
is in [`ADR-0007`](../adr/0007-tool-registry-refactor.md).

### MITL (Man-In-The-Loop) gate

Each tool has a default — read = auto-allow, write/execute =
require approval. Per-tool override via `MITLOverrides` in
config. For descriptor-registered tools the default comes
straight from `descriptor.MITLDefault`; for shell scripts it
comes from `Tool.NeedsMITL()`; the prefix branches in
`IsToolMITLRequired` (mcp__ / sandbox-) act as defense in
depth so a missing descriptor cannot accidentally grant a
zero-friction sandbox call. The Settings → Tools UI reads
from the same registry the dispatcher does, so the toggle
state and the actual gate cannot drift.

Special MITL flows surfaced via `descriptor.MITLCategoryOverride`:

- `query-sql` → SQL preview dialog before execution
  (`MITLCategoryOverride = "sql_preview"`).
- `analyze-data` → analysis-plan confirmation dialog
  (`MITLCategoryOverride = "analysis_plan"`).

A reject can include free-text feedback that's piped back to the
LLM as a tool result so it can revise the call.

## 6. Storage layout

```
~/Library/Application Support/shell-agent-v2/
├── config.json                       # User settings (LLM, sandbox, theme, …)
├── global_memory.json                # Cross-session Global Memory (v0.2.0)
├── pinned.json                       # Legacy v0.1.x; ignored on launch
├── findings.json                     # Legacy v0.1.x; ignored on launch
├── objects/
│   ├── index.json                    # Object metadata + session attribution
│   └── <id-prefix>/<id>              # Object content (images / blobs / reports)
├── sessions/<session-id>/
│   ├── chat.json                     # Records (the conversation transcript)
│   ├── session_memory.json           # Session Memory entries (fact / context)
│   ├── findings.json                 # Findings (data-analysis discoveries)
│   ├── summaries.json                # contextbuild content-keyed summary cache
│   ├── analysis.duckdb               # Session-scoped DuckDB
│   └── work/                         # Sandbox /work mount (also $SHELL_AGENT_WORK_DIR)
└── tools/                            # User shell scripts (bundled + custom)
```

All JSON files on this path go through
`internal/atomicio.WriteFileAtomic` (tmp file → rename + parent-dir
fsync) so a crash mid-save leaves the previous file intact.

Session deletion removes the entire `sessions/<id>/` directory
plus the session's owned objstore objects. As of v0.4.2 this is
orchestrated by `agent.DeleteSession` (not the binding layer
directly), running under the agent state-machine Busy slot —
see §2 for why. Global Memory is unaffected.

**v0.4.0: `.shellagent` bundle import / export.** A session can
be packaged into a single ZIP bundle (`internal/sessionio`) that
carries `chat.json`, `session_memory.json`, `findings.json`,
`summaries.json`, `analysis.duckdb`, the recursive `work/`
subtree, and an `objects/` subdirectory with the session's
objstore blobs and metadata. The bundle is portable across
machines (DuckDB's binary format is cross-platform). On import
the new session gets a fresh sess-id and every objstore object
is re-stored with a fresh ID; references in `chat.json`
(`Record.ObjectIDs[]` and `object:ID` markdown in
`Record.Content`) and `summaries.json` (`SummaryEntry.Summary`)
are rewritten through the same `agent.Mu`-gated state-machine
slot. Privacy flag is preserved verbatim. Full design:
[`ADR-0001`](../adr/0001-session-import-export.md).

### Object reference conventions

Objects are referenced in two directions — into the LLM's input
and out of the LLM's output. The two surfaces use distinct
shapes; collapsing them is the bug ADR-0014 codified out of the
codebase. The rules:

1. **`Image (object ID: ID):`** — input-only anchor for
   user-attached images, prepended to the user-message text by
   `llm.imageIDPrefix` so the model sees the ID adjacent to the
   image part. The LLM must never emit this form in its own
   output — it does not render as an image. To cite or surface
   an image in chat or in a report, use rule 3 below.
2. **`Document (object ID: ID, name: NAME, N tokens):`** —
   input-only anchor for user-attached markdown / text,
   prepended to a user-message text by
   `llm.PrependDocumentAnchors`
   (`internal/contextbuild/render.go:85`). Like rule 1, this is
   an INPUT shape only; cite a document in output via rule 4.
3. **`![alt](object:ID)`** — LLM output for inline images.
   Rendered as `<ObjectImage>` (data URL → `<img>` + lightbox).
   If the ID resolves to a non-image type the renderer
   degrades to the chip from rule 4 — no broken-image glyph.
4. **`[title](object:ID)`** — LLM output for inline
   document / report / blob references. Rendered as a clickable
   chip via the `ObjectLink` component. Click dispatches to
   `GetObjectText` → `ReportViewer` for markdown / report, or
   `ExportObject` save-as for blob. If the ID resolves to an
   image type the renderer degrades to inline `<ObjectImage>` —
   no dead anchor.
5. **Reports do not gain document anchors retroactively.** A
   `Role: "report"` record carries the full report body inline
   (`tools.go:374-382` pending-report append). Anchors are a
   surrogate for content the LLM cannot see in the message
   text — reports are visible, so they need no surrogate. No
   code path adds `DocumentIDs` to a `Role: "report"` record,
   and no future path should.

Rules 1 / 2 are produced by `internal/llm.imageIDPrefix` and
`internal/llm.PrependDocumentAnchors`. Rules 3 / 4 are honoured
by `frontend/src/markdown/objectMarkdown.tsx` (`objectComponents`
factory) and its companion `ObjectLink` /  `ObjectImage`
components. The export resolver
(`bindings.go:resolveObjectRefsForExport`) is type-aware to match:
images are inlined as `data:` URLs for self-contained exports,
non-images keep their `object:` href so re-import resolves
cleanly. Full design:
[`ADR-0014`](../adr/0014-object-link-rendering.md).

## 7. LLM backends

`internal/llm.Backend` is the common interface:

```go
type Backend interface {
    Name() string
    Chat(ctx, messages, tools) (Response, error)
}
```

Implementations:

- **`local.go`** — LM Studio's OpenAI-compatible REST API. Tool
  calls map onto OpenAI's `function_call` shape; returned
  `tool` messages are folded into a synthesised `user` turn
  (`<TOOL_RESULT name=…>…</TOOL_RESULT>`) because some local
  models drop the dedicated role.
- **`vertex.go`** — Vertex AI Gemini via the `google.golang.org/genai`
  SDK. Tool calls use Gemini's `FunctionCall` / `FunctionResponse`
  parts; responses can be streaming (parsed token-by-token).
- **`mock.go`** — deterministic mock for tests.

Backend swap is runtime: `/model local` / `/model vertex`. The
active backend is read once per agent loop round, so a swap
mid-conversation takes effect on the next user turn.

Per-backend config (`config.LocalConfig` / `VertexAIConfig`) holds
endpoints, model names, retry policy, request timeout, max tool-call
args size, and `ContextBudget`. Resolved by
`cfg.ContextBudgetFor(backend)` so the same session adapts to the
active model's window.

### Multi-profile resolution (v0.12.0, ADR-0016)

The `(Local, VertexAI)` pair is one *profile* among many in
`config.json`'s `llm.profiles[]`. Each session references one
profile via a per-session `session.json` file alongside
`chat.json`:

```
sessions/<id>/
├── chat.json        # transcript (records, title, private)
└── session.json     # {schema_version, profile_id}  ← v0.12.0+
```

`agent.currentProfile()` resolves the session's profile (falling
back to the default profile when no session is loaded, or the
recorded profile_id was deleted), and `setBackend` sources the
Local / VertexAI config from that profile rather than the
top-level config. `/model` toggles the active backend *within*
the session's profile; new in v0.12.0 is `/profile <name>` to
switch the session's profile binding (atomically rewrites
`session.json`; emits `agent:profile:changed`).

Backwards compat: a v0.11.x `config.json` is migrated on first
v0.12.0 load by synthesising one profile named "Default" from
the legacy fields. A v0.11.x session without `session.json` is
treated as binding to the default profile and gets a fresh
`session.json` lazy-written on first load.

## 8. Frontend architecture

React + TypeScript, single-page, no router. Wails generates the JS
shim for `window.go.main.Bindings`; `frontend/src/bindings.ts`
declares the TypeScript view of that shim.

Top-level structure:

```
App.tsx
├── Sidebar (sessions / memory tabs)
│   ├── Sessions list
│   └── Memory tab
│       ├── Global Memory section (bulk select / delete)
│       └── Session Memory section (bulk select / Pin button)
├── DataDisclosure (chat-pane top, per-session)
│   ├── Objects card grid
│   ├── Tables list
│   └── /work files (sandbox-only)
├── FindingsDisclosure (chat-pane, per-session)
│   ├── Severity filter + search
│   ├── Bulk delete
│   └── Per-row Pin / Delete
├── Messages stream (with MessageItem renderer)
├── ChatInput (with image attach, history navigation)
├── Status footer (backend, tokens, bg-tasks)
└── Dialogs
    ├── SettingsDialog (General / Tools / MCP / Sandbox tabs)
    ├── MITLDialog (approval prompts)
    ├── PinToGlobalDialog (category picker)
    ├── Lightbox / ReportViewer / BlobPreview
```

### Wails events (backend → frontend)

| Event | Trigger |
|-------|---------|
| `agent:stream` | Token stream (Vertex AI) |
| `agent:activity` | tool_start / tool_end / tool_progress / thinking / assistant_text |
| `agent:state` | Idle / Busy transitions |
| `session:title` | Auto-generated session title |
| `global_memory:updated` | Global Memory store changed |
| `session_memory:updated` | Session Memory store changed |
| `findings:updated` | Findings store changed |
| `report:created` | New report bubble |
| `mitl:request` | MITL approval needed |
| `bg-task:start` / `bg-task:end` | Background task lifecycle |

**v0.4.1: `tool_progress` event.** Long-running tools (currently
`analyze-data`'s sliding-window summarizer) emit `tool_progress`
ActivityEvents carrying the parent tool's `tool_call_id` plus an
updated display string. The frontend matches by id (not by
content text) and overwrites the running bubble's content
in-place — one bubble that updates from "analyze-data" →
"analyze-data — window 1/3" → … → reverts to "analyze-data" →
status flips on `tool_end`. Replaces the pre-v0.4.1 behaviour
where each window emitted its own `tool_start` with no matching
`tool_end`, leaving N permanent "running" pills (issue #5).
Full design: [`ADR-0002`](../adr/0002-tool-progress-events.md).

### Theming

`themes.css` defines four themes (dark / light / warm / midnight)
as CSS custom properties. Surface-level tokens (`--bg-primary`,
`--bg-sidebar`, `--bg-input`) are opaque rgb; layered tokens
(`--bg-msg-*`, `--bg-input-field`, `--bg-hover`) keep rgba alpha
for tinting. WebView itself is opaque
(`main.go: WebviewIsTransparent: false`); window-level
translucency would need private macOS APIs and was traded away
for readable code blocks.

## 9. Security posture

shell-agent-v2 is a single-user local-first app, but it processes
attacker-controlled bytes from many sources (CSV cells, MCP
responses, OCR'd image text, fetched web pages). The threat model
is mostly **prompt-injection through tool output** rather than
network exposure.

Defences:

- **`nlk/guard` wrapping** — every user-provided text and every
  tool-result body is wrapped in a noncesha-tagged XML block
  (`<user_data_NONCE>…</user_data_NONCE>`) so the LLM can't be
  steered into treating untrusted bytes as instructions.
  Fail-closed: chat / contextbuild / extraction all return an
  error rather than silently falling back to unwrapped content.
- **Self-referential filter** — `memory.IsSelfReferential` blocks
  THINK-leak class facts ("the assistant", "system prompt",
  `<think>`, etc.) at extraction time so they can't re-inject into
  future system prompts.
- **Category allowlist** — only the documented 4 extraction
  categories are accepted; anything else is dropped.
- **Provenance tagging** — every Global / Session Memory and
  Finding entry carries a `Source` and renders as `[user-stated]`
  vs `[derived]` in the system prompt so the LLM can discount
  derived content.
- **MITL gates** — write / execute tool categories require
  approval by default; analysis-plan and SQL-preview have
  dedicated dialogs.
- **Sandbox** — opt-in container isolation for code execution
  with `/work` as the only persistent surface.
- **Sandbox image trust** — `SandboxImageStatus.ActivePinnedByDigest`
  surfaces a banner when the active image is a mutable upstream
  tag (`python:3.12-slim`); locally-built `<TagPrefix>:<sha>` and
  `@sha256:` upstream pins are treated as safe.
- **Atomic IO** — every state file uses `WriteFileAtomic` so a
  crash can't corrupt mid-save.
- **Tool-call args cap** — 1 MiB by default
  (`LocalConfig.MaxToolCallArgsBytes` /
  `VertexAIConfig.MaxToolCallArgsBytes`).
- **Symlink rejection** — `analysis.validateFilePath` uses
  `os.Lstat` and rejects symlinks for `load-data` and any other
  host-path entry point.

## 10. Build, test, release

- `cd app && make build` → `dist/shell-agent-v2.app`. Never
  `go build` directly — the binary leaks into the project root.
- `cd app && make test` → `go test -tags no_duckdb_arrow ./...`
  for the standard suite. `lmstudio` / `vertexai` build tags
  enable integration tests against the live backends.
- Frontend: `cd app/frontend && npm run build` for the
  TypeScript / Vite check (also runs as part of `make build`).
- Manual smoke before release: see the `Pre-release smoke`
  checklist in CHANGELOG / RELEASE notes.

The Mac config in `main.go` keeps the WebView opaque, hides the
title, and uses a transparent (cosmetic) titlebar. There is no
launcher app — direct `.app` launch only.

## 11. Pointers into the history

For "why was X done" / "what was the previous shape" questions,
[`../history/`](../history/) preserves the original design notes.
Some of them no longer reflect current code (notably the v1 Hot/
Warm/Cold compaction notes and the original Pinned Memory design)
but are kept as the audit trail behind the v0.2.0 rewrite. When
in doubt, prefer this document and `memory-model.md` for current
behaviour.

Notable history docs that still describe behaviour shipped in
v0.2.0:

- `../history/agent-data-flow.md` — analysis tool dataflow, mostly
  current.
- `../history/agent-loop-resilience.md` — loop guards (still in
  effect).
- `../history/sandbox-execution.md` / `sandbox-image-build.md` —
  current sandbox setup.
- `../history/security-hardening-2.md` — the canonical threat model
  reference (this file's §9 summarises it).
- `../history/work-dir-shell-bridge.md` — current `$SHELL_AGENT_WORK_DIR`
  contract.
- `../history/object-storage.md` — current objstore behaviour.
- `../history/tool-event-restore.md` — current session-restore tool
  bubble shape.
- `../history/llm-abstraction.md` — current Backend interface.
- `../history/multi-image-handling.md` — current multimodal flow.

Notable history docs that describe v0.1.x and have been
superseded:

- `../history/memory-architecture-v2.md` — v1's Hot/Warm/Cold +
  contextbuild rationale. The non-destructive contextbuild path
  is current; the v1 destructive compaction it discusses is gone.
- `../history/memory-injection-hardening.md` — original
  Pinned-Memory threat model. Still useful for the *defences*,
  but the storage design is superseded by `memory-model.md`.
- `../history/information-display-redesign.md` — Phase 2 of the UI
  layout, mostly still in effect; note that Findings moved out
  of the sidebar in v0.2.0 Phase 8.
- `../history/frontend-decomposition.md` — Phase 3 component split.
- `../history/background-task-indicator.md` — `pinned-extraction`
  bg-task name was renamed to `memory-extraction` in v0.2.0.
- `../history/shell-agent-v2-architecture.md` /
  `shell-agent-v2-rfp.md` — the original RFP and architecture
  doc that shaped the v0.1 baseline. This document supersedes
  them as the current source of truth.

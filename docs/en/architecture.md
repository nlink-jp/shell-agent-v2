# shell-agent-v2 Architecture (v0.2.0)

This document describes how shell-agent-v2 is put together **as of
v0.2.0**. For the evolution history of individual subsystems see
[`history/`](history/).

The companion document [`memory-model.md`](memory-model.md) is the
canonical reference for the 4-facility memory design; this file
gives the wider system context and points into it where memory is
involved.

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
`CHANGELOG.md` for the breaking-change summary.

## 2. Process model

### Idle / Busy state machine

The agent occupies the active session exclusively while it works:

- **Idle** — accepts user input, can switch sessions freely.
- **Busy** — input field disabled, session switch requires explicit
  abort. Background tasks (title generation, memory extraction)
  count as Busy until they finish, so the next user message can't
  race them.

State transitions are owned by `internal/agent.Agent` and surfaced
to the frontend via the `agent:state` Wails event plus the
`GetState` binding. The Busy guard is also enforced backend-side:
binding entry-points that would mutate session state during Busy
return an error rather than queuing.

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
`history/agent-loop-resilience.md`.

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
counterparts, MCP — flow through one dispatcher (`agent.dispatchTool`)
and one MITL gate (`agent.IsToolMITLRequired`).

### Sources

- **Built-in analysis tools** (`internal/agent/tools.go`):
  `load-data`, `query-sql`, `query-preview`, `quick-summary`,
  `analyze-data`, `promote-finding`, `create-report`,
  `register-object`, `describe-data`, `list-tables`,
  `suggest-analysis`, `reset-analysis`, `get-object`.
- **Shell scripts** (`internal/toolcall/`): user-registered scripts
  with header-driven metadata. Bundled scripts (`file-info`,
  `preview-file`, `list-files`, `weather`, `get-location`,
  `write-note`) are scaffolded on first launch via `go:embed`.
- **Sandbox tools** (`internal/agent/sandbox_tools.go`): eight
  `sandbox-*` tools that execute in a per-session container with
  `/work` mounted from the session data dir.
- **MCP tools** — discovered at startup from the configured
  guardian profiles and namespaced as `<guardian>__<tool>`.

### MITL (Man-In-The-Loop) gate

Each tool has a default — read = auto-allow, write/execute =
require approval. Per-tool override via `MITLOverrides` in
config. The default is computed by `analysisToolMITLDefault` /
`Tool.NeedsMITL()` (shell), and the Settings → Tools UI reflects
the same dispatcher truth.

Special MITL flows:

- `query-sql` → SQL preview dialog before execution.
- `analyze-data` → analysis-plan confirmation dialog.

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

Session deletion (`DeleteSessionDir`) removes the entire
`sessions/<id>/` directory atomically — one operation removes
records, session memory, findings, summaries cache, and the
DuckDB file. Global Memory and the objstore are unaffected.

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
| `agent:activity` | tool_start / tool_end / thinking |
| `agent:state` | Idle / Busy transitions |
| `session:title` | Auto-generated session title |
| `global_memory:updated` | Global Memory store changed |
| `session_memory:updated` | Session Memory store changed |
| `findings:updated` | Findings store changed |
| `report:created` | New report bubble |
| `mitl:request` | MITL approval needed |
| `bg-task:start` / `bg-task:end` | Background task lifecycle |

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
[`history/`](history/) preserves the original design notes. Some
of them no longer reflect current code (notably the v1 Hot/Warm/
Cold compaction notes and the original Pinned Memory design)
but are kept as the audit trail behind the v0.2.0 rewrite. When
in doubt, prefer this document and `memory-model.md` for current
behaviour.

Notable history docs that still describe behaviour shipped in
v0.2.0:

- `history/agent-data-flow.md` — analysis tool dataflow, mostly
  current.
- `history/agent-loop-resilience.md` — loop guards (still in
  effect).
- `history/sandbox-execution.md` / `sandbox-image-build.md` —
  current sandbox setup.
- `history/security-hardening-2.md` — the canonical threat model
  reference (this file's §9 summarises it).
- `history/work-dir-shell-bridge.md` — current `$SHELL_AGENT_WORK_DIR`
  contract.
- `history/object-storage.md` — current objstore behaviour.
- `history/tool-event-restore.md` — current session-restore tool
  bubble shape.
- `history/llm-abstraction.md` — current Backend interface.
- `history/multi-image-handling.md` — current multimodal flow.

Notable history docs that describe v0.1.x and have been
superseded:

- `history/memory-architecture-v2.md` — v1's Hot/Warm/Cold +
  contextbuild rationale. The non-destructive contextbuild path
  is current; the v1 destructive compaction it discusses is gone.
- `history/memory-injection-hardening.md` — original
  Pinned-Memory threat model. Still useful for the *defences*,
  but the storage design is superseded by `memory-model.md`.
- `history/information-display-redesign.md` — Phase 2 of the UI
  layout, mostly still in effect; note that Findings moved out
  of the sidebar in v0.2.0 Phase 8.
- `history/frontend-decomposition.md` — Phase 3 component split.
- `history/background-task-indicator.md` — `pinned-extraction`
  bg-task name was renamed to `memory-extraction` in v0.2.0.
- `history/shell-agent-v2-architecture.md` /
  `shell-agent-v2-rfp.md` — the original RFP and architecture
  doc that shaped the v0.1 baseline. This document supersedes
  them as the current source of truth.

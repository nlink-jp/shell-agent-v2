# shell-agent-v2 Architecture

## System Overview

```
┌──────────────────────────────────────────────────────────────────┐
│                         Wails v2 App                             │
│  ┌──────────────┐    ┌─────────────────────────────────────┐     │
│  │  React UI    │◄──►│  bindings.go (thin delegation)      │     │
│  │  App.tsx     │    │  EventsEmit  (streaming, activity,  │     │
│  │  components/ │    │   bg-task, mitl, sandbox build…)    │     │
│  │  dialogs/    │    └────────────────────┬────────────────┘     │
│  │  sidebar/    │                         │                      │
│  └──────────────┘                         │                      │
│                                ┌──────────▼──────────┐           │
│                                │   agent/ package    │           │
│                                │ Idle / Busy + post- │           │
│                                │ task gate           │           │
│                                └──────────┬──────────┘           │
│         ┌────────┬────────┬─────────┬─────┴──────┬───────┐       │
│         ▼        ▼        ▼         ▼            ▼       ▼       │
│      ┌─────┐ ┌─────────┐ ┌──┐ ┌──────────┐ ┌────────┐ ┌────┐    │
│      │chat/│ │analysis/│ │llm│ │ toolcall │ │ sandbox│ │ mcp│    │
│      └──┬──┘ │  DuckDB │ │   │ │ + bundled│ │  /work │ │    │    │
│         │    └─────────┘ └─┬─┘ └──────────┘ └────────┘ └────┘    │
│         │                  │                                      │
│  ┌──────▼──────┐    ┌──────▼─────────┐                           │
│  │ contextbuild│    │  Local         │                           │
│  │ (memory v2) │    │  Vertex AI     │                           │
│  └──────┬──────┘    └────────────────┘                           │
│         │                                                         │
│  ┌──────▼──────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐       │
│  │  memory/    │  │  pinned  │  │ findings │  │ objstore │       │
│  │  Hot tier   │  │  facts   │  │  insights│  │ images / │       │
│  │  + summaries│  │          │  │          │  │ blobs /  │       │
│  └─────────────┘  └──────────┘  └──────────┘  │ reports  │       │
│                                                └──────────┘       │
│                                                                   │
│  Helpers: pathfix/ (Homebrew PATH on .app launch)                 │
│           bundled/ (first-run install of shell tool scripts)      │
│                                                                   │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │                   Persistent Storage                         │  │
│  │  sessions/{id}/chat.json + analysis.duckdb +                 │  │
│  │                summaries.json + work/ (sandbox host mount)   │  │
│  │  objects/data/{hex-id} + objects/index.json                  │  │
│  │  findings.json    pinned.json    config.json                 │  │
│  │  app.log          tools/ (user-edited shell-tool scripts)    │  │
│  │  sandbox image cache (managed by podman/docker)              │  │
│  └─────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

## Agent State Machine

```
         User Input / Command
              │
              ▼
        ┌───────────┐
        │   Idle    │◄──────────────────────────┐
        └─────┬─────┘                           │
              │ Send() / SendWithImages         │
              ▼                                 │
        ┌───────────┐                           │
        │   Busy    │                           │
        │ (agentLoop)│                          │
        └─────┬─────┘                           │
              │ tool rounds (max N) /           │
              │ final reply / Cancelled         │
              ▼                                 │
        ┌───────────────────────────┐           │
        │ Busy (post-response tasks)│           │
        │  • title generation       │           │
        │  • memory compaction      │           │
        │  • pinned-fact extraction │           │
        └─────┬───────────────────┬─┘           │
              │                   │             │
              │ all 3 finish      │ Abort()     │
              │ (success/error/   │  fires      │
              │  canceled)        │  postCancel │
              ▼                   ▼             │
        ┌──────────────────────────────────┐    │
        │ trailing goroutine: state = Idle ├────┘
        │ (cancel / postCancel cleared)    │
        └──────────────────────────────────┘
```

**Invariants:**
- Chat input + Sidebar New / Load / Delete + slash commands all
  blocked during Busy — including the post-response window.
  This prevents a quickly-typed second message from racing
  pinned-fact extraction (which only sees the latest 4 hot
  records and would silently lose facts on cancellation).
- Session switch and `/model` switch both require Idle.
- `Abort` fires both `cancel` (in-flight agentLoop) and
  `postCancel` (post-response tasks) so the trailing goroutine
  can drop state to Idle promptly. Earlier auto-cancel-on-
  next-Send was tried and reverted: see
  [`background-task-indicator.md`](./background-task-indicator.md).

## Session-Scoped Analysis

Each session owns an independent DuckDB instance and a private
`/work` directory the sandbox container mounts:

```
sessions/
├── sess-1713945600000/
│   ├── chat.json          # Conversation records (Hot/Warm/Cold)
│   ├── analysis.duckdb    # Session-owned database (lazy)
│   ├── summaries.json     # contextbuild summary cache (memory v2)
│   └── work/              # Mounted at /work inside the sandbox
│       └── …              # Files the LLM produces (CSVs, charts…)
└── sess-1713952800000/
    └── …
```

**Lifecycle:**
1. `NewSession()` — creates directory + empty `chat.json`.
2. First `load-data` (or `sandbox-load-into-analysis`) — lazily
   creates `analysis.duckdb`.
3. First sandbox `Exec` — creates `work/` and starts the
   per-session container (`shell-agent-v2-<sessionID>`).
4. `LoadSession()` — drains any post-response goroutines, closes
   current DuckDB, opens the target session's DB. Sandbox
   containers are session-scoped so the swap is naturally clean.
5. `DeleteSession()` — removes the entire session directory and
   stops + removes the sandbox container; objstore entries
   bound to the session are cleaned up via
   `objects.DeleteBySession`.

**Data isolation:** Tables loaded in Session A are invisible to
Session B; sandbox `/work` is a separate host directory per
session. Cross-session knowledge sharing is via Global Findings
or Pinned Memory only.

## Memory Architecture

Two compaction implementations coexist; selection is gated by a
`Memory.UseV2` config flag.

```
System Prompt
├── Base prompt + temporal context
├── Pinned Memory (cross-session facts; "(learned YYYY-MM-DD)" suffix)
└── Global Findings (analysis insights)

Session Records (immutable when UseV2 = true)
├── Cold: LLM summaries of old conversations  (legacy v1 only)
├── Warm: LLM summaries of recent past         (legacy v1 only)
└── Hot:  Conversation messages
    ├── user messages
    ├── assistant responses (with [Calling: ...] markers excluded
    │   from LLM context — gemma mimics them otherwise)
    └── tool results
```

**v1 Compaction (UseV2 = false, default):** When hot tier exceeds
the per-backend `HotTokenLimit`, older messages are summarized by the
LLM and the original hot records are *replaced* by a warm-tier
summary. Always preserves at least the most recent record (Vertex 400
"empty contents" regression fix). Warm records carry `SummaryRange`
timestamps.

**v2 Non-Destructive Compaction (UseV2 = true):** Records remain
immutable and full-fidelity. `internal/contextbuild` derives the LLM
context per call, sized to the active backend's
`MaxContextTokens`, with the older tail folded into a content-keyed
cached summary. Cache lives at `sessions/<id>/summaries.json`. Time
markers are added to every channel (raw records, summary block,
pinned, findings) so the LLM can reason about *when* each piece
happened. See [`memory-architecture-v2.md`](./memory-architecture-v2.md).

**Per-backend budget.** Each LLM backend has its own
`HotTokenLimit` and `ContextBudget` (`MaxContextTokens` /
`MaxWarmTokens` / `MaxToolResultTokens`). Resolution falls back to
the legacy top-level `Memory` / `ContextBudget` fields when a
per-backend value is zero.

```
config.json (excerpt)
├── llm
│   ├── default_backend
│   ├── local
│   │   ├── endpoint, model, api_key_env
│   │   ├── hot_token_limit            ← per-backend, optional
│   │   └── context_budget             ← per-backend, optional
│   │       ├── max_context_tokens
│   │       ├── max_warm_tokens
│   │       └── max_tool_result_tokens
│   └── vertex_ai
│       ├── project_id, region, model
│       ├── hot_token_limit            ← per-backend, optional
│       └── context_budget             ← per-backend, optional
├── memory
│   ├── hot_token_limit                ← legacy fallback
│   └── use_v2                         ← v2 opt-in flag
└── context_budget                     ← legacy fallback
```

**Pinned vs Findings:**
- Pinned: General facts (key-value), manually managed; rendered with
  `(learned YYYY-MM-DD)` suffix per fact
- Findings: Analysis insights with provenance, auto or manual promotion

## LLM Backend Abstraction

```go
type Backend interface {
    Chat(ctx, messages, tools) (*Response, error)
    ChatStream(ctx, messages, tools, callback) (*Response, error)
    Name() string
}
```

Two implementations:
- **Local:** OpenAI-compatible SSE streaming, tool call parsing
- **Vertex AI:** google/genai SDK, ADC authentication

Runtime switching via `/model local` or `/model vertex`.

## Tool System

Five tool sources, unified in the agent loop:

| Source | Examples | MITL |
|--------|----------|------|
| **Builtin** | `resolve-date`, `create-report` | No |
| **Analysis** | `load-data`, `query-sql`, `describe-data`, `promote-finding`, `reset` | No |
| **Sandbox** (opt-in, per-session container) | `sandbox-run-shell`, `sandbox-run-python`, `sandbox-write-file`, `sandbox-copy-object`, `sandbox-register-object`, `sandbox-info`, `sandbox-load-into-analysis`, `sandbox-export-sql` | execute (all eight) |
| **Shell script** | bundled (`file-info`, `preview-file`, `list-files`, `weather`, `get-location`, `write-note`) + user-added scripts | read: No, write/execute: Yes |
| **MCP** | `mcp__<server>__<tool>` proxied via mcp-guardian | Delegated |

**Dynamic filtering.** Analysis tools are conditionally exposed
based on data presence so local LLMs are never asked to choose
from a long list. Sandbox tools register only when
`sandbox.enabled` is on **and** the configured image is present
on the local container engine — both checks happen at agent
construction; otherwise the tools stay hidden so a misconfigured
sandbox can't 1) crash mid-turn and 2) tempt the model to call
something that won't work.

**Tool-call timeline.** Every `tool_start`/`tool_end` activity
event is rendered inline in the chat as a transient pill. The
underlying `memory.Record` for each tool result also persists
its success/error status (added in v0.1.19), so on session
reload the bubbles return — see
[`tool-event-restore.md`](./tool-event-restore.md).

### Bundled Shell Tools

Default scripts ship inside the binary via Go `embed`. On startup,
`internal/bundled.Install(cfg.Tools.ScriptDir)` copies any missing
file from the embedded `tools/` directory into the user's tool dir.

- Files that already exist in the user's dir are **never overwritten**
  — user customizations are preserved across upgrades.
- New tools added in a release ship to existing users automatically
  on next launch.
- The `examples/` subdirectory is intentionally excluded — example
  scripts are reference material the user copies in deliberately.

Source layout: `app/internal/bundled/tools/` (kept inside the Go
module so `//go:embed` can reach it). The user-facing tool dir is
`~/Library/Application Support/shell-agent-v2/tools/`.

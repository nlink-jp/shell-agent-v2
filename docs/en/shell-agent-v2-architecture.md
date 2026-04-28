# shell-agent-v2 Architecture

## System Overview

```
┌─────────────────────────────────────────────────────────┐
│                    Wails v2 App                         │
│  ┌──────────────┐    ┌─────────────────────────────┐    │
│  │  React UI    │◄──►│  bindings.go (thin layer)   │    │
│  │  App.tsx     │    │  EventsEmit (streaming)     │    │
│  └──────────────┘    └──────────┬──────────────────┘    │
│                                 │                       │
│                      ┌──────────▼──────────┐            │
│                      │   agent/ package    │            │
│                      │   Idle ◄──► Busy    │            │
│                      └──────────┬──────────┘            │
│              ┌──────────┬───────┼───────┬────────┐      │
│              ▼          ▼       ▼       ▼        ▼      │
│          ┌──────┐  ┌────────┐ ┌─────┐ ┌──────┐ ┌────┐  │
│          │chat/ │  │analysis│ │llm/ │ │tools/│ │mcp/│  │
│          │      │  │DuckDB  │ │     │ │      │ │    │  │
│          └──────┘  └────────┘ └──┬──┘ └──────┘ └────┘  │
│                                  │                      │
│                         ┌────────┴────────┐             │
│                         ▼                 ▼             │
│                    ┌─────────┐      ┌──────────┐        │
│                    │  Local  │      │ Vertex AI│        │
│                    │LM Studio│      │ Gemini   │        │
│                    └─────────┘      └──────────┘        │
│                                                         │
│  ┌──────────────────────────────────────────────────┐   │
│  │              Persistent Storage                   │   │
│  │  sessions/{id}/chat.json + analysis.duckdb       │   │
│  │  findings.json    pinned.json    config.json     │   │
│  │  objects/data/{hex-id}                           │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

## Agent State Machine

```
         User Input / Command
              │
              ▼
        ┌───────────┐
        │   Idle    │◄──────────────────┐
        └─────┬─────┘                   │
              │ Send()                  │
              ▼                         │
        ┌───────────┐    Abort()   ┌────┴────┐
        │   Busy    │─────────────►│ Cleanup │
        └─────┬─────┘              └─────────┘
              │
       ┌──────┼──────┐
       ▼      ▼      ▼
    Chat   Analysis  Tool
    LLM    DuckDB    Shell/MCP
       │      │      │
       └──────┼──────┘
              │ Complete / Max rounds
              ▼
        ┌───────────┐
        │   Idle    │
        └───────────┘
```

**Invariants:**
- Chat input blocked during Busy
- Session switch requires Idle (or abort to Idle)
- `/model` switch requires Idle

## Session-Scoped Analysis

Each session owns an independent DuckDB instance:

```
sessions/
├── sess-1713945600000/
│   ├── chat.json          # Conversation records (Hot/Warm/Cold)
│   └── analysis.duckdb    # Session-owned database (lazy)
└── sess-1713952800000/
    ├── chat.json
    └── analysis.duckdb
```

**Lifecycle:**
1. `NewSession()` — creates directory + empty `chat.json`
2. First `load-data` tool call — creates `analysis.duckdb`
3. `LoadSession()` — closes current DuckDB, opens target session's DB
4. `DeleteSession()` — removes entire session directory

**Data isolation:** Tables loaded in Session A are invisible to Session B.
Cross-session knowledge sharing is via Global Findings only.

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

Three tool sources, unified in the agent loop:

| Source | Examples | MITL |
|--------|----------|------|
| **Builtin** | resolve-date | No |
| **Analysis** | load-data, query-sql, promote-finding | No |
| **Shell script** | bundled (file-info, preview-file, list-files, weather, get-location, write-note) + user-added scripts | read: No, write/execute: Yes |
| **MCP** | mcp-guardian tools | Delegated |

**Dynamic filtering:** Analysis tools are conditionally exposed
based on data presence to keep tool count manageable for local LLMs.

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

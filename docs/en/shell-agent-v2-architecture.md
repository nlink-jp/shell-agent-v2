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

```
System Prompt
├── Base prompt + temporal context
├── Pinned Memory (cross-session facts)
└── Global Findings (analysis insights)

Session Records
├── Cold: LLM summaries of old conversations
├── Warm: LLM summaries of recent past
└── Hot: Current conversation messages
    ├── user messages
    ├── assistant responses
    └── tool results
```

**Compaction:** When hot tier exceeds token budget, older messages
are summarized by the LLM and moved to warm tier. Warm records
carry `SummaryRange` timestamps.

**Pinned vs Findings:**
- Pinned: General facts (key-value), manually managed
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
| **Shell script** | User-registered scripts | read: No, write/execute: Yes |
| **MCP** | mcp-guardian tools | Delegated |

**Dynamic filtering:** Analysis tools are conditionally exposed
based on data presence to keep tool count manageable for local LLMs.

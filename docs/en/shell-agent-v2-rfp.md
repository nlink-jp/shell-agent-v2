# RFP: shell-agent v2

> Generated: 2026-04-24
> Status: Draft
> Predecessor: shell-agent v0.7.9 (util-series)

## 1. Problem Statement

shell-agent v1 is a macOS GUI chat and agent tool powered by local LLM with
embedded data analysis capabilities (DuckDB). While the interactive,
dialogue-driven approach to data analysis proved superior to the
purpose-built data-agent's rigid workflow, v1 suffers from fundamental
architectural issues:

1. **Chat-Analysis state inconsistency** — The analysis engine (DuckDB) is
   global while chat sessions are independent. Switching sessions breaks
   table references, metadata is lost on restart, and the two engines can
   diverge into unexpected states.

2. **No execution exclusivity** — The chat interface remains active during
   analysis execution. Users can send messages while the agent is busy,
   creating race conditions and confusing state.

3. **Monolithic implementation** — All business logic (chat, analysis, tools,
   memory, MCP) resides in a single 73KB `app.go`, making the codebase
   difficult to maintain and extend.

4. **Local LLM only** — Longer analysis tasks monopolize CPU for extended
   periods. No cloud LLM option exists for heavier workloads.

shell-agent v2 is a complete redesign that preserves v1's strength
(interactive, dialogue-driven data analysis) while solving these
architectural problems.

### Target User

Same as v1: individual user on macOS who wants a local-first chat and
agent tool with data analysis capabilities.

## 2. Functional Specification

### Core Concepts

#### Agent Execution Model: Idle / Busy

The agent operates in two states:

| State | Chat Input | Session Switch | UI Display |
|-------|-----------|----------------|------------|
| **Idle** | Accepts input | Allowed | Input field active |
| **Busy** | Blocked | Requires abort | Progress / streaming output |

All agent work (LLM response, tool execution, data analysis) transitions
the agent to Busy. The agent returns to Idle only when the work completes
or is aborted. This eliminates the v1 problem of concurrent chat input
during analysis.

**Abort behavior:**
- User can abort the current task at any time (cancel button / keyboard shortcut)
- Session switch during Busy triggers an abort confirmation dialog
- Aborted analysis rolls back any partial DuckDB state within the session

#### Session-Scoped Analysis

Each chat session owns its own DuckDB instance:

```
~/Library/Application Support/shell-agent-v2/
├── config.json
├── pinned.json              # Cross-session facts (unchanged from v1)
├── findings.json            # Global findings store (NEW)
├── sessions/
│   ├── {session-id}/
│   │   ├── chat.json        # Conversation records
│   │   └── analysis.duckdb  # Session-owned database
│   └── {session-id}/
│       ├── chat.json
│       └── analysis.duckdb
└── ...
```

- Loading a CSV in Session A creates a table only visible to Session A
- Switching to Session B sees Session B's tables (or none)
- No cross-session DuckDB state leakage
- Table descriptions persist in DuckDB (via `COMMENT ON TABLE`) — no
  metadata loss on restart

#### Global Findings

Findings are analysis-derived insights promoted to a global knowledge store,
accessible across all sessions.

```json
{
  "id": "f-20260424-001",
  "content": "Monthly sales peak in April — likely seasonal factor",
  "origin_session_id": "sess-abc123",
  "origin_session_title": "Sales Data Analysis",
  "tags": ["sales", "seasonality"],
  "created_at": "2026-04-24T14:30:00+09:00",
  "created_label": "2026-04-24 (Thursday)"
}
```

**Promotion (hybrid):**
- **Autonomous**: LLM judges a result significant and promotes it
  automatically (with user notification in chat)
- **Explicit**: User instructs "remember this" or uses a `/finding` command

**Cross-session usage:**
- All sessions can read global findings via system prompt injection
- When a finding references an origin session, the UI renders a clickable
  link to switch to that session
- If another session needs deeper analysis on a finding, the link guides
  the user to the origin session where the data already exists

**Relationship with Pinned Memory:**
- **Pinned Memory**: General cross-session facts (user preferences,
  environment info, recurring context) — unchanged from v1
- **Findings**: Data analysis insights with provenance (origin session,
  timestamp, tags)
- These are separate systems with separate storage

#### Temporal Context for LLM

Local LLMs are weak at date arithmetic (e.g., computing "last Thursday"
from a date string). v2 injects enriched temporal context into the system
prompt to enable reliable relative date resolution:

```
Current date and time: 2026-04-24 (Thursday) 15:30:00 JST
Yesterday: 2026-04-23 (Wednesday)
```

This allows the LLM to correctly resolve references like "last Thursday's
matter" or "yesterday's analysis results" without date computation.

Findings also carry a human-readable date label so cross-session temporal
references can be matched reliably:

```json
{
  "created_at": "2026-04-17T14:30:00+09:00",
  "created_label": "2026-04-17 (Thursday)"
}
```

When a user says "the finding from last Thursday," the LLM can match the
label directly instead of parsing and computing ISO timestamps.

For complex cases beyond "today" and "yesterday" (e.g., "3 weeks ago on
Wednesday", "first business day of last month"), a built-in `resolve-date`
tool is provided as a system tool:

```json
{
  "name": "resolve-date",
  "description": "Resolve relative date expressions to absolute dates. Use when you need to calculate dates like 'last Thursday', '3 weeks ago', 'first Monday of last month'.",
  "parameters": {
    "expression": "string — natural language date expression",
    "reference_date": "string — optional, ISO date to calculate from (default: today)"
  }
}
```

The tool computes dates deterministically using Go's `time` package,
eliminating LLM arithmetic errors. The LLM can self-select when to use
it — if confident in simple cases (from the system prompt context), it
skips the tool call; if uncertain, it delegates to the tool.

This two-layer design (system prompt for common cases + tool for complex
cases) minimizes unnecessary tool call round-trips while guaranteeing
correctness for arbitrary relative date expressions.

#### Hybrid LLM Backend

Two backends available simultaneously:

| Backend | Engine | Use Case |
|---------|--------|----------|
| **Local** | LM Studio (OpenAI-compatible API) | Quick chat, lightweight tasks, offline use |
| **Vertex AI** | Gemini (via google/genai SDK) | Heavy analysis, long context, complex reasoning |

**Switching:**
- `/model` command shows current engine + available options
- `/model local` or `/model vertex` switches immediately
- Switch only allowed in Idle state
- Default engine configurable in settings
- Per-session engine choice resets to default on new session

**Authentication:**
- Local: Optional API key via `SHELL_AGENT_API_KEY` env var (same as v1)
- Vertex AI: ADC (Application Default Credentials), requires
  `roles/aiplatform.user`

### Commands / API Surface

**Main App (Wails v2 + React):**
- Chat window with Idle/Busy state indication
- Abort button (visible during Busy)
- Sidebar: session list, tool list, findings panel
- Session link navigation (from findings to origin session)
- Settings UI: API config (local + Vertex AI), memory, tools, MCP, theme

**Chat Commands:**
- `/model [local|vertex]` — Show or switch LLM backend
- `/finding [text]` — Explicitly promote a finding to global store
- `/findings` — List all global findings

### Input / Output

Same as v1 for LLM communication, shell script tools, and MCP.
Vertex AI backend uses `google.golang.org/genai` SDK with streaming.

### Configuration

**Settings (JSON file at `~/Library/Application Support/shell-agent-v2/config.json`):**

```json
{
  "llm": {
    "default_backend": "local",
    "local": {
      "endpoint": "http://localhost:1234/v1",
      "model": "google/gemma-4-26b-a4b",
      "api_key_env": "SHELL_AGENT_API_KEY"
    },
    "vertex_ai": {
      "project_id": "PROJECT_ID",
      "region": "us-central1",
      "model": "gemini-2.5-flash"
    }
  },
  "memory": {
    "hot_token_limit": 4096,
    "warm_retention": "24h",
    "cold_retention": "7d"
  },
  "tools": {
    "script_dir": "~/Library/Application Support/shell-agent-v2/tools",
    "mcp_guardian": {
      "binary": "/usr/local/bin/mcp-guardian",
      "config": "~/.config/mcp-guardian/config.json"
    }
  },
  "ui": {
    "theme": "dark",
    "startup_mode": "last"
  }
}
```

### External Dependencies

| Dependency | Type | Required |
|-----------|------|----------|
| LM Studio (OpenAI-compatible API server) | Local service | Yes (when using local backend) |
| Vertex AI (Gemini) | Cloud service | Yes (when using vertex backend) |
| mcp-guardian | Binary (stdio child process) | Yes (when using MCP) |
| nlk | Go library (direct import) | Yes |

## 3. Design Decisions

**Language & Framework:**
- Main app: Go + Wails v2 + React — same as v1. Proven stack, enables
  nlk library reuse and Vertex AI SDK integration.
**Why a full rewrite instead of refactoring v1:**
- The state management architecture (global DuckDB, always-active chat)
  is load-bearing in v1's design. Retrofitting session-scoped analysis
  and the Idle/Busy model requires touching virtually every integration
  point.
- The 73KB monolith (`app.go`) needs structural decomposition that is
  impractical as incremental refactoring.
- v2 can inherit proven patterns (memory tiers, tool dispatch, MCP
  integration, security) while redesigning the state architecture.

**Relationship with Existing Tools:**
- `shell-agent v1`: Direct predecessor. Inherits memory model, tool
  system, MCP integration, security architecture. Redesigns state
  management and adds LLM backend flexibility.
- `data-agent`: Lessons learned — analysis-only tool proved too rigid
  for exploratory work. Vertex AI backend pattern reused.
- `nlk`: guard, jsonfix, strip, backoff, validate — same as v1.
- `mcp-guardian`: MCP delegation — same as v1.

**Out of Scope:**
- Cloud sync
- Multi-user support
- Server mode
- Cross-session DuckDB sharing (findings bridge this at the knowledge level)
- Container-based analysis (data-agent approach — not needed for v2's scope)

## 4. Development Plan

### Phase 1: Core — State Architecture

- Project scaffold (Wails v2 + React) in `_wip/shell-agent-v2/`
- **Agent state machine** (Idle/Busy) with UI lockout
- **Session-scoped DuckDB** lifecycle (create, open, close, persist)
- Chat engine with session persistence
- Hot memory with timestamps (carry from v1)
- Abort mechanism (context cancellation, DuckDB rollback)
- Dual LLM backend abstraction (`local` + `vertex_ai`)
- `/model` command for runtime switching
- nlk integration (guard, jsonfix, strip)
- Basic chat UI with Idle/Busy indicator
- Tests for state transitions, session isolation, backend switching

**Independently reviewable — validates the core architectural change**

### Phase 2: Features — Agent Capabilities

- Shell script Tool Calling (carry from v1)
- MCP integration via mcp-guardian (carry from v1)
- **Global Findings store** (CRUD, promotion, origin linking)
- Autonomous finding promotion (LLM-driven)
- `/finding` and `/findings` chat commands
- Findings panel in sidebar with session link navigation
- Warm/Cold memory tiers with LLM summarization
- Pinned Memory (carry from v1)
- Multimodal support — image input (carry from v1)
- Data analysis tools (load-data, query-sql, query-preview, analyze-data, etc.)
- Dynamic tool filtering (carry from v1, now session-scoped)
- Settings UI (dual backend config, memory, tools, MCP, theme)
**Independently reviewable**

### Phase 3: Release — Documentation & Quality

- Test expansion (state edge cases, session switch during analysis, etc.)
- README.md / README.ja.md
- Architecture documentation (en + ja)
- CHANGELOG.md
- AGENTS.md
- Release build and distribution
- Migration guide from v1 (session data conversion if feasible)

## 5. Required API Scopes / Permissions

| Service | Scope / Role | Purpose |
|---------|-------------|---------|
| Vertex AI | `roles/aiplatform.user` | Gemini API access |
| MCP | Delegated to mcp-guardian | No direct auth needed |
| Local LLM | None (optional API key) | LM Studio access |

## 6. Series Placement

Series: **util-series**
Reason: Same as v1. Successor to shell-agent in the same series.

## 7. External Platform Constraints

| Constraint | Details |
|-----------|---------|
| LM Studio | Local server must be running when using local backend |
| Vertex AI | Requires ADC setup (`gcloud auth application-default login`), network access, billing-enabled GCP project |
| Wails v2 | Requires macOS 10.15+ |
| gemma-4-26b-a4b | Requires ~16GB VRAM (Apple Silicon M1/M2 Pro+) |
| DuckDB per-session | Disk usage grows with number of sessions. No auto-GC in v2 scope; manual session deletion cleans up DB |

---

## Architecture Overview

### Package Structure (Target)

```
shell-agent-v2/
├── app/
│   ├── main.go
│   ├── internal/
│   │   ├── agent/           # Agent state machine (Idle/Busy), execution loop
│   │   ├── chat/            # Chat engine, message building, system prompt
│   │   ├── llm/             # Backend abstraction (local + vertex_ai)
│   │   ├── analysis/        # DuckDB engine (session-scoped lifecycle)
│   │   ├── memory/          # Hot/Warm/Cold tiers, sessions, pinned
│   │   ├── findings/        # Global findings store, promotion logic
│   │   ├── toolcall/        # Shell script registry, MITL
│   │   ├── mcp/             # mcp-guardian stdio
│   │   ├── objstore/        # Image/blob repository
│   │   ├── config/          # JSON config with path expansion
│   │   └── logger/          # Structured logging
│   ├── bindings.go          # Wails bindings (thin delegation layer)
│   ├── frontend/src/
│   │   ├── App.tsx
│   │   ├── ChatInput.tsx
│   │   └── ...
│   └── Makefile
├── docs/
│   ├── en/
│   └── ja/
├── CLAUDE.md
├── AGENTS.md
├── README.md
├── README.ja.md
└── CHANGELOG.md
```

**Key structural change from v1:**
- `app.go` (73KB monolith) is decomposed into `agent/`, `chat/`, `llm/`,
  `analysis/`, `findings/`, and a thin `bindings.go` for Wails integration
- `agent/` owns the Idle/Busy state machine and orchestrates all other packages
- `llm/` provides a unified interface over local and Vertex AI backends
- `findings/` is a new package for the global knowledge store

### State Flow

```
User Input
  │
  ▼
bindings.go (Wails) ──→ agent.Send(msg)
                              │
                              ▼
                        [Idle] ──→ [Busy]
                              │
                     ┌────────┼────────┐
                     ▼        ▼        ▼
                  chat/    analysis/  toolcall/
                     │        │        │
                     ▼        ▼        ▼
                   llm/ (local or vertex_ai)
                     │
                     ▼
                  [Busy] ──→ [Idle]
                              │
                              ▼
                        UI unlocked
```

### Session Lifecycle

```
NewSession()
  ├── Create session directory
  ├── Initialize chat.json (empty records)
  └── DuckDB: not created yet (lazy — created on first load-data)

LoadSession(id)
  ├── Close current session's DuckDB (if open)
  ├── Load chat.json
  └── Open session's analysis.duckdb (if exists)

DeleteSession(id)
  ├── Close DuckDB connection
  ├── Remove session directory (chat.json + analysis.duckdb)
  └── Remove orphaned findings? (No — findings persist independently)
```

---

## Discussion Log

1. **data-agent retrospective**: Purpose-built data analysis tool proved
   too rigid for exploratory work. Interactive dialogue-driven analysis
   (shell-agent v1 approach) is better for ad-hoc investigation.
2. **Root cause of v1 instability**: Chat engine and analysis engine have
   independent lifecycles with shared mutable state (global DuckDB).
   Session switches break referential integrity.
3. **Session-scoped analysis**: Each session owns its DuckDB. Eliminates
   cross-session state leakage entirely.
4. **Global Findings**: Analysis insights promoted to a shared knowledge
   store with origin session provenance. Bridges sessions at the
   knowledge level, not the data level.
5. **Findings vs Pinned Memory**: Separate systems. Pinned = general facts,
   Findings = analysis insights with provenance.
6. **Idle/Busy execution model**: Agent occupies the session exclusively
   during work. Chat input blocked during Busy. Session switch requires
   abort. Eliminates race conditions from concurrent input.
7. **Hybrid LLM backend**: Local (gemma-4) + Vertex AI (gemini) with
   `/model` runtime switching. Default configurable in settings.
   Addresses v1's CPU monopolization on longer tasks.
8. **Full rewrite**: State architecture changes are too fundamental for
   incremental refactoring. v2 developed in `_wip/` as separate project.
9. **Monolith decomposition**: 73KB `app.go` split into focused packages
   with thin Wails binding layer.
10. **Enriched temporal context**: System prompt includes day-of-week and
    yesterday's date for reliable relative date resolution. Findings carry
    human-readable date labels for cross-session temporal references.
    Local LLMs cannot reliably compute "last Thursday" from a date string.

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
│   │   ├── memory/          # Hot/Warm/Cold compaction, sessions, pinned
│   │   ├── findings/        # Global findings store (cascade delete with sessions)
│   │   ├── objstore/        # Central object repository (image/blob/report)
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
- Synchronous compaction before BuildMessages (HotTokenLimit=4096)
- Post-response tasks (title, compaction, pinned extraction) via async WaitGroup

### Context Budget Control
- BuildMessagesWithBudget: newest-first selection, tool result truncation, [Calling:] skip
- Root cause of tool calling failure: [Calling:] pattern contamination, NOT context length
- gemma-4 supports 256K tokens; MaxContextTokens defaults to 0 (unlimited)

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
- `pinned:updated` — pinned memory changes
- `report:created` — report content for display
- `mitl:request` — MITL approval dialog

### MCP Integration
- Multiple guardian profiles (name, binary, profile_path, enabled)
- Tool names prefixed: `mcp__guardianName__toolName`
- Guardian lifecycle: start on app launch, restart from Settings
- Path expansion (~/ supported)

### UI
- Sidebar: v1-style icon navigation (Sessions/Status panels, collapse/expand, resize)
- Settings: tabbed (General/Tools/MCP) near-fullscreen overlay
- Tools tab: unified list with Enabled + MITL toggles per tool
- Commands (/help, /model, /findings): popup panel, not chat messages
- MITL dialog: SQL preview, analysis plan, feedback input

## Design Documents

All implementation must follow these design documents:
- **agent-data-flow.md** — agent loop, context budget, MITL, events, tool confirmation
- **object-storage.md** — central object storage, lifecycle, LLM tools
- **llm-abstraction.md** — backend role mapping, tool format conversion, multimodal

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

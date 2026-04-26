# AGENTS.md — shell-agent-v2

## Summary

macOS local-first chat & agent tool with interactive data analysis.
Wails v2 (Go + React) desktop application. Redesign of shell-agent v1
with session-scoped DuckDB, Idle/Busy agent model, global Findings,
hybrid LLM backend (Local + Vertex AI), and central object storage.

## Build & Test

```bash
cd app
make build      # Build .app bundle → dist/shell-agent-v2.app
make dev        # Wails dev server with hot reload
make test       # go test ./... (add -tags no_duckdb_arrow for CGO builds)
make clean      # Remove build artifacts

# Integration tests (require running services):
go test ./internal/llm/ -tags lmstudio -v    # LM Studio at localhost:1234
VERTEX_PROJECT=xxx go test ./internal/llm/ -tags vertexai -v  # Vertex AI
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
│   │   ├── agent/           # State machine, execution loop, tool dispatch
│   │   ├── chat/            # Message building, temporal context
│   │   ├── llm/             # Backend abstraction (Local + Vertex AI)
│   │   │   ├── backend.go   # Role types, Backend interface
│   │   │   ├── local.go     # LM Studio (OpenAI compat, tool→user mapping)
│   │   │   ├── vertex.go    # Vertex AI (genai SDK, FunctionCall/Response)
│   │   │   └── mock.go      # Mock backend for testing
│   │   ├── analysis/        # Session-scoped DuckDB engine
│   │   ├── memory/          # Hot/Warm/Cold compaction, sessions, pinned
│   │   ├── findings/        # Global findings store
│   │   ├── objstore/        # Central object repository (image/blob/report)
│   │   ├── toolcall/        # Shell script registry, MITL
│   │   ├── mcp/             # mcp-guardian stdio
│   │   ├── config/          # JSON config
│   │   └── logger/          # File-based logging
│   ├── frontend/src/
│   │   ├── App.tsx          # Main UI
│   │   ├── ChatInput.tsx    # Input with IME, history, image drop
│   │   └── ObjectImage.tsx  # Lazy object:ID URL resolver
│   ├── build/               # macOS app assets
│   ├── wails.json
│   └── Makefile
├── docs/
│   ├── en/                  # Design documents (authoritative)
│   │   ├── agent-data-flow.md
│   │   ├── object-storage.md
│   │   ├── llm-abstraction.md
│   │   └── shell-agent-v2-rfp.md
│   └── ja/                  # Japanese translations
└── CHANGELOG.md
```

## Design Documents

All implementation must follow these design documents:
- **agent-data-flow.md** — agent loop state machine, session records, memory compaction
- **object-storage.md** — central object storage, lifecycle, LLM tools
- **llm-abstraction.md** — backend role mapping, tool format conversion, multimodal

## Environment

- Config: `~/Library/Application Support/shell-agent-v2/config.json`
- Sessions: `~/Library/Application Support/shell-agent-v2/sessions/{id}/`
- Objects: `~/Library/Application Support/shell-agent-v2/objects/`
- Log: `~/Library/Application Support/shell-agent-v2/app.log`

## Gotchas

- DuckDB requires CGO — use `-tags no_duckdb_arrow` for builds
- `wails build` outputs to `build/bin/`, Makefile copies to `dist/`
- Local backend maps tool→user role (gemma-4 workaround)
- Vertex AI uses native FunctionCall/FunctionResponse
- Agent loop uses Chat() for all rounds (no streaming in loop)
- BuildMessages passes application-level roles — backends map internally
- Reports have dedicated UI (report-container), not regular chat bubbles

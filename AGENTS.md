# AGENTS.md — shell-agent-v2

## Summary

macOS local-first chat & agent tool with interactive data analysis.
Wails v2 (Go + React) desktop application. Redesign of shell-agent v1
with session-scoped DuckDB, Idle/Busy agent model, global Findings, and
hybrid LLM backend (Local + Vertex AI).

## Build & Test

```bash
cd app
make build      # Build .app bundle → dist/shell-agent-v2.app
make dev        # Wails dev server with hot reload
make test       # go test ./... (add -tags no_duckdb_arrow for CGO builds)
make clean      # Remove build artifacts
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
│   │   ├── agent/           # State machine (Idle/Busy), execution loop, tool dispatch
│   │   │                      agent.go, tools.go, integration_test.go
│   │   ├── chat/            # Message building, temporal context, resolve-date tool
│   │   ├── llm/             # Backend interface + Local (OpenAI SSE) + Vertex AI (genai)
│   │   ├── analysis/        # Session-scoped DuckDB engine (CSV, SQL, COMMENT ON TABLE)
│   │   ├── memory/          # Hot/Warm/Cold compaction, sessions, pinned memory
│   │   ├── findings/        # Global findings store with provenance
│   │   ├── toolcall/        # Shell script registry, header parsing, MITL, execution
│   │   ├── mcp/             # mcp-guardian stdio, JSON-RPC 2.0
│   │   ├── objstore/        # Image/blob repository (12-char hex IDs)
│   │   ├── config/          # JSON config with path expansion
│   │   └── logger/          # Structured logging
│   ├── frontend/src/        # React + TypeScript UI
│   │   ├── App.tsx          # Chat, sidebar (sessions/findings/settings), Idle/Busy
│   │   └── App.css          # Dark theme styles
│   ├── build/               # macOS app assets (Info.plist, icon)
│   ├── wails.json
│   └── Makefile
├── docs/
│   ├── en/                  # English documentation (RFP)
│   └── ja/                  # Japanese documentation (RFP)
├── CLAUDE.md
├── README.md / README.ja.md
└── CHANGELOG.md
```

## Environment

- Config: `~/Library/Application Support/shell-agent-v2/config.json`
- Sessions: `~/Library/Application Support/shell-agent-v2/sessions/{id}/`
  - Each session has: `chat.json` + `analysis.duckdb` (lazy-created)
- Pinned memory: `~/Library/Application Support/shell-agent-v2/pinned.json`
- Findings: `~/Library/Application Support/shell-agent-v2/findings.json`
- Objects: `~/Library/Application Support/shell-agent-v2/objects/`

## Test Coverage

95 tests across 9 packages (agent, analysis, chat, config, findings, llm, mcp, memory, objstore, toolcall).

Run with: `cd app && go test ./internal/... -tags no_duckdb_arrow`

## Gotchas

- DuckDB requires CGO — use `-tags no_duckdb_arrow` to exclude Arrow extensions
- `wails build` outputs to `build/bin/`, Makefile copies to `dist/`
- Frontend assets are embedded via `//go:embed all:frontend/dist`
- Vertex AI requires ADC (`gcloud auth application-default login`)
- Agent must be in Idle state for `/model` switch and session switch
- Analysis tools are dynamically filtered: minimal set (load-data, reset) when no data, full set when data exists
- Shell tool scripts must have `@tool:` header comments for auto-discovery

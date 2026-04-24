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
make test       # go test ./...
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
│   │   ├── agent/           # State machine (Idle/Busy), execution loop
│   │   ├── chat/            # Message building, system prompt, temporal context
│   │   ├── llm/             # Backend abstraction (local + vertex_ai)
│   │   ├── analysis/        # DuckDB engine (session-scoped)
│   │   ├── memory/          # Hot/Warm/Cold tiers, sessions, pinned
│   │   ├── findings/        # Global findings store, promotion
│   │   ├── toolcall/        # Shell script registry, MITL
│   │   ├── mcp/             # mcp-guardian stdio
│   │   ├── objstore/        # Image/blob repository
│   │   ├── config/          # JSON config
│   │   └── logger/          # Structured logging
│   ├── frontend/src/        # React + TypeScript UI
│   ├── build/               # macOS app assets (Info.plist, icon)
│   ├── wails.json
│   └── Makefile
├── docs/
│   ├── en/                  # English documentation
│   └── ja/                  # Japanese documentation
├── CLAUDE.md
├── README.md / README.ja.md
└── CHANGELOG.md
```

## Environment

- Config: `~/Library/Application Support/shell-agent-v2/`
- Sessions: `~/Library/Application Support/shell-agent-v2/sessions/{id}/`
- Each session has: `chat.json` + `analysis.duckdb` (lazy-created)

## Gotchas

- DuckDB requires CGO — cannot cross-compile without Podman/Docker
- `wails build` outputs to `build/bin/`, Makefile copies to `dist/`
- Frontend assets are embedded via `//go:embed all:frontend/dist`
- Vertex AI requires ADC (`gcloud auth application-default login`)
- Agent must be in Idle state for `/model` switch and session switch

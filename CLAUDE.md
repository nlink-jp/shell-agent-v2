# CLAUDE.md — shell-agent-v2

## Overview

macOS local-first chat & agent tool with interactive data analysis.
Wails v2 (Go + React) desktop application. Successor to shell-agent v1.

## Build

- Always `cd app && make build`
- Development: `cd app && make dev`
- Tests: `cd app && make test` or `go test ./internal/... -tags no_duckdb_arrow`

## Architecture

- **bindings.go** — Wails bindings (thin delegation to internal packages)
- **internal/agent/** — Agent state machine (Idle/Busy), execution loop, tool dispatch
- **internal/chat/** — Chat engine, message building, temporal context, resolve-date tool
- **internal/llm/** — Backend abstraction (local OpenAI-compatible + Vertex AI genai SDK)
- **internal/analysis/** — DuckDB engine (session-scoped, lazy init, COMMENT ON TABLE)
- **internal/memory/** — Hot/Warm/Cold compaction, sessions, pinned memory
- **internal/findings/** — Global findings store, promotion logic, origin linking
- **internal/toolcall/** — Shell script registry, header parsing, MITL categories, execution
- **internal/mcp/** — mcp-guardian stdio, JSON-RPC 2.0
- **internal/objstore/** — Central object repository (images/blobs, 12-char hex IDs)
- **internal/config/** — JSON config with path expansion
- **internal/logger/** — Structured logging
- **frontend/src/** — React UI with sidebar tabs (sessions/findings/settings)

## Key Design Decisions

- **Idle/Busy** — Agent occupies session exclusively during work. Chat input blocked in Busy state. Session switch requires abort.
- **Session-scoped analysis** — Each session owns its own DuckDB. No global shared database. Lazy initialization on first data load.
- **Findings** — Analysis insights promoted to global store with origin session provenance. Separate from Pinned Memory. Autonomous promotion via promote-finding tool.
- **Hybrid LLM** — Local + Vertex AI, switchable via `/model` command. Default configurable in settings.
- **Temporal context** — System prompt includes day-of-week + yesterday. `resolve-date` builtin tool for complex relative dates.
- **Dynamic tool filtering** — Minimal tools when no data (load-data, reset), full set when data exists. Keeps tool count low for local LLMs.
- **Thin bindings** — Wails binding layer delegates to agent package. No business logic in bindings.
- **Agent loop** — Simple tool-calling feedback (max 10 rounds, not ReAct). Suitable for local LLMs.
- **MITL** — Required for write/execute tool categories, not read.
- **No launcher app** — Direct application launch only.

## Series

util-series (umbrella: nlink-jp/util-series)

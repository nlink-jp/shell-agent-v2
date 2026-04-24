# CLAUDE.md — shell-agent-v2

## Overview

macOS local-first chat & agent tool with interactive data analysis.
Wails v2 (Go + React) desktop application. Successor to shell-agent v1.

## Build

- Always `cd app && make build`
- Development: `cd app && make dev`
- Tests: `cd app && make test`

## Architecture

- **bindings.go** — Wails bindings (thin delegation to internal packages)
- **internal/agent/** — Agent state machine (Idle/Busy), execution loop
- **internal/chat/** — Chat engine, message building, system prompt, temporal context
- **internal/llm/** — Backend abstraction (local OpenAI-compatible + Vertex AI)
- **internal/analysis/** — DuckDB engine (session-scoped lifecycle)
- **internal/memory/** — Hot/Warm/Cold tiers, sessions, pinned memory
- **internal/findings/** — Global findings store, promotion logic, origin linking
- **internal/toolcall/** — Shell script registry, job workspace, MITL
- **internal/mcp/** — mcp-guardian stdio
- **internal/objstore/** — Central object repository (images/blobs)
- **internal/config/** — JSON config with path expansion
- **internal/logger/** — Structured logging
- **frontend/src/** — React UI

## Key Design Decisions

- **Idle/Busy** — Agent occupies session exclusively during work. Chat input blocked in Busy state. Session switch requires abort.
- **Session-scoped analysis** — Each session owns its own DuckDB. No global shared database.
- **Findings** — Analysis insights promoted to global store with origin session provenance. Separate from Pinned Memory.
- **Hybrid LLM** — Local + Vertex AI, switchable via `/model` command. Default configurable in settings.
- **Temporal context** — System prompt includes day-of-week + yesterday. `resolve-date` builtin tool for complex relative dates.
- **Thin bindings** — Wails binding layer delegates to agent package. No business logic in bindings.
- **Agent loop** — Simple tool-calling feedback (not ReAct). Suitable for local LLMs.
- **MITL** — Required for write/execute tool categories, not read.
- **No launcher app** — Direct application launch only.

## Series

util-series (umbrella: nlink-jp/util-series)

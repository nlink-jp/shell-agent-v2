# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.1.0] - Unreleased

### Added

- Agent state machine with Idle/Busy execution model and UI lockout
- Session-scoped DuckDB analysis engine with lazy initialization
- Table description persistence via `COMMENT ON TABLE`
- Dual LLM backend: Local (OpenAI-compatible, SSE streaming) and Vertex AI (google/genai SDK)
- `/model` command for runtime LLM backend switching
- Enriched temporal context: day-of-week, yesterday in system prompt
- `resolve-date` system tool for deterministic relative date calculation
- Global Findings store with origin session provenance and date labels
- Autonomous finding promotion via `promote-finding` tool
- `/finding` and `/findings` chat commands
- Data analysis tools: load-data, describe-data, query-sql, list-tables, reset-analysis
- Dynamic tool filtering based on data presence
- Shell script Tool Calling with header-based auto-discovery and MITL categories
- Pinned Memory (cross-session persistent facts, separate from Findings)
- Hot/Warm/Cold memory compaction with LLM summarization
- MCP integration via mcp-guardian stdio (JSON-RPC 2.0)
- Object store for images and blobs (12-char hex IDs)
- Settings UI: dual backend configuration, save/load
- Findings panel in sidebar with session link navigation
- Abort mechanism with context cancellation
- 95 passing tests

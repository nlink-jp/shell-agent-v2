# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.1.0] - Unreleased

### Added

- Agent state machine with Idle/Busy execution model and UI lockout
- Session-scoped DuckDB analysis engine with lazy initialization
- Dual LLM backend: Local (LM Studio) and Vertex AI (Gemini) with runtime switching
- LLM abstraction layer: role mapping in backends, tool format conversion, multimodal
- Vertex AI: native FunctionCall/FunctionResponse, tool definitions, image Parts
- Central object storage with session affinity (TypeImage, TypeBlob, TypeReport)
- `list-objects` and `get-object` LLM tools for object repository access
- Frontend `object:ID` URL resolution via ObjectImage component
- Report display: dedicated container with Expand/Copy/Save, fullscreen modal
- Reports stored in objstore as TypeReport with object ID reference
- Data analysis tools: load-data, query-sql, query-preview, suggest-analysis,
  quick-summary, describe-data, list-tables, reset-analysis, create-report
- Dynamic tool filtering based on data presence
- JSON/JSONL data loading support
- Shell script Tool Calling with header-based auto-discovery and MITL categories
- Memory: Hot/Warm/Cold compaction with LLM summarization (connected post-response)
- Autonomous Pinned Memory extraction (bilingual, post-response background task)
- Global Findings store with origin session provenance
- Markdown rendering with syntax highlighting (ReactMarkdown + rehype-highlight)
- Session management: create, list, delete, rename, auto-title, startup restore
- Image input: drag/drop, paste, file picker with objstore persistence
- Image display: inline in Markdown, lightbox viewer
- 4 themes: Dark, Light, Warm, Midnight (CSS custom properties)
- Settings modal with auto-save
- MITL approval UI for write/execute tools
- MCP integration via mcp-guardian stdio (JSON-RPC 2.0)
- File-based logging (app.log)
- LM Studio integration tests (7 tests, `-tags lmstudio`)
- Vertex AI integration tests (6 tests, `-tags vertexai`)
- Context cancellation propagation to tool LLM calls
- Session deletion cleans up associated objstore objects
- `resolve-date` system tool for deterministic relative date calculation
- Enriched temporal context: day-of-week, yesterday in system prompt
- `/model`, `/finding`, `/findings` chat commands

### Fixed

- Tool calling loop: gemma-4 tool role handling (tool→user mapping in Local backend)
- Agent loop: Chat() for all rounds (non-streaming) to prevent gemma tag leakage
- Session auto-save after each record mutation (crash resilience)
- Report persistence in session records (survives reload)
- Empty assistant messages not recorded (only tool call requests + non-empty text)

### Design Documents

- `docs/en/agent-data-flow.md` — agent loop, session records, memory compaction
- `docs/en/object-storage.md` — central object storage design
- `docs/en/llm-abstraction.md` — LLM backend abstraction layer
- `docs/en/shell-agent-v2-rfp.md` — requirements specification

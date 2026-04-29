# shell-agent-v2

macOS local-first chat & agent tool with interactive data analysis.

Successor to [shell-agent](https://github.com/nlink-jp/shell-agent) v0.7.x,
redesigned with session-scoped analysis, an Idle/Busy agent execution model,
and hybrid LLM backend (Local + Vertex AI).

## Features

- **Interactive data analysis** — dialogue-driven exploration with embedded DuckDB
- **Session-scoped analysis** — each session owns its own database, no cross-session state leakage
- **Agent execution model** — Idle/Busy states with UI lockout during processing
- **Hybrid LLM backend** — Local LLM (LM Studio) and Vertex AI (Gemini), switchable at runtime via `/model`
- **Per-backend context budgets** — `HotTokenLimit` and `ContextBudget` configured separately for Local and Vertex (Settings → Local/Vertex AI). The global memory limits stay as a fallback.
- **Memory v2 (opt-in)** — non-destructive context build: records stay full-fidelity, the LLM context is derived per call from `internal/contextbuild`, older portions condensed via a content-keyed summary cache, time-range markers added so the model can reason about *when* events happened. See [memory-architecture-v2.md](docs/en/memory-architecture-v2.md).
- **Container sandbox (opt-in)** — eight `sandbox-*` tools that execute shell or Python in a per-session `podman`/`docker` container with `/work` mounted from the session's data dir, MITL-gated, network-off by default. Includes `sandbox-load-into-analysis` (CSV/JSON in `/work` → DuckDB) and `sandbox-export-sql` (SQL query → CSV in `/work`) so query results flow between analysis and Python without round-tripping through chat. See [sandbox-execution.md](docs/en/sandbox-execution.md) for the macOS setup guide.
- **Global Findings** — promote analysis insights to cross-session knowledge with origin provenance
- **Shell script Tool Calling** — register scripts as tools with MITL approval for write/execute
- **Bundled scripts** — `file-info`, `preview-file`, `list-files`, `weather`, `get-location`, `write-note`. Auto-installed on first launch via `go:embed`; user customizations are preserved.
- **Tool-call timeline** — every tool start/end appears inline in the chat as a transient pill, in addition to the existing status-bar indicator. Ephemeral, not persisted.
- **MCP support** — via mcp-guardian stdio proxy
- **Multi-turn memory** — Hot/Warm/Cold three-tier sliding window with timestamps
- **Pinned Memory** — persistent cross-session facts (rendered with a `(learned YYYY-MM-DD)` suffix so the model can weigh recency)
- **Multimodal** — image input via drag & drop, paste, or file picker
- **Object repository panel** — sidebar Objects tab lists every image / report / blob; click a report to preview, bulk-select to delete (with reference scan that warns when an object is still in use elsewhere) or per-row export.
- **Bulk select / delete** — Findings and Pinned Memory entries can be checked individually or all-at-once, with two-click confirm.
- **Temporal context** — enriched date/time injection + `resolve-date` system tool

## Installation

```bash
cd app
make build
# Output: dist/shell-agent-v2.app
```

## Configuration

Settings stored at `~/Library/Application Support/shell-agent-v2/config.json`.

### LLM Backend

```bash
# In chat:
/model           # Show current engine
/model local     # Switch to local LLM
/model vertex    # Switch to Vertex AI
```

### Vertex AI Setup

```bash
gcloud auth application-default login
# Requires roles/aiplatform.user
```

## Requirements

- macOS 10.15+
- LM Studio (for local backend) — Apple Silicon M1/M2 Pro+ recommended
- GCP project with billing enabled (for Vertex AI backend)

## Building

```bash
cd app
make build      # Build .app bundle
make dev        # Development with hot reload
make test       # Run tests
```

## Documentation

- [Architecture overview](docs/en/shell-agent-v2-architecture.md)
- [Agent data flow & state control](docs/en/agent-data-flow.md)
- [Memory architecture v2 design](docs/en/memory-architecture-v2.md)
- [Sandbox execution design + macOS setup](docs/en/sandbox-execution.md)
- [Object storage design](docs/en/object-storage.md)
- [LLM backend abstraction](docs/en/llm-abstraction.md)
- [RFP (English)](docs/en/shell-agent-v2-rfp.md) · [RFP (Japanese)](docs/ja/shell-agent-v2-rfp.ja.md)

Japanese mirrors live under `docs/ja/`.

## License

MIT

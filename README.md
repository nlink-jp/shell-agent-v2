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
- **Global Findings** — promote analysis insights to cross-session knowledge with origin provenance
- **Shell script Tool Calling** — register scripts as tools with MITL approval for write/execute
- **MCP support** — via mcp-guardian stdio proxy
- **Multi-turn memory** — Hot/Warm/Cold three-tier sliding window with timestamps
- **Pinned Memory** — persistent cross-session facts
- **Multimodal** — image input via drag & drop, paste, or file picker
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

- [RFP (English)](docs/en/shell-agent-v2-rfp.md)
- [RFP (Japanese)](docs/ja/shell-agent-v2-rfp.ja.md)

## License

MIT

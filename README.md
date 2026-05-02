# shell-agent-v2

macOS local-first chat & agent tool with interactive data analysis.

Successor to [shell-agent](https://github.com/nlink-jp/shell-agent) v0.7.x,
redesigned with session-scoped analysis, an Idle/Busy agent execution model,
and hybrid LLM backend (Local + Vertex AI).

## Features

- **Interactive data analysis** — dialogue-driven exploration with embedded DuckDB. Every analysis tool (`load-data`, `query-sql`, `describe-data`, `analyze-data`, etc.) is exposed to the LLM every round so the model can plan multi-step workflows up front instead of discovering tools round-by-round. See [agent-tool-visibility.md](docs/en/agent-tool-visibility.md). Set `tools.hide_analysis_tools_until_data_loaded: true` in `config.json` to restore the pre-v0.1.21 hide-until-load behaviour (opt-in for weaker local backends).
- **Session-scoped analysis** — each session owns its own database, no cross-session state leakage
- **Agent execution model** — Idle/Busy states with UI lockout during processing
- **Hybrid LLM backend** — Local LLM (LM Studio) and Vertex AI (Gemini), switchable at runtime via `/model`
- **Per-backend context budgets** — `HotTokenLimit` and `ContextBudget` configured separately for Local and Vertex (Settings → Local/Vertex AI). The global memory limits stay as a fallback.
- **Memory v2 (opt-in)** — non-destructive context build: records stay full-fidelity, the LLM context is derived per call from `internal/contextbuild`, older portions condensed via a content-keyed summary cache, time-range markers added so the model can reason about *when* events happened. See [memory-architecture-v2.md](docs/en/memory-architecture-v2.md).
- **Container sandbox (opt-in)** — eight `sandbox-*` tools that execute shell or Python in a per-session `podman`/`docker` container with `/work` mounted from the session's data dir, MITL-gated, network-off by default. Includes `sandbox-load-into-analysis` (CSV/JSON in `/work` → DuckDB) and `sandbox-export-sql` (SQL query → CSV in `/work`) so query results flow between analysis and Python without round-tripping through chat. See [sandbox-execution.md](docs/en/sandbox-execution.md) for the macOS setup guide.
- **Global Findings** — promote analysis insights to cross-session knowledge with origin provenance
- **Shell script Tool Calling** — register scripts as tools with MITL approval for write/execute. Per-tool `@timeout: N` header (seconds) overrides the 30-second default for legitimately long-running tools — see [agent-tool-visibility.md](docs/en/agent-tool-visibility.md) and [tool-execution-timeout.md](docs/en/tool-execution-timeout.md). Scripts can write to `$SHELL_AGENT_WORK_DIR` (the same physical directory the sandbox bind-mounts at `/work`); use the built-in `register-object` tool to surface the artefact in chat as `object:<ID>` — see [work-dir-shell-bridge.md](docs/en/work-dir-shell-bridge.md).
- **MITL approval, end-to-end** — every tool source (analysis / shell / sandbox / MCP) routes through one gate. Destructive analysis tools (`load-data`, `reset-analysis`, `promote-finding`) and SQL/analyze prompts are MITL-by-default; metadata reads (`describe-data`, `list-tables`, etc.) are not. Override per-tool from **Settings → Tools** — the toggle reflects the actual dispatcher default. See [security-hardening-2.md](docs/en/security-hardening-2.md).
- **Bundled scripts** — `file-info`, `preview-file`, `list-files`, `weather`, `get-location`, `write-note`. Auto-installed on first launch via `go:embed`; user customizations are preserved.
- **Tool-call timeline** — every tool start/end appears inline in the chat as a transient pill, in addition to the existing status-bar indicator. The pill is restored on session reload as a compact tool-name + status (success / error) bubble; live argument and result text remain ephemeral. See [tool-event-restore.md](docs/en/tool-event-restore.md).
- **Background task visibility** — when the agent kicks off post-response work (title generation, memory compaction, pinned-fact extraction), a small badge appears in the input-status-bar naming what's running. The input field stays disabled until those tasks finish, so the next user message can't race them and lose pinned facts. See [background-task-indicator.md](docs/en/background-task-indicator.md).
- **MCP support** — via mcp-guardian stdio proxy
- **Multi-turn memory** — Hot/Warm/Cold three-tier sliding window with timestamps
- **Pinned Memory** — persistent cross-session facts (rendered with a `(learned YYYY-MM-DD)` suffix so the model can weigh recency)
- **Multimodal** — image input via drag & drop, paste, or file picker
- **Per-session Data panel** — collapsible disclosure at the top of the chat pane showing the current session's objects (images / reports / blobs as cards with thumbnails), DuckDB tables (click for a 20-row preview), and sandbox `/work` files. Click an image for the lightbox, a report for the markdown viewer, or a CSV / text blob for an in-app preview — CSV / TSV render as an HTML table, other text MIMEs (JSON, plain text, HTML, etc.) drop to a fixed-width pre. Bulk-select and delete with separate Yes / No confirmation.
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

### Settings reference

Knobs exposed in the **Settings** dialog. Per-backend values
override the legacy top-level fallbacks in `config.json`.

#### Agent loop

| Setting | JSON path | Default | Notes |
|---|---|---|---|
| Max tool rounds per message | `agent.max_tool_rounds` | 10 | Hard cap on tool-call rounds for one user message. The loop-detection ring buffer (Feature 1, v0.1.16) catches stuck same-error stretches early, so raising this is reasonably safe when a long, legitimate analysis legitimately needs more rounds. |

#### Per-backend context budget (Local / Vertex AI)

| Setting | JSON path | Default (Local) | Default (Vertex) | Notes |
|---|---|---|---|---|
| Hot Token Limit | `llm.{local,vertex_ai}.hot_token_limit` | 4096 | 65536 | Compaction trigger. When the total token count of the Hot tier exceeds this, the oldest Hot records are summarised into Warm. |
| Max Context Tokens | `llm.{local,vertex_ai}.context_budget.max_context_tokens` | 16384 | 524288 | Total token budget sent to the model per call. 0 = unlimited. |
| Max Warm Summary Tokens | `llm.{local,vertex_ai}.context_budget.max_warm_tokens` | 1024 | 16384 | Cap for the warm-summary block. Older summaries are dropped past this. |
| Max Tool-Result Tokens | `llm.{local,vertex_ai}.context_budget.max_tool_result_tokens` | 2048 | 32768 | Per-tool-result truncation before insertion into the LLM message list. |
| Output Reserve | `llm.{local,vertex_ai}.context_budget.output_reserve` | 4096 | 4096 | Tokens reserved for the model's reply. Subtracted from `max_context_tokens` before context packing, so the request stays under the model's window. |
| Per-request timeout (s) | `llm.{local,vertex_ai}.request_timeout_seconds` | 300 | 180 | Per-attempt cap inside the retry layer. |
| Retry max attempts | `llm.{local,vertex_ai}.retry_max_attempts` | 3 | 3 | Total LLM call attempts including the first (1 = no retries). Settings → Local LLM / Vertex AI surface this. |
| Retry backoff base (s) | `llm.{local,vertex_ai}.retry_backoff_base_seconds` | 5 | 5 | Initial backoff between retries. Doubles on each subsequent retry, capped at the max below. Config-only — not in Settings UI. |
| Retry backoff max (s) | `llm.{local,vertex_ai}.retry_backoff_max_seconds` | 120 | 120 | Cap on the per-retry wait. Config-only. |
| Retry jitter (s) | `llm.{local,vertex_ai}.retry_jitter_seconds` | 1 | 1 | Uniform `±jitter` randomisation around each backoff. Config-only. |

#### Sandbox (`sandbox.*`)

| Setting | JSON path | Default | Notes |
|---|---|---|---|
| Enabled | `sandbox.enabled` | false | Master toggle. When off, the eight `sandbox-*` tools are not registered. |
| Engine | `sandbox.engine` | `auto` | `auto` picks `podman` then `docker` from PATH. |
| Image | `sandbox.image` | (empty until you Build) | Active container image. Locally-built images (`shell-agent-v2-sandbox:<sha>`) and `@sha256:`-pinned references are treated as safe; mutable upstream tags (e.g. `python:3.12-slim`) trigger an advisory banner in the Settings → Sandbox tab. |
| Max output bytes | `sandbox.max_output_bytes` | `8388608` (8 MiB) | Per-`exec` cap on each of stdout / stderr. Excess is dropped with a `[output truncated at N bytes]` marker — defends against an LLM-issued `cat /dev/zero` etc. OOMing the app. Config-only; no UI surface. |
| Network | `sandbox.network` | false | Egress; default off. |
| CPU limit | `sandbox.cpu_limit` | `2` | Passed to `--cpus`. |
| Memory limit | `sandbox.memory_limit` | `1g` | Passed to `--memory`. |
| Per-call timeout (s) | `sandbox.timeout_seconds` | 60 | Per-`exec` cap. |

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
- [Background task indicator](docs/en/background-task-indicator.md)
- [Tool-event restore on session reload](docs/en/tool-event-restore.md)
- [Tool-call round-trip (Vertex / Local)](docs/en/tool-call-roundtrip.md)
- [Security hardening (round 1, v0.1.18)](docs/en/security-hardening.md)
- [Security hardening (round 2, v0.1.20)](docs/en/security-hardening-2.md)
- [Agent tool visibility (v0.1.21)](docs/en/agent-tool-visibility.md)
- [Shell tool execution timeout (`@timeout: N`)](docs/en/tool-execution-timeout.md)
- [Shell tool ↔ /work bridge (`SHELL_AGENT_WORK_DIR`, `register-object`)](docs/en/work-dir-shell-bridge.md)
- [RFP (English)](docs/en/shell-agent-v2-rfp.md) · [RFP (Japanese)](docs/ja/shell-agent-v2-rfp.ja.md)

Japanese mirrors live under `docs/ja/`.

## License

MIT

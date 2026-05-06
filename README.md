# shell-agent-v2

macOS local-first chat & agent tool with interactive data analysis.

Successor to [shell-agent](https://github.com/nlink-jp/shell-agent) v0.7.x,
redesigned with session-scoped analysis, an Idle/Busy agent execution model,
and hybrid LLM backend (Local + Vertex AI).

## Features

- **Interactive data analysis** — dialogue-driven exploration with embedded DuckDB. Every analysis tool (`load-data`, `query-sql`, `describe-data`, `analyze-data`, etc.) is exposed to the LLM every round so the model can plan multi-step workflows up front instead of discovering tools round-by-round. See [agent-tool-visibility.md](docs/en/history/agent-tool-visibility.md). Set `tools.hide_analysis_tools_until_data_loaded: true` in `config.json` to restore the pre-v0.1.21 hide-until-load behaviour (opt-in for weaker local backends).
- **Session-scoped analysis** — each session owns its own database, no cross-session state leakage
- **Agent execution model** — Idle/Busy states with UI lockout during processing
- **Hybrid LLM backend** — Local LLM (LM Studio) and Vertex AI (Gemini), switchable at runtime via `/model`
- **Per-backend context budgets** — `ContextBudget` configured separately for Local and Vertex (Settings → Local/Vertex AI).
- **Memory model (v0.2.0 rewrite)** — four facilities work together. Records (immutable conversation history) live in `chat.json`. **Session Memory** auto-extracts `fact` / `context` per session. **Findings** are session-scoped data-analysis discoveries surfaced in a dedicated chat-pane panel. **Global Memory** holds `preference` / `decision` across sessions. Auto-extraction routes by category; "Pin to Global Memory" is the explicit user action that promotes a Session Memory entry or a Finding into the cross-session pool. Context-budget enforcement is non-destructive (`internal/contextbuild` summary cache). See [memory-model.md](docs/en/memory-model.md).
- **Container sandbox (opt-in)** — eight `sandbox-*` tools that execute shell or Python in a per-session `podman`/`docker` container with `/work` mounted from the session's data dir, MITL-gated, network-off by default. Includes `sandbox-load-into-analysis` (CSV/JSON in `/work` → DuckDB) and `sandbox-export-sql` (SQL query → CSV in `/work`) so query results flow between analysis and Python without round-tripping through chat. See [sandbox-execution.md](docs/en/history/sandbox-execution.md) for the macOS setup guide.
- **Findings panel** — chat-pane disclosure with severity filter, free-text search, bulk delete, real-time refresh, and a Pin-to-Global-Memory star button per row.
- **Shell script Tool Calling** — register scripts as tools with MITL approval for write/execute. Per-tool `@timeout: N` header (seconds) overrides the 30-second default for legitimately long-running tools — see [agent-tool-visibility.md](docs/en/history/agent-tool-visibility.md) and [tool-execution-timeout.md](docs/en/history/tool-execution-timeout.md). Scripts can write to `$SHELL_AGENT_WORK_DIR` (the same physical directory the sandbox bind-mounts at `/work`); use the built-in `register-object` tool to surface the artefact in chat as `object:<ID>` — see [work-dir-shell-bridge.md](docs/en/history/work-dir-shell-bridge.md).
- **MITL approval, end-to-end** — every tool source (analysis / shell / sandbox / MCP) routes through one gate. Destructive analysis tools (`load-data`, `reset-analysis`, `promote-finding`) and SQL/analyze prompts are MITL-by-default; metadata reads (`describe-data`, `list-tables`, etc.) are not. Override per-tool from **Settings → Tools** — the toggle reflects the actual dispatcher default. See [security-hardening-2.md](docs/en/history/security-hardening-2.md).
- **Bundled scripts** — `file-info`, `preview-file`, `list-files`, `weather`, `get-location`, `write-note`. Auto-installed on first launch via `go:embed`; user customizations are preserved.
- **Tool-call timeline** — every tool start/end appears inline in the chat as a transient pill, in addition to the existing status-bar indicator. The pill is restored on session reload as a compact tool-name + status (success / error) bubble; live argument and result text remain ephemeral. See [tool-event-restore.md](docs/en/history/tool-event-restore.md).
- **Background task visibility** — when the agent kicks off post-response work (title generation, memory extraction), a small badge appears in the input-status-bar naming what's running. The input field stays disabled until those tasks finish, so the next user message can't race them and lose extracted facts. See [background-task-indicator.md](docs/en/history/background-task-indicator.md).
- **MCP support** — via mcp-guardian stdio proxy
- **Multimodal** — image input via drag & drop, paste, or file picker
- **Per-session Data panel** — collapsible disclosure at the top of the chat pane showing the current session's objects (images / reports / blobs as cards with thumbnails), DuckDB tables (click for a 20-row preview), and sandbox `/work` files. Click an image for the lightbox, a report for the markdown viewer, or a CSV / text blob for an in-app preview — CSV / TSV render as an HTML table, other text MIMEs (JSON, plain text, HTML, etc.) drop to a fixed-width pre. Bulk-select and delete with separate Yes / No confirmation.
- **Bulk select / delete** — Findings, Global Memory, and Session Memory entries can be checked individually or all-at-once, with two-click confirm.
- **Session import / export** — package a complete session (chat, session memory, findings, summaries, sandbox `work/`, analysis DuckDB, and every objstore object the session owns) into a single `.shellagent` ZIP bundle and re-import it on the same or a different machine. Per-row Export icon in the sidebar, Import Chat button in the bottom-nav, `/export` and `/import` slash commands. Privacy flag preserved across the round-trip; object IDs are always regenerated on import with bounded reference rewriting in `chat.json` and `summaries.json`. See [session-import-export.md](docs/en/session-import-export.md).
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

### Cross-session memory trust

shell-agent-v2 auto-extracts important facts from each conversation.
Cross-session entries (Global Memory) are re-injected into every
future session's system prompt as authoritative context — which
means anything that ever appears in an *assistant* turn (a quoted
CSV cell, an MCP response, OCR'd image text, a fetched web page)
can structurally end up steering future sessions. Each entry
carries a provenance tag:

- **user-stated** — came from a user turn, a manual pin, or an
  explicit "Pin to Global Memory" promotion. Treated as
  authoritative.
- **derived** — extracted from an assistant turn, or a finding the
  LLM promoted via `promote-finding`. Lower trust because the
  content traces back through the LLM and may carry attacker-
  influenced bytes.

The sidebar (Global / Session Memory) and the chat-pane Findings
panel show the badge inline. If a fact starts driving weird
behaviour (the recoverable case being the THINK leak that prompted
this hardening), open the relevant list, select the offending
entries, and bulk-delete them. See
[docs/en/history/memory-injection-hardening.md](docs/en/history/memory-injection-hardening.md)
for the full threat model and
[docs/en/memory-model.md](docs/en/memory-model.md) for the v0.2.0
4-facility design.

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

Current state of the system:

- [**Architecture overview (v0.2.0)**](docs/en/architecture.md) ⭐ start here
- [**Memory model**](docs/en/memory-model.md) — 4-facility design
- [**Data analysis**](docs/en/data-analysis.md) — DuckDB engine, sliding-window analyze-data, Findings lifecycle

Past design notes are kept under [`docs/en/history/`](docs/en/history/)
as the audit trail behind v0.2.0. Some no longer reflect current
behaviour — see the README in that directory for an annotated
index. Notable still-current ones:

- [Sandbox execution + macOS setup](docs/en/history/sandbox-execution.md)
- [Object storage design](docs/en/history/object-storage.md)
- [LLM backend abstraction](docs/en/history/llm-abstraction.md)
- [Tool-event restore on session reload](docs/en/history/tool-event-restore.md)
- [Tool-call round-trip (Vertex / Local)](docs/en/history/tool-call-roundtrip.md)
- [Security hardening (round 2, v0.1.20)](docs/en/history/security-hardening-2.md)
- [Shell tool execution timeout (`@timeout: N`)](docs/en/history/tool-execution-timeout.md)
- [Shell tool ↔ /work bridge (`SHELL_AGENT_WORK_DIR`, `register-object`)](docs/en/history/work-dir-shell-bridge.md)
- [RFP (English)](docs/en/history/shell-agent-v2-rfp.md) · [RFP (Japanese)](docs/ja/history/shell-agent-v2-rfp.ja.md)

Japanese mirrors live under `docs/ja/`.

## License

MIT

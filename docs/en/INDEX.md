# Documentation Index

This is the entry point for shell-agent-v2's maintainer-facing
documentation. For user-facing material see
[`README.md`](../../README.md); for contributor onboarding see
[`CONTRIBUTING.md`](../../CONTRIBUTING.md).

Japanese mirror: [`INDEX.ja.md`](../ja/INDEX.ja.md) (full parity).

## Reference

Current behaviour. Evergreen — updated in place as the code evolves.

- [`reference/architecture.md`](reference/architecture.md) — system
  overview, package layout, dispatch flow
- [`reference/memory-model.md`](reference/memory-model.md) —
  4-facility memory architecture
- [`reference/data-analysis.md`](reference/data-analysis.md) — DuckDB
  engine, analyze-data sliding window, findings lifecycle
- [`reference/privacy-controls.md`](reference/privacy-controls.md) —
  private sessions, log-level filter
- [`reference/system-rules.md`](reference/system-rules.md) —
  user-authored standing instructions injected into the system
  prompt (v0.7.0)

## ADRs (Architecture Decision Records)

Point-in-time design decisions. Immutable after acceptance;
superseded by a new ADR if the underlying decision changes (typo
fixes and link updates are exempt). Numbering is sequential in
release order.

- [`ADR-0001`](adr/0001-session-import-export.md) — `.shellagent`
  bundle format, ID regeneration (v0.4.0)
- [`ADR-0002`](adr/0002-tool-progress-events.md) — `tool_progress`
  activity event for in-place bubble updates (v0.4.1)
- [`ADR-0003`](adr/0003-session-delete-ux.md) — session deletion
  under the agent state machine (v0.4.2)
- [`ADR-0004`](adr/0004-sandbox-uid-mapping.md) — keep-id userns
  remap for corp / LDAP large host UIDs (v0.4.3)
- [`ADR-0005`](adr/0005-analyze-data-row-cap.md) — split chat-output
  cap from sliding-window analyze cap (v0.4.4)
- [`ADR-0006`](adr/0006-markdown-attachments.md) — `TypeMarkdown`
  object type + `analyze-text` / `grep-text` / `get-text` tools
  (v0.5.0)
- [`ADR-0007`](adr/0007-tool-registry-refactor.md) — `ToolDescriptor`
  as single source of truth across the tool registry (v0.6.0)
- [`ADR-0008`](adr/0008-mcp-abort.md) — MCP tool-call abort via
  `CallToolContext` + per-guardian restart (v0.6.1)
- [`ADR-0009`](adr/0009-gemini-thought-signatures.md) — Gemini 3+
  thought-signature capture and replay across tool-use turns
  (v0.6.2)
- [`ADR-0010`](adr/0010-duckdb-result-rendering.md) — DuckDB
  result rendering: type-dispatched scalar conversion (UUID,
  BLOB, DECIMAL, INTERVAL, MAP, TIME) across Preview / QuerySQL
  / CSV paths (v0.6.4, Phase 1)
- [`ADR-0011`](adr/0011-timestamptz-local-render.md) — TIMESTAMPTZ
  rendering: convert to `time.Local` with explicit offset
  (supersedes ADR-0010 §2 TIMESTAMPTZ deferral) (v0.6.5)
- [`ADR-0012`](adr/0012-system-rules.md) — System Rules: user-
  authored Markdown file injected near the top of the system
  prompt; separate from the four memory facilities (v0.7.0)
- [`ADR-0013`](adr/0013-saved-query-tables.md) — Saved-query
  derived tables: single `save-query` tool that materialises a
  SELECT result as a new DuckDB base table; `analyze-data` runs
  sliding-window analysis on the filtered slice via its existing
  `table` parameter; no engine schema or bundle-format changes
  (v0.8.0)
- [`ADR-0014`](adr/0014-object-link-rendering.md) — Object-link
  rendering: symmetric `a`-component override for
  `[name](object:ID)` previews matching the existing `img`
  override; new `ObjectLink` component + `GetObjectMeta` binding;
  type-aware `resolveObjectRefsForExport`; centralised
  object-aware markdown defaults to retire 3-site parallel-list
  drift; codifies the `Image`/`Document` anchor input-only rule
  (v0.9.0)
- [`ADR-0015`](adr/0015-deferred-extraction-send.md) — Deferred
  extraction + single-slot send queue: UI unlocks immediately
  after the response is delivered while `extractMemories` keeps
  running in background; a SEND issued during background
  extraction is queued single-slot and auto-fires once
  extraction completes, so the next turn's `BuildSystemPrompt`
  always sees the prior turn's facts. Zero fact loss, no
  abort — only the UI gate changes (v0.11.0, implemented)
- [`ADR-0016`](adr/0016-multi-profile-llm-backend.md) — Multi-profile
  LLM backend: replace the single (Local, Vertex, default) triple
  with a list of named profiles + a default-profile pointer;
  `UnmarshalJSON` migrates v0.11.x configs on first load (v0.12.0)
- [`ADR-0017`](adr/0017-prompt-prefix-stability.md) — Prompt prefix
  stability for KV-cache reuse: keep the system-prompt prefix
  byte-identical across turns so local llama.cpp reuses its
  single prefix-KV slot instead of cold re-encoding (v0.13.0)
- [`ADR-0018`](adr/0018-guard-nonce-stability.md) — Guard nonce
  stability: hold the prompt-injection guard nonce stable within a
  session so it doesn't bust the KV-cache prefix every turn (v0.13.1)
- [`ADR-0019`](adr/0019-llm-driven-memory-tool.md) — LLM-driven
  memory tool + extraction toggle: explicit memory tool plus an
  `auto_extract_enabled` switch (local default off to protect the
  prefix-KV cache, vertex default on) (v0.13.2)
- [`ADR-0020`](adr/0020-title-generation-toggle.md) — Title
  generation toggle: `auto_title_enabled` gates the one-shot
  title-gen LLM call, heuristic fallback when off (v0.13.2)
- [`ADR-0021`](adr/0021-state-machine-consistency.md) — State machine
  consistency: formal Idle/Busy FSM + authoritative Send response so
  UI state never diverges from agent state (v0.14.0)
- [`ADR-0022`](adr/0022-agent-file-decomposition.md) — Minimum-viable
  decomposition of the monolithic `agent.go` into focused files
  without behaviour change (v0.14.3)
- [`ADR-0023`](adr/0023-tool-name-normalization.md) — Canonical
  `snake_case` tool names + boundary normalization: built-ins/bundled
  renamed, kebab inputs normalised at the edge so MCP servers
  publishing kebab names keep working (v0.14.5)
- [`ADR-0024`](adr/0024-non-blocking-startup-session-restore.md) —
  Non-blocking startup + deterministic session restore: window sized
  at creation, external init on a goroutine behind a readiness gate
  (v0.14.7)
- [`ADR-0025`](adr/0025-restore-tool-call-assistant-text.md) — Restore
  the assistant's tool-call explanation text on session reload so a
  reopened session reads the same as the live one (v0.14.8)
- [`ADR-0026`](adr/0026-surface-guardian-process-exit-status.md) —
  Surface the MCP guardian process exit status on Start failure
  (`exit status N` / `signal: killed`) instead of an opaque pipe
  symptom (v0.14.9)
- [`ADR-0027`](adr/0027-global-memory-export-import.md) — Global
  Memory export / import: round-trip the entire cross-session memory
  store, omitting machine-local provenance (v0.15.0)
- [`ADR-0028`](adr/0028-drop-unused-global-memory-provenance.md) — Drop
  unused provenance fields (`SessionID`, `SourceTurnIndex`,
  `PromotedFromID`) from Global Memory entries; simplifies ADR-0027
  (v0.15.0)
- [`ADR-0029`](adr/0029-configurable-analysis-row-caps.md) —
  Configurable analysis row caps: `max_query_rows` (chat-output,
  default 10 000) and `max_export_rows` (sandbox CSV handoff, default
  1 000 000); fixes `export-sql-to-csv` mis-sharing the chat cap;
  surfaced in Settings → General → Data analysis (v0.16.0)

## History

Pre-v0.2.0 audit trail. Frozen — not updated; consult only for
historical context. See [`history/`](history/) for the annotated
index.

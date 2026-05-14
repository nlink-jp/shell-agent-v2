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

## History

Pre-v0.2.0 audit trail. Frozen — not updated; consult only for
historical context. See [`history/`](history/) for the annotated
index.

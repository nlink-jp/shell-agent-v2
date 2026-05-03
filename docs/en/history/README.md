# shell-agent-v2 — design history

Design notes from previous milestones, kept for the audit trail
behind the v0.2.0 rewrite. **Some of these no longer reflect
current behaviour**; the canonical sources for v0.2.0 are
[`../architecture.md`](../architecture.md) and
[`../memory-model.md`](../memory-model.md).

`../architecture.md` §11 has an annotated index of which docs
here still describe shipped behaviour and which have been
superseded.

Quick map:

- **Still mostly current:** `agent-data-flow.md`,
  `agent-loop-resilience.md`, `sandbox-execution.md`,
  `sandbox-image-build.md`, `security-hardening-2.md`,
  `work-dir-shell-bridge.md`, `object-storage.md`,
  `tool-event-restore.md`, `llm-abstraction.md`,
  `multi-image-handling.md`.
- **Partially superseded:** `information-display-redesign.md`,
  `frontend-decomposition.md`,
  `background-task-indicator.md` (rename only).
- **Superseded by v0.2.0 rewrite:**
  `memory-architecture-v2.md` (the v1 Hot/Warm/Cold rationale —
  the destructive compaction it discusses is gone),
  `memory-injection-hardening.md` (storage design replaced by
  `memory-model.md`; defence list still useful),
  `shell-agent-v2-architecture.md`, `shell-agent-v2-rfp.md`
  (the v0.1 baseline).

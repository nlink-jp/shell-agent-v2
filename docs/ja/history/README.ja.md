# shell-agent-v2 — 設計史

過去マイルストーンの設計メモを v0.2.0 rewrite の audit trail
として保持。**一部は現在の挙動を反映していない**。
v0.2.0 時点の正準資料は
[`../architecture.ja.md`](../architecture.ja.md) と
[`../memory-model.ja.md`](../memory-model.ja.md)。

`../architecture.ja.md` §11 (英語版 `architecture.md` §11 を
参照) に各 doc の現状適用度の注釈付き index あり。

簡易マップ:

- **今もほぼ妥当:** `agent-data-flow.ja.md`,
  `agent-loop-resilience.ja.md`, `sandbox-execution.ja.md`,
  `sandbox-image-build.ja.md`, `security-hardening-2.ja.md`,
  `work-dir-shell-bridge.ja.md`, `object-storage.ja.md`,
  `tool-event-restore.ja.md`, `llm-abstraction.ja.md`,
  `multi-image-handling.ja.md`.
- **一部 superseded:** `information-display-redesign.ja.md`,
  `frontend-decomposition.ja.md`,
  `background-task-indicator.ja.md` (rename のみ)。
- **v0.2.0 rewrite で superseded:**
  `memory-architecture-v2.ja.md` (v1 Hot/Warm/Cold rationale —
  そこで論じている destructive compaction は撤廃済),
  `memory-injection-hardening.ja.md` (ストレージ設計は
  `memory-model.ja.md` に置き換え; 防御リストはなお有用),
  `shell-agent-v2-architecture.ja.md`,
  `shell-agent-v2-rfp.ja.md` (v0.1 baseline)。

# ドキュメントインデックス

shell-agent-v2 メンテナ向けドキュメントの入口。ユーザー向けは
[`README.ja.md`](../../README.ja.md)、新規開発者向けは
[`CONTRIBUTING.ja.md`](../../CONTRIBUTING.ja.md) を参照。

英語版: [`INDEX.md`](../en/INDEX.md) (full parity)。

## Reference

現在の挙動。Evergreen — コード進化に合わせて in-place 更新。

- [`reference/architecture.ja.md`](reference/architecture.ja.md) —
  システム全体像、パッケージ構成、ディスパッチフロー
- [`reference/memory-model.ja.md`](reference/memory-model.ja.md) —
  4-facility メモリアーキテクチャ
- [`reference/data-analysis.ja.md`](reference/data-analysis.ja.md) —
  DuckDB エンジン、analyze-data sliding window、findings ライフ
  サイクル
- [`reference/privacy-controls.ja.md`](reference/privacy-controls.ja.md)
  — プライベートセッション、ログレベルフィルタ
- [`reference/system-rules.ja.md`](reference/system-rules.ja.md) —
  system prompt に注入されるユーザー定義の恒久指示 (v0.7.0)

## ADRs (Architecture Decision Records)

設計判断の point-in-time 記録。承認後 immutable。根本判断が
変わる場合は新 ADR で supersede (誤字修正・リンク更新は例外)。
連番はリリース順。

- [`ADR-0001`](adr/0001-session-import-export.ja.md) —
  `.shellagent` バンドル形式、ID 再生成 (v0.4.0)
- [`ADR-0002`](adr/0002-tool-progress-events.ja.md) —
  `tool_progress` activity event による in-place バブル更新
  (v0.4.1)
- [`ADR-0003`](adr/0003-session-delete-ux.ja.md) — agent state
  machine 下でのセッション削除 (v0.4.2)
- [`ADR-0004`](adr/0004-sandbox-uid-mapping.ja.md) — 企業 / LDAP
  環境での keep-id userns remap (v0.4.3)
- [`ADR-0005`](adr/0005-analyze-data-row-cap.ja.md) — チャット
  出力 cap と sliding-window analyze cap の分離 (v0.4.4)
- [`ADR-0006`](adr/0006-markdown-attachments.ja.md) —
  `TypeMarkdown` オブジェクト型 + `analyze-text` / `grep-text` /
  `get-text` ツール (v0.5.0)
- [`ADR-0007`](adr/0007-tool-registry-refactor.ja.md) —
  `ToolDescriptor` をツールレジストリの単一 source of truth に
  (v0.6.0)
- [`ADR-0008`](adr/0008-mcp-abort.ja.md) — `CallToolContext` +
  単一 guardian 再起動による MCP ツール呼び出し Abort (v0.6.1)
- [`ADR-0009`](adr/0009-gemini-thought-signatures.ja.md) —
  Gemini 3+ thought signature の tool-use ターン間キャプチャ
  ・再生 (v0.6.2)
- [`ADR-0010`](adr/0010-duckdb-result-rendering.ja.md) — DuckDB
  結果レンダリング: 型ディスパッチ式スカラ変換 (UUID, BLOB,
  DECIMAL, INTERVAL, MAP, TIME) を Preview / QuerySQL / CSV 全
  経路で統一 (v0.6.4, Phase 1)
- [`ADR-0011`](adr/0011-timestamptz-local-render.ja.md) —
  TIMESTAMPTZ レンダリング: `time.Local` への変換 + 明示オフ
  セット出力 (ADR-0010 §2 TIMESTAMPTZ 後送りを supersede)
  (v0.6.5)
- [`ADR-0012`](adr/0012-system-rules.ja.md) — System Rules: ユーザー
  がオーサリングする Markdown ファイルを system prompt の冒頭
  近くに注入; 4 メモリ施設とは別系統 (v0.7.0)
- [`ADR-0013`](adr/0013-saved-query-tables.ja.md) — 保存クエリ
  派生テーブル: 単一の `save-query` ツールで SELECT 結果を新規
  DuckDB ベーステーブルとしてマテリアライズ; `analyze-data` は
  既存 `table` パラメータで絞り込み後のスライスに対して
  sliding-window 解析を実行; エンジンスキーマ・バンドルフォーマット
  変更なし (v0.8.0)

## History

v0.2.0 以前の audit trail。Frozen — 更新されない、過去文脈の
参照用のみ。[`history/`](history/) のアノテート index 参照。

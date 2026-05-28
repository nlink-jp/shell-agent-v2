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
- [`ADR-0014`](adr/0014-object-link-rendering.ja.md) —
  オブジェクトリンクレンダリング: 既存 `img` オーバーライドと対称な
  `a` コンポーネントオーバーライドで `[name](object:ID)` を
  プレビュー描画; 新規 `ObjectLink` コンポーネント +
  `GetObjectMeta` バインディング; 型認識
  `resolveObjectRefsForExport`; 3 箇所 parallel-list drift を
  解消する object-aware markdown defaults 集約; `Image`/`Document`
  アンカーの入力専用ルールを成文化 (v0.9.0)
- [`ADR-0015`](adr/0015-deferred-extraction-send.ja.md) —
  抽出遅延化 + シングルスロット送信キュー: レスポンス配信直後に
  UI ロック解除し `extractMemories` はバックグラウンド継続;
  バックグラウンド抽出中の SEND はシングルスロットでキューされ
  抽出完了後に自動発射、次ターン `BuildSystemPrompt` は常に前
  ターン facts を含む memory を読む。Facts 損失ゼロ、abort なし
  — UI gate のみ変更 (v0.11.0, 実装済み)
- [`ADR-0016`](adr/0016-multi-profile-llm-backend.ja.md) — マルチ
  プロファイル LLM バックエンド: 単一 (Local, Vertex, default) の組を
  名前付きプロファイルのリスト + デフォルトプロファイルポインタに置換;
  `UnmarshalJSON` が初回ロード時に v0.11.x config を migrate (v0.12.0)
- [`ADR-0017`](adr/0017-prompt-prefix-stability.ja.md) — KV cache
  再利用のための prompt prefix 安定化: system prompt の prefix を
  ターン間でバイト同一に保ち、ローカル llama.cpp が単一 prefix-KV
  スロットを cold 再エンコードせず再利用するようにする (v0.13.0)
- [`ADR-0018`](adr/0018-guard-nonce-stability.ja.md) — Guard nonce
  安定化: prompt-injection guard nonce をセッション内で固定し、毎ターン
  KV-cache prefix を壊さないようにする (v0.13.1)
- [`ADR-0019`](adr/0019-llm-driven-memory-tool.ja.md) — LLM 主導の
  記憶ツール + 抽出トグル: 明示的な記憶ツール + `auto_extract_enabled`
  スイッチ (local は prefix-KV 保護のためデフォルト off、vertex は on)
  (v0.13.2)
- [`ADR-0020`](adr/0020-title-generation-toggle.ja.md) — タイトル
  生成トグル: `auto_title_enabled` が一発のタイトル生成 LLM 呼び出しを
  gate、off 時はヒューリスティック fallback (v0.13.2)
- [`ADR-0021`](adr/0021-state-machine-consistency.ja.md) — ステート
  マシン整合性: formal な Idle/Busy FSM + 権威ある Send 応答で UI 状態が
  agent 状態から乖離しないようにする (v0.14.0)
- [`ADR-0022`](adr/0022-agent-file-decomposition.ja.md) — モノリシックな
  `agent.go` を挙動を変えずに焦点を絞ったファイル群へ最小限分解 (v0.14.3)
- [`ADR-0023`](adr/0023-tool-name-normalization.ja.md) — tool 名を
  canonical な `snake_case` に + 境界で正規化: 組込/同梱をリネームし、
  kebab 入力を端で正規化することで kebab 名を公開する MCP サーバーも
  動き続ける (v0.14.5)
- [`ADR-0024`](adr/0024-non-blocking-startup-session-restore.ja.md) —
  非ブロッキング startup + 決定論的セッション復元: 窓は生成時にサイズ確定、
  外部 init は readiness gate の背後で goroutine 実行 (v0.14.7)
- [`ADR-0025`](adr/0025-restore-tool-call-assistant-text.ja.md) —
  ツール呼び出しターンのアシスタント説明テキストをセッション復元時に
  復元し、開き直したセッションがライブと同じに読めるようにする (v0.14.8)
- [`ADR-0026`](adr/0026-surface-guardian-process-exit-status.ja.md) —
  MCP guardian の Start 失敗時に不透明なパイプ症状ではなくプロセス終了
  ステータス (`exit status N` / `signal: killed`) を可視化する (v0.14.9)
- [`ADR-0027`](adr/0027-global-memory-export-import.ja.md) — グローバル
  メモリの Export / Import: クロスセッション記憶ストア全体を往復、
  マシンローカルな provenance は除外 (v0.15.0)
- [`ADR-0028`](adr/0028-drop-unused-global-memory-provenance.ja.md) —
  グローバルメモリエントリから未使用 provenance フィールド
  (`SessionID`, `SourceTurnIndex`, `PromotedFromID`) を削除、ADR-0027 を
  簡素化 (v0.15.0)
- [`ADR-0029`](adr/0029-configurable-analysis-row-caps.ja.md) —
  データ分析の行数上限を設定可能化: `max_query_rows` (チャット出力、
  デフォルト 10,000) と `max_export_rows` (サンドボックス CSV 受け渡し、
  デフォルト 1,000,000); `export-sql-to-csv` のチャット上限誤共有を修正;
  Settings → General → Data analysis に露出 (v0.16.0)
- [`ADR-0030`](adr/0030-session-pointer-race.ja.md) — `agentLoop` の
  防御的 nil-init と `generateTitleIfNeeded` の読み出しの間で `a.session`
  ポインタアクセスを同期。Issue #13 調査中に `-race` が捕捉: title 生成
  goroutine と抽出の auto-dispatched SendWithAttachments の両者が
  `a.mu` を保持せずに `a.session` を触っていた。両箇所に lock 下
  スナップショットパターン適用 — 最小、API 変更なし (post-v0.16.0、未リリース)

## History

v0.2.0 以前の audit trail。Frozen — 更新されない、過去文脈の
参照用のみ。[`history/`](history/) のアノテート index 参照。

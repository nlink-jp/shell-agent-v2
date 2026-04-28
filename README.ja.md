# shell-agent-v2

macOS ローカルファースト チャット & エージェントツール（対話的データ分析機能付き）。

[shell-agent](https://github.com/nlink-jp/shell-agent) v0.7.x の後継。
セッションスコープ分析、Idle/Busy エージェント実行モデル、
ハイブリッド LLM バックエンド (Local + Vertex AI) で再設計。

## 機能

- **対話的データ分析** — DuckDB 組み込みによる対話駆動型データ探索
- **セッションスコープ分析** — 各セッションが自身のデータベースを所有、セッション間の状態リークなし
- **エージェント実行モデル** — Idle/Busy 状態による処理中 UI ロックアウト
- **ハイブリッド LLM バックエンド** — ローカル LLM (LM Studio) と Vertex AI (Gemini)、`/model` でランタイム切替
- **per-backend コンテキスト予算** — `HotTokenLimit` と `ContextBudget` をローカルと Vertex で個別設定可（Settings → Local/Vertex AI）。グローバルメモリ設定はフォールバックとして残存
- **メモリ v2 (opt-in)** — 非破壊的コンテキストビルド: レコードは完全忠実度で保持、LLM コンテキストは `internal/contextbuild` がコール毎に導出、古い部分は content-key 付きキャッシュで圧縮、時間レンジマーカでモデルが「いつ起きたか」を認識可能。詳細は [memory-architecture-v2.ja.md](docs/ja/memory-architecture-v2.ja.md)
- **コンテナサンドボックス (opt-in)** — 6 個の `sandbox-*` ツールで shell / Python をセッション毎の `podman`/`docker` コンテナで実行、`/work` をセッションデータディレクトリからマウント、MITL 必須、ネットワーク既定 off。macOS セットアップは [sandbox-execution.ja.md](docs/ja/sandbox-execution.ja.md) 参照
- **グローバル Findings** — 分析知見を来歴付きでセッション横断の知識に昇格
- **シェルスクリプト Tool Calling** — スクリプトをツールとして登録、write/execute は MITL 承認
- **内蔵スクリプト** — `file-info`, `preview-file`, `list-files`, `weather`, `get-location`, `write-note`。初回起動時に `go:embed` から自動インストール、ユーザーカスタマイズは保護
- **ツールコールタイムライン** — ツール開始/終了がチャット内に一時的なピル表示として並ぶ。ステータスバー表示と併存、永続化はしない
- **MCP サポート** — mcp-guardian stdio プロキシ経由
- **マルチターンメモリ** — Hot/Warm/Cold 3層スライディングウィンドウ（タイムスタンプ付き）
- **Pinned Memory** — セッション横断の永続的事実記憶（各事実に `(learned YYYY-MM-DD)` suffix が付き、モデルが新旧を判断可能）
- **マルチモーダル** — ドラッグ&ドロップ、ペースト、ファイルピッカーによる画像入力
- **オブジェクトリポジトリパネル** — サイドバー Objects タブで全 image / report / blob を一覧表示。レポートはクリックでプレビュー、bulk-select 削除（参照スキャン付き、利用中なら警告）、行毎エクスポート可
- **一括選択 / 削除** — Findings と Pinned Memory のエントリを個別/全選択可、2クリック確認
- **時間コンテキスト** — 強化された日時注入 + `resolve-date` システムツール

## インストール

```bash
cd app
make build
# 出力: dist/shell-agent-v2.app
```

## 設定

設定ファイル: `~/Library/Application Support/shell-agent-v2/config.json`

### LLM バックエンド

```bash
# チャット内:
/model           # 現在のエンジンを表示
/model local     # ローカル LLM に切替
/model vertex    # Vertex AI に切替
```

### Vertex AI セットアップ

```bash
gcloud auth application-default login
# roles/aiplatform.user が必要
```

## 要件

- macOS 10.15+
- LM Studio (ローカルバックエンド用) — Apple Silicon M1/M2 Pro+ 推奨
- 課金有効な GCP プロジェクト (Vertex AI バックエンド用)

## ビルド

```bash
cd app
make build      # .app バンドルをビルド
make dev        # ホットリロードで開発
make test       # テスト実行
```

## ドキュメント

- [アーキテクチャ概要](docs/ja/shell-agent-v2-architecture.ja.md)
- [エージェントデータフロー & ステート制御](docs/ja/agent-data-flow.ja.md)
- [メモリアーキテクチャ v2 設計](docs/ja/memory-architecture-v2.ja.md)
- [サンドボックス実行設計 + macOS セットアップ](docs/ja/sandbox-execution.ja.md)
- [オブジェクトストレージ設計](docs/ja/object-storage.ja.md)
- [LLM バックエンド抽象化](docs/ja/llm-abstraction.ja.md)
- [RFP (英語)](docs/en/shell-agent-v2-rfp.md) · [RFP (日本語)](docs/ja/shell-agent-v2-rfp.ja.md)

英語ミラーは `docs/en/` 配下。

## ライセンス

MIT

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
- **グローバル Findings** — 分析知見を来歴付きでセッション横断の知識に昇格
- **シェルスクリプト Tool Calling** — スクリプトをツールとして登録、write/execute は MITL 承認
- **MCP サポート** — mcp-guardian stdio プロキシ経由
- **マルチターンメモリ** — Hot/Warm/Cold 3層スライディングウィンドウ（タイムスタンプ付き）
- **Pinned Memory** — セッション横断の永続的事実記憶
- **マルチモーダル** — ドラッグ&ドロップ、ペースト、ファイルピッカーによる画像入力
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

- [RFP (英語)](docs/en/shell-agent-v2-rfp.md)
- [RFP (日本語)](docs/ja/shell-agent-v2-rfp.ja.md)

## ライセンス

MIT

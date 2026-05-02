# shell-agent-v2

macOS ローカルファースト チャット & エージェントツール（対話的データ分析機能付き）。

[shell-agent](https://github.com/nlink-jp/shell-agent) v0.7.x の後継。
セッションスコープ分析、Idle/Busy エージェント実行モデル、
ハイブリッド LLM バックエンド (Local + Vertex AI) で再設計。

## 機能

- **対話的データ分析** — DuckDB 組み込みによる対話駆動型データ探索。全 analysis tool（`load-data`, `query-sql`, `describe-data`, `analyze-data` 等）を毎ラウンド LLM に公開し、ラウンド毎の発見ではなく事前計画で複数ステップワークフローを組めるようにしている。詳細は [agent-tool-visibility.ja.md](docs/ja/agent-tool-visibility.ja.md)。`config.json` で `tools.hide_analysis_tools_until_data_loaded: true` を設定すると v0.1.20 以前の load 後表示挙動に戻せる（弱いローカルバックエンド向け opt-in）
- **セッションスコープ分析** — 各セッションが自身のデータベースを所有、セッション間の状態リークなし
- **エージェント実行モデル** — Idle/Busy 状態による処理中 UI ロックアウト
- **ハイブリッド LLM バックエンド** — ローカル LLM (LM Studio) と Vertex AI (Gemini)、`/model` でランタイム切替
- **per-backend コンテキスト予算** — `HotTokenLimit` と `ContextBudget` をローカルと Vertex で個別設定可（Settings → Local/Vertex AI）。グローバルメモリ設定はフォールバックとして残存
- **メモリ v2 (opt-in)** — 非破壊的コンテキストビルド: レコードは完全忠実度で保持、LLM コンテキストは `internal/contextbuild` がコール毎に導出、古い部分は content-key 付きキャッシュで圧縮、時間レンジマーカでモデルが「いつ起きたか」を認識可能。詳細は [memory-architecture-v2.ja.md](docs/ja/memory-architecture-v2.ja.md)
- **コンテナサンドボックス (opt-in)** — 8 個の `sandbox-*` ツールで shell / Python をセッション毎の `podman`/`docker` コンテナで実行、`/work` をセッションデータディレクトリからマウント、MITL 必須、ネットワーク既定 off。`sandbox-load-into-analysis`（`/work` の CSV/JSON → DuckDB）と `sandbox-export-sql`（SQL 結果 → `/work` の CSV）でクエリ結果をチャットを介さず分析エンジンと Python の間で受け渡し可能。macOS セットアップは [sandbox-execution.ja.md](docs/ja/sandbox-execution.ja.md) 参照
- **グローバル Findings** — 分析知見を来歴付きでセッション横断の知識に昇格
- **シェルスクリプト Tool Calling** — スクリプトをツールとして登録、write/execute は MITL 承認。per-tool `@timeout: N` ヘッダ (秒) でデフォルト 30 秒キャップを上書き可能 (実行に時間が掛かる正当な tool 向け) — 詳細は [tool-execution-timeout.ja.md](docs/ja/tool-execution-timeout.ja.md)
- **MITL approval（全ツール統一）** — analysis / shell / sandbox / MCP の全 tool source が単一ゲートを経由。破壊的な分析ツール（`load-data`, `reset-analysis`, `promote-finding`）と SQL/analyze プロンプトはデフォルトで MITL 必須、メタデータ系（`describe-data`, `list-tables` 等）は不要。ツール毎に **Settings → Tools** で上書き可能、トグルは dispatcher の実デフォルトと一致する。詳細は [security-hardening-2.ja.md](docs/ja/security-hardening-2.ja.md)
- **内蔵スクリプト** — `file-info`, `preview-file`, `list-files`, `weather`, `get-location`, `write-note`。初回起動時に `go:embed` から自動インストール、ユーザーカスタマイズは保護
- **ツールコールタイムライン** — ツール開始/終了がチャット内に一時的なピル表示として並ぶ。セッション復元時はコンパクトなツール名 + ステータス（success / error）バブルとして再現される（引数と結果本文はライブ時のみ）。詳細は [tool-event-restore.ja.md](docs/ja/tool-event-restore.ja.md)
- **バックグラウンドタスク可視化** — 応答後のタスク（タイトル生成・メモリ圧縮・pinned-fact 抽出）が走っている間、入力ステータスバーに小さいバッジで内容を表示し、入力欄を disable に保つ。次のユーザメッセージがそれらと競合して pinned-fact を取りこぼすことを防ぐ。詳細は [background-task-indicator.ja.md](docs/ja/background-task-indicator.ja.md)
- **MCP サポート** — mcp-guardian stdio プロキシ経由
- **マルチターンメモリ** — Hot/Warm/Cold 3層スライディングウィンドウ（タイムスタンプ付き）
- **Pinned Memory** — セッション横断の永続的事実記憶（各事実に `(learned YYYY-MM-DD)` suffix が付き、モデルが新旧を判断可能）
- **マルチモーダル** — ドラッグ&ドロップ、ペースト、ファイルピッカーによる画像入力
- **per-session データパネル** — チャットペイン上部の折りたたみディスクロージャに、現在セッションのオブジェクト（image / report / blob をサムネイル付きカードで表示）、DuckDB テーブル（クリックで先頭 20 行プレビュー）、サンドボックス `/work` ファイルを集約。画像はライトボックス、レポートはマークダウンビューア、CSV / テキスト blob はインアプリプレビュー（CSV / TSV は HTML テーブル描画、その他テキスト MIME（JSON / plain text / HTML 等）は等幅 pre）。バルク選択 / 削除は別ボタン Yes / No 確認
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

### 設定項目リファレンス

**Settings** ダイアログから変更できる項目。per-backend 値は
レガシーなトップレベルフォールバックを上書きする。

#### Agent loop

| 項目 | JSON パス | 既定値 | 備考 |
|---|---|---|---|
| 1 メッセージあたりの最大ツールラウンド | `agent.max_tool_rounds` | 10 | 1 つの user message に対するツール呼び出しラウンド数の上限。loop-detection ring buffer（v0.1.16 の Feature 1）が同一エラー連発を早期検出するので、長い正規分析が本当にラウンド不足になった時のみ引き上げる。 |

#### per-backend コンテキスト予算（Local / Vertex AI）

| 項目 | JSON パス | Local 既定 | Vertex 既定 | 備考 |
|---|---|---|---|---|
| Hot Token Limit | `llm.{local,vertex_ai}.hot_token_limit` | 4096 | 65536 | コンパクション発火閾値。Hot 階層の総トークンがこれを超えると古い Hot レコードが Warm へ要約される。 |
| Max Context Tokens | `llm.{local,vertex_ai}.context_budget.max_context_tokens` | 16384 | 524288 | 1 コール毎にモデルへ送る総トークン予算。0 = 無制限。 |
| Max Warm Summary Tokens | `llm.{local,vertex_ai}.context_budget.max_warm_tokens` | 1024 | 16384 | warm-summary ブロックの上限。これを超えた古い summary は drop される。 |
| Max Tool-Result Tokens | `llm.{local,vertex_ai}.context_budget.max_tool_result_tokens` | 2048 | 32768 | LLM メッセージ列に投入する前にツール結果を切り詰めるサイズ。 |
| Output Reserve | `llm.{local,vertex_ai}.context_budget.output_reserve` | 4096 | 4096 | モデル応答用に確保するトークン量。コンテキスト詰め込み前に `max_context_tokens` から差し引かれ、リクエストがモデルウィンドウを超えないようにする。 |
| Per-request timeout (秒) | `llm.{local,vertex_ai}.request_timeout_seconds` | 300 | 180 | リトライ層内の per-attempt キャップ。 |
| リトライ最大回数 | `llm.{local,vertex_ai}.retry_max_attempts` | 3 | 3 | 初回を含む LLM コール総試行数（1 = リトライなし）。Settings → Local LLM / Vertex AI に露出。 |
| リトライ backoff base (秒) | `llm.{local,vertex_ai}.retry_backoff_base_seconds` | 5 | 5 | 最初のリトライ間隔。以降 2 倍ずつ伸び、下記 max でキャップ。設定ファイル直編集のみ。 |
| リトライ backoff max (秒) | `llm.{local,vertex_ai}.retry_backoff_max_seconds` | 120 | 120 | リトライ間隔の上限。設定ファイル直編集のみ。 |
| リトライ jitter (秒) | `llm.{local,vertex_ai}.retry_jitter_seconds` | 1 | 1 | 各 backoff に対し ±jitter の一様分布で揺らぎを足す。設定ファイル直編集のみ。 |

#### Sandbox (`sandbox.*`)

| 項目 | JSON パス | 既定値 | 備考 |
|---|---|---|---|
| Enabled | `sandbox.enabled` | false | マスタートグル。OFF のとき 8 個の `sandbox-*` ツールは登録されない。 |
| Engine | `sandbox.engine` | `auto` | `auto` は PATH から `podman` → `docker` の順で選択。 |
| Image | `sandbox.image` | (未設定、Build まで空) | アクティブなコンテナイメージ。ローカルビルド (`shell-agent-v2-sandbox:<sha>`) と `@sha256:` digest pin は安全とみなす。`python:3.12-slim` 等の mutable upstream tag のとき Settings → Sandbox に注意バナーが表示される。 |
| 出力上限 (バイト) | `sandbox.max_output_bytes` | `8388608` (8 MiB) | `exec` 1 回あたりの stdout / stderr 上限。超えた分は `[output truncated at N bytes]` マーカー付きで破棄。LLM が `cat /dev/zero` 等を発行してもアプリが OOM しない。設定ファイル経由のみ、UI なし。 |
| Network | `sandbox.network` | false | 外向き通信。既定は OFF。 |
| CPU limit | `sandbox.cpu_limit` | `2` | `--cpus` に渡す。 |
| Memory limit | `sandbox.memory_limit` | `1g` | `--memory` に渡す。 |
| Per-call timeout (秒) | `sandbox.timeout_seconds` | 60 | 1 回の `exec` あたりの上限。 |

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
- [バックグラウンドタスク表示](docs/ja/background-task-indicator.ja.md)
- [セッション復元時のツールイベント復活](docs/ja/tool-event-restore.ja.md)
- [Tool-call round-trip (Vertex / Local)](docs/ja/tool-call-roundtrip.ja.md)
- [セキュリティ強化（第 1 ラウンド、v0.1.18）](docs/ja/security-hardening.ja.md)
- [セキュリティ強化（第 2 ラウンド、v0.1.20）](docs/ja/security-hardening-2.ja.md)
- [Agent ツール可視性（v0.1.21）](docs/ja/agent-tool-visibility.ja.md)
- [Shell tool 実行タイムアウト (`@timeout: N`)](docs/ja/tool-execution-timeout.ja.md)
- [RFP (英語)](docs/en/shell-agent-v2-rfp.md) · [RFP (日本語)](docs/ja/shell-agent-v2-rfp.ja.md)

英語ミラーは `docs/en/` 配下。

## ライセンス

MIT

# shell-agent-v2 アーキテクチャ (v0.2.0)

このドキュメントは shell-agent-v2 が **v0.2.0 時点で**
どのように構成されているかを説明します。各サブシステムの
進化過程は [`history/`](history/) に保存されています。

姉妹ドキュメント:

- [`memory-model.ja.md`](memory-model.ja.md) — v0.2.0 4-facility
  メモリモデル正準資料
- [`data-analysis.ja.md`](data-analysis.ja.md) — per-session DuckDB
  エンジン、analyze-data の sliding-window summarizer、Findings
  ライフサイクル

本文書は両者を適宜参照します。

詳細な内容は英語版 [`docs/en/architecture.md`](../en/architecture.md)
を参照してください。日本語版は要点のみ抜粋:

## 1. 概要

shell-agent-v2 は Wails v2 (Go バックエンド + React + TypeScript
フロントエンド) で構築された macOS ネイティブ chat / agent
アプリケーション。1 ユーザーの会話を以下の機能で支援する:

- **データ分析** — CSV / JSON / JSONL を per-session DuckDB に
  ロードし、SELECT / sliding-window summary / promote-finding を
  実行可能。発見事項は chat-pane の Findings panel に表示。
- **シェルスクリプト Tool Calling** — スクリプトをツールとして
  登録、ヘッダで category / timeout / mitl を指定可能。
- **コンテナサンドボックス (opt-in)** — per-session の
  podman / docker コンテナで Python / shell を実行。
- **MCP** — `mcp-guardian` 経由で stdio JSON-RPC 2.0。
- **跨セッションメモリ** — Global Memory (preference / decision)
  + per-session Session Memory + Findings。明示的な
  「Pin to Global Memory」UI で昇格制御。
  詳細: [`memory-model.ja.md`](memory-model.ja.md)。

v0.1.x の Hot/Warm/Cold メモリ階層、`/finding` slash command、
グローバル Pinned ストアは v0.2.0 で全面置き換え。詳細は
`CHANGELOG.md` の v0.2.0 entry 参照。

## 2. プロセスモデル

### Idle / Busy ステートマシン

agent は作業中、active session を排他的に占有する:

- **Idle** — ユーザー入力受付可、session 切替自由
- **Busy** — 入力 disable、session 切替は明示的 abort 必要。
  バックグラウンドタスク (タイトル生成、メモリ抽出) も完了まで
  Busy 扱いで、次の user message と競合しない。

state は `internal/agent.Agent` が所有、`agent:state` イベントで
frontend に通知。Busy ガードは backend 側でも binding entry-point
で enforce される。

### Agent loop

`Agent.Send(ctx, message)` は同期 tool-calling loop:

```
buildMessages → backend.Chat → tool_calls 解析
  ↓ tool call なし
  reply 返却
  ↓ tool call あり
  各 call をディスパッチ → 結果記録
  ↓ 次ラウンド (最大 10)
```

ハードキャップ: `cfg.MaxToolRounds` (default 10)。
ループ検出 (同一エラー連続) は早期に止める。
詳細: `history/agent-loop-resilience.ja.md`。

ReAct ではない: tool 結果を次ラウンドにそのまま戻す。
ローカル LLM (ReAct grammar が苦手) との互換性を優先。

### post-response バックグラウンドタスク

reply 返却後、agent は WaitGroup で 2 つのタスクを async 起動:

- **タイトル生成** (session の最初の user turn のみ)
- **メモリ抽出** — §4 参照

両方とも `bg-task:*` イベントで surface。

## 3. パッケージ構成

```
app/
├── main.go                  # Wails App config + lifecycle
├── bindings.go              # Wails binding 層 (薄いデリゲート)
├── internal/
│   ├── agent/               # Idle/Busy state machine、ツールディスパッチ、extractMemories
│   ├── chat/                # System prompt assembly、BuildMessages、temporal context
│   ├── llm/                 # Backend 抽象化 (Local / Vertex AI / Mock)
│   ├── analysis/            # per-session DuckDB エンジン + sliding-window summarizer
│   ├── memory/              # Records、GlobalMemoryStore、SessionMemoryStore、sessions
│   ├── findings/            # per-session findings store + Jaccard dedup
│   ├── contextbuild/        # 非破壊コンテキスト組立 + summary cache
│   ├── objstore/            # 中央オブジェクトリポジトリ (image/blob/report; 32-hex IDs)
│   ├── toolcall/            # シェルスクリプトレジストリ、ヘッダ解析、MITL カテゴリ
│   ├── mcp/                 # mcp-guardian stdio JSON-RPC 2.0 クライアント
│   ├── sandbox/             # per-session podman/docker コンテナ
│   ├── bundled/             # 内蔵スクリプトの初回 scaffold
│   ├── pathfix/             # macOS app-launch PATH 正規化 (Homebrew)
│   ├── atomicio/            # WriteFileAtomic
│   ├── config/              # JSON config、path expansion
│   ├── logger/              # ファイルベースログ
│   └── frontendlint/        # CI ガード
├── frontend/src/            # React + TypeScript UI
└── cmd/                     # テストヘルパーバイナリ
```

## 4. メモリモデル (要約)

4 facility、3 ストレージスコープ、v1 destructive compaction なし:

| Facility | カテゴリ | スコープ | ファイル |
|----------|---------|---------|---------|
| Records | — | per-session | `sessions/<id>/chat.json` |
| Session Memory | fact / context | per-session | `sessions/<id>/session_memory.json` |
| Findings | (データ分析発見) | per-session | `sessions/<id>/findings.json` |
| Global Memory | preference / decision | 跨セッション | `<dataDir>/global_memory.json` |

auto-extraction (`agent.extractMemories`) は応答後に実行。
最後の 4 user/assistant turn (tool record はスキップして遡る) を
窓に取り、抽出 LLM に `category|turn-N|fact|native` 形式で
返してもらう。カテゴリで routing:

- `preference` / `decision` → GlobalMemoryStore
- `fact` / `context` → SessionMemoryStore

Findings は 2 経路 + insert 時 dedup:

- **Auto-promote** — `analyze-data` の sliding-window 解析から
- **Explicit** — `promote-finding` ツール呼び出しから

3 層 dedup (完全一致 / 正規化一致 / Jaccard ≥ 0.5) で
「同じ観察を微妙に違う表現で」の重複を防ぐ。

ユーザーは Session Memory entry または Finding を
**Pin to Global Memory** UI (sidebar ★ ボタン or chat-pane
panel ★ ボタン → category-picker dialog) で Global Memory に
昇格できる。元 entry はそのまま残る (additive)。

contextbuild が呼び出し毎にコンテキストを非破壊的に組み立てる:
records は完全忠実度を保持、古い部分は content-keyed cache で
圧縮 (`sessions/<id>/summaries.json`)。

詳細設計 + 脅威モデル: [`memory-model.ja.md`](memory-model.ja.md)。

## 5. ツールシステム

すべてのツール (analysis 内蔵、shell スクリプト、sandbox、MCP) は
1 つのディスパッチャ (`agent.dispatchTool`) と 1 つの MITL ゲート
(`agent.IsToolMITLRequired`) を経由する。

### ソース

- **内蔵 analysis ツール** — `load-data`, `query-sql`, ...
- **シェルスクリプト** — ユーザー登録 + bundled 6 個
- **Sandbox ツール** — 8 個の `sandbox-*`
- **MCP ツール** — guardian profile から起動時 discovery、
  `<guardian>__<tool>` で namespacing

### MITL ゲート

各ツールにデフォルトあり (read = 自動許可、write/execute =
承認必要)。`MITLOverrides` でツール毎に上書き可。
特殊フロー: `query-sql` は SQL preview、`analyze-data` は
analysis-plan 確認。reject 時は free-text feedback を
LLM に戻して revise 可能。

## 6. ストレージレイアウト

```
~/Library/Application Support/shell-agent-v2/
├── config.json                       # ユーザー設定
├── global_memory.json                # 跨セッション Global Memory (v0.2.0)
├── pinned.json                       # legacy v0.1.x; 起動時に無視
├── findings.json                     # legacy v0.1.x; 起動時に無視
├── objects/
│   ├── index.json                    # オブジェクトメタデータ
│   └── <id-prefix>/<id>              # オブジェクト本体
├── sessions/<session-id>/
│   ├── chat.json                     # Records (会話履歴)
│   ├── session_memory.json           # Session Memory (fact / context)
│   ├── findings.json                 # Findings
│   ├── summaries.json                # contextbuild summary cache
│   ├── analysis.duckdb               # per-session DuckDB
│   └── work/                         # Sandbox /work mount
└── tools/                            # ユーザーシェルスクリプト
```

データパスのすべての JSON ファイルは
`internal/atomicio.WriteFileAtomic` (tmp file → rename +
parent-dir fsync) を経由するため、save 中のクラッシュでも
前回ファイルは無事。

session 削除 (`DeleteSessionDir`) は `sessions/<id>/` 全体を
原子的に削除 — 1 操作で records / session memory / findings /
summaries cache / DuckDB を削除する。Global Memory と objstore
は影響を受けない。

## 7. LLM バックエンド

`internal/llm.Backend` 共通インタフェース実装:

- **`local.go`** — LM Studio (OpenAI 互換 REST)。tool 呼び出しは
  `function_call`、tool 結果は `<TOOL_RESULT>` で synthesised user
  turn にマップ (一部ローカルモデルが dedicated tool role を
  drop するため)
- **`vertex.go`** — Vertex AI Gemini (`google.golang.org/genai`
  SDK)。tool は `FunctionCall` / `FunctionResponse`、streaming 対応
- **`mock.go`** — テスト用決定的モック

ランタイム切替: `/model local` / `/model vertex`。
per-backend config (`config.LocalConfig` / `VertexAIConfig`) は
endpoint / model / retry / timeout / max args / `ContextBudget`
を保持。

## 8. フロントエンド構成

React + TypeScript、SPA、ルーターなし。Wails が
`window.go.main.Bindings` の JS shim を生成、
`frontend/src/bindings.ts` がその TypeScript view を宣言。

```
App.tsx
├── Sidebar (sessions / memory タブ)
│   ├── Sessions list
│   └── Memory タブ
│       ├── Global Memory section (bulk select / delete)
│       └── Session Memory section (bulk select / Pin button)
├── DataDisclosure (chat-pane top, per-session)
├── FindingsDisclosure (chat-pane, per-session)
│   ├── Severity フィルタ + 検索
│   ├── Bulk delete
│   └── 行ごと Pin / Delete
├── Messages stream
├── ChatInput
├── Status footer
└── Dialogs (Settings / MITL / PinToGlobal / Lightbox / ...)
```

### Wails events (backend → frontend)

| Event | 起動契機 |
|-------|---------|
| `agent:stream` | トークンストリーム (Vertex AI) |
| `agent:activity` | tool_start / tool_end / thinking |
| `agent:state` | Idle / Busy 遷移 |
| `session:title` | 自動生成セッションタイトル |
| `global_memory:updated` | Global Memory ストア変更 |
| `session_memory:updated` | Session Memory ストア変更 |
| `findings:updated` | Findings ストア変更 |
| `report:created` | 新レポートバブル |
| `mitl:request` | MITL 承認要求 |
| `bg-task:start` / `bg-task:end` | バックグラウンドタスクライフサイクル |

### テーマ

`themes.css` で 4 テーマ定義。surface 系 (`--bg-primary` 他) は
不透明 rgb、レイヤー系 (`--bg-msg-*` 他) は rgba。WebView は
不透明 (`main.go: WebviewIsTransparent: false`) — window-level
透明化は private macOS API が必要なため不採用。

## 9. セキュリティ姿勢

shell-agent-v2 は single-user ローカルアプリだが、複数の経路から
攻撃者制御バイトを処理する (CSV セル / MCP 応答 / 画像 OCR /
取得した Web ページ)。脅威モデルの中心は **ツール出力経由の
prompt-injection** で、ネットワーク露出ではない。

防御:

- **`nlk/guard` ラッピング** — user 提供テキスト・tool 結果本体は
  nonce 付き XML タグでラップ。fail-closed
- **Self-referential filter** — THINK 漏洩級の fact をブロック
- **Category allowlist** — 文書化された 4 カテゴリのみ受理
- **Provenance タグ** — 各 entry に `Source`、system prompt で
  `[user-stated]` vs `[derived]` をレンダリング
- **MITL ゲート** — write / execute はデフォルト承認必須
- **Sandbox** — opt-in のコンテナ隔離
- **Sandbox image trust** — mutable upstream tag 警告バナー
- **Atomic IO** — すべての state file が WriteFileAtomic 経由
- **Tool args 上限** — デフォルト 1 MiB
- **Symlink rejection** — host-path entry point は `os.Lstat`

## 10. ビルド / テスト / リリース

- `cd app && make build` → `dist/shell-agent-v2.app`。
  `go build` 直接実行禁止 (バイナリが project root に流出)
- `cd app && make test` → `go test -tags no_duckdb_arrow ./...`。
  `lmstudio` / `vertexai` build tag で integration test 有効化
- Frontend: `cd app/frontend && npm run build`
- リリース前手動 smoke test: CHANGELOG / RELEASE notes 参照

`main.go` の Mac config は WebView 不透明、タイトル隠し、
タイトルバーのみ装飾的に透明。launcher アプリなし
(直接 `.app` 起動のみ)。

## 11. 履歴ドキュメントへのポインタ

「なぜ X をしたか」「以前はどんな形だったか」は
[`history/`](history/) に保存。一部は現在のコードを反映していない
(特に v1 Hot/Warm/Cold compaction、original Pinned Memory) が、
v0.2.0 rewrite の audit trail として保持。
現在の挙動は本ドキュメントと `memory-model.ja.md` を優先。

英語版 [`architecture.md`](../en/architecture.md) §11 に
各 history doc の現状適用度を記載しています。

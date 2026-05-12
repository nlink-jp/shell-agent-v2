# shell-agent-v2 アーキテクチャ

本ドキュメントは正準のシステム全体リファレンス。**現在
どうシステムが組み合わさっているか** — パッケージ境界、
ステートマシン、ディスパッチフロー — を記述する。骨格は
v0.2.0 で確立、その後のリリース (v0.3.0–v0.6.1) で追加された
横断的機能は対応するサブシステムが進化するに従って本文中に
inline で記述する。

設計判断の **why** — 設計根拠・却下された代替案・個別判断の
脅威モデル — は ADR カタログにあります:
[`../INDEX.ja.md`](../INDEX.ja.md) を参照。

同ディレクトリの姉妹リファレンスドキュメント:

- [`memory-model.ja.md`](memory-model.ja.md) — 4-facility
  メモリモデル正準資料
- [`data-analysis.ja.md`](data-analysis.ja.md) — per-session DuckDB
  エンジン、analyze-data の sliding-window summarizer、Findings
  ライフサイクル
- [`privacy-controls.ja.md`](privacy-controls.ja.md) —
  プライベートセッション、ログレベルフィルタ、audit エントリ

v0.2.0 以前の audit trail は [`../history/`](../history/) 参照。

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
[`../../CHANGELOG.md`](../../CHANGELOG.md) の v0.2.0 entry 参照。

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

**Busy guard 配下の操作** (導入バージョン順):

- v0.2.0: `Send` / `SendWithImages`、`LoadSession`
- v0.4.0: `ExportSession`、`ImportSession`
- v0.4.2: `DeleteSession` — 以前は binding 層で entry-time の
  `IsBusy()` チェックのみだったが、現在は `agent.DeleteSession`
  経由でルーティングされ操作の全期間スロットを保持する。
  v0.4.2 以前のゆるい経路で許されていた失敗モード (アクティブ
  セッション削除中の Send が dir RemoveAll と race 等) について
  は [`ADR-0003`](../adr/0003-session-delete-ux.ja.md) §2 参照。

アクティブセッション削除では `RemoveAll` 実行前に `a.session`、
`a.sessionMemory`、`a.findings` を nil クリアし、analysis Engine
を `Close()` する。これにより stray な Save / Engine 呼出が
セッションディレクトリを蘇らせない。

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
詳細: `../history/agent-loop-resilience.ja.md`。

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
│   ├── sessionio/           # .shellagent bundle pack/unpack + 参照書換器 (v0.4.0)
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

**v0.3.0: `Session` のプライバシーフラグ**。`Session.Private bool`
(`chat.json` に `omitempty` で persist、legacy 互換のため非 private
default) はセッションを跨セッション promotion から opt-out する:
extraction が `preference` / `decision` fact を drop、Pin handler
(`PromoteSessionMemoryToGlobal`、`PromoteFindingToGlobal`) は
サーバ側で reject、frontend は ★ Pin UI を hide + 🔒 indicator
を表示。プライバシーはセッション作成時に固定。詳細設計:
[`privacy-controls.ja.md`](privacy-controls.ja.md)。

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
1 つのディスパッチャ (`agent.executeTool`) と 1 つの MITL ゲート
(`agent.IsToolMITLRequired`) を経由する。v0.6 以降、
analysis + builtin + sandbox の 3 ソースは加えて単一の
メタデータ source — `ToolDescriptor` (Agent ごとの
`a.toolDescriptors` slice、name で `a.toolDescriptorIndex`
にインデックス) — を共有する。

### ソース

- **Builtin ツール** (`internal/agent/tool_descriptors_builtin.go`):
  `resolve-date`, `list-objects`, `get-object`, `register-object`。
  analysis エンジンに依存しない。
- **Analysis ツール** (`internal/agent/tool_descriptors_analysis.go`):
  `load-data`, `describe-data`, `query-sql`, `query-preview`,
  `quick-summary`, `analyze-data`, `promote-finding`,
  `create-report`, `list-tables`, `suggest-analysis`,
  `reset-analysis`, `analyze-text`, `grep-text`, `get-text`。
  `a.analysis == nil` のとき LLM ツールリストから除外、
  legacy モードかつテーブル未ロードのとき data-gated サブセットを
  非表示。
- **Sandbox ツール** (`internal/agent/tool_descriptors_sandbox.go`):
  per-session container で実行される 8 個の `sandbox-*` ツール
  (セッション data dir から `/work` をマウント)。
  `a.sandbox == nil` のとき LLM ツールリスト + Settings UI
  両方から除外 (エンジンは `RestartSandbox` で動的に
  ライフサイクルが変わるため)。
- **シェルスクリプト** (`internal/toolcall/`): ユーザー登録
  スクリプト + bundled 6 個 (`go:embed` で初回起動時に
  scaffold)。ディスクリプタレジストリには入らず、
  toolcall.Registry から別に登録され `buildToolDefs` で
  LLM ツールリストに合流。
- **MCP ツール** — guardian profile から起動時 discovery、
  `<guardian>__<tool>` で namespacing。ディスクリプタレジストリ
  には入らず、`buildToolDefs` で動的に合流。

### ディスクリプタレジストリ

各 `ToolDescriptor` は Name, Description, Parameters
(JSON Schema), Category (`read`/`write`/`execute`), Source
(`analysis`/`builtin`/`sandbox`), MITLDefault,
MITLCategoryOverride (SQL preview / analysis plan 専用ダイアログ用),
HideUntilDataLoaded (legacy hide-until-table-loaded ゲート),
Handle (`*Agent` を capture して下層ツールメソッドに dispatch する
クロージャ) を保持する。同じディスクリプタが LLM ツール def
(`descriptorToolDefs`)、Settings → Tools エントリ
(`ListTools`)、MITL デフォルト (`IsToolMITLRequired` /
`toolMITLDefault`)、ディスパッチ (`dispatchDescriptor`) すべての
裏付けとなる — analysis / builtin / sandbox ツールの追加は
1 ファイル編集だけで済む。

v0.6 以前の設計はこれら 4 つの surface を並列リストとして
手で維持していた。v0.5.0 → v0.5.1 マニュアル smoke で 2 件の
drift バグ (Settings タブにツールが欠落、stale な MITL マップ
エントリ) を catch しており、v0.6 では `tool_descriptor_structural_test.go`
の構造テストで invariant を機械的に enforce する。
詳細な設計理由は [`ADR-0007`](../adr/0007-tool-registry-refactor.ja.md)。

### MITL ゲート

各ツールにデフォルトあり (read = 自動許可、write/execute =
承認必要)。`MITLOverrides` でツール毎に上書き可。
ディスクリプタ登録ツールではデフォルトは
`descriptor.MITLDefault` から直接、シェルスクリプトでは
`Tool.NeedsMITL()` から取得。`IsToolMITLRequired` の prefix
分岐 (mcp__ / sandbox-) は defense in depth として残し、
ディスクリプタが欠落した場合でも sandbox 呼び出しが摩擦
ゼロで通過しないようにしている。Settings → Tools UI は
ディスパッチャと同じレジストリから読むので、トグル状態と
実際のゲートが drift し得ない。

`descriptor.MITLCategoryOverride` で表面化される特殊フロー:

- `query-sql` → 実行前 SQL preview ダイアログ
  (`MITLCategoryOverride = "sql_preview"`)
- `analyze-data` → analysis-plan 確認ダイアログ
  (`MITLCategoryOverride = "analysis_plan"`)

reject 時は free-text feedback を LLM に戻して revise 可能。

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

session 削除は `sessions/<id>/` ディレクトリ全体に加え、その
セッションが所有する objstore object も削除する。v0.4.2 以降は
`agent.DeleteSession` がオーケストレーションし (binding 層で直接
ではなく)、agent state-machine Busy スロット配下で実行される —
理由は §2 参照。Global Memory は影響を受けない。

**v0.4.0: `.shellagent` bundle import / export**。セッションは
1 つの ZIP bundle (`internal/sessionio`) にパッケージ可能で、
`chat.json`、`session_memory.json`、`findings.json`、
`summaries.json`、`analysis.duckdb`、`work/` 再帰サブツリー、
そしてセッションの objstore blob とメタデータを含む `objects/`
サブディレクトリを運ぶ。Bundle はマシン間ポータブル (DuckDB の
バイナリ形式はクロスプラットフォーム)。Import 時に新セッション
は fresh sess-id を取得、各 objstore object も新 ID で再格納
される; `chat.json` 内の参照 (`Record.ObjectIDs[]` および
`Record.Content` 内の `object:ID` markdown) と `summaries.json`
(`SummaryEntry.Summary`) は同じ `agent.mu` gated state-machine
スロットを通じて書き換えられる。プライバシーフラグは逐語保持。
詳細設計: [`ADR-0001`](../adr/0001-session-import-export.ja.md)。

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
| `agent:activity` | tool_start / tool_end / tool_progress / thinking / assistant_text |
| `agent:state` | Idle / Busy 遷移 |
| `session:title` | 自動生成セッションタイトル |
| `global_memory:updated` | Global Memory ストア変更 |
| `session_memory:updated` | Session Memory ストア変更 |
| `findings:updated` | Findings ストア変更 |
| `report:created` | 新レポートバブル |
| `mitl:request` | MITL 承認要求 |
| `bg-task:start` / `bg-task:end` | バックグラウンドタスクライフサイクル |

**v0.4.1: `tool_progress` event**。長時間ツール (現在は
`analyze-data` の sliding-window summarizer) は親ツールの
`tool_call_id` + 更新表示文字列を運ぶ `tool_progress` ActivityEvent
を emit する。Frontend は ID (content text ではなく) でマッチし、
running バブルの content を in-place で上書き — 1 つのバブルが
"analyze-data" → "analyze-data — window 1/3" → … →
"analyze-data" に復帰 → `tool_end` で status flip、と更新される。
v0.4.1 以前は各 window が自身の `tool_start` を emit するが対応
する `tool_end` がなく N 個の永続的 "running" pill を残していた
(issue #5)。詳細設計:
[`ADR-0002`](../adr/0002-tool-progress-events.ja.md)。

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
[`../history/`](../history/) に保存。一部は現在のコードを反映していない
(特に v1 Hot/Warm/Cold compaction、original Pinned Memory) が、
v0.2.0 rewrite の audit trail として保持。
現在の挙動は本ドキュメントと `memory-model.ja.md` を優先。

英語版 [`architecture.md`](../../en/reference/architecture.md) §11 に
各 history doc の現状適用度を記載しています。

# shell-agent-v2 アーキテクチャ

## システム概要

```
┌──────────────────────────────────────────────────────────────────┐
│                         Wails v2 App                             │
│  ┌──────────────┐    ┌─────────────────────────────────────┐     │
│  │  React UI    │◄──►│  bindings.go (薄い委譲層)            │     │
│  │  App.tsx     │    │  EventsEmit  (ストリーミング、       │     │
│  │  components/ │    │   activity, bg-task, mitl, sandbox  │     │
│  │  dialogs/    │    │   ビルド進捗 等)                     │     │
│  │  sidebar/    │    └────────────────────┬────────────────┘     │
│  └──────────────┘                         │                      │
│                                ┌──────────▼──────────┐           │
│                                │   agent/ パッケージ  │           │
│                                │ Idle / Busy +        │          │
│                                │ post-task ゲート     │          │
│                                └──────────┬──────────┘           │
│         ┌────────┬────────┬─────────┬─────┴──────┬───────┐       │
│         ▼        ▼        ▼         ▼            ▼       ▼       │
│      ┌─────┐ ┌─────────┐ ┌──┐ ┌──────────┐ ┌────────┐ ┌────┐    │
│      │chat/│ │analysis/│ │llm│ │ toolcall │ │ sandbox│ │ mcp│    │
│      └──┬──┘ │  DuckDB │ │   │ │ + bundled│ │  /work │ │    │    │
│         │    └─────────┘ └─┬─┘ └──────────┘ └────────┘ └────┘    │
│         │                  │                                      │
│  ┌──────▼──────┐    ┌──────▼─────────┐                           │
│  │ contextbuild│    │  Local         │                           │
│  │ (memory v2) │    │  Vertex AI     │                           │
│  └──────┬──────┘    └────────────────┘                           │
│         │                                                         │
│  ┌──────▼──────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐       │
│  │  memory/    │  │  pinned  │  │ findings │  │ objstore │       │
│  │  Hot tier   │  │ 永続事実  │  │  分析知見 │  │ 画像 /    │      │
│  │  + summaries│  │          │  │          │  │ blob /    │      │
│  └─────────────┘  └──────────┘  └──────────┘  │ レポート  │      │
│                                                └──────────┘       │
│                                                                   │
│  ヘルパ: pathfix/ (.app 起動時の Homebrew PATH 補正)              │
│         bundled/  (シェルツールスクリプトの初回展開)               │
│                                                                   │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │                   永続化ストレージ                            │  │
│  │  sessions/{id}/chat.json + analysis.duckdb +                 │  │
│  │                summaries.json + work/ (sandbox ホスト側)     │  │
│  │  objects/data/{hex-id} + objects/index.json                  │  │
│  │  findings.json    pinned.json    config.json                 │  │
│  │  app.log          tools/ (ユーザー編集のシェルツール)         │  │
│  │  sandbox イメージキャッシュ (podman/docker 管理)              │  │
│  └─────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

## エージェント状態マシン

```
         ユーザ入力 / コマンド
              │
              ▼
        ┌───────────┐
        │   Idle    │◄──────────────────────────┐
        └─────┬─────┘                           │
              │ Send() / SendWithImages         │
              ▼                                 │
        ┌───────────┐                           │
        │   Busy    │                           │
        │(agentLoop)│                           │
        └─────┬─────┘                           │
              │ ツールラウンド (max N) /         │
              │ 最終応答 / Cancelled             │
              ▼                                 │
        ┌───────────────────────────┐           │
        │ Busy (post-response tasks)│           │
        │  • タイトル生成             │          │
        │  • メモリ圧縮               │          │
        │  • pinned-fact 抽出         │          │
        └─────┬───────────────────┬─┘           │
              │                   │             │
              │ 全 3 件完了        │ Abort()     │
              │ (success/error/   │ → postCancel│
              │  canceled)         │             │
              ▼                   ▼             │
        ┌──────────────────────────────────┐    │
        │ trailing goroutine: state = Idle ├────┘
        │ (cancel / postCancel もクリア)    │
        └──────────────────────────────────┘
```

**不変条件:**
- Busy 中はチャット入力欄、Sidebar の New / Load / Delete、
  スラッシュコマンドすべて操作不可。これは post-response の
  期間も含む — pinned-fact 抽出は直近 hot 4 件しか見ないので、
  途中で次メッセージが入ると cancel 扱いになり pinned が
  欠損するため。
- セッション切替・`/model` 切替は Idle のみ。
- `Abort` は `cancel` (in-flight agentLoop) と `postCancel`
  (post-response tasks) の両方を発火、trailing goroutine が
  Idle に戻す。「次の Send で auto-cancel」案は試行 → 撤回。
  詳細は
  [`background-task-indicator.ja.md`](./background-task-indicator.ja.md)。

## セッションスコープ分析

各セッションが独立した DuckDB インスタンスとサンドボックス
コンテナがマウントするプライベートな `/work` ディレクトリを所有:

```
sessions/
├── sess-1713945600000/
│   ├── chat.json          # 会話記録 (Hot/Warm/Cold)
│   ├── analysis.duckdb    # セッション所有 DB (遅延作成)
│   ├── summaries.json     # contextbuild サマリーキャッシュ (memory v2)
│   └── work/              # サンドボックスコンテナの /work にマウント
│       └── …              # LLM 生成物 (CSV / グラフ等)
└── sess-1713952800000/
    └── …
```

**ライフサイクル:**
1. `NewSession()` — ディレクトリ + 空 `chat.json` 作成
2. 最初の `load-data` または `sandbox-load-into-analysis` —
   `analysis.duckdb` を遅延生成
3. 最初のサンドボックス `Exec` — `work/` を作りセッション専用
   コンテナ (`shell-agent-v2-<sessionID>`) を起動
4. `LoadSession()` — 走行中の post-response goroutine をドレイン、
   現在の DuckDB を閉じて対象セッションを開く。サンドボックス
   コンテナはセッション単位なので置換は自然にクリーン
5. `DeleteSession()` — セッションディレクトリ削除 + コンテナ
   stop+rm。当該セッションに紐づく objstore エントリも
   `objects.DeleteBySession` で除去

**データ分離:** Session A のテーブルは Session B から不可視、
サンドボックス `/work` もセッション単位のホスト側別ディレクトリ。
セッション横断の知識共有はグローバル Findings または Pinned Memory のみ。

## メモリアーキテクチャ

圧縮実装が2系統あり、`Memory.UseV2` で切替可能。

```
システムプロンプト
├── ベースプロンプト + 時間コンテキスト
├── Pinned Memory (セッション横断の事実、各行に "(learned YYYY-MM-DD)")
└── Global Findings (分析知見)

セッション記録 (UseV2=true 時は不変)
├── Cold: 古い会話の LLM 要約         (legacy v1 のみ)
├── Warm: 近い過去の LLM 要約         (legacy v1 のみ)
└── Hot:  会話メッセージ
    ├── ユーザメッセージ
    ├── アシスタント応答 ([Calling: ...] マーカは LLM コンテキストから除外
    │   — 含めると gemma が text として模倣する)
    └── ツール結果
```

**v1 圧縮 (UseV2=false、デフォルト):** Hot 階層が per-backend の
`HotTokenLimit` を超過した場合、古いメッセージを LLM で要約し、
元の Hot レコードを Warm サマリーで **置換**。最低 1 件は最新レコードを
Hot に残す（Vertex 400 "empty contents" 退行 fix）。Warm レコードは
`SummaryRange` タイムスタンプを保持。

**v2 非破壊圧縮 (UseV2=true):** レコードは不変・完全忠実度のまま。
`internal/contextbuild` がコール毎にアクティブバックエンドの
`MaxContextTokens` 予算サイズで LLM コンテキストを生成し、古い tail を
content-key 付きキャッシュ要約に折り畳む。キャッシュは
`sessions/<id>/summaries.json`。生レコード・サマリーブロック・pinned・
findings の全チャネルに時間マーカが付与され、LLM が時系列推論可能。
詳細は [`memory-architecture-v2.ja.md`](./memory-architecture-v2.ja.md)。

**per-backend 予算.** 各 LLM バックエンドが個別の `HotTokenLimit` と
`ContextBudget` (`MaxContextTokens` / `MaxWarmTokens` /
`MaxToolResultTokens`) を持つ。per-backend が 0 のフィールドは legacy
top-level `Memory` / `ContextBudget` から継承。

```
config.json (抜粋)
├── llm
│   ├── default_backend
│   ├── local
│   │   ├── endpoint, model, api_key_env
│   │   ├── hot_token_limit            ← per-backend (optional)
│   │   └── context_budget             ← per-backend (optional)
│   │       ├── max_context_tokens
│   │       ├── max_warm_tokens
│   │       └── max_tool_result_tokens
│   └── vertex_ai
│       ├── project_id, region, model
│       ├── hot_token_limit            ← per-backend (optional)
│       └── context_budget             ← per-backend (optional)
├── memory
│   ├── hot_token_limit                ← legacy fallback
│   └── use_v2                         ← v2 opt-in flag
└── context_budget                     ← legacy fallback
```

**Pinned vs Findings:**
- Pinned: 一般的事実 (キーバリュー)、手動管理。各行に
  `(learned YYYY-MM-DD)` suffix
- Findings: 来歴付き分析知見、自動または手動昇格

## LLM バックエンド抽象化

```go
type Backend interface {
    Chat(ctx, messages, tools) (*Response, error)
    ChatStream(ctx, messages, tools, callback) (*Response, error)
    Name() string
}
```

2つの実装:
- **Local:** OpenAI 互換 SSE ストリーミング、ツール呼び出し解析
- **Vertex AI:** google/genai SDK、ADC 認証

`/model local` または `/model vertex` でランタイム切替。

## ツールシステム

5つのツールソースをエージェントループで統合:

| ソース | 例 | MITL |
|--------|------|------|
| **ビルトイン** | `resolve-date`, `create-report` | 不要 |
| **分析** | `load-data`, `query-sql`, `describe-data`, `promote-finding`, `reset` | 不要 |
| **サンドボックス** (opt-in、セッション専用コンテナ) | `sandbox-run-shell`, `sandbox-run-python`, `sandbox-write-file`, `sandbox-copy-object`, `sandbox-register-object`, `sandbox-info`, `sandbox-load-into-analysis`, `sandbox-export-sql` | execute (8 種すべて) |
| **シェルスクリプト** | 内蔵 (`file-info`, `preview-file`, `list-files`, `weather`, `get-location`, `write-note`) + ユーザ追加スクリプト | read: 不要, write/execute: 必要 |
| **MCP** | `mcp__<server>__<tool>` を mcp-guardian でプロキシ | 委譲 |

**動的フィルタリング.** 分析ツールはデータの有無に基づいて条件付き
公開、ローカル LLM のツール数を抑える。サンドボックスツールは
`sandbox.enabled` ON **かつ** 設定イメージがローカルエンジンに存在
する場合のみ登録される。両条件は agent 構築時に判定し、不成立なら
ツールが見えない状態にして 1) ターン中のクラッシュ防止 2) 動かない
ツールを LLM が呼んでしまう誘惑を排除している。

**ツールコールタイムライン.** 各 `tool_start` / `tool_end` 活動
イベントはチャット内で一時ピル表示される。さらに `memory.Record`
の各ツール結果には success/error ステータスが永続化される
(v0.1.19 から)。これによりセッション再読み込み時もバブルが復活する
— [`tool-event-restore.ja.md`](./tool-event-restore.ja.md) 参照。

### 内蔵シェルツール

デフォルトスクリプトは Go `embed` で実行ファイルに埋め込み。起動時に
`internal/bundled.Install(cfg.Tools.ScriptDir)` が embedded `tools/`
からユーザーツールディレクトリへ未配置のファイルだけをコピーする。

- ユーザーディレクトリに既存のファイルは **絶対に上書きしない** —
  ユーザーカスタマイズはアップグレードを跨いで保護される。
- リリースで新規追加された内蔵ツールは、次回起動時に既存ユーザーに
  自動展開される。
- `examples/` サブディレクトリは自動インストール対象外 — 参考用
  スクリプトはユーザーが意図的にコピーする扱い。

ソース配置: `app/internal/bundled/tools/`（`//go:embed` がアクセス
できるよう Go モジュール内に配置）。ユーザー側ツールディレクトリは
`~/Library/Application Support/shell-agent-v2/tools/`。

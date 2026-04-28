# shell-agent-v2 アーキテクチャ

## システム概要

```
┌─────────────────────────────────────────────────────────┐
│                    Wails v2 App                         │
│  ┌──────────────┐    ┌─────────────────────────────┐    │
│  │  React UI    │◄──►│  bindings.go (薄い委譲層)    │    │
│  │  App.tsx     │    │  EventsEmit (ストリーミング) │    │
│  └──────────────┘    └──────────┬──────────────────┘    │
│                                 │                       │
│                      ┌──────────▼──────────┐            │
│                      │   agent/ パッケージ  │            │
│                      │   Idle ◄──► Busy    │            │
│                      └──────────┬──────────┘            │
│              ┌──────────┬───────┼───────┬────────┐      │
│              ▼          ▼       ▼       ▼        ▼      │
│          ┌──────┐  ┌────────┐ ┌─────┐ ┌──────┐ ┌────┐  │
│          │chat/ │  │analysis│ │llm/ │ │tools/│ │mcp/│  │
│          │      │  │DuckDB  │ │     │ │      │ │    │  │
│          └──────┘  └────────┘ └──┬──┘ └──────┘ └────┘  │
│                                  │                      │
│                         ┌────────┴────────┐             │
│                         ▼                 ▼             │
│                    ┌─────────┐      ┌──────────┐        │
│                    │  Local  │      │ Vertex AI│        │
│                    │LM Studio│      │ Gemini   │        │
│                    └─────────┘      └──────────┘        │
│                                                         │
│  ┌──────────────────────────────────────────────────┐   │
│  │              永続化ストレージ                      │   │
│  │  sessions/{id}/chat.json + analysis.duckdb       │   │
│  │  findings.json    pinned.json    config.json     │   │
│  │  objects/data/{hex-id}                           │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

## エージェント状態マシン

```
         ユーザ入力 / コマンド
              │
              ▼
        ┌───────────┐
        │   Idle    │◄──────────────────┐
        └─────┬─────┘                   │
              │ Send()                  │
              ▼                         │
        ┌───────────┐    Abort()   ┌────┴────┐
        │   Busy    │─────────────►│クリーンアップ│
        └─────┬─────┘              └─────────┘
              │
       ┌──────┼──────┐
       ▼      ▼      ▼
    Chat   Analysis  Tool
    LLM    DuckDB    Shell/MCP
       │      │      │
       └──────┼──────┘
              │ 完了 / 最大ラウンド
              ▼
        ┌───────────┐
        │   Idle    │
        └───────────┘
```

**不変条件:**
- Busy 中はチャット入力ブロック
- セッション切替は Idle が必要 (または abort で Idle に戻す)
- `/model` 切替は Idle が必要

## セッションスコープ分析

各セッションが独立した DuckDB インスタンスを所有:

```
sessions/
├── sess-1713945600000/
│   ├── chat.json          # 会話記録 (Hot/Warm/Cold)
│   └── analysis.duckdb    # セッション所有 DB (遅延作成)
└── sess-1713952800000/
    ├── chat.json
    └── analysis.duckdb
```

**ライフサイクル:**
1. `NewSession()` — ディレクトリ + 空 `chat.json` 作成
2. 最初の `load-data` ツール呼び出し — `analysis.duckdb` 作成
3. `LoadSession()` — 現在の DuckDB を閉じ、対象セッションの DB を開く
4. `DeleteSession()` — セッションディレクトリ全体を削除

**データ分離:** Session A でロードしたテーブルは Session B から見えない。
セッション間の知識共有はグローバル Findings のみ。

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

3つのツールソースをエージェントループで統合:

| ソース | 例 | MITL |
|--------|------|------|
| **ビルトイン** | resolve-date | 不要 |
| **分析** | load-data, query-sql, promote-finding | 不要 |
| **シェルスクリプト** | 内蔵 (file-info, preview-file, list-files, weather, get-location, write-note) + ユーザ追加スクリプト | read: 不要, write/execute: 必要 |
| **MCP** | mcp-guardian ツール | 委譲 |

**動的フィルタリング:** 分析ツールはデータの有無に基づいて条件付き公開。
ローカルLLMでのツール数を管理可能に保つ。

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

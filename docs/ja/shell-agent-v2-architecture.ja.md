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

```
システムプロンプト
├── ベースプロンプト + 時間コンテキスト
├── Pinned Memory (セッション横断の事実)
└── Global Findings (分析知見)

セッション記録
├── Cold: 古い会話の LLM 要約
├── Warm: 近い過去の LLM 要約
└── Hot: 現在の会話メッセージ
    ├── ユーザメッセージ
    ├── アシスタント応答
    └── ツール結果
```

**圧縮:** Hot 階層がトークン予算を超過した場合、古いメッセージを
LLM で要約し Warm 階層に移動。Warm 記録は `SummaryRange`
タイムスタンプを保持。

**Pinned vs Findings:**
- Pinned: 一般的事実 (キーバリュー)、手動管理
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
| **シェルスクリプト** | ユーザ登録スクリプト | read: 不要, write/execute: 必要 |
| **MCP** | mcp-guardian ツール | 委譲 |

**動的フィルタリング:** 分析ツールはデータの有無に基づいて条件付き公開。
ローカルLLMでのツール数を管理可能に保つ。

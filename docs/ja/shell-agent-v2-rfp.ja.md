# RFP: shell-agent v2

> 作成日: 2026-04-24
> ステータス: Draft
> 前身: shell-agent v0.7.9 (util-series)

## 1. 課題定義

shell-agent v1 はローカルLLMによる macOS GUI チャット & エージェントツールであり、
DuckDB を組み込んだデータ分析機能を持つ。対話的なデータ分析アプローチは、
専用ツール data-agent の固定的なワークフローより優れていることが実証されたが、
v1 には根本的なアーキテクチャ上の問題がある:

1. **チャット-分析の状態不整合** — 分析エンジン (DuckDB) がグローバルである一方、
   チャットセッションは独立している。セッション切替でテーブル参照が壊れ、
   リスタートでメタデータが消失し、2つのエンジンが想定外の状態に陥りやすい。

2. **実行排他性の欠如** — 分析実行中もチャットインターフェースがアクティブなまま。
   エージェント作業中にユーザが入力でき、競合状態や混乱した状態を引き起こす。

3. **モノリシックな実装** — 全ビジネスロジック (チャット、分析、ツール、メモリ、MCP)
   が 73KB の `app.go` 単一ファイルに集中し、保守・拡張が困難。

4. **ローカルLLMのみ** — 長時間の分析タスクでCPU占有時間が長くなりすぎる。
   重いワークロード向けのクラウドLLMオプションがない。

shell-agent v2 は、v1 の強み (対話的・探索的データ分析) を維持しつつ、
これらのアーキテクチャ上の問題を解決する完全再設計である。

### 対象ユーザ

v1 と同様: ローカルファーストのチャット & エージェントツールとデータ分析機能を
必要とする macOS 個人ユーザ。

## 2. 機能仕様

### コアコンセプト

#### エージェント実行モデル: Idle / Busy

エージェントは2つの状態で動作する:

| 状態 | チャット入力 | セッション切替 | UI表示 |
|------|-----------|--------------|-------|
| **Idle** | 受付可 | 可能 | 入力フィールドアクティブ |
| **Busy** | ブロック | abort 必要 | 進捗 / ストリーミング出力 |

全てのエージェント作業 (LLM応答、ツール実行、データ分析) はエージェントを
Busy に遷移させる。作業が完了またはアボートされた場合のみ Idle に戻る。
これにより、v1 の「分析中に並行チャット入力」問題を排除する。

**アボート動作:**
- ユーザはいつでも現在のタスクをアボート可能 (キャンセルボタン / キーボードショートカット)
- Busy 中のセッション切替はアボート確認ダイアログを表示
- アボートされた分析はセッション内の部分的な DuckDB 状態をロールバック

#### セッションスコープ分析

各チャットセッションが自身の DuckDB インスタンスを所有する:

```
~/Library/Application Support/shell-agent-v2/
├── config.json
├── pinned.json              # セッション横断の事実記憶 (v1から変更なし)
├── findings.json            # グローバル Findings ストア (新規)
├── sessions/
│   ├── {session-id}/
│   │   ├── chat.json        # 会話記録
│   │   └── analysis.duckdb  # セッション所有のデータベース
│   └── {session-id}/
│       ├── chat.json
│       └── analysis.duckdb
└── ...
```

- Session A で CSV をロードすると、Session A でのみテーブルが見える
- Session B に切り替えると、Session B のテーブル (またはなし) が見える
- セッション間の DuckDB 状態リークなし
- テーブル説明は DuckDB に永続化 (`COMMENT ON TABLE`) — リスタートでのメタデータ消失なし

#### グローバル Findings

Findings は分析から導出された知見をグローバル知識ストアに昇格させたもので、
全セッションからアクセス可能。

```json
{
  "id": "f-20260424-001",
  "content": "月次売上は4月にピーク — 季節要因の可能性",
  "origin_session_id": "sess-abc123",
  "origin_session_title": "売上データ分析",
  "tags": ["sales", "seasonality"],
  "created_at": "2026-04-24T14:30:00+09:00",
  "created_label": "2026-04-24 (Thursday)"
}
```

**昇格 (ハイブリッド):**
- **自律的**: LLM が結果を重要と判断し自動昇格 (チャットでユーザに通知)
- **明示的**: ユーザが「これを覚えて」と指示、または `/finding` コマンドを使用

**セッション間利用:**
- 全セッションがシステムプロンプト注入経由でグローバル Findings を参照可能
- Finding がオリジンセッションを参照する場合、UI にクリック可能なリンクを表示
- 別セッションで Finding の深掘り分析が必要な場合、リンクがオリジンセッション
  (データが既に存在する場所) へユーザを誘導

**Pinned Memory との関係:**
- **Pinned Memory**: 一般的なセッション横断の事実 (ユーザの好み、環境情報、
  繰り返し使う文脈) — v1 から変更なし
- **Findings**: 来歴付きのデータ分析知見 (オリジンセッション、タイムスタンプ、タグ)
- これらは別システム、別ストレージ

#### LLM への時間コンテキスト

ローカルLLMは日付計算 (例: 日付文字列から「先週の木曜日」を算出) が不得意である。
v2 では信頼性のある相対日時解決のため、強化された時間コンテキストを
システムプロンプトに注入する:

```
Current date and time: 2026-04-24 (Thursday) 15:30:00 JST
Yesterday: 2026-04-23 (Wednesday)
```

これにより、「先週の木曜日の件」や「昨日の分析結果について」といった
相対的な日時参照を、日付計算なしで正しく解決できる。

Findings にも人間可読な日付ラベルを付与し、セッション間の時間参照を
確実にマッチ可能にする:

```json
{
  "created_at": "2026-04-17T14:30:00+09:00",
  "created_label": "2026-04-17 (Thursday)"
}
```

ユーザが「先週の木曜日の Finding」と言った場合、LLM は ISO タイムスタンプを
パース・計算する代わりに、ラベルを直接マッチできる。

「今日」「昨日」を超える複雑なケース (例: 「3週間前の水曜日」
「先月の最終営業日」) には、システムツールとして `resolve-date` を提供する:

```json
{
  "name": "resolve-date",
  "description": "Resolve relative date expressions to absolute dates. Use when you need to calculate dates like 'last Thursday', '3 weeks ago', 'first Monday of last month'.",
  "parameters": {
    "expression": "string — natural language date expression",
    "reference_date": "string — optional, ISO date to calculate from (default: today)"
  }
}
```

Go の `time` パッケージで確定的に計算するため、LLM の算術誤りを排除できる。
LLM は自律的にツール使用を判断する — システムプロンプトの情報で十分な
単純なケースではツール呼び出しをスキップし、不確実な場合はツールに委譲する。

この2層設計 (一般的なケースはシステムプロンプト + 複雑なケースはツール) により、
不要なツール呼び出しのラウンドトリップを最小化しつつ、任意の相対日付表現の
正確性を保証する。

#### ハイブリッド LLM バックエンド

2つのバックエンドが同時に利用可能:

| バックエンド | エンジン | ユースケース |
|------------|---------|------------|
| **Local** | LM Studio (OpenAI互換API) | 軽い対話、簡単なタスク、オフライン利用 |
| **Vertex AI** | Gemini (google/genai SDK) | 重い分析、長いコンテキスト、複雑な推論 |

**切替:**
- `/model` コマンドで現在のエンジン + 選択肢を表示
- `/model local` または `/model vertex` で即座に切替
- 切替は Idle 状態でのみ可能
- デフォルトエンジンは設定で変更可能
- セッション内のエンジン選択は新規セッションでデフォルトにリセット

**認証:**
- Local: 任意の API キー (`SHELL_AGENT_API_KEY` 環境変数、v1 と同様)
- Vertex AI: ADC (Application Default Credentials)、`roles/aiplatform.user` 必要

### コマンド / API サーフェス

**メインアプリ (Wails v2 + React):**
- Idle/Busy 状態表示付きチャットウィンドウ
- アボートボタン (Busy 中に表示)
- サイドバー: セッション一覧、ツール一覧、Findings パネル
- セッションリンクナビゲーション (Findings からオリジンセッションへ)
- 設定 UI: API 設定 (local + Vertex AI)、メモリ、ツール、MCP、テーマ

**チャットコマンド:**
- `/model [local|vertex]` — LLM バックエンドの表示・切替
- `/finding [text]` — Finding をグローバルストアに明示的に昇格
- `/findings` — 全グローバル Findings を一覧表示

### 入出力

LLM通信、シェルスクリプトツール、MCP については v1 と同様。
Vertex AI バックエンドは `google.golang.org/genai` SDK でストリーミング対応。

### 設定

**設定ファイル (`~/Library/Application Support/shell-agent-v2/config.json`):**

```json
{
  "llm": {
    "default_backend": "local",
    "local": {
      "endpoint": "http://localhost:1234/v1",
      "model": "google/gemma-4-26b-a4b",
      "api_key_env": "SHELL_AGENT_API_KEY"
    },
    "vertex_ai": {
      "project_id": "PROJECT_ID",
      "region": "us-central1",
      "model": "gemini-2.5-flash"
    }
  },
  "memory": {
    "hot_token_limit": 4096,
    "warm_retention": "24h",
    "cold_retention": "7d"
  },
  "tools": {
    "script_dir": "~/Library/Application Support/shell-agent-v2/tools",
    "mcp_guardian": {
      "binary": "/usr/local/bin/mcp-guardian",
      "config": "~/.config/mcp-guardian/config.json"
    }
  },
  "ui": {
    "theme": "dark",
    "startup_mode": "last"
  }
}
```

### 外部依存

| 依存先 | 種別 | 必須 |
|--------|------|------|
| LM Studio (OpenAI互換APIサーバ) | ローカルサービス | Yes (local バックエンド使用時) |
| Vertex AI (Gemini) | クラウドサービス | Yes (vertex バックエンド使用時) |
| mcp-guardian | バイナリ (stdio 子プロセス) | Yes (MCP 使用時) |
| nlk | Go ライブラリ (直接インポート) | Yes |

## 3. 設計判断

**言語 & フレームワーク:**
- メインアプリ: Go + Wails v2 + React — v1 と同様。実績あるスタックで、
  nlk ライブラリ再利用と Vertex AI SDK 統合が可能。
**v1 のリファクタリングではなく完全リライトとする理由:**
- 状態管理アーキテクチャ (グローバル DuckDB、常時アクティブなチャット) は
  v1 の設計の根幹。セッションスコープ分析と Idle/Busy モデルの後付けは、
  事実上全ての統合点に影響する。
- 73KB モノリス (`app.go`) の構造分解は、段階的リファクタリングでは非現実的。
- v2 は実績あるパターン (メモリ階層、ツールディスパッチ、MCP統合、セキュリティ)
  を継承しつつ、状態アーキテクチャを再設計できる。

**既存ツールとの関係:**
- `shell-agent v1`: 直接の前身。メモリモデル、ツールシステム、MCP統合、
  セキュリティアーキテクチャを継承。状態管理を再設計し LLM バックエンドの
  柔軟性を追加。
- `data-agent`: 教訓 — 分析特化ツールは探索的作業には硬直的すぎた。
  Vertex AI バックエンドパターンを再利用。
- `nlk`: guard, jsonfix, strip, backoff, validate — v1 と同様。
- `mcp-guardian`: MCP 委譲 — v1 と同様。

**スコープ外:**
- クラウド同期
- マルチユーザサポート
- サーバモード
- セッション間 DuckDB 共有 (Findings が知識レベルでこれを橋渡し)
- コンテナベース分析 (data-agent アプローチ — v2 のスコープでは不要)

## 4. 開発計画

### Phase 1: コア — 状態アーキテクチャ

- `_wip/shell-agent-v2/` にプロジェクトスキャフォールド (Wails v2 + React)
- **エージェント状態マシン** (Idle/Busy) と UI ロックアウト
- **セッションスコープ DuckDB** ライフサイクル (作成、オープン、クローズ、永続化)
- チャットエンジンとセッション永続化
- タイムスタンプ付き Hot メモリ (v1 から移行)
- アボート機構 (コンテキストキャンセル、DuckDB ロールバック)
- デュアル LLM バックエンド抽象化 (`local` + `vertex_ai`)
- `/model` コマンドによるランタイム切替
- nlk 統合 (guard, jsonfix, strip)
- Idle/Busy インジケータ付き基本チャット UI
- 状態遷移、セッション分離、バックエンド切替のテスト

**独立してレビュー可能 — コアのアーキテクチャ変更を検証**

### Phase 2: 機能 — エージェント機能

- シェルスクリプト Tool Calling (v1 から移行)
- mcp-guardian 経由の MCP 統合 (v1 から移行)
- **グローバル Findings ストア** (CRUD、昇格、オリジンリンク)
- 自律的 Finding 昇格 (LLM駆動)
- `/finding` および `/findings` チャットコマンド
- サイドバーの Findings パネルとセッションリンクナビゲーション
- Warm/Cold メモリ階層と LLM 要約
- Pinned Memory (v1 から移行)
- マルチモーダル対応 — 画像入力 (v1 から移行)
- データ分析ツール (load-data, query-sql, query-preview, analyze-data 等)
- 動的ツールフィルタリング (v1 から移行、セッションスコープ化)
- 設定 UI (デュアルバックエンド設定、メモリ、ツール、MCP、テーマ)
**独立してレビュー可能**

### Phase 3: リリース — ドキュメント & 品質

- テスト拡充 (状態エッジケース、分析中のセッション切替等)
- README.md / README.ja.md
- アーキテクチャドキュメント (en + ja)
- CHANGELOG.md
- AGENTS.md
- リリースビルドと配布
- v1 からの移行ガイド (セッションデータ変換が可能であれば)

## 5. 必要な API スコープ / 権限

| サービス | スコープ / ロール | 用途 |
|---------|----------------|------|
| Vertex AI | `roles/aiplatform.user` | Gemini API アクセス |
| MCP | mcp-guardian に委譲 | 直接の認証不要 |
| Local LLM | なし (任意の API キー) | LM Studio アクセス |

## 6. シリーズ配置

シリーズ: **util-series**
理由: v1 と同様。同シリーズ内の shell-agent の後継。

## 7. 外部プラットフォーム制約

| 制約 | 詳細 |
|------|------|
| LM Studio | local バックエンド使用時はローカルサーバの起動が必要 |
| Vertex AI | ADC セットアップ (`gcloud auth application-default login`)、ネットワーク接続、課金有効な GCP プロジェクトが必要 |
| Wails v2 | macOS 10.15+ が必要 |
| gemma-4-26b-a4b | ~16GB VRAM 必要 (Apple Silicon M1/M2 Pro+) |
| セッション別 DuckDB | セッション数に応じてディスク使用量が増加。v2 スコープでは自動 GC なし。手動セッション削除で DB もクリーンアップ |

---

## アーキテクチャ概要

### パッケージ構成 (目標)

```
shell-agent-v2/
├── app/
│   ├── main.go
│   ├── internal/
│   │   ├── agent/           # エージェント状態マシン (Idle/Busy)、実行ループ
│   │   ├── chat/            # チャットエンジン、メッセージ構築、システムプロンプト
│   │   ├── llm/             # バックエンド抽象化 (local + vertex_ai)
│   │   ├── analysis/        # DuckDB エンジン (セッションスコープライフサイクル)
│   │   ├── memory/          # Hot/Warm/Cold 階層、セッション、Pinned
│   │   ├── findings/        # グローバル Findings ストア、昇格ロジック
│   │   ├── toolcall/        # シェルスクリプトレジストリ、MITL
│   │   ├── mcp/             # mcp-guardian stdio
│   │   ├── objstore/        # 画像/blob リポジトリ
│   │   ├── config/          # JSON 設定、パス展開
│   │   └── logger/          # 構造化ロギング
│   ├── bindings.go          # Wails バインディング (薄い委譲層)
│   ├── frontend/src/
│   │   ├── App.tsx
│   │   ├── ChatInput.tsx
│   │   └── ...
│   └── Makefile
├── docs/
│   ├── en/
│   └── ja/
├── CLAUDE.md
├── AGENTS.md
├── README.md
├── README.ja.md
└── CHANGELOG.md
```

**v1 からの主要な構造変更:**
- `app.go` (73KB モノリス) を `agent/`, `chat/`, `llm/`, `analysis/`,
  `findings/` と薄い `bindings.go` (Wails 統合) に分解
- `agent/` が Idle/Busy 状態マシンを所有し、他の全パッケージをオーケストレーション
- `llm/` が local と Vertex AI バックエンドの統一インターフェースを提供
- `findings/` がグローバル知識ストアの新パッケージ

### 状態フロー

```
ユーザ入力
  │
  ▼
bindings.go (Wails) ──→ agent.Send(msg)
                              │
                              ▼
                        [Idle] ──→ [Busy]
                              │
                     ┌────────┼────────┐
                     ▼        ▼        ▼
                  chat/    analysis/  toolcall/
                     │        │        │
                     ▼        ▼        ▼
                   llm/ (local or vertex_ai)
                     │
                     ▼
                  [Busy] ──→ [Idle]
                              │
                              ▼
                        UI アンロック
```

### セッションライフサイクル

```
NewSession()
  ├── セッションディレクトリ作成
  ├── chat.json 初期化 (空レコード)
  └── DuckDB: まだ作成しない (遅延 — 最初の load-data で作成)

LoadSession(id)
  ├── 現在のセッションの DuckDB をクローズ (開いていれば)
  ├── chat.json をロード
  └── セッションの analysis.duckdb をオープン (存在すれば)

DeleteSession(id)
  ├── DuckDB 接続をクローズ
  ├── セッションディレクトリ削除 (chat.json + analysis.duckdb)
  └── 孤立 Findings の削除? (しない — Findings は独立して永続化)
```

---

## 議論ログ

1. **data-agent の振り返り**: 分析特化ツールは探索的作業には硬直的すぎた。
   対話的・対話駆動型の分析 (shell-agent v1 アプローチ) がアドホック調査に適している。
2. **v1 不安定性の根本原因**: チャットエンジンと分析エンジンが独立したライフサイクルで
   共有ミュータブル状態 (グローバル DuckDB) を持つ。セッション切替で参照整合性が壊れる。
3. **セッションスコープ分析**: 各セッションが自身の DuckDB を所有。
   セッション間の状態リークを完全に排除。
4. **グローバル Findings**: 分析知見を来歴付き共有知識ストアに昇格。
   セッション間をデータレベルではなく知識レベルで橋渡し。
5. **Findings vs Pinned Memory**: 別システム。Pinned = 一般的事実、
   Findings = 来歴付き分析知見。
6. **Idle/Busy 実行モデル**: エージェントが作業中はセッションを排他的に占有。
   Busy 中はチャット入力ブロック。セッション切替はアボート必要。
   並行入力による競合状態を排除。
7. **ハイブリッド LLM バックエンド**: Local (gemma-4) + Vertex AI (gemini) を
   `/model` コマンドでランタイム切替。デフォルトは設定で変更可能。
   v1 の長時間タスクでの CPU 独占問題に対応。
8. **完全リライト**: 状態アーキテクチャの変更が根本的すぎ、段階的リファクタリングは
   非現実的。v2 は `_wip/` で独立プロジェクトとして開発。
9. **モノリス分解**: 73KB `app.go` を目的別パッケージに分割し、
   薄い Wails バインディング層で統合。
10. **時間コンテキスト強化**: システムプロンプトに曜日と前日の日付を含め、
    相対日時の確実な解決を実現。Findings に人間可読な日付ラベルを付与し、
    セッション間の時間参照に対応。ローカルLLMは日付文字列から
    「先週の木曜日」を確実に計算できないため。

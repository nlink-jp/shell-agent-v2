# エージェントデータフロー & ステート制御 — 設計ドキュメント

> 作成日: 2026-04-25
> ステータス: Draft — v2 実装テストで発見された問題に対応
> スコープ: エージェントループ、セッションレコード、メモリ圧縮、オブジェクトリポジトリ

## 1. 問題の要約

v2 実装テストで以下のデータフロー・ステート制御の問題が発見された:

1. **ツール呼び出しループ** — ツール除去後も LLM がツールを再呼び出し (gemma テキスト形式が API をバイパス)
2. **セッションレコード汚染** — 空 assistant メッセージ、tool result がレコードに蓄積しマルチターン LLM コンテキストを汚染
3. **メモリ圧縮なし** — CompactIfNeeded は実装済みだが一度も呼ばれない。Hot メッセージが無限増大してコンテキストオーバーフロー
4. **セッション永続化なし** — session.Save() がエージェントループ中に呼ばれない。クラッシュ = データ消失
5. **レポート未永続化** — レポートはイベント経由で配信されるがセッションレコードに保存されない。リロードで消失
6. **画像処理** — 画像が生の data URL としてレコードに保存 (肥大化)。オブジェクト ID ベースの参照なし。LLM がセッション画像を参照不可
7. **フロントエンド/バックエンドのフィルタリング不一致** — tool result がフロントエンドでフィルタされるがバックエンドでは未フィルタ

根本原因: 全体設計なしの段階的パッチ。

## 2. エージェントループ ステートマシン

### 2.1 LM Studio ツールコーリング仕様

LM Studio ドキュメントの推奨フロー:

```
1. LLM を tools 付きで呼ぶ → レスポンス取得
2. tool_calls がある場合:
   a. ツール実行
   b. assistant の tool_call メッセージ + tool 結果を messages に追加
   c. LLM を tools なしで呼ぶ → 最終テキストレスポンス取得
3. tool_calls がない場合: テキストレスポンスを返す
```

重要ルール: **ツール実行後の次の LLM 呼び出しは tools なしで行う。**
LLM にテキスト生成を強制し、ツール再呼び出しを防ぐ。

### 2.2 v2 エージェントループ設計

```
SendWithImages(ctx, message, imageURLs)
  │
  ├── 状態: Idle → Busy
  ├── /コマンド処理 (即座に return)
  │
  └── agentLoop(ctx, message, imageURLs)
        │
        ├── 画像を objstore に保存 → ID 取得
        ├── ユーザレコードをセッションに追加 (画像 ID、data URL ではない)
        ├── セッション自動保存
        │
        └── ループ (最大 10 ラウンド):
              │
              ├── ツールリスト構築:
              │   ├── toolsExecutedLastRound なら: tools = nil
              │   └── それ以外: tools = buildToolDefs()
              │
              ├── セッションレコードからメッセージ構築
              │   ├── システムプロンプト + 時間コンテキスト + pinned + findings
              │   ├── Warm/Cold サマリーを先頭に
              │   ├── Hot レコード (空 assistant はスキップ)
              │   ├── user と tool のコンテンツを guard ラップ
              │   └── 最新画像: data URL 全体; 古い画像: テキスト参照
              │
              ├── LLM 呼び出し (ChatStream, tools あり or nil)
              │
              ├── レスポンスクリーニング:
              │   ├── strip.ThinkTags()
              │   └── stripGemmaToolCallTags() (毎ラウンド実行)
              │
              ├── tool_calls なし AND コンテンツ非空:
              │   ├── assistant レコードをセッションに保存
              │   ├── セッション自動保存
              │   ├── バックグラウンド: タイトル生成, pinned 抽出, メモリ圧縮
              │   └── RETURN コンテンツ
              │
              ├── tool_calls なし AND コンテンツ空:
              │   └── RETURN "" (ループ終了、ツール再有効化しない)
              │
              └── tool_calls あり:
                    ├── assistant レコード保存 (コンテンツ非空の場合のみ)
                    ├── 各ツール呼び出し実行:
                    │   ├── MITL チェック (write/execute)
                    │   ├── ツール実行
                    │   ├── tool result レコード保存
                    │   └── ツールが成果物生成 → objstore に保存
                    ├── セッション自動保存
                    └── toolsExecutedLastRound = true
```

### 2.3 重要ルール

1. **空レスポンス後にツールを再有効化しない。** 以前の再有効化ロジックが create-report 無限ループの原因。

2. **gemma テキストツール呼び出しを常に除去。** tools なしラウンドだけでなく毎ラウンド実行。

3. **空 assistant メッセージを記録しない。** クリーニング後にコンテンツが非空の場合のみ保存。

4. **変更のたびに自動保存。** ユーザメッセージ、assistant メッセージ、tool result — 即座にディスクに保存。

## 3. セッションレコード データモデル

### 3.1 レコード構造

```go
type Record struct {
    Timestamp    time.Time   `json:"timestamp"`
    Role         string      `json:"role"`      // user|assistant|tool|report|summary
    Content      string      `json:"content"`
    Tier         Tier        `json:"tier"`       // hot|warm|cold
    ToolCallID   string      `json:"tool_call_id,omitempty"`
    ToolName     string      `json:"tool_name,omitempty"`
    ObjectIDs    []string    `json:"object_ids,omitempty"`  // objstore への参照
    SummaryRange *TimeRange  `json:"summary_range,omitempty"`
    InTokens     int         `json:"in_tokens,omitempty"`
    OutTokens    int         `json:"out_tokens,omitempty"`
}
```

### 3.2 ロール

| ロール | 保存元 | LLM に送信 | UI に表示 | 備考 |
|--------|--------|-----------|----------|------|
| `user` | agentLoop | Yes (guarded) | Yes | ユーザメッセージ |
| `assistant` | agentLoop | Yes | Yes | LLM 応答 (非空のみ) |
| `tool` | agentLoop | Yes (guarded) | No | ツール結果 |
| `report` | toolCreateReport | Yes | Yes (特別表示) | レポート、セッションに保存 |
| `summary` | CompactIfNeeded | Yes | No | Warm/Cold サマリー |

### 3.3 コンテキスト予算制御

`BuildMessagesWithBudget` (v1) と `contextbuild.Build` (v2) は LLM
コンテキストを清浄かつ運用上限内に保つため、2 つのメカニズムを
提供する:

1. **`[Calling:]` 除外** (主) — パターン汚染を防止
2. **トークン予算** (任意) — リソース管理用にコンテキストサイズを制限

#### 根本原因: `[Calling:]` パターン汚染

gemma-4-26b との結合テストで判明 — ツールコーリング失敗の原因は
コンテキスト長ではなかった (gemma-4 は 256K トークンサポート)。
真の原因: `[Calling: tool_name]` という合成 assistant メッセージを
LLM コンテキストに含めると、モデルは API ツールコール代わりに
そのパターンをテキスト出力として模倣する。

**テスト結果 (Before/After):**

| 条件 | 最終成功ターン | 失敗時のレコード数 |
|------|---------------|------------------|
| `[Calling:]` 含 (NoBudget) | 3〜5 ターン (非決定的) | 10〜16 レコード |
| `[Calling:]` 除外 (WithBudget) | 13 ターン以降全て成功 | 52 レコード以上 |

確率的な失敗 — 同一条件でも実行毎に成功・失敗するが、`[Calling:]`
汚染は失敗確率を有意に高める。

#### 重い分析シナリオでの観測

シナリオ: CSV ロード (30 行) → クエリ → analyze-data → JSONL ロード
→ クエリ → 第二テーブル分析 → クロステーブルクエリ。

主な観測:
- `analyze-data` の結果はコール毎に ~1,800 トークン蓄積
- ツール定義 14 個 + ~3,000 トークン推定でツールコーリング劣化
- 同期圧縮 (HotTokenLimit=4096) が自動回復: 22 レコード → 11 レコード
  + warm summary
- 圧縮後はツールコーリング正常再開

#### メッセージ選択アルゴリズム

```
1. システムプロンプト構築 (既存ロジック)
2. warm summary レコードを集約、MaxWarmTokens で truncate
3. hot レコードを逆時系列で集約:
   - [Calling: ...] メッセージはスキップ (パターン汚染防止)
   - tool result を MaxToolResultTokens で truncate
   - トークン予算尽きたら停止 (最新メッセージ優先保持)
4. 時系列順に戻す
5. 組み立て: system + warm + hot
```

全バックエンドに適用 (ローカル限定ではない)。

#### 設定

```go
type ContextBudgetConfig struct {
    MaxContextTokens    int // default: 0 (無制限)
    MaxWarmTokens       int // default: 1024
    MaxToolResultTokens int // default: 2048
}
```

per-backend 設定: `cfg.LLM.Local.ContextBudget` /
`cfg.LLM.VertexAI.ContextBudget` がデフォルトのフォールバックを
オーバーライドする (v0.1.2+)。

#### 推奨パラメータ

| パラメータ | デフォルト | 根拠 |
|-----------|-----------|------|
| `MaxContextTokens` | 0 (無制限) | gemma-4 は 256K サポート、`[Calling:]` 除外が主 fix |
| `MaxWarmTokens` | 1024 | 要約 1 段落 |
| `MaxToolResultTokens` | 2048 | クエリ結果に十分、analyze-data レポートは truncate |
| `HotTokenLimit` (圧縮) | 4096 | ツールコーリング劣化前に圧縮発火 |

リソース制約環境やより小型モデル向けには `MaxContextTokens` を
保守的な値 (例: 8192) に設定。

#### 同期圧縮

agentLoop 開始時、およびツールラウンド間 (round > 0) で同期実行 →
`BuildMessages` が呼ばれる前にコンテキスト圧縮済みを保証。
async post-response 圧縮はセーフティネットとして残存。

レスポンス後タスク (タイトル生成、圧縮、pinned 抽出) は WaitGroup
で同期され、次の `Send()` 呼び出しと race しない。

### 3.4 保存しないもの

- 空 assistant メッセージ (クリーニング後 content == "")
- ストリーミング中間コンテンツ
- イベント専用データ

### 3.5 ツール結果フォーマット

```
[Tool: resolve-date]
Output:
2026-04-24 (Thursday)
```

生のツール出力ではなく、ツール名プレフィックス付きで LLM コンテキストに渡す。

## 4. メモリ圧縮

圧縮実装は2系統あり、`Memory.UseV2` で切り替わる:

- **v1 破壊的圧縮** — Hot レコードを Warm サマリーで上書き置換
- **v2 非破壊的圧縮** — `internal/contextbuild` がレコード本体は不変のまま、コール毎に LLM コンテキストをビルド

v2 設計の詳細は [memory-architecture-v2.ja.md](./memory-architecture-v2.ja.md) 参照。

### 4.1 呼び出しタイミング

LLM コール毎の同期実行と、レスポンス後の async セーフティネットの両方:

```
agentLoop iteration
  → compactIfOverBudget(ctx)         // 同期、BuildMessages の前
  → backend.Chat(...)
  ...
agentLoop 完了
  → go generateTitleIfNeeded(ctx)
  → go compactMemoryIfNeeded(ctx)    // async post-response
  → go extractPinnedMemories(ctx)
  → session.Save()                   // 同期的な最終保存
```

`Memory.UseV2 == true` のとき `compactIfOverBudget` と
`compactMemoryIfNeeded` の両者が即座に no-op で返る。v2 の
`ContextBuilder.Build` がコール毎に LLM メッセージを生成し、
content-key 付きキャッシュから要約を取得する（レコード本体は変更しない）。

### 4.2 v1 破壊的圧縮フロー (UseV2 == false)

```
compactIfOverBudget(ctx) / compactMemoryIfNeeded(ctx):
  1. Hot トークン合計計算
  2. hotTokens <= a.currentHotTokenLimit() (per-backend) なら return
  3. split point を決定 — 最新レコードが単独で予算超過の場合でも
     最低 1 件は hot に残す（v0.1.1 の Vertex 400 退行 fix:
     contents 空配列でリクエスト拒否される問題を防止）
  4. LLM 呼び出し: 要約生成 (tools なし)
  5. Warm サマリーレコード作成 (Role="summary", Tier=TierWarm)
  6. 選択 Hot レコードを Warm サマリーで置換
  7. セッション保存
```

破壊的: 元レコードは削除されサマリーで置換される。v0.1.3 以前のセッション
には typically Warm サマリーレコードが残存。v2 はこれらを不透明な事前要約
ブロックとして読む（§4.4 参照）。

### 4.3 v2 非破壊的経路 (UseV2 == true)

レコードは不変。`agent.buildMessagesV2` が毎ラウンド `contextbuild.Build`
を呼ぶ。Builder は `session.Records` を newest → oldest に walk し、
アクティブバックエンドの `MaxContextTokens` 予算内まで生レコードを
include、残りの古い tail はセッション毎キャッシュ
(`sessions/<id>/summaries.json`) から取得 or 生成した要約に折り畳む。
キャッシュキーは「レコード content + summarizer ID」のハッシュなので
キャッシュ再利用は自動、レコード編集で旧エントリは無効化される。

per-backend 予算は `cfg.ContextBudgetFor(backend)` と
`cfg.HotTokenLimitFor(backend)` で解決。Local の 16K コンテキストでは
小さい acc + 要約に、Vertex の 1M+ ウィンドウでは要約分岐は休眠したまま
全レコードが生で残る。

### 4.4 BuildMessages とサマリー扱い

順序は両経路とも同じ:

```
1. システムプロンプト（pinned + findings をマージ済み）
2. サマリーブロック — v1: legacy "summary" tier レコード、v2: キャッシュ要約
   （`【N 件の過去ターンの要約 — … から … まで】` のレンジヘッダ付き、
   LLM が古いコンテンツの時系列を認識可能）
3. 生レコード（chronological）
   - role="summary" はスキップ（step 2 で処理済）
   - "[Calling: ...]" assistant プレースホルダはスキップ（gemma 模倣防止）
   - user / tool は guard wrap（プロンプトインジェクション防御）
   - v2: 直前から 30 分超の間隔がある最初のレコード or tool/report に
     `【YYYY-MM-DD HH:MM TZ】` タイムスタンプマーカを付加
     （memory-architecture-v2.ja.md §6.5 参照）
```

## 5. オブジェクトリポジトリ

### 5.1 統一オブジェクトモデル

すべての成果物を objstore で管理:

| タイプ | 例 | 生成元 |
|--------|-----|-------|
| image | ユーザ添付画像 | SendWithImages |
| report | create-report の出力 | toolCreateReport |
| result | query-sql の結果 | toolQuerySQL |

### 5.2 画像フロー

```
ユーザが画像をドロップ
  → objstore.SaveDataURL() → ID "abc123"
  → Record.ObjectIDs = ["abc123"]
  → LLM コンテキスト: 最新画像は data URL 全体、古い画像はテキスト参照
```

### 5.3 LLM ツール

```
list-objects  — セッション内の全オブジェクト一覧
get-object    — ID でオブジェクト取得
```

### 5.4 フロントエンドでの解決

ReactMarkdown の img コンポーネント:
- `src` が `object:` で始まる → GetImageDataURL(id) で解決
- `src` が `data:` で始まる → そのまま使用

## 6. ツール実行確認 (MITL)

### 6.1 確認カテゴリ

| ツール | 確認 | 根拠 |
|--------|------|------|
| query-sql | **SQL プレビュー** | 実行前に生成 SQL を表示 |
| analyze-data | **計画承認** | 分析パースペクティブ + 対象テーブルを表示 |
| create-report | なし | read-only 出力 |
| load-data | なし | ファイルパスはユーザー指定済み |
| シェルツール (write/execute) | **MITL ダイアログ** | 危険操作の既存承認 |
| シェルツール (read) | なし | 安全な操作 |
| その他ビルトインツール | なし | 低リスク read 操作 |
| MCP ツール | **MITL ダイアログ** (デフォルト on) | 外部サービス操作 |
| Sandbox ツール | **MITL ダイアログ** (デフォルト on) | コード実行 |

### 6.2 SQL プレビュー (query-sql)

```
LLM が query-sql を {sql: "SELECT ..."} で呼ぶ
  ↓
Agent が MITL リクエストを emit: type=sql_preview
  - 表示: SQL クエリテキスト
  - 表示: 対象テーブル
  ↓
ユーザー選択:
  - 承認 → SQL 実行、結果を LLM に返却
  - 拒否 → "User rejected" を LLM に返却、LLM 再応答
  - 拒否 + フィードバック → ユーザーフィードバックを LLM にコンテキストとして返却
    ("User rejected this SQL. Feedback: ...")
    LLM が次ラウンドで新 SQL を生成
```

### 6.3 分析計画承認 (analyze-data)

data-agent の Planning → Approval → Execution パターンに着想を得た。

```
LLM が analyze-data を {prompt: "...", table: "..."} で呼ぶ
  ↓
Agent が MITL リクエストを emit: type=analysis_plan
  - 表示: 分析パースペクティブ (prompt)
  - 表示: 対象テーブル名 + 行数
  - 表示: 推定ウィンドウ数
  ↓
ユーザー選択:
  - 承認 → スライディングウィンドウ分析実行
  - 拒否 → "User rejected" を LLM に返却
  - 拒否 + フィードバック → フィードバックを LLM に返却
    ("User wants to focus on X instead of Y")
    LLM が修正パースペクティブで新 analyze-data コール生成
```

### 6.4 MITL レスポンスモデル

```go
type MITLResponse struct {
    Approved bool   // true = 続行、false = 拒否
    Feedback string // 拒否+理由のときのみ非空
}
```

フロントエンド MITL ダイアログは 3 アクション:
- **Approve** ボタン
- **Reject** ボタン (フィードバックなし)
- **Reject + テキスト入力** (フィードバックフィールド + 拒否ボタン)

フィードバック付き拒否時、LLM に返るツール結果:
```
User rejected this operation.
Feedback: <ユーザーフィードバックテキスト>
Please revise your approach based on the feedback.
```

LLM は新ユーザーメッセージ不要で次ラウンドで SQL や分析パースペクティブを修正できる。

### 6.5 シェルツール MITL (既存)

現行実装から変更なし。write/execute カテゴリのシェルツールは承認必須、
read カテゴリは直接実行。

## 7. レポート生成

### 7.1 フロー

```
ユーザー: "レポートを作成して"
  → LLM が create-report ツールを呼ぶ
    → コンテンツを objstore に保存 → report ID
    → セッションレコードとして保存 (role="report", ObjectIDs=[reportID])
    → report:created イベントをフロントエンドに emit
    → "Report created" 短文返却 (ループ防止)
  → LLM が短い確認受信
  → 次ラウンド tools=nil → LLM がテキスト応答 or 空 → ループ終了
```

### 7.2 レポート内の画像参照

LLM 記述: `![description](object:abc123)`

- `abc123` = セッション内画像の objstore ID
- フロントエンドが `object:abc123` → data URL を GetImageDataURL で解決
- ファイル保存時: 全 `object:` 参照を inline base64 に解決

### 7.3 レポート永続化

レポートはセッションレコード (role="report") **および** objstore に保存。
セッション再ロード時はレコードからレンダ。インライン外なら objstore から
コンテンツロード。

## 8. フロントエンド表示ルール

| レコードロール | 表示 | スタイル | 備考 |
|---------------|------|---------|------|
| user | Yes | 右寄せバブル | 画像サムネイル付き |
| assistant | Yes | 左寄せバブル | Markdown レンダ |
| tool | 非表示 | — | フロントエンドでフィルタ |
| report | Yes | 全幅、特別ヘッダ | タイトル + 保存ボタン |
| summary | Yes (legacy) | 専用ブロック | 凝縮表示。v2 は cache 経由でレンダ |

### 8.1 ツール実行インジケータ

ツール実行中 (Busy 状態):
- ツール名付きスピナー表示
- フォーマット済み引数を表示 (安全な場合)
- 完了時: インジケータ非表示、ストリーミング継続

加えてチャット内に「ツールコールタイムライン」が一時的なピル表示される
(v0.1.2+):
- `running`: 緑のパルスドット
- `done`: チェックマーク
- 永続化なし、セッション再ロードで消える

### 8.2 イベント

| イベント | 方向 | ペイロード | 目的 |
|---------|------|-----------|------|
| `agent:stream` | Backend → FE | `{token, done}` | ストリーミングトークン (最終応答のみ) |
| `agent:activity` | Backend → FE | `{type, detail}` | エージェント実行ステータス |
| `session:title` | Backend → FE | `{session_id, title}` | 自動生成タイトル |
| `report:created` | Backend → FE | `{title, content}` | レポートコンテンツ表示用 |
| `pinned:updated` | Backend → FE | `nil` | pinned memory 変更 |
| `mitl:request` | Backend → FE | `{tool_name, arguments, category}` | ツール承認要求 |

#### agent:activity タイプ

| タイプ | Detail | UI 表示 |
|--------|--------|---------|
| `tool_start` | ツール名 | "Executing: query-sql" (ステータスバー) |
| `tool_end` | ツール名 | ステータスバークリア |
| `thinking` | LLM 説明テキスト | 一時的な note (チャットメッセージ**ではない**) |

`thinking` タイプは旧 `agent:explanation` イベントを置き換えた。
LLM 説明テキスト (例: "I will calculate the total revenue...") は
一時インジケータとして表示、チャットメッセージリストには追加されない。
テキストはセッションレコードに既に保存されている。

## 9. 実装チェックリスト

### Phase 1: 重大修正 (完了)
- [x] レスポンス後に CompactIfNeeded を呼ぶ
- [x] Summarizer を LLM バックエンドに接続
- [x] レコード変更のたびにセッション自動保存
- [x] レポートをセッションレコードに保存 (role="report")
- [x] ツール再有効化ロジックを削除 (空 → ループ終了)
- [x] gemma タグを毎ラウンド除去

### Phase 2: オブジェクトリポジトリ (完了)
- [x] 画像を data URL → objstore ID に移行
- [x] list-objects / get-object ツール
- [x] フロントエンド object: URL 解決
- [x] レポートの画像参照をオブジェクト ID で

### Phase 3: コンテキスト予算 + イベントアーキテクチャ (完了)
- [x] ContextBudgetConfig (per-backend へ拡張済み、v0.1.2+)
- [x] BuildMessagesWithBudget (トークン予算、tool result truncation、`[Calling:]` skip)
- [x] agentLoop 中の同期圧縮
- [x] agent:activity イベント (旧 agent:explanation + agent:progress 統合)
- [x] フロントエンド activity 状態 (一時表示、チャットメッセージ非追加)
- [x] postResponseTasks WaitGroup 同期化
- [x] LM Studio / Vertex AI 結合テスト
- [x] 根本原因特定: `[Calling:]` パターン汚染

### Phase 4: ツール実行確認 (完了)
- [x] MITLResponse に Feedback 追加 (Approve / Reject / Reject+Feedback)
- [x] query-sql の SQL プレビュー MITL
- [x] analyze-data の分析計画 MITL
- [x] フロントエンド MITL ダイアログ (フィードバック入力)
- [x] フィードバックベースのツール結果による LLM 再生成
- [x] シェルツール MITL 検証
- [x] システムプロンプト言語マッチング
- [x] per-tool Enabled/MITL オーバーライド (DisabledTools + MITLOverrides)

### Phase 5: MCP + UI 仕上げ (完了)
- [x] MCP guardian 統合 (config、agent、ツール dispatch)
- [x] ツールチェイニング (毎ラウンドツール、`[Calling:]` 除外)
- [x] サイドバー v1 風再設計 (アイコンナビ、折り畳み、リサイズ)
- [x] Settings タブ化 (General/Tools/MCP)
- [x] 統一ツール管理 (Enabled + MITL トグル)
- [x] コマンドポップアップ (/help, /findings, /model)

### Phase 6+: メモリ v2 / Sandbox / Objects パネル (完了、v0.1.3+)
- [x] memory v2 (`internal/contextbuild`、opt-in)
- [x] Objects サイドバーパネル (一覧、bulk delete、export、参照スキャン)
- [x] Findings/Pinned 一括選択削除
- [x] 内蔵ツール embed + auto-install
- [x] サンドボックス実行 (6 個の sandbox-* ツール、v0.1.7)

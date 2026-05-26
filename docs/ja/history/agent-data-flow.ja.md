# エージェントデータフロー & ステート制御 — 設計ドキュメント

> 作成日: 2026-04-25 (初版), 2026-05-02 (v0.1.19 反映の改訂)
> ステータス: リリース済み — v0.1.19 における agent loop / メモリ
> ハンドリング / オブジェクトリポジトリ / post-response タスクの
> ライフサイクル / ツールイベント永続化を記述する。以前の改訂は
> 同じループの進化を残す（v0.1.16 の resilience、v0.1.18 の
> security hardening 等）が、差分は本文中で示す。
> スコープ: agent loop、セッションレコード、メモリ圧縮、オブジェクト
> リポジトリ、post-response バックグラウンドタスク、tool-event 復元。

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

### 2.1 ツールコーリング設計

ツールは毎ラウンドモデルに渡す（v1 パターン）。これによりツール
チェイン（例: 1 ターンで `get-location` → `weather`）が成立する。
最終応答 = ツール呼び出しの無いテキスト応答が返ってきた時点で
ループを終える。

以前の設計はツール実行後に `tools=nil` を渡してテキスト応答を強制
していたが、検証で:
- gemma-4 はツールが常に提示されていてもループしない
- tool calling の失敗原因はコンテキスト長ではなく `[Calling:]`
  汚染（後述）

ことが分かったので削除した。

ストリーミングは利用しない: ツールチェインが入るため「最終ラウンド」
を事前に特定できず、`Chat()` (非ストリーミング) で統一する。

### 2.2 エージェントループ設計 (v0.1.19)

```
SendWithImages(ctx, message, imageURLs)
  │
  ├── mu 取得、state == Idle を確認 (違えば ErrBusy)
  ├── 状態: Idle → Busy   (この Busy は post-task まで継続する)
  ├── ctx, a.cancel を in-flight ループ用にセット
  │
  ├── スラッシュコマンド (/model, /finding, /findings, /help)
  │   └── Busy 窓内で処理し、終わったら自分で state を Idle に戻す
  │       (agentLoop には進まない、post-task もトリガしない)
  │
  └── agentLoop(ctx, message, imageURLs)
        │
        ├── defer postResponseTasks(ctx)   ← すべての return path で発火
        ├── 画像を objstore に保存 → ID 取得
        ├── ユーザレコードをセッションに追加
        ├── セッション自動保存
        ├── 同期コンパクション (compactIfOverBudget; v1 のみ)
        │
        └── ループ (最大 maxToolRounds — config.agent.max_tool_rounds):
              │
              ├── round > 0 なら再コンパクション (v1; v2 は no-op)
              │
              ├── tools = allTools (毎ラウンド、チェイン許容)
              │   ├── builtin: resolve-date, create-report
              │   ├── analysis: load-data, query-sql, describe-data,
              │   │   promote-finding, reset, …
              │   ├── sandbox: 8 個の sandbox-* (有効化 + イメージ
              │   │   READY 時のみ登録)
              │   ├── shell: toolRegistry (内蔵 + ユーザ追加)
              │   ├── MCP: guardians (mcp__server__tool)
              │   └── filter: 無効化されたツールを除外
              │
              ├── メッセージ組み立て
              │   ├── v1: chat.BuildMessagesWithBudget
              │   │   (system+temporal+pinned+findings+warm+hot
              │   │    最新優先、[Calling:] スキップ、tool result
              │   │    切り詰め)
              │   └── v2: agent.buildMessagesV2 → contextbuild.Build
              │       (レコード不変、content-key サマリーキャッシュ、
              │        時間マーカー；memory-architecture-v2.ja.md 参照)
              │
              ├── ループ検出 ring buffer (v0.1.16) — 同じツールが
              │   連続失敗していたら次の LLM 呼び出しに 1 回限りの
              │   system note を前置して別アプローチを促す。
              │
              ├── LLM 呼び出し: backend.Chat() (非ストリーミング)。
              │   Vertex の並列 FunctionResponse 集約は vertex.go
              │   buildContents 内で処理。
              │
              ├── レスポンスクリーニング:
              │   ThinkTags + GemmaToolCallTags (Gemini の
              │   Thought-tagged Part は parseResponse で先に除去)
              │
              ├── tool_calls 無し かつ コンテンツ非空:
              │   ├── assistant レコード保存
              │   ├── セッション自動保存
              │   └── RETURN content (defer が post-task 発火)
              │
              ├── tool_calls 無し かつ コンテンツ空:
              │   ├── 1 回目: wrap-up nudge を入れて 1 ラウンド延長
              │   │   (Vertex の tokens=N/0 silent exit 対策)
              │   └── 2 回目: RETURN "" (defer はそのまま発火)
              │
              └── tool_calls あり:
                    ├── narration があれば thinking activity を発火
                    │   (progressTool バナー、チャットには出さない)
                    ├── assistant レコード保存 (Content + ToolCalls)
                    ├── 各ツール実行:
                    │   ├── tool_start activity
                    │   ├── MITL 判定（カテゴリ + per-tool override）
                    │   ├── ルーティング: builtin / analysis /
                    │   │   sandbox / MCP / shell
                    │   ├── tool result レコード保存（Status =
                    │   │   "success"/"error" を含む。LoadSession が
                    │   │   tool-event バブル復元時に使う —
                    │   │   tool-event-restore.ja.md 参照）
                    │   ├── tool_end activity
                    │   └── 生成物があれば objstore に保存
                    └── セッション自動保存

postResponseTasks(parentCtx)         ← agentLoop の defer から起動
  ├── ctx, postCancel = WithCancel(parentCtx); a.postCancel に保存
  ├── postTasksWg.Add(3)
  ├── go trackBg("title",            generateTitleIfNeeded)
  ├── go trackBg("memory-compaction", compactMemoryIfNeeded)
  ├── go trackBg("pinned-extraction", extractPinnedMemories)
  └── go { postTasksWg.Wait(); state=Idle; cancel/postCancel クリア }
       ↑ この trailing goroutine が Busy を解除する。
         フロントエンドは Busy で入力欄と Sidebar の New / Load /
         Delete を disable する — background-task-indicator.ja.md。

Abort()
  ├── a.cancel       (in-flight agentLoop を中断)
  └── a.postCancel   (走行中の post-task を中断)
       trailing goroutine が Idle に戻す。canceled タスクは INFO
       ログ + Error 空で報告 → フッタは赤フラッシュしない。
```

### 2.3 重要ルール

1. **ツールは毎ラウンド渡す.** チェインを許容する。`[Calling:]`
   汚染は build pipeline 側で除去するため再発しない。
2. **gemma テキストツールタグを毎ラウンド除去.**
   `<|tool_call>` 系の text 漏れを content として扱わない。
3. **空 assistant メッセージは記録しない.** クリーニング後の
   content が空かつ ToolCalls も無いラウンドはレコードを残さない。
4. **変更のたびに自動保存.** ユーザメッセージ、tool 結果、最終
   応答 — それぞれ即座にディスクへ。
5. **Busy は post-task 完了まで保持.** ライブと復元の表示一致 +
   pinned-fact 欠損防止のための仕様（v0.1.19 から、auto-cancel 案
   は撤回）。詳細は background-task-indicator.ja.md。

## 3. セッションレコード データモデル

### 3.1 レコード構造

```go
type Record struct {
    Timestamp    time.Time         `json:"timestamp"`
    Role         string            `json:"role"`      // user|assistant|tool|report|summary
    Content      string            `json:"content"`
    Tier         Tier              `json:"tier"`      // hot|warm|cold
    ToolCallID   string            `json:"tool_call_id,omitempty"`
    ToolName     string            `json:"tool_name,omitempty"`
    ToolCalls    []ToolCallRecord  `json:"tool_calls,omitempty"`    // tool 呼び出しを発した assistant turn
    ObjectIDs    []string          `json:"object_ids,omitempty"`    // objstore 参照
    ImageURLs    []string          `json:"image_urls,omitempty"`    // 非推奨; 旧セッションのみ
    SummaryRange *TimeRange        `json:"summary_range,omitempty"`
    Status       string            `json:"status,omitempty"`        // role=tool: "success"|"error" (v0.1.19+)
}

type ToolCallRecord struct {
    ID        string `json:"id"`
    Name      string `json:"name"`
    Arguments string `json:"arguments"`
}
```

`ToolCalls` は次ラウンドのプロトコル正準ラウンドトリップに必須:
Vertex は前 assistant turn に該当 FunctionCall Part を要求し、
OpenAI は assistant メッセージに `tool_calls` を要求する — 両者
ともこのスライスからメッセージビルド時に再構築する。詳細は
[`tool-call-roundtrip.ja.md`](./tool-call-roundtrip.ja.md)。

`Status` は `role=tool` レコードでのみ使われ、LoadSession が
tool-event バブルを success/error 表示で復元するための情報源。
v0.1.19 より前のレコードは Status 空 — restore path で
`success` にデフォルトされ、過去チャットも壊れず読める。詳細は
[`tool-event-restore.ja.md`](./tool-event-restore.ja.md)。

### 3.2 ロール

| ロール | 保存元 | LLM に送信 | UI 表示 | 備考 |
|--------|--------|-----------|--------|------|
| `user` | agentLoop | Yes (guarded) | Yes | ユーザメッセージ |
| `assistant` | agentLoop | Yes | Yes | turn のテキストは `ToolCalls` の有無に関わらずバブルとして復元する: ツール呼び出しの説明テキスト（「これから何をするか」）は `assistant_text` アクティビティでライブ表示され、復元時も同様に表示される (ADR-0025)。スキップするもの: 空 Content の純ツール呼び出し turn、レガシー `[Calling: …]` プレースホルダ。ephemeral な progressTool「思考中」バナーは復元しない。 |
| `tool` | agentLoop | Yes (guarded) | tool-event ピル (ライブ + 復元) | 結果テキスト + Status; LoadSession が tool-event バブル復元 |
| `report` | toolCreateReport | Yes | Yes (特別表示) | レポート、セッションと objstore 両方に保存 |
| `summary` | CompactIfNeeded (v1) | Yes | Yes (legacy block) | v1 destructive サマリー; v2 では生成されない |

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
| `agent:stream` | Backend → FE | `{token, done}` | ストリーミングトークン（現在は無効化、Chat() 利用; 将来 ChatStream 復活時のために予約） |
| `agent:activity` | Backend → FE | `{type, detail, status?}` | エージェント実行ステータス（tool_start / tool_end / thinking / retry_backoff）。`status` は tool_end で `success`/`error` |
| `session:title` | Backend → FE | `{session_id, title}` | 自動生成タイトル |
| `report:created` | Backend → FE | `{title, content}` | レポートコンテンツ表示用 |
| `pinned:updated` | Backend → FE | `nil` | pinned memory 変更 |
| `mitl:request` | Backend → FE | `{tool_name, arguments, category}` | ツール承認要求 |
| `bg-task:start` | Backend → FE | `{name}` | post-response タスク開始 (`name` = title / memory-compaction / pinned-extraction) |
| `bg-task:end` | Backend → FE | `{name, error}` | post-response タスク終了。`error` 空 = 成功 / cancel；非空でフッタが 5 秒赤フラッシュ |
| `sandbox:build:line` | Backend → FE | `{line}` | sandbox イメージビルドの 1 行 stdout/stderr |
| `sandbox:build:done` | Backend → FE | `{ok, error}` | sandbox イメージビルド終了 |

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

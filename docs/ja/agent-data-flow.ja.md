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

### 3.3 保存しないもの

- 空 assistant メッセージ (クリーニング後 content == "")
- ストリーミング中間コンテンツ
- イベント専用データ

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

## 6. 実装チェックリスト

### Phase 1: 重大修正
- [ ] レスポンス後に CompactIfNeeded を呼ぶ
- [ ] Summarizer を LLM バックエンドに接続
- [ ] レコード変更のたびにセッション自動保存
- [ ] レポートをセッションレコードに保存 (role="report")
- [ ] ツール再有効化ロジックを削除 (空 → ループ終了)
- [ ] gemma タグを毎ラウンド除去

### Phase 2: オブジェクトリポジトリ
- [ ] 画像を data URL → objstore ID に移行
- [ ] list-objects / get-object ツール
- [ ] フロントエンド object: URL 解決
- [ ] レポートの画像参照をオブジェクト ID で

### Phase 3: クリーンアップ
- [ ] ロールフィルタリングの一貫性 (バックエンドで統一)
- [ ] ツール実行進捗イベント
- [ ] ツール内 LLM 呼び出しのキャンセル伝播
- [ ] レコードのトークン追跡

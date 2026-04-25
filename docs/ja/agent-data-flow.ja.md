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

### 4.1 呼び出しタイミング

レスポンス後のバックグラウンドタスクとして毎回実行:

```
agentLoop 完了
  → go generateTitleIfNeeded(ctx)
  → go compactMemoryIfNeeded(ctx)    // 必須: 呼び出すこと
  → go extractPinnedMemories(ctx)
  → session.Save()                   // 同期的な最終保存
```

### 4.2 圧縮フロー

```
compactMemoryIfNeeded(ctx):
  1. Hot トークン合計計算
  2. hotTokens <= cfg.Memory.HotTokenLimit なら return
  3. 目標: リミットの 75% まで削減
  4. 最も古い Hot レコードを選択 (最新の user+assistant ペアは保持)
  5. 選択レコードから会話テキスト構築
  6. LLM 呼び出し: 要約生成 (tools なし)
  7. Warm サマリーレコード作成 (Role="summary", Tier=TierWarm)
  8. 選択 Hot レコードを Warm サマリーで置換
  9. セッション保存
  10. "memory:compacted" イベント発行
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

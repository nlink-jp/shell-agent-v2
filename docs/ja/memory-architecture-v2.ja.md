# メモリアーキテクチャ v2 — 設計ドキュメント

> 作成日: 2026-04-27
> ステータス: レビュー用 Draft
> スコープ: `internal/memory/`、`internal/chat/BuildMessages*`、`internal/agent/agent.go`（圧縮呼び出し）

## 1. 問題の要約

現行のメモリシステムは2つの異なる関心事を結合してしまっている:

1. **ストレージ** — アプリがセッションについて記憶する内容
2. **LLM コンテキスト** — 任意の LLM コール時に送信する内容

現状ではこれらが同じものとして扱われている。`Session.Records`
は `Tier`（`hot`/`warm`/`cold`）フィールドを持ち、圧縮処理は古い hot
レコードを単一の warm 要約で **置換**する。元の詳細は破棄される。

帰結:

- **ロッシーなストレージ。** 一度圧縮されると、tool result や
  assistant のターンを完全な忠実度で復元することは二度と出来ない。
  ディスクは安価であり、ユーザーが後で詳細を必要とする可能性も十分
  あるにもかかわらず。
- **唯一のコンテキスト。** グローバル単一の `HotTokenLimit`（v0.1.2
  でバックエンド毎に分割したが、根本的な結合は残存）が容量を決定。
  Vertex の 1M+ ウィンドウとローカル gemma の ~16K は同じセッション
  を最適に共有できない。一方が枯渇するか、他方が過剰圧縮される。
- **圧縮失敗でデータ消失。** Summarizer の途中失敗で、有用なステート
  が一切残らない状態に陥り得る（v0.1.1 の Vertex-400 事例 — 根本原因
  は圧縮が会話全体を空にしてしまったこと）。
- **リプレイ・別視点が不可。** セッション途中でバックエンド切替を
  しても、過去の要約を生のターンに戻せない。

## 2. ゴール

- レコードは **不変** かつ完全忠実度。ストレージは決して破壊的に
  要約しない。
- 各 LLM コールへのコンテキストは、レコードと **アクティブバックエンド
  の予算** から導出する。コール間で保持せず、毎回新規構築する。
- 予算外の古い部分は **キャッシュされた要約** で凝縮する。キャッシュ
  ミスは再計算可能。レコード本体が常に source of truth。
- 既存セッションファイルとの **後方互換**（legacy `tier` フィールド、
  legacy warm-summary レコード）。
- バックエンド毎のコンテキスト整形が自然に成立する — 同じセッションが
  Local と Vertex で異なる `Build()` 出力を返す。

## 3. 非ゴール

- 新たな長期保管ポリシー（cold/warm のセマンティクスはアクティブ経路
  から外す。アーカイブは別問題）。
- ベクトルストアではない、RAG でもない。レコードは依然として順序付き
  追記。
- pinned-memory と findings ストアは変更なし。

## 4. アーキテクチャ概要

```
                                         ┌─────────────────────────┐
  Session                                 │   ContextBuilder        │
  ┌──────────────────────┐                │                         │
  │ Records (immutable)  │ ─────────────▶ │  Build(session, budget) │
  │   user / assistant   │                │      ↓                  │
  │   tool / report      │                │  最新→古い順に walk     │
  │   summary (legacy)   │                │  予算が許す間:          │
  │   ...                │                │     生レコードを include│
  └──────────────────────┘                │  古い tail に対し:      │
                                          │     getOrSummarize()    │
  Summary cache                           │      ↓                  │
  ┌──────────────────────┐ ◀────────────▶ │  llm.Message[] を return│
  │ keyed by             │                └─────────────────────────┘
  │ (range, summarizer)  │
  │  -> cached text      │
  └──────────────────────┘
```

3つのコンポーネント:

1. **Session.Records** — 追記専用ログ。新レコードは逐語追加。Tier と
   `summary` ロールは互換シムとして残るが新コードからは書き込まれない。
2. **SummaryCache** — レコードレンジの **content-stable hash** と
   summarizer モデル識別子をキーとする。レコードを上書きしない。
   セッション横の `summaries.json` に保存。
3. **ContextBuilder** — `(session, budget, summarizer)` から
   `[]llm.Message` への純粋関数。エージェントループが LLM 呼び出しを
   必要とする度に呼ばれる。

## 5. データモデル

### 5.1 Record（ほぼ変更なし）

```go
type Record struct {
    Timestamp  time.Time  // セッション内ユニーク
    Role       string     // user | assistant | tool | report | summary(legacy)
    Content    string     // 完全忠実
    ToolCallID string
    ToolName   string
    ObjectIDs  []string   // objstore への参照
    // Tier と SummaryRange は legacy load 用にのみ残存
}
```

`Tier` は痕跡フィールド化。新コードは読み書きしない。legacy セッションは
次回の保存時に `omitempty` で省略され消えていく。

### 5.2 Session

```go
type Session struct {
    ID      string
    Title   string
    Records []Record
    // Summaries は別ファイルに保存。インラインでシリアライズしない。
}
```

### 5.3 Summary cache

```go
type SummaryEntry struct {
    RangeKey       string    // content-stable hash (§6.2 参照)
    SummarizerID   string    // 生成したバックエンド+モデル
    FromTimestamp  time.Time
    ToTimestamp    time.Time
    RecordCount    int
    Summary        string
    CreatedAt      time.Time
}

type SummaryCache struct {
    Entries []SummaryEntry  // CreatedAt 昇順、LRU/FIFO 退避
}
```

`sessions/<id>/summaries.json` に保存することで `chat.json` を軽量に
保つ。

## 6. ContextBuilder アルゴリズム

### 6.1 概要

```
Build(session, opts) -> []Message:
    msgs = [system_with_temporal + pinned + findings]
    sysTokens = estimate(msgs)

    budget = opts.MaxContextTokens - sysTokens - opts.OutputReserve

    acc = []                       // newest → oldest、prepend
    splitIdx = len(records)        // 含めない最初のレコード index
    used = 0

    for i = len(records)-1; i >= 0; i--:
        r = records[i]
        if r.Role == "summary": continue  // legacy summary は別途処理
        rendered = renderForLLM(r, opts)  // tool-result truncation を適用
        t = estimate(rendered)
        if used + t > budget && len(acc) > 0:
            splitIdx = i + 1
            break
        acc = [rendered] + acc
        used += t
        splitIdx = i

    older = records[:splitIdx]
    if non-trivial(older):
        summary = getOrCreateSummary(older, opts.SummarizerID)
        rendered = renderSummaryWithRange(summary, older[0].Timestamp, older[-1].Timestamp, len(older))
        msgs += [{role: "summary", content: rendered}]

    msgs += acc
    return msgs
```

不変条件:

- **常に最低 1 件の raw レコード。** v0.1.1 の圧縮 fix と同じガード
  で、Vertex 400（contents 空）退行を防止。
- **Tool-result の record 単位 truncation。** レンダ時のみ適用、
  ストレージ時には適用しない。バックエンド毎に異なる truncation
  レベルでレンダ可能。
- **順序は安定。** 生レコードは元の順序を維持。要約は system ブロック
  の直後、生レコードの前に挿入。
- **要約への時間レンジメタ情報。** LLM コンテキストに挿入される
  要約メッセージには必ず明示的な期間情報が付与される。詳細は §6.6。
- **全チャネルへの時間アノテーション。** LLM に情報を届ける全経路
  （生レコード・要約・pinned・findings）に時間マーカを付け、モデルが
  時系列推論できるようにする。詳細は §6.5。

### 6.5 時間アノテーション: 横断原則

LLM はコンテキスト内の **あらゆる情報がいつのものか** を認識できる
べきである。要約のレンジヘッダ（§6.6）はその一例にすぎず、同じ
原則がプロンプトに流れ込むすべての情報チャネルに適用される。

| チャネル | 時間情報の内容 | レンダ形式 |
|---------|---------------|------------|
| プロンプト先頭の temporal context | 「現在」 | 既存（`buildTemporalContext`）: 今日の日付、曜日、昨日 |
| 生レコード（user/assistant/tool/report） | レコード作成時刻 | 先頭にタイムスタンプマーカ（§6.5.1） |
| 古い tail のキャッシュ要約 | カバー範囲 | レンジヘッダ（§6.6） |
| Legacy `Role=summary` レコード | カバー範囲 | `SummaryRange` からレンジヘッダ |
| Pinned memory | 事実を学習した日 | 各事実行に `(learned 2026-04-15)` 付加 |
| Findings | 発見日 | 既存 `created_label` をプロンプトまで通す |

#### 6.5.1 生レコードのタイムスタンプ

以下のいずれかに該当する場合、レコードのレンダ済みコンテンツの
先頭にタイムスタンプマーカを付与:

- 直前のレコードから 30 分以上の間隔がある最初のレコード（連続
  したターンに対する冗長性を抑えつつ、「ここでセッション再開」
  というアンカーを LLM に与える）。
- Tool result、report、その他タイミングがドメイン的に意味を
  持つロール（クエリ実行時刻などを LLM が知る必要があるため）。

セッション最初のレコードは **付与しない**。system ブロックの
temporal context が既に「現在」を注入しているため、新規セッションの
最初のユーザターンに先頭タイムスタンプを付けると gemini-2.5-flash
が「ログ済み・過去のイベント」として読みツールコールを発行しなくなる
事象が発生したため。

フォーマット（日本語ロケール例）:

```
【2026-04-27 14:32 JST】
<コンテンツ>
```

英語ロケール:

```
[2026-04-27 14:32 JST]
<content>
```

マーカはレンダ層が付与し、レコードの `Content` 自体には保存しない。
マーカ分のトークンは予算にカウントする。

#### 6.5.2 Pinned memory と findings

- `pinned.FormatForPrompt()` は事実が学習された日時を保持
  （Pinned 構造体に `LearnedAt`）。レンダ出力に各行
  `(learned 2026-04-15)` の suffix を付加して LLM が新旧を
  判断できるようにする。
- `findings.FormatForPrompt()` は既に `CreatedLabel` を含む。
  プロンプトフォーマッタを通って LLM まで届くことを明示的な
  不変条件として確認する（現状 OK だが明文化）。

### 6.6 期間情報付き要約のレンダリング

キャッシュエントリ `SummaryEntry` には LLM 生成テキストのみを格納する。
期間情報はレンダ時に付与する（フォーマット変更があっても同じキャッシュ
エントリを再利用可能とするため）。

フォーマット（日本語ロケール例）:

```
【N 件の過去ターンの要約 — 2026-04-25 14:32 から 2026-04-27 09:18 JST】
<要約本文>
```

英語ロケールでは:

```
[Summary of N earlier turns — from 2026-04-25 14:32 to 2026-04-27 09:18 JST]
<summary text>
```

理由:

- LLM は「いつ X について話したか」「古い文脈が現在も有効か」を
  判断する必要がある場面が頻繁にある。
- 明示的な期間表記がないと、要約された内容がタイムレス／直近
  として読まれてしまい、古い出来事を現在のものとして扱うバイアス
  が生じる。
- system ブロック先頭に注入される temporal context（今日の日付・
  昨日）は **現在** をカバー。要約のレンジは **古いコンテンツが
  いつのものか** をカバー。両方が必要。

実装メモ:

- タイムゾーンはユーザーローカル（`buildTemporalContext` と同じ）
- **Legacy** `Role=summary` レコード（v2 以前の破壊的圧縮由来）は
  既存の `SummaryRange` フィールド（`From`/`To`）を使って同じ
  ヘッダをレンダ。旧データは既にレンジ情報を保持。
- 将来、複数の legacy/cached 要約を 1 older slot に統合する機能
  が入った場合は、本文を連結せず時系列順に header+body ブロック
  として並べる。

### 6.2 Summary cache のレンジキー

目標: 同じレンジ + 同じ summarizer = 同じキー（実行・プロセス間で安定）。
別の summarizer モデル = 別キー（gemma の要約と gemini の要約は互換
しない）。

```
key = sha256(
    summarizer_id || "|" ||
    record_count  || "|" ||
    first.timestamp.UnixNano || "|" ||
    last.timestamp.UnixNano  || "|" ||
    sha256(concat(レンジ内のレコードcontent))
)
```

content hash を含めることで、想定外のレコード変更（手動編集、将来の
redaction 機能など）がキャッシュを失効させる。レンジ境界だけでは不十分。

### 6.3 getOrCreateSummary

```
getOrCreateSummary(records, summarizerID):
    key = computeKey(records, summarizerID)
    if cache.has(key): return cache.get(key).Summary
    summary = summarize(records)            // LLM コール
    cache.put(key, {key, summarizerID, ..., summary})
    saveCache()
    return summary
```

失敗モード: summarizer コールが失敗しても、空要約を返して続行する。
LLM には空でない `acc`（不変条件 1）が渡るのでリクエストは拒否されない。
オペレータはエラーログで把握可能。

### 6.4 退避

ユーザーが会話を重ねるほどキャッシュは増える。デフォルトポリシー:

- セッション毎 64 エントリ上限（設定可）
- 上限超で `CreatedAt` 昇順に退避

長いセッションでも通常は数個のレンジしか発生しないため、これで十分。

## 7. マイグレーション

### 7.1 Legacy セッション読み込み

既存セッションは破壊的圧縮で生成された `Tier=warm` / `Role=summary`
レコードを持つ。ContextBuilder はこれを **不透明な事前要約レコード**
として扱う:

- Walk 中、`Role=summary` レコードは生 include 経路からスキップ。
- ただし、`older` が非空でかつ legacy summary の `SummaryRange` が
  `older` に含まれる場合、legacy summary のテキストをキャッシュ由来の
  要約に追加。

これにより旧セッションを再要約せずに読み込める。

### 7.2 書き込み

新コードは `Tier=warm` や `Role=summary` を生成しない。
`compactIfOverBudget` と `compactMemoryIfNeeded` は no-op になり、
最終的に削除される。

### 7.3 ファイル配置

- 既存: `sessions/<id>/chat.json`
- 新規: `sessions/<id>/summaries.json`（初回キャッシュ書き込み時に作成）

ロード側はこのファイルが存在しないケース（空キャッシュ）を許容する。

## 8. バックエンド毎の挙動

`Build()` に渡す `opts` はアクティブバックエンドから取得:

```
opts = BuildOptions{
    MaxContextTokens:    cfg.ContextBudgetFor(backend).MaxContextTokens,
    MaxToolResultTokens: cfg.ContextBudgetFor(backend).MaxToolResultTokens,
    SummarizerID:        activeBackend.ID(),
    OutputReserve:       4096,  // 一般値
}
```

- **Vertex**（~1M トークン）: ループは通常 break しない。要約分岐は
  休眠。Tool result は完全忠実度でレンダ。
- **ローカル gemma**（~16K-32K）: 直近の数レコードのみ生、それ以外は
  要約。Tool-result truncation も積極的に。
- **セッション中の切替**: 次回コールで新予算で再ビルド。キャッシュは
  `summarizer_id` でフィルタされるので、切替直後は一時的にミスして
  新規要約が走る。

## 9. 検証計画

### 9.1 単体テスト（`memory` および新 `context` パッケージ）

- `Build_FitsInBudget`: 各種入力で総トークン ≤ 予算
- `Build_AlwaysIncludesRecent`: 巨大な単一レコード + 極小予算でも
  最新レコードは生 include される（Vertex 400 退行防止）
- `Build_OlderFolds`: 予算超レコードあり → 要約が正しいスロットに
  挿入、生レコードは正しい順序
- `SummaryCache_KeyStability`: 同レンジ + 同 summarizer → 異プロセス
  でも同キー
- `SummaryCache_ContentMutationInvalidates`: レンジ内のレコード
  content 変更でキー変化
- `Build_LegacySummaryRecordsRespected`: `Role=summary` レコードが
  older slot に寄与
- `Build_SummaryHasTimeRangeHeader`: レンダされた要約メッセージが
  `【N 件の過去ターンの要約 — … から … まで】` 形式のヘッダで始まる
  こと（新規要約・legacy 経路の双方）
- `Build_RawRecordTimestampMarker`: 30 分以上の間隔後の最初のレコード、
  および tool/report レコードに `【YYYY-MM-DD HH:MM TZ】` プレフィックス
  が付与され、密集したターンには付かないこと
- `PinnedFormatForPrompt_HasLearnedDate`: `LearnedAt` を持つ pinned
  事実に日付 suffix が付くこと
- `FindingsFormatForPrompt_HasCreatedLabel`: findings の日付ラベルが
  レンダされること

### 9.2 統合テスト（`agent`）

- 同セッション、2 バックエンド: Vertex 予算の `Build()` の方が Local
  予算より生ターンが多い
- 長セッションでキャッシュヒット: 2回目の `Build()` は summarizer を
  呼ばない（モックバックエンドの呼び出し回数で assert）

### 9.3 手動 / staging 検証

- 既存 v0.1.x セッションファイルが変更なくチャットビューに表示
- セッション途中の backend 切替が次回ターンで適切に LLM コンテキスト
  を更新
- Summarizer 失敗経路: エージェントループはエラーなく完了、ログに
  失敗が記録、応答は（空要約 + 直近生レコード）から生成される

### 9.4 性能予算

- `Build()` 自体は records に対し O(n)、1000-record セッションで
  <50ms（キャッシュヒット時の I/O なし、ミス時の I/O 1回は許容）
- レコード毎のトークン推定はレコードに lazy field として cache し
  再計算回避

## 10. 段階導入

| Phase | スコープ | 挙動変化 |
|-------|---------|---------|
| 1 | 新 `internal/context` パッケージで `ContextBuilder`、summary cache、テスト。エージェント未統合 | なし — コードは休眠 |
| 2 | エージェントが config flag (`memory.use_v2: false` default) 経由で `ContextBuilder` を使用。OFF 時は既存破壊的圧縮を維持 | テスト用 opt-in |
| 3 | デフォルト v2。破壊的圧縮コールは no-op に。Legacy summary レコードは依然読める | 全新規セッションが追記専用 |
| 4 | `Tier` 書き込み撤廃。Legacy 圧縮コード削除。Record JSON タイト化 | クリーンなコードベース、旧ファイルのスキーマは不変 |

各フェーズが個別の PR/リリース。

## 11. リスクと open question

### リスク

- **キャッシュ drift。** 将来の機能（redaction、ユーザー駆動メッセージ
  削除）でレコード編集が起きる場合、キャッシュ無効化が漏れない必要が
  ある。content hash キーで mitigate。
- **要約品質。** 質の悪い要約は truncation よりモデル性能を落とす。
  キャッシュ内に要約長 / 品質シグナルを記録して診断可能に。
- **ストレージ増大。** ツール呼び出しの多い長期セッションは数 MB に
  なり得る。今は許容（ディスクは安価）だが、将来のアーカイブ・退避
  ポリシーは検討余地あり。

### Open question

1. **要約粒度** — 古い tail 全体を1要約 vs N レコード毎の sliding
   chunks。1要約はシンプルで v1 と同じ挙動。chunk は境界が動いた時の
   キャッシュ再利用が効く。**推奨: まず単一要約で開始。**
2. **要約用モデル選定** — 常にアクティブバックエンドを使うか、専用
   の安価モデルを使うか。**推奨: 当面アクティブバックエンド、コスト・
   レイテンシが問題化したら再検討。**
3. **キャッシュファイル形式** — 他と同じ JSON か、軽量 KV ストアか。
   **推奨: JSON、他と一貫。**
4. **pinned-memory・findings がセッション中に変化したら？** これらは
   system ブロックに影響するがキャッシュには影響しない。何もしなくて
   よい — どのみち毎コール再レンダリングされる。
5. **Legacy summary を生に戻す経路は必要？** 自動経路は無し。将来
   「セッション再水和」ツールとして公開する余地はある。

## 12. 依存関係 / 触る箇所

- `internal/memory` — Record / Session のインターフェースは不変。
  Tier が痕跡化。
- `internal/chat/BuildMessages*` — ContextBuilder で置換。Guard tag /
  pinned / findings ロジックは builder 内に移動。
- `internal/agent/agent.go` — `compactIfOverBudget` /
  `compactMemoryIfNeeded` は phase 4 で削除。Build 呼び出し
  （現 `BuildMessagesWithBudget`）が `ContextBuilder.Build` に。
- `internal/llm` — 変更なし。
- `internal/config` — 既に per-backend budget あり。追加は
  `SummarizerID`（アクティブバックエンド名 + モデルから derive）の
  配線のみ。

## 13. まとめ

レコードを single source of truth とし、不変・追記専用にする。LLM
コンテキストはコール毎に `ContextBuilder` で導出、アクティブバック
エンドの予算に合わせ、古い部分は安全な再利用のため content-key 付き
キャッシュ要約で凝縮する。

マイグレーションは段階化されており、各フェーズは独立してテスト可能、
phase 2 では config flag で revert 可能。

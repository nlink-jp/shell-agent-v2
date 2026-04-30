# 情報表示リデザイン — 設計ドキュメント

> 日付: 2026-04-30
> ステータス: ドラフト（実装未着手）
> 範囲: サイドバータブ、チャットペインのデータビュー、ステータス系の表示

## 1. 問題整理

v0.1.x の機能追加を重ねるうちに、情報表示面が場当たり的になり、
ユーザにもコードにも分かりにくい状態になっている。具体的には:

- サイドバーの `status` パネルが、性質の違う 3 つ — Findings
  （グローバル）、Pinned Memory（グローバル）、Tokens（現セッ
  ションの直近コール）— をスコープ揃わずに 1 つの汎用名でまとめ
  ている。
- サイドバーの `objects` パネルは全セッションのオブジェクトを
  混合表示するため、セッション数が増えるほど干し草の山になる。
  LLM 向けの `list-objects` は既に per-session フィルタ済み
  （`tools.go:548-552`）なのに、UI だけ全セッション混合になって
  おり、ユーザが見ている景色と LLM が見ている景色が一致しない。
- セッションにロード済みの DuckDB テーブルは LLM に問い合わせる
  まで見えない。アプリ再起動 → セッション復元後に「自分のデータが
  ちゃんと残っているか」を UI で確認する手段がない。
- サンドボックスの `/work` 一覧は `sandbox-info` ツール経由でしか
  見えず、UI 表面が存在しない。
- Token テレメトリがコンテンツパネル内に置かれているが、これは
  ナビゲーションすべき情報ではない。

根本原因: 新機能を、空きのあるパネルに次々追加してきただけで、
ユーザのメンタルモデルを軸にした整理がされていない。

## 2. 設計原則

1. **場所でスコープを表す。** サイドバーはセッション切替を跨いで
   永続する情報（グローバル）。チャットペインは現在選択中のセッ
   ションに紐づく情報。セッション切替時にチャットペインだけ視覚的
   にリセットされ、サイドバーは安定する。
2. **1 パネル 1 概念。** パネル名は単一の対象クラスを表すこと。
   `status` はこれを破っている、`Memory` は守っている。
3. **ストレージレイアウトと UX を分離。** セントラルオブジェクト
   ストアは物理レイアウト（コンテンツアドレス dedup、エクスポート
   時の cross-session 解決）として残す。ユーザに見える表面は
   どこでも per-session にする。
4. **テレメトリはナビゲーションではない。** Token カウントなどの
   ライブ更新数値はナビゲーションパネルではなく周辺の帯に置く。
5. **空は見えない。** ゼロ件のセクションは "(empty)" のような
   プレースホルダ行を出さず、自身を畳んで消える。

## 3. 最終レイアウト

```
┌────────────────────┐  ┌────────────────────────────────────┐
│  サイドバー          │  │  チャットペイン                       │
│                    │  │                                    │
│  ▣ Sessions        │  │  [Session: Q3 Sales Analysis]      │
│  ▣ Memory          │  │  ┌── Data ▾ ────────────────────┐  │
│                    │  │  │  Objects (3)                 │  │
│                    │  │  │   ▸ chart.png    234 KB       │  │
│                    │  │  │   ▸ report.md      4 KB       │  │
│                    │  │  │  Tables (2)                  │  │
│                    │  │  │   ▸ sales       1.0M rows     │  │
│                    │  │  │   ▸ customers   10K rows      │  │
│                    │  │  │  /work (5 files)             │  │
│                    │  │  │   ▸ data.csv      1.2 MB      │  │
│                    │  │  └────────────────────────────┘  │
│                    │  │                                    │
│                    │  │  …チャットメッセージ…                │
│                    │  │                                    │
│                    │  │  ┌── footer 帯 ──────────────────┐ │
│                    │  │  │ vertex_ai · hot 12 · 4.2k tok │ │
│                    │  │  └─────────────────────────────┘ │
└────────────────────┘  └────────────────────────────────────┘
```

### 3.1 サイドバー — Sessions

変更なし。全セッション一覧、クリックで切替、新規作成サポート。

### 3.2 サイドバー — Memory

現在の `status` パネルを置換。中身は Findings（グローバル）+
Pinned Memory（グローバル）。両方ともバルク選択 / 削除操作対応
（既存 UI 流用）。Token 数値と Objects リストはこのパネルから
撤去する。

### 3.3 チャットペイン — Data ディスクロージャ

チャットパネル上部に置く `<details>` 風の折りたたみブロック。
中身は現在選択中のセッションにスコープされる。3 つのサブセクション:

- **Objects** — `ListBySession(currentSessionID)` の結果を
  type フィルタ（image / blob / report）と共に列挙。画像クリック
  で既存の lightbox、blob クリックでダウンロード、report クリッ
  クで既存のレポートビューア。バルク選択 / 削除はサイドバー旧
  `objects` パネルからこちらに移設。
- **Tables** — セッションの分析エンジン上の DuckDB テーブル群。
  各行: 名前、行数、カラムリスト。テーブルクリックでモーダルを
  開き、先頭 20 行をプレビュー（読み取り専用）。編集なし。
- **/work** — サンドボックスのファイル群。`sandbox-info` の裏で
  使われているのと同じ `sandbox.WorkDir(sid)` のホスト側 walk で
  列挙。読み取り専用。サンドボックスエンジン無効時はセクション
  ごと非表示。

ディスクロージャ全体はデフォルト折りたたみ。セクションヘッダに
合計件数を出す（"Data (10)"）ので、ユーザは何かロード済みかどうか
を一目で判断できる。

3 つのサブセクションすべて空の場合（新規セッション直後）は、
「Data — empty」の muted 1 行表示にして、重い折りたたみバーを
出さない。

#### 3.3.1 サブセクションのレイアウト: 縦積み vs. タブ

Phase 1〜4 では **縦積み** レイアウト（Objects → Tables → /work
を 1 つ開いたパネル内に並べる）。各セクションがそれぞれ少数件
の段階ではこれが最もシンプルで正しい形。

**タブ式バリアント** — ディスクロージャ本体上部に 3 タブを置き、
1 セクションだけ表示 — は、いずれかのセクションが他を画面外に
押し出すほど長くなったとき（典型的には Tables ≥ 5、または /work
ファイル数が数十件規模）への進化路として用意する。タブ化は純粋に
`DataDisclosure` コンポーネント内のレンダ変更で、裏側のバインディ
ングやデータモデルは動かない。§6 の Phase 6 として記録。

### 3.4 チャットペイン — Footer 帯

メッセージリスト下、入力欄上の横一行。現バックエンド名、hot/warm
メッセージ数、直近コールの prompt/output トークン合計を muted
テキストでコンパクトに表示。既存のデータフローはそのままで、
表示場所だけサイドバーから移設する。

### 3.5 Settings モーダル

変更なし。既にナビゲーション階層から分離されている。

## 4. バックエンドバインディング

### 4.1 既存 — そのまま使う

- `b.objects.ListBySession(sid)` — 既存。`toolListObjects` で
  使われている。UI Objects サブセクションでも流用。
- `b.objects.LoadAsDataURL(id)` — lightbox で使用中。変更なし。
- `b.objects.DeleteObjects(ids)` — バルク削除で使用中。変更なし。
- `analysis.Engine.Tables()` — 既存。`[]*TableMeta`（名前 + カラム
  + 行数）を返す。
- `analysis.Engine.QuerySQL(query)` — LLM が使用、プレビュー用に
  も流用。

### 4.2 新規 — 追加するもの

| バインディング | 目的 | 裏付け |
|---|---|---|
| `GetSessionObjects(sessionID) []ObjectInfo` | UI の Objects サブセクション | `objects.ListBySession` |
| `GetSessionTables(sessionID) []TableInfo` | UI の Tables 一覧 | `analysis.Tables()` |
| `PreviewTable(name string, limit int) PreviewResult` | プレビューモーダルの先頭 N 行 | `analysis.QuerySQL("SELECT * FROM " + name + " LIMIT ?")` |
| `GetWorkFiles(sessionID) []WorkFile` | UI の /work 一覧 | `sessions/<id>/work/` を直接 walk（ホスト側マウント、コンテナホップ不要、`sandbox-info` と同じ） |

DTO:

```go
type TableInfo struct {
    Name     string   `json:"name"`
    RowCount int64    `json:"row_count"`
    Columns  []string `json:"columns"`
    Comment  string   `json:"comment,omitempty"`
}

type PreviewResult struct {
    Columns   []string `json:"columns"`
    Rows      [][]any  `json:"rows"`
    Total     int64    `json:"total"`
    Truncated bool     `json:"truncated"`
}

type WorkFile struct {
    Path  string `json:"path"`  // /work からの相対
    Size  int64  `json:"size"`
    MTime int64  `json:"mtime"` // unix ms
}
```

`PreviewTable` は数 MB ペイロードを防ぐため上限付き（既定 200 行）。
それ以上の探索は LLM 経由の `query-sql` フローに任せる。

## 5. フロントエンド変更

### 5.1 コンポーネントツリー

```
App
├── Sidebar
│   ├── SessionsPanel        （変更なし）
│   └── MemoryPanel          （旧 StatusPanel をリネーム + 整理）
└── ChatPane
    ├── SessionHeader
    ├── DataDisclosure       （新規）
    │   ├── ObjectsSection   （旧 ObjectsPanel から移設）
    │   ├── TablesSection    （新規）
    │   └── WorkFilesSection （新規）
    ├── MessageList          （変更なし）
    ├── FooterStrip          （新規。旧 StatusPanel 内 Tokens から移設）
    └── InputBar             （変更なし）
```

### 5.2 撤去するもの

- サイドバー第 3 ナビボタン (Objects) — 中身は
  `DataDisclosure > ObjectsSection` に移動。
- 旧 `status` パネル内の Tokens セクション — `FooterStrip` に移動。

### 5.3 新規セクション

- `TablesSection`: 行リスト。クリックで `PreviewTable` モーダル
  を開き、カラムヘッダ + 先頭 20 行表示。
- `WorkFilesSection`: 行リスト、読み取り専用。ダウンロード /
  プレビューは未実装（§8 未決事項）。
- `FooterStrip`: 1 行 muted テキスト、agent state イベントで更新。

### 5.4 状態管理

`DataDisclosure` は次にサブスクライブ:
- セッション切替イベント → 3 セクション全リフェッチ
- agent done / tool result イベント → リフェッチ（新ロード済み
  データ、書き込み済みオブジェクト、サンドボックスファイル反映用）
- 各セクションヘッダの明示的な「リフレッシュ」ボタン

スパム防止のため 500ms デバウンス。

## 6. 実装フェーズ

各フェーズは独立コミット / PR で、出した時点でアプリは shippable。

### Phase 1 — バックエンドバインディング

- `GetSessionTables`, `PreviewTable`, `GetWorkFiles` バインディング
  + DTO 追加。
- 各単体テスト（t.TempDir + ダミーセッション）。
- フロント未変更。既存 `Objects` パネルは継続動作、`list-objects`
  LLM ツールも無影響。

### Phase 2 — Data ディスクロージャ（読み取り専用）

- `DataDisclosure` を 3 サブセクション読み取り専用で追加。
- チャットペイン上部にレンダ、デフォルト折りたたみ。
- 既存サイドバー Objects パネルはこの段階では残す（並行）。
- 動作確認: データロード済みセッション開く → tables 表示、
  サンドボックス ON でコード実行 → /work 表示。

### Phase 3 — Objects を DataDisclosure に移設

- バルク選択 + 削除 UI をサイドバー旧 Objects パネルから
  `DataDisclosure > ObjectsSection` に移植。
- 現セッションでフィルタ（`GetSessionObjects(sid)` の新バインディ
  ング、または既存 `GetObjects` が sessionID を取れるならそれ）。
- サイドバー第 3 ナビボタン削除。
- 旧 `objects` サイドバーパネル JSX 削除。

### Phase 4 — Memory リネーム + footer 帯

- `status` パネル → `Memory` リネーム。Tokens セクション撤去。
- チャットペイン下端に `FooterStrip` 追加。Tokens で表示していた
  内容を移植。
- `GetLLMStatus` バインディングは中身そのまま（表示場所のみ変更）。

### Phase 5 — ドキュメント / 用語整理

- `docs/en/object-storage.md`（と JA 双子）: ユーザ向けセクション
  の "central object repository" 表現を "session objects" に置換。
  物理レイアウトの説明箇所のみ "central blob store" 維持。
- `README.md` / `README.ja.md`: "central object repository" 言及
  を削除または書き換え。
- `AGENTS.md`: Sidebar セクションを新レイアウトに更新。

### Phase 6 — タブ化サブセクション（条件付き、MVP 後）

`DataDisclosure` 本体を縦積みからタブ列（Objects / Tables /
/work）に refactor、1 セクションのみ表示するよう変更。実フィード
バックでスタックレイアウトが他セクションを画面外に押し出してい
ると判明した時点で着手する（先回りでは入れない）。コンポーネント
契約（props、バインディング、リフレッシュロジック）は変えず、
内部レイアウトだけ切り替える。

## 7. マイグレーション / 互換性

- **データマイグレーションなし。** `<DataDir>/objects/`、
  セッション DuckDB ファイル、サンドボックス `/work` ディレクトリ
  すべて未変更。
- **設定 / API 破壊変更なし。** Wails バインディング層に 4 メソッド
  追加するのみ、削除はゼロ。
- **LLM ツール変更なし。** `list-objects` は既に per-session フィ
  ルタなのでモデル視点は変わらず、`query-sql` / `sandbox-info` 等
  も従来通り。
- **既存ユーザレポート / 保存済み markdown** — `object:ID`
  参照は変更なしの `LoadAsDataURL` 経由で従来通り解決、SaveReport
  の `resolveObjectRefsForExport` も維持。

## 8. 未決事項

1. **`DataDisclosure` の配置**: メッセージ上部 vs. 右ペイン分割。
   既定案は上部折りたたみ。横分割は広い画面で綺麗だがレイアウト
   複雑度が倍。Phase 6+ で検討、もしくは見送り。
2. **/work ファイルの操作**: Phase 1 では読み取り専用。後で
   「デフォルトアプリで開く」「クリップボードにコピー」を追加？
   テキストファイルは Yes、バイナリは No（セキュリティ）が妥当。
   将来課題として扱う。
3. **cross-session オブジェクト参照 UI**: 現状、アクティブ以外の
   セッションのオブジェクトを表示する UX フローはゼロ。意思決定:
   `LoadAsDataURL` のグローバル ID 解決はそのまま残す（エクス
   ポートで必要）が、セッション横断ブラウズ用 UI は追加しない。
   実需が出たら、グローバル *リスト* ではなくグローバル *検索*
   面として後付け。
_（レビュー時に解決: 元の footer 帯あふれ懸念は「2 行折り返し
まで許容、メディアクエリは不要」で確定。項目削除。）_

## 9. テスト計画

各フェーズ:

- **Phase 1**: バインディング単体テスト、`make test` グリーン。
- **Phase 2**: dev セッション起動、`load-data` で CSV ロード、
  サンドボックスでコード実行、DataDisclosure に正しいカウントが
  出ることを確認。折りたたみ状態がレンダ間で保持されること。
- **Phase 3**: Data 内からオブジェクトをバルク削除、ディスクから
  削除されることと LLM の `list-objects` 出力からも消えることを
  確認。
- **Phase 4**: セッション切替で footer が更新されること、Memory
  パネルに Tokens が出なくなっていること。
- **Phase 5**: ユーザ向けドキュメントから "central object
  repository" が消えていることを grep 確認。

最終: `make build` でフルリビルド、3 サブセクションすべて使う
セッションでレイアウト目視確認。

## 10. 範囲外

- セッション横断検索（先送り）。
- UI からのテーブル / `/work` ファイル編集。
- リフェッチなしのリアルタイム更新（websocket 風 push）。500ms
  デバウンスで十分。
- Settings パネルの再編（別案件）。

# 保存クエリ派生テーブル — analyze-data 用の絞り込みサブセット — 設計ノート

**ステータス:** v0.8.0 で実装済み (2026-05-15)。
**ターゲット:** v0.8.0 (v0.7.0 からの minor bump — 分析ツール 1 個
追加、エンジン API 変更なし、破壊的変更なし)。
**報告者:** ユーザー — `analyze-data` は現状テーブル全体を対象に
する (`toolAnalyzeData` 内の `SELECT * FROM "<table>"`、
`tools.go:539`)。27k 行のログテーブルでも、絞り込んだサブセット
(直近 24 時間、エラーのみ、特定顧客のイベントのみ) に対して
sliding-window 解析したい場面が多い。現状の回避策は (a) `load-data`
の前にデータファイルを外部で前処理する、または (b) `quick-summary`
に SQL を渡す (ただし sliding-window でなく one-shot) のいずれか。

本ノートでは新ツール `save-query` を 1 つ追加し、SELECT 文の結果を
新しい DuckDB **ベーステーブル**としてマテリアライズする。
`analyze-data` は既存の `table` パラメータでその派生テーブルを
参照できる — 新しいツールカテゴリなし、エンジンスキーマ変更なし、
export/import コード変更なし、絞り込みなしの既存挙動も変更なし。

---

## 1. 問題

`analyze-data` はロード済みのテーブルに対して deep sliding-window
解析を実行する。現行の契約:

```go
// tools.go:539
query := fmt.Sprintf("SELECT * FROM \"%s\"", tableName)
results, err := a.analysis.QuerySQLForAnalyze(query)
```

唯一の調整可能パラメータは `table` のみ。「同じ sliding-window
解析を `status = 'failed'` かつ `ts >= '2026-05-01'` の行に対して
だけ」という指定はできない。

近隣ツールのカバレッジは非対称:

- `query-sql` — 呼び出し側が書いた SELECT を実行し、行を返す。
  絞り込みは可能だが出力は**チャットに出る生の行**であり、
  sliding-window 解析にはならない。スポットチェック向きで
  「異常を見つける」用途には不向き。
- `quick-summary` — 呼び出し側の SELECT を実行し **one-shot** で
  LLM 要約。絞り込みは可能だが one-shot は大規模な絞り込み集合
  には浅すぎる (`analyze-data` が積み上げる running summary の
  クロスウィンドウ文脈が失われる)。
- `query-preview` — 自然言語 → SQL → 行。解析側は quick-summary と
  同じく one-shot 制約。
- `analyze-data` — sliding-window 解析の唯一の経路だが、
  テーブル全体限定。

「絞り込みたい」と「deep sliding-window 解析したい」を組み合わせる
手段がない。

---

## 2. ゴール

1. **絞り込み → deep 解析**: ユーザー (もしくはユーザーの代理として
   LLM) が絞り込み条件を定義し、`analyze-data` を絞り込み後の集合
   に対して既存セマンティクスのまま実行できる (sliding-window、
   3-tier dedup、Findings auto-promotion、running summary、
   `MaxAnalyzeRows` 上限すべて維持)。
2. **既存のテーブル抽象を再利用**: 絞り込みサブセットはエンジン
   の `tables` マップの単なる 1 エントリ。`analyze-data`、
   `list-tables`、`describe-data`、`query-sql`、`quick-summary`、
   `query-preview` すべて変更なしで動く (どれも「X という名前の
   テーブル」しか見ない)。
3. **エンジン内部変更ゼロ**: 新規メタデータフィールドなし、
   `information_schema` 移行なし、view-vs-table 分岐なし。既存の
   `refreshTableMeta(name)` パスをそのまま再利用。
4. **export / import 変更ゼロ**: バンドルにはすでに
   `analysis.duckdb` がそのまま含まれる
   (`sessionio/export.go:38`)。派生テーブルはそのファイル内を
   他のテーブルと同様に旅する。

非ゴール:

- **同名上書きによる iteration。** `save-query` は名前衝突を
  silent な上書きではなくエラーで扱う。§3.1 参照。
- **ツールレベルの drop / クリーンアップ。** `drop-table` ツール
  なし。陳腐な派生テーブルは `reset-analysis` まで残る。§6.3
  参照。
- **DuckDB views。** 検討の上で棄却 (§6.1)。view は行重複を避ける
  が、並行するメタデータカテゴリ、外部リーダーバリデータ問題、
  ダウングレードの半透明 catalog 問題を追加する。マテリアライズ
  テーブルはシングルユーザーローカルツールに関わる全次元で
  シンプル。
- **`analyze-data` の WHERE 句のみパラメータ。** 棄却 (§6.2):
  全 SELECT より厳密に弱く、すべての呼び出し側がもう 1 つの
  パラメータを学ぶ必要が出る。
- **`analyze-data` への `sql` パラメータ直付け。** 棄却 (§6.4):
  `list-tables` での絞り込みの可視性を破壊、`analyze-data` 複数
  回呼び出しでの再利用ができない。

---

## 3. 設計

### 3.1 ツール表面

新規の分析エンジンツール 1 個、data-gated
(`HideUntilDataLoaded: true` — ベーステーブルなしでは無意味):

```yaml
name: save-query
parameters:
  sql:          required string  # 完全な SELECT 文
  name:         required string  # 新規テーブル識別子 (英数字 + アンダースコア)
  description:  optional string  # 用途の人間可読な説明
mitl: sql_preview                 # 承認ダイアログで SQL をプレビュー
category: write
```

挙動:

1. `name` が `^[A-Za-z_][A-Za-z0-9_]*$` にマッチするか検証。
2. `sql` が read-only であることを既存の `isReadOnlySQL` ガードで
   検証 (INSERT / UPDATE / DELETE / DROP / CREATE / ALTER / LOAD /
   INSTALL / PRAGMA で始まる場合は拒否)。
3. `e.tables` (エンジンの in-memory map) で名前衝突を確認:
   - 名前が存在する: 3 つの候補を含むエラーで拒否 — `<name>_v2`、
     `<name>_filtered`、`<name>_derived`。LLM が選んで再発行する。
     silent な上書きではなく明示的エラー — `load-data` でロード
     したベーステーブルが、`save-query` の偶発的な衝突で置き
     換えられてはならない。
   - 名前が空き: 続行。
4. `CREATE TABLE "<name>" AS <sql>` を実行。テーブル名は
   `sanitizeIdentifier` でサニタイズ。body SQL はそれ以上ラップ
   しない — 上記の read-only 検証がガードであり、LLM が書いた
   JOIN や列射影はそのまま残る。
5. `refreshTableMeta(name)` を呼び、新規テーブルをエンジンの
   `tables` マップに登録。これは `load-data` がすでに使用して
   いる経路と同じ; 新メソッド不要。
6. `description` が指定されていれば、既存の
   `SetTableDescription(name, description)` を呼ぶ。
7. markdown 形式のサマリを返す: テーブル名、行数、列一覧、
   source SQL の先頭 (最初の 200 文字)、description (あれば)。

エラーケース:

- 識別子 regex にマッチしない → "テーブル名は英数字とアンダースコアのみ; 受信: `<name>`"。
- SQL が read-only でない → "クエリは SELECT 文である必要があります; INSERT/UPDATE/DELETE/DROP 等は拒否されます"。
- 既存テーブルと名前衝突 → "名前 `<name>` はすでに使用中; 代わりに `<name>_v2`、`<name>_filtered`、`<name>_derived` を試してください"。
- DuckDB が SQL を拒否 (構文、不明な列) → DuckDB のエラーを `save query: ` プレフィックスつきでそのまま透過。
- 行数が `MaxAnalyzeRows` (1,000,000) を超える → 成功出力に警告を含める: "このテーブルは N 行; analyze-data は 1,000,000 を上限とするため拒否されます — analyze-data 実行前に更に絞り込んでください"。CREATE 自体は拒否しない (まず query-sql で探索したい場合があるため)。

### 3.2 エンジン

変更なし。上記 step 4 と step 5 は既存メソッドを使用。エンジンの
`tables` マップに `load-data` 呼び出しとまったく同じ方法で
エントリが増える — 同じ `refreshTableMeta` パス、同じ
`information_schema.columns` + `SELECT COUNT(*)` ルックアップ、
同じ `duckdb_tables().comment` description 取得。

`Engine.Reset()` はすでに `e.tables` 内のすべてのテーブルを
既存ループで drop する; 派生テーブルもそのループにそのまま
落ちる。

### 3.3 ツール descriptor 更新

`internal/agent/tool_descriptors_analysis.go`:

- 新規 descriptor 1 個 `save-query` を追加 (data-gated、
  `Source: "analysis"`、`Category: "write"`、
  `MITLDefault: true`、`MITLCategoryOverride: "sql_preview"`)。
- `analyze-data.Description` に 1 文追加: "絞り込み解析の場合は、
  まず `save-query` で SELECT 結果を派生テーブルとしてマテリア
  ライズしてから、そのテーブル名をここに渡してください"。LLM
  にチェーンを明示的に伝えるため、ワークフローが 1 つの
  descriptor 読み取りから発見可能になる。
- `list-tables` や `describe-data` の説明文に編集なし — 派生
  テーブルはメタデータレベルではロード済みテーブルと同一に見える。

`internal/agent/tools.go`:

- 新規ハンドラ `toolSaveQuery`。既存パターンに従う: 引数
  unmarshal → エンジン呼び出し → 結果整形。
- `formatTableMeta` 変更なし — 派生テーブルはベーステーブルと
  同一にレンダリングされる。

descriptor registry の構造的テスト (v0.6.0 で追加) が並列リスト
の漏れを自動検知する; descriptor リストに 1 個追加するだけで
全消費表面が populate される。

### 3.4 フロントエンド

フロントエンド変更なし。`save-query` は他の解析ツールと同様に
チャットに表面化する: ツール bubble、MITL ダイアログ (sql_preview
バリアントを `query-sql` から継承)、ToolDescriptor registry 駆動の
Settings → Tools タブ。

### 3.5 派生テーブル上での sliding-window 機構

`analyze-data` は `QuerySQLForAnalyze("SELECT * FROM \"<table>\"")`
を変更なしで呼ぶ。DuckDB から見れば派生テーブルはベーステーブル;
呼び出しパスは既存のものとバイト同一。analyze-data の既存テスト
(sliding-window cross-context、3-tier dedup、running-summary
cache) はすべて変更なしで通る。

### 3.6 設定

新規 config キーなし。1,000,000 行の上限 (`MaxAnalyzeRows`) は
派生テーブルにも同様に適用。

### 3.7 メモリモデルとの相互作用

`analyze-data` から auto-promote される findings はターゲット
テーブル名で tag される。派生テーブルの場合 tag は `save-query`
で指定された名前であり、これは有意味 (ユーザーは後から
`describe-data` をそのテーブルに対して呼ぶことで finding を生成
した絞り込み条件まで辿れる)。メモリモデル変更は不要。

---

## 4. テスト

### 4.1 ツール層テスト (`tools_save_query_test.go`)

- `toolSaveQuery` happy path: JSON 引数 round-trip、出力に
  新テーブルの行数と列一覧が含まれ、テーブルが `list-tables`
  に現れる。
- 名前衝突: 同名既存テーブルあり → リテラル候補リストを含む
  エラー。
- 無効な識別子 (`123`, `foo bar`, `drop table`): regex 要件を
  述べたエラー。
- 非 SELECT body (`INSERT ...`, `DROP TABLE ...`):
  `isReadOnlySQL` で拒否、read-only 要件を述べたエラー。
- 有効な SELECT だが DuckDB が拒否 (不明な列): DuckDB エラーが
  `save query: ` プレフィックスつきで透過。
- description plumbing: `description` 指定時、新テーブルへの
  `describe-data` がそれを含む。

### 4.2 統合テスト (既存 tools_test.go を拡張)

- E2E: CSV ロード → WHERE 絞り込みで save-query → 派生テーブル
  名で analyze-data → LLM アダプタが絞り込み後の行数 (mock
  backend が呼び出し引数を記録)、全体行数ではないことを assert。

### 4.3 Export / Import ラウンドトリップ

バンドルにはすでに `analysis.duckdb` がそのまま含まれる
(`sessionio/export.go:38`)。派生テーブルは DuckDB の `main`
スキーマ内の行であり、ロード済みテーブルとまったく同じように
旅する。

既存 export/import テストと同じ場所に regression テストを 1 つ:
CSV ロード → save-query → ExportSession → ImportSession →
派生テーブルが import 後セッションの `list-tables` に正しい
行数・列とともに現れる。バンドルフォーマット変更なし、version
bump なし、fixture 固定不要。

### 4.4 構造的テスト

既存の `tool_descriptor_structural_test.go` は `analysisDescriptors()`
内の全名前が全消費先 (analysisTools builder、MITL category map、
dispatcher、ListTools view) から到達可能であることを assert する。
`save-query` を descriptor リストに追加するだけで全並列パスが
自動的に運動する。新規構造的テスト不要。

### 4.5 ドキュメントミラーチェック

`scripts/docs-mirror-check.sh` は en/ja パリティを強制する。
ADR-0013 EN/JA ミラー + 両言語の INDEX エントリ。

---

## 5. リスク

- **派生テーブル蓄積によるディスク増加。** `reset-analysis` を
  挟まずに多数の `save-query` 呼び出しを行うユーザーは
  `analysis.duckdb` 内にテーブルを蓄積する。Mitigation: 派生
  テーブルは小さい (ユーザーは絞り込んでいる、増やしているわけ
  ではない)、DuckDB ファイルは per-session、`reset-analysis` で
  全消去。本リリースで per-table drop ツールを追加しない理由は
  限界効用が 2 個目の新ツールを正当化するほど大きくないため;
  ディスク枯渇のユーザー報告があれば再検討 (§6.3)。
- **名前衝突候補の枯渇。** `foo`、`foo_v2`、`foo_filtered`、
  `foo_derived` をすでに定義しているユーザーは候補リストの
  すべてが既使用となる。エラーメッセージにはリテラル 3 候補を
  含めるが、すべて使用済みなら 4 つ目はユーザーが選ぶ。
  `foo_v3`, `foo_v4`, … を反復生成はしない; LLM が選ぶ。
  descriptor は候補が示唆的で網羅的でないことを示す。
- **巨大絞り込みのマテリアライズコスト。** 5000 万行のテーブル
  と小さなルックアップテーブルとの JOIN が 1000 万行を返す場合、
  CTAS でそれら 1000 万行を物理化する。DuckDB はトランザクション
  内で高速に扱うが、メモリと時間を要する。Mitigation:
  `MaxAnalyzeRows` が下流で 100 万に上限を設ける; ユーザーが
  1000 万行の探索を行いたければ、まず `query-sql` で絞り込み
  形状を試してからマテリアライズできる。`save-query` 時点での
  エンジンガードはなし; コストはユーザー意図的。
- **ロード済みテーブルの偶発的上書き。** §3.1 でこれを silent
  OR REPLACE ではなく hard error にしている — LLM は新しい
  名前を選ぶ必要がある。これは ergonomic な iteration との
  意図的なトレードオフ; 安全性が勝る。

---

## 6. 棄却された代替案

### 6.1 マテリアライズテーブルの代わりに DuckDB VIEW

`CREATE VIEW <name> AS SELECT ...` で絞り込みを行コピーではなく
遅延 SQL 定義として扱う。**棄却**: view は行重複を避ける点で
魅力的に見えるが、不釣り合いな複雑さを追加する:

- 新メタデータカテゴリ (`TableMeta` に `IsView`, `ViewSQL`)。
- `rebuildTableMeta` を `SHOW TABLES` から
  `information_schema.tables` に移行して view を発見する必要。
- `refreshTableMeta` が `BASE TABLE` vs `VIEW` で分岐。
- view 本体は外部リーダー (`read_csv_auto` のホストファイル
  パス) を参照可能; これは export/import 後に silent に壊れる。
  バリデータが必要。
- ダウングレードで DuckDB catalog が半透明状態になる: view は
  物理的に存在するが、古い engine の `SHOW TABLES` はそれを
  列挙しない。SchemaVersion bump が必要、あるいは半透明状態を
  ドキュメント脚注として受け入れるかのいずれか。
- `Reset()` は CASCADE エッジケースを避けるためテーブルより先
  に view を drop する必要。

view 設計が回避しようとした行重複コストは*ユースケース上
現実的でない*: 絞り込みは定義上行数を減らす、DuckDB は
per-session ローカル、ディスクは安い。複雑さの税金が想像上の
便益のために支払われる。CTAS は本コードベースに影響する全
次元で厳密にシンプル。

### 6.2 `analyze-data` の `where` パラメータ

`analyze-data(prompt, table, where?)` を追加。**棄却**: WHERE
単体は完全 SELECT より厳密に弱い。ユーザーの実際の絞り込みは
ルックアップテーブルとの JOIN や派生列を含む; `where`
パラメータは些細なケースを扱い、それ以外には fallback を強い、
結局 2 つの表面を持つことになる。`save-query` は両ケースを
1 機構に collapse する。

### 6.3 同じリリースに `drop-table` ツールも追加

対称ペア: `save-query` + `drop-table` で LLM が個別派生テーブル
を掃除できるようにする。**v0.8.0 で棄却**: per-session DB に
すでに `reset-analysis` がある状況で、唯一の目的が housekeeping
である 2 つ目の新ツールを追加する。ディスク圧迫は仮想的; まず
コア機能を出荷、ユーザーが実際に症状を報告したら cleanup を
追加。

### 6.4 `analyze-data` への `sql` パラメータ直付け

`analyze-data(prompt, sql?)` で `sql` を `table` の代替に。
**棄却**: `list-tables` での絞り込みの可視性を破壊
(ユーザーから絞り込みが見えない)、`analyze-data` 複数回呼び出し
での再利用ができない (各呼び出しが SQL を再供給)、descriptor を
「2 つあるがどちらかだけ」ルールで複雑化する。`save-query` →
`analyze-data` は中間体が発見可能な 2 つのクリーンなステップ。

### 6.5 ツール追加せず LLM への意図伝達のみ

`analyze-data.Description` に「ユーザーは `load-data` 前に
エージェント外で前処理できます」と書く。**棄却**: これはすでに
ある回避策。「エージェント内のデータを使った絞り込み」という
摩擦の本丸の問題を解決しない。

### 6.6 silent な CREATE OR REPLACE TABLE

`save-query` が衝突時に上書きする。エラー+候補提案 (§3.1) を
優先して**棄却**。偶発的な派生テーブル衝突によるロード済み
テーブルの silent な上書きは現実的なリスク; 安全性のマージンが
サフィックス iteration の小さな ergonomic コストを上回る。

---

## 7. 互換性とロールアウト

- **永続化フォーマット**: 変更なし。`analysis.duckdb` はすでに
  すべてのテーブルを同一に格納する; 派生テーブルはロード済み
  テーブルと同様に `main` スキーマ内の 1 行。
- **バンドルフォーマット / SchemaVersion**: 変更なし。v0.7.0
  バンドルと v0.8.0 バンドルはバイト互換 — 派生テーブルは
  バンドルレベルではロード済みテーブルと区別不可能。前方、
  ラウンドトリップ、後方互換性すべて自明に成立。
- **Config スキーマ**: 変更なし。
- **LLM 観測可能**: 一覧に新規 1 ツール。`analyze-data.Description`
  に 1 文のポインタ。既存解析はバイト完全同一の結果を生成。
- **UI 観測可能**: Settings → Tools に新規 1 ツール。MITL
  ダイアログは既存 `sql_preview` レンダリング。
- **Import / Export**: 自明に動作。派生テーブルはテーブル。
- **ロールアウト**: v0.8.0 として出荷。CHANGELOG、README、
  README.ja に `save-query` エントリを使用例 1 つとともに
  追加。リファレンスドキュメント
  `docs/{en,ja}/reference/data-analysis.md` に「絞り込み解析」
  小節を追加。

---

## 8. 参照

- 報告者ユーザー: 2026-05-15, "analyze-data はテーブル全体しか
  解析できないので、絞り込みできると効率が上がる"。設計議論は
  view、where-param、inline-sql、educate-only のバリアントを
  検討したのち save-query CTAS に収束。
- `feedback_full_restructure_over_patch` — ツールが本来の形を
  超えたとき、パラメータ bolt-on より新ツールを優先。本ノート
  で適用: §6.2, §6.4 を棄却し新ツール 1 個に着地。
- `feedback_doc_completeness` — スキャフォールド時点でリリース
  可能なドキュメント; ADR + リファレンスドキュメント + README
  セクション + CHANGELOG エントリが同一リリースで着地する。
- v0.6.0 ToolDescriptor registry (`docs/en/adr/0007`): 新規
  descriptor 1 個が、registry 単一 source of truth 経由で全
  消費表面を完全に populate する。並列リストの手動更新不要。
- v0.4.4 analyze-data row-cap (`docs/en/adr/0005`):
  `MaxAnalyzeRows` は `QuerySQLForAnalyze` を支配、派生テーブル
  にも同様に適用。上限を超える絞り込みはユーザーの責任で
  絞り込む。
- 影響箇所:
  - `app/internal/agent/tool_descriptors_analysis.go` — 新規
    descriptor 1 個、`analyze-data` の説明文を 1 箇所微調整。
  - `app/internal/agent/tools.go` — `toolSaveQuery`。
  - `app/internal/agent/tools_save_query_test.go` — §4.1 の
    新規テストファイル。
  - `app/internal/agent/tools_test.go` (またはピア) — §4.2 統合
    ケースと §4.3 export/import ラウンドトリップを拡張で追加。
  - `docs/{en,ja}/INDEX.md` — ADR-0013 エントリ追加。
  - `docs/{en,ja}/reference/data-analysis.md` — 「save-query
    経由の絞り込み解析」小節を追加。
  - `README.md`、`README.ja.md` — 分析ツール一覧に `save-query`
    を追加。
  - `CHANGELOG.md` — v0.8.0 エントリ。

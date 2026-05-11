# analyze-data 行数上限 (v0.4.4)

## 1. 症状

ユーザーが 27,000 行のテーブルを取り込み、スライドウィンドウ
要約に処理させるつもりで `analyze-data` を実行したところ、
ツールは即座に失敗した:

```
query result exceeds 10000 rows; refine query
(e.g. add LIMIT or WHERE)
```

これはスライドウィンドウ機能の意図と真逆である —
`analyze-data` は単一の LLM コンテキストに収まらないテーブル
を扱うために存在しており、サマライザはそのようなテーブルを
チャンク単位で歩き回る設計になっている。

## 2. 根本原因

`analyze-data` は対話的な `query-sql` と同じコードパスで
対象テーブルの全行を取得している:

```go
// app/internal/agent/tools.go:807-808 (toolAnalyzeData)
query := fmt.Sprintf("SELECT * FROM %q", tableName)
results, err := a.analysis.QuerySQL(query)
```

`Engine.QuerySQL` は `MaxQueryRows = 10000` で hard-cap されて
いる:

```go
// app/internal/analysis/engine.go:295,479-482
const MaxQueryRows = 10000
...
if rowCount >= MaxQueryRows {
    return nil, fmt.Errorf("query result exceeds %d rows; refine query (e.g. add LIMIT or WHERE)", MaxQueryRows)
}
```

10,000 行という cap は対話チャット出力用の 3 つの caller
(`query-sql`, `query-preview`, `quick-summary`) にとっては
**正しい**: 結果は LLM の tool result として JSON シリアライ
ズされ、無制限の SELECT は容易にメモリ枯渇やコンテキスト
ウィンドウ突破を引き起こす。しかし `analyze-data` にとって
は **間違っている**: ここで取得した行はチャットには入らず、
100 行ウィンドウに分割されサマライザの per-window LLM 呼び
出しで消費されるだけだ。

結果として、スライドウィンドウ機能は 10,000 行を超える
テーブルでは到達不能になっている — まさにこの機能が意味を
持つ唯一の領域で。

## 3. 修正

cap を 2 つの定数に分割し、analyze 経路用に専用の `Engine`
メソッドを追加する:

```go
// app/internal/analysis/engine.go
const MaxQueryRows    = 10_000     // 変更なし: 対話チャット出力
const MaxAnalyzeRows  = 1_000_000  // 新規: スライドウィンドウ analyze 用 backstop

func (e *Engine) QuerySQLForAnalyze(query string) ([]map[string]any, error) {
    return e.querySQLBounded(query, MaxAnalyzeRows, /*hint*/ analyzeRefineHint)
}
```

既存の `QuerySQL` は挙動変更なし。内部的には両者で
`querySQLBounded(query, max, hintWhenExceeded)` ヘルパーを
共有し、read-only チェック・statement 準備・スキャンループ・
値変換を 1 箇所にまとめる — ただしこれはコード衛生のための
リファクタリングであり、挙動変更ではない。リファクタによる
ノイズが修正本体に対して大きすぎると判断したら、実装段階で
`QuerySQLForAnalyze` にループをインライン展開し既存関数は
触らない選択肢もあり。

`tools.go::toolAnalyzeData` は唯一の呼出箇所を新メソッドに
切り替える:

```go
// app/internal/agent/tools.go:808
results, err := a.analysis.QuerySQLForAnalyze(query)
```

他 3 つの caller (`toolQuerySQL`, `toolQueryPreview`,
`toolQuickSummary`) は `QuerySQL` と 10,000 行 cap を維持 —
これらは現状で正しい。

### 3.1. なぜ専用メソッドにし、QuerySQL のパラメータにしないのか

`QuerySQL(query string, max int)` のような署名変更は、既存
全 caller に数値を選ばせることになる。「チャット出力だから
10,000」が正答である caller は 4 つ中 3 つだ。名付け済み
メソッドなら呼出箇所で意図が見える: `QuerySQLForAnalyze(...)`
は *「スライドウィンドウ分析用に行を取得する; 上限はメモリ
だけ」* と読める。

### 3.2. なぜ高い数字にし、無制限にしないのか

cap を完全撤廃するのは誤り: 10 億行テーブルへの
`SELECT * FROM` は agent プロセスを OOM させ、進行中の
セッション状態を巻き添えにする。cap は **メモリ安全装置**
として存在する — クエリ形状に対する助言ではない。エラー
メッセージはこの区別を反映する (§3.3 参照)。

### 3.3. 値の選定

既存の `[]map[string]any` 表現でマテリアライズした際の
1 行あたりメモリコストの目安:

| 形状 | 1 行あたり | 27 k 行 | 100 k 行 | 1 M 行 |
|------|-----------|---------|----------|--------|
| 細い (10 列, 平均 ~30 B) | ~700 B raw + Go map overhead ≈ 1.4 KB | 38 MB | 140 MB | 1.4 GB |
| 太い (50 列, 平均 ~30 B) | ~3.2 KB                                | 86 MB | 320 MB | 3.2 GB |
| 太い+長い値 (50 列, 平均 ~200 B) | ~13 KB                       | 350 MB | 1.3 GB | 13 GB |

サマライザはその後 `[]string` (JSON エンコード) に変換し、
変換中のピーク working-set はざっくり倍になる。一般的な
16-32 GB Mac で paging なしに耐えられる上限は analyze
ステップで working-set 1-2 GB 前後。

**LLM 呼び出しレイテンシ**による現実的な上限はメモリ上限
よりはるかに低い。デフォルトの `SummarizerConfig` (100 行/
ウィンドウ、10% オーバーラップ → step 90):

| 行数 | ウィンドウ数 | LLM 時間 @ 5 秒/呼 | LLM 時間 @ 15 秒/呼 |
|------|-------------|--------------------|---------------------|
| 27 k  | ~300    | 25 分              | 75 分               |
| 100 k | ~1,100  | 90 分              | 4.5 時間            |
| 500 k | ~5,500  | 7.5 時間           | 23 時間             |
| 1 M   | ~11,000 | 15 時間            | 46 時間             |

**提案: `MaxAnalyzeRows = 1_000_000`。**

理由: cap は真のメモリ backstop であるべきで、LLM 呼び出し
への影 rate-limit ではない。ユーザーの実時間予算は実用上
~100 k 行付近で頭打ちになるため、cap が必要なのは「analyze-
data を 10 億行テーブルに誤って向けた」ようなケースだけ。
1 M 行ワーストケース (太い+長い形状で ~13 GB) は 16 GB Mac
を OOM するが、細いテーブル 1 M 行 (~1.4 GB) なら問題ない。
config knob を出さずにこの余地を確保したい。

ユーザーテストで 1 M が攻撃的すぎる (実ワークロードで連続
クラッシュ) と判明したら v0.4.5 で 500,000 に引き下げる。
定数はパッケージ private なので、後で下げても破壊的変更
ではない。

### 3.4. エラーメッセージ

cap 超過時、`QuerySQLForAnalyze` は次を返す:

```
table is too large for analyze-data (>1000000 rows);
pre-aggregate via query-sql first (GROUP BY, sample, or
date-range filter) before re-running analyze-data
```

文言は意図的に `QuerySQL` の "refine query (e.g. add LIMIT
or WHERE)" と異なる — analyze-data に `LIMIT` を加えると
スライドウィンドウの存在意義が消える (先頭 10,000 行だけ
分析してそれをテーブル全体だと言い張ることになる)。
`query-sql` で集約を実体化して小さい派生テーブルを作るのが
正しい回避策で、エラーはそう述べる。

## 4. テスト

### 4.1. Engine テスト (`internal/analysis/engine_test.go` または新規 `engine_analyze_test.go`)

- **`TestQuerySQLForAnalyze_AllowsBeyond10k`** — 12,000 行を
  ロード; `QuerySQLForAnalyze("SELECT * FROM t")` がエラーなく
  きっちり 12,000 行を返すことをアサート。リグレッション固定:
  修正前のコードパスは 10,001 で落ちる。
- **`TestQuerySQLForAnalyze_RespectsMaxAnalyzeRows`** —
  `MaxAnalyzeRows + 100` 行をロード (高コストになる可能性;
  あるいはビルドタグで定数を一時的に縮小する、あるいはテスト
  専用に小さい cap を inject するヘルパーを使う)。事前集約に
  関する具体的なエラー文字列をアサート。
- **`TestQuerySQLForAnalyze_RejectsWrite`** — read-only
  enforcement が依然有効であることを確認 (analyze 経路でも
  write SQL は不可)。
- **`TestQuerySQL_StillCapsAt10k`** — 対話 cap 不変の明示
  ガード。既存の `TestQuerySQLRowLimit` で既に cover されて
  いる可能性あり; `MaxQueryRows == 10000` を再確認し、定数に
  alias が付いていれば更新。

### 4.2. Agent テスト (`internal/agent/tools_test.go` または新規ファイル)

ストレッチゴール: モック LLM と 12 k 行テーブルで
`toolAnalyzeData` を end-to-end 走らせるスモークテストを追加
し、SQL 行数制限エラーなく完走しウィンドウ数相当のレポート
を生成することをアサート。`analyze-data` の既存テストハーネス
が重い/不在なら、§4.1 の Engine レベル cover に委ね、配線は
1 行で §6 のマニュアルスモークで verify する。

### 4.3. メモリ予算

ユニットテストで Go に 1 M 行をロードするのは遅くメモリも
重く、CI flake の元になる。`TestQuerySQLForAnalyze_RespectsMaxAnalyzeRows`
は次のいずれかを使うべき:

1. unexported な `setMaxAnalyzeRowsForTesting(t, 100)` で
   小さい cap を露出する `t.Helper()` (テスト専用 seam、
   `t.Cleanup` で revert)。
2. `-tags=slowtest` 外ではスキップする build-tag-gated test。

オプション 1 を推奨 — build matrix を増やさず高速で決定論的。

## 5. 互換性

- **公開 API**: 新メソッド 1 つ (`QuerySQLForAnalyze`) と新
  公開定数 1 つ (`MaxAnalyzeRows`) を追加。削除や signature
  変更なし。Downstream consumer (本リポジトリ内の `bindings.go`
  と agent パッケージのみ) には `toolAnalyzeData` の 1 行
  以外変更不要。
- **オンディスク形式**: 不変。DuckDB schema、manifest、config
  field 何にも触らない。
- **ユーザーから見える挙動変化**: `analyze-data` が 10,000 行
  超えのテーブルでも成功するようになる (新たな 1,000,000 行
  backstop は適用)。他のツールは挙動同一。新エラーメッセージ
  に到達するのは `MaxAnalyzeRows` を踏んだ場合のみで、意図
  的なストレス以外では非現実的。
- **Settings UI**: v0.4.4 では新ノブなし。Field report で
  固定値が不十分と判明したら `Settings → Tools → analyze-
  data max rows` ノブが明らかな後続作業; Engine メソッドの
  signature は per-call max を後で受けられる形に既になって
  いる。

## 6. マニュアルスモーク

1. `load-data` で 27,000 行 CSV をロード。
2. 意味のある perspective で `analyze-data` 実行 (例: "日次
   エラー率のスパイクを検出")。
3. `query result exceeds 10000 rows` エラーが出ないことを
   確認。
4. per-window 進捗バブル (`window 1/300`, `window 2/300`, ...)
   が in-place 更新されることを確認 — このパスは v0.4.1 で
   配線されており、リグレッションさせてはならない。
5. 最終レポートと自動 promote された findings が、それぞれ
   chat pane と Findings panel に出ることを確認。
6. 途中キャンセル (セッション閉じる / abort) → ゴルーチン
   leak が無いことを確認 (既存の agent state-machine
   `postTasksWg.Wait` がカバーするため、スモークはバブルが
   `error` / `cancelled` 状態に着地する目視確認のみで足りる)。

上限エラー経路は合成テストで十分 — スモークパスのために
1,000,001 行を手で生成するのはやり過ぎ。

## 7. スコープ外

- **`MaxAnalyzeRows` の config 化** — 固定値が間違っている
  という証拠が出てから knob を追加。
- **DuckDB から summarizer へ行を直接ストリーミング** —
  任意スケール対応に繋がるが、summarizer は `[]string` を
  取るので iterator 消費形にリファクタが必要。期待される
  利得 (full slice をマテリアライズしない) は ~500 k 行を
  超えてからしか効かないが、§3.3 の LLM 時間表が現実的に
  そこを既に除外している。
- **巨大テーブルの自動サンプリング** — "N > 100 k なら
  ランダムに 100 k サンプル" は別の UX 機能であって fix では
  ない。analyze-data が全行スキャンしたと誤解するユーザーを
  驚かせもする。
- **per-row トークンコスト推定** — summarizer は設定された
  ウィンドウサイズを信用している; per-row レベルの予算化は
  別の設計問題 (token-budget feedback memory 参照)。
- **`query-sql` / `query-preview` / `quick-summary` の
  10,000 行 cap 解除** — これらの cap は正しい。

## 8. レビュー用 open question

1. **`MaxAnalyzeRows` 値** — 1,000,000 が正しい上限か、それ
   とも 500,000 から始めるべきか。トレードオフは §3.3。
2. **エラーメッセージ文言** — 提案した "pre-aggregate via
   query-sql first" テキストがユーザーを回避策に十分誘導
   できるか、それとも具体的なヘルパー名 / docs セクションへ
   のリンクを名指しすべきか。
3. **テスト seam vs build tag** — §4.3 では unexported
   `setMaxAnalyzeRowsForTesting` ヘルパーを推奨。これで OK か、
   純度のために `slowtest` build tag を選ぶか。
4. **リファクタ範囲** — 共有 `querySQLBounded` ヘルパーに
   抽出するか、`QuerySQLForAnalyze` でループをインライン
   展開するか。前者がクリーン; 後者はバグ修正リリースとして
   diff が小さい。

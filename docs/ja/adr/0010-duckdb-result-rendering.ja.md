# DuckDB 結果レンダリング — 型ディスパッチ式スカラ変換 — 設計ノート

**Status:** Design draft (2026-05-14); 承認待ち。
**Target:** v0.6.4 (v0.6.3 の上に重ねる point release)
**報告者:** ユーザー — load-data で 17 MB JSON ファイルをロード
した際、Data パネルの GUID 列に表示不能バイトが現れた。

このノートは `internal/analysis` パッケージが DuckDB のスカラ値
を Go 値に変換する処理を、3 経路 (Data パネル preview / LLM
ツール結果 JSON / CSV エクスポート) すべてで型ディスパッチに
統一する方針を定める。既存コードは盲目的に `[]byte → string`
を行っており、`database/sql` 層でバイナリやストリンガブルでない
struct として返される DuckDB 型を片端から壊している。

discovery sweep (DuckDB の各サポート型につき 1 行を合成し、3
経路で読み戻す診断テスト) によって、報告された 1 症状 (UUID)
から **6 種のデータ正当性バグと複数の表示品質問題** に範囲が
広がることが判明した。本 ADR は Phase 1 (6 種のデータ正当性
バグ) を扱う。Phase 2 (表示品質) は別 ADR で扱う。

---

## 1. Problem

ユーザー報告: 17 MB の JSON 配列を `load-data` で読み込んだ
ところ、Data パネルのサマリで `guid` 列に表示不能文字が出る。
最初の仮説 (巨大行 truncate / 並列 JSON リーダ misalign) は
DuckDB CLI で同じファイルを読んでも再現しなかったため棄却。
`read_json_auto` のどのパラメータ組合せでも CLI ではクリーン
だった = 破損は shell-agent-v2 の読み出し経路に固有。

`engine.go:449-456` (Preview 経路) と `engine.go:374-388`
(CSV 経路) に共通する以下の変換が原因:

```go
if b, ok := v.([]byte); ok {
    values[i] = string(b)
}
```

意図は「VARCHAR 列が `database/sql` を通すと `[]byte` で返る
ので可読テキストに戻す (base64 化を防ぐ)」だった。だが述語
`v.([]byte)` は **VARCHAR に限らず** go-duckdb のバイト戻り値
すべてにマッチする。DuckDB の `read_json_auto` は値が canonical
8-4-4-4-12 形式に一致する列を UUID 型に推論する。go-duckdb
v1.8.5 は UUID 列を **canonical 文字列ではなく 16 バイトの
バイナリ形式** で返す。`string(b)` がその 16 バイトを Go 文字列
にラップ → ユーザーが見たガベージになる。

同じ盲目 cast はより広いクラスのレンダリング不具合の根本原因
でもある。discovery sweep (`engine_typesweep_test.go`) は DuckDB
の主要型を網羅した 1 行を合成して 3 経路の出力と
`rows.ColumnTypes()` メタデータをすべて吐き出す。結果が次の
マトリクス。

### 1.1 Discovery sweep 結果マトリクス

| DuckDB 型       | DatabaseTypeName    | Preview UI                              | QuerySQL → JSON (LLM)                   | CSV エクスポート             | 判定        |
|-----------------|---------------------|-----------------------------------------|------------------------------------------|------------------------------|-------------|
| VARCHAR / INTEGER / BIGINT / UBIGINT / HUGEINT / DOUBLE / BOOLEAN | (各ネイティブ) | OK                                      | OK                                       | OK                           | OK          |
| DATE            | DATE                | Go `time.Time` リテラル                 | RFC3339                                  | RFC3339                      | OK          |
| TIME            | TIME                | `0001-01-01T12:34:56Z` (epoch 年混入)   | `0001-01-01T12:34:56Z`                   | 同                           | **バグ**    |
| TIMESTAMP       | TIMESTAMP           | Go `time.Time` リテラル                 | RFC3339                                  | RFC3339                      | OK (Phase 2)|
| TIMESTAMPTZ     | TIMESTAMPTZ         | UTC 化、元 TZ 喪失                      | UTC 化                                   | UTC 化                       | Phase 2     |
| DECIMAL         | `DECIMAL(p,s)`      | `{10 3 123456}` (struct 露出)           | `{"Width":10,"Scale":3,"Value":123456}` | `{10 3 123456}`              | **バグ**    |
| INTERVAL        | INTERVAL            | `{0 14 0}` (struct 露出)                | `{"days":0,"months":14,"micros":0}`     | `{0 14 0}`                   | **バグ**    |
| UUID            | UUID                | 16 バイト raw を string にラップ        | バイナリ形式の base64                    | 16 バイト raw                | **バグ**    |
| BLOB            | BLOB                | raw bytes ラップ                        | base64 (JSON 仕様)                       | raw bytes                    | **バグ**    |
| MAP(K,V)        | `MAP(...)`          | `map[k1:v1 ...]` (Go fmt)               | **空文字列** (行全体の marshal が失敗)   | `map[...]`                   | **バグ**    |
| LIST `T[]`      | `T[]`               | `[1 2 3]` (Go fmt)                      | `[1,2,3]`                                | `[1 2 3]`                    | Phase 2     |
| STRUCT          | `STRUCT(...)`       | `map[a:1 b:x]` (Go fmt)                 | `{"a":1,"b":"x"}`                        | `map[a:1 b:x]`               | Phase 2     |
| JSON (DuckDB 型) | VARCHAR (sic)      | `map[x:1]` (Go fmt — 既に parse 済み)   | `{"x":1}`                                | `map[x:1]`                   | Phase 2     |

「**バグ**」行の 6 件はデータの正当性が崩れている (LLM を誤誘導 :
UUID/BLOB/DECIMAL/INTERVAL/MAP / ユーザーに無意味な値を見せる :
TIME の epoch 年 / Wails イベントの payload 全体を黙殺 : MAP の
行全体 `json.Marshal` 失敗)。これらが Phase 1。

「Phase 2」行は表示品質問題 (Go の default formatter が
`[1 2 3]` を返すのを `[1,2,3]` 形にしたい等)、または DuckDB の
内部仕様 (TIMESTAMPTZ は UTC normalise される) に由来する。
修正は望ましいがブロッキングではないので別 ADR に分離する。

### 1.2 なぜ今まで露見しなかったか

- 既存テストカバレッジは VARCHAR / INTEGER / DOUBLE のみ
  (`TestPreviewTable` は名前 + 年齢の CSV)。バグクラスの型は
  ひとつもフィクスチャに登場していなかった。
- Preview 経路の盲目 cast はあらゆるバイナリ型を「文字列っぽく」
  見せていた。UUID 型に推論される列を含む JSON を実ユーザーが
  load するまで、誰も破損に気づけなかった。
- DuckDB CLI は UUID / DECIMAL を出力時に auto format するため、
  CLI ユーザーはバイナリ形式に触れない。`database/sql` を通す
  consumer だけが影響を受ける。

---

## 2. Goals

1. **正しいレンダリング**: UUID / BLOB / DECIMAL / INTERVAL /
   MAP / TIME が 3 経路で正しく表示される。
2. **1 helper、3 呼出箇所**: Preview / QuerySQL / QuerySQLToCSV
   が同じ変換を経由する。将来の追加修正を 3 箇所に当て直さなくて済む。
3. **値 sniffing でなく型ディスパッチ**: dispatch key は
   `rows.ColumnTypes()[i].DatabaseTypeName()` のみ。
   `len(b) == 16` 推測や `utf8.Valid(b)` 経験則は使わない (16
   バイト VARCHAR や偶然 valid UTF-8 のバイナリを誤分類する)。
4. **任意の型組合せで行全体の JSON marshal が成功する**: 現状
   MAP 由来でサイレント失敗 → React に Preview が届かない。これを潰す。
5. **discovery sweep を恒久 regression guard として残す**:
   将来の DuckDB upgrade、新規型、driver 差替え時に sweep の出力
   差分で気づける。
6. **永続化フォーマット変更なし**: セッション、`.shellagent`
   エクスポート、メモリレコードはバイト互換。

Non-goals (Phase 2 へ送り):

- LIST / STRUCT / JSON-as-VARCHAR の表示品質改善
- TIMESTAMPTZ の元 TZ 保持 (DuckDB 内部で normalise されるので
  Go 側の修正ではない)
- `github.com/marcboeker/go-duckdb/v2` への移行 (別評価。
  型ディスパッチ修正はどの driver 版でも必要)

---

## 3. Design

### 3.1 Helper シグネチャ

```go
// renderScalar は database/sql 経由で受け取った 1 値を、後段の
// JSON marshal にも人間表示にも安全な Go 値に変換する。型分岐
// は DuckDB の型名 (sql.ColumnType.DatabaseTypeName) に基づく。
// 実行時 Go 型を sniff する手法は、テキスト形のバイナリを誤分類
// するので採用しない。
func renderScalar(v any, dbTypeName string) any
```

型ごとの戻り値:

| dbTypeName            | 戻り Go 値                                                       |
|-----------------------|------------------------------------------------------------------|
| `UUID`                | canonical 小文字 `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` (16 byte → format) |
| `BLOB`                | `[]byte` (そのまま) — JSON は base64 (仕様)、CSV は明示的 base64 化 |
| `DECIMAL(p,s)`        | canonical decimal 文字列 `"123.456"` (`duckdb.Decimal` の Width/Scale/Value から組み立て) |
| `INTERVAL`            | ISO-8601 文字列 `"P1Y2M3DT4H5M6S"` (`duckdb.Interval` の Months/Days/Micros から) |
| `MAP(K,V)`            | `map[string]any` (`duckdb.Map` から構築。JSON marshal を可能化) |
| `TIME`                | 文字列 `"12:34:56"` (時刻のみ。`0001-01-01` 接頭辞を落とす) |
| `VARCHAR` / `*INTEGER` / `*BIGINT` / `DOUBLE` / `BOOLEAN` / `DATE` / `TIMESTAMP*` | 変更なし |
| その他 (LIST, STRUCT, ENUM, BIT, 将来型) | 変更なし (Phase 2 領域) |

### 3.2 Dispatch table

上の対応は `internal/analysis` 内の小さい table
(`map[string]func(any) any`)。`DECIMAL(*)` / `MAP(*)` はパラメ
ータ構文が変わるので prefix match。

理由: 素の型名 switch + 2 つの prefix 検査で読みやすさを保ち、
次の type-class バグが出たときに 1 行追加で済む。

### 3.3 配線 — 3 呼出箇所

3 経路はいずれも `rows.Next()` で iterate して `rows.Scan` を
`[]any` に行う構造。修正の形は同一:

1. `rows.Columns()` の直後に `rows.ColumnTypes()` も呼び、
   各列の `DatabaseTypeName()` を 1 回キャッシュ
2. `rows.Next()` ループ内で既存の条件 cast を
   `values[i] = renderScalar(values[i], dbTypeNames[i])` に
   置換
3. CSV 経路: `renderScalar` 通過後、既存 `csvFormat` が
   `string` / `time.Time` / その他を均等に扱える。BLOB だけ
   例外で、`csvFormat` に明示的な `[]byte → base64` (or `hex`)
   分岐を 1 つ足す → CSV cell に raw binary が入らない

### 3.4 BLOB の CSV エンコード選択

Preview / JSON 経路は明確: JSON で標準のバイナリエンコードは
base64 のみ、JS クライアントは straightforward に decode できる。
CSV にはバイナリ標準がない。候補 2 つ:

- **base64**: 短い、ユビキタス、ただし `+` `/` を含むため一部
  の下流 CSV パーサが unquoted フィールドで誤動作する。引用
  内なら常に安全 (`encoding/csv` はデフォルトで引用する)
- **hex**: 出力最長、引用懸念なし、grep しやすい

**base64** を採用。`encoding/csv` は `,` `"` `\n` を含むフィー
ルドを quote するため、base64 の `+/=` 文字は安全に round-trip
する。`csvFormat` 直上のコメントで明文化。

### 3.5 HUGEINT と UBIGINT

sweep の結果、両者とも 3 経路で正しくレンダリングされる:
`*big.Int` は自前の `MarshalJSON` を持ち、uint64 も標準ライブラリ
で正しく扱われる。修正不要。

### 3.6 行全体の JSON marshal

`MAP(...)` 列が `duckdb.Map` でなく `map[string]any` を返すよう
になれば、`json.Marshal(prev.Rows[0])` は任意の型組合せで成功
する。追加修正不要。

---

## 4. Testing

### 4.1 discovery sweep を regression test に格上げ

現状の `engine_typesweep_test.go` は `t.Logf` のみの diagnostic。
修正後:

- 明示的な `want` table (列ごとに: 期待 Go 型 + 期待 JSON 形 +
  期待 CSV 形) を 6 種固定型に対して定義し、assert
- 未修正型は `t.Logf` のみで残し、将来 DuckDB バージョンで
  LIST / STRUCT 等の rendering が変わったら sweep が可視 diff
  を吐くようにする
- ファイル冒頭に「これは type-dispatch 挙動の恒久 guard、新
  DuckDB 型を追加する PR は必ずこのテストを拡張する」と明記

### 4.2 ユーザー報告症状の固定 regression test

`TestUUIDLoadedFromJSON_RendersCanonically` を別途追加。
UUID 形式の文字列 1 個を含む JSONL を合成して load、`DESCRIBE`
で UUID 推論を確認、Preview / QuerySQL / CSV すべてで canonical
形が出ることを assert。報告された原症状を sweep とは独立に固定。

### 4.3 行全体 marshal テスト

`TestPreviewRowMarshalsToJSON_AllSweepTypes` で sweep table に
対して `json.Marshal(prev.Rows[0])` を呼び、空でない有効な
JSON object が返ることを assert。MAP 由来のサイレント失敗を固定。

---

## 5. Risks & open questions

- **将来の DuckDB 型追加** (UNION, BIT-vector 拡張, 固定長
  ARRAY 等) は `[]byte` か custom struct で着地する可能性が高く、
  新たな dispatch エントリが必要になる。sweep test が可視 diff
  で気づかせるが auto-fix ではない。許容: 1 行追加で済む構造
- **go-duckdb v1.8.5 → v2.4.3 移行** で一部のバイナリ表現が
  変わる可能性 (UUID が v2 で `string` 戻りになる等)。dispatch
  table は graceful degrade — 既に string な型はそのまま流れる。
  v2 移行は別 ADR で評価し、今回の修正は前提なし
- **DECIMAL の精度エッジ**: `duckdb.Decimal` は Width/Scale/Value
  (big.Int) を露出。素朴な `strconv` では scale > 0 を正しく扱え
  ない。実装は `value.String()` 後、長さ - scale 位置に小数点を
  挿入 (scale ≥ 長さ なら先頭 0 埋め)。
  Width=10, Scale=3, Value=123456 → `"123.456"` /
  Width=18, Scale=2, Value=1 → `"0.01"` でテスト
- **INTERVAL の ISO-8601 vs `duckdb.Interval` フィールド**:
  ISO-8601 が最もポータブル。ただし `duckdb.Interval` は
  months / days / micros の tuple で DuckDB 自身は normalise
  しない (`P1M30D` と `P2M0D` は別の値)。出力は 0 のコンポー
  ネントも含めて全成分保持、例えば 14 ヶ月は `P0Y14M0DT0S`
- **TIME の sub-second 精度**: DuckDB TIME はマイクロ秒精度。
  Go の format `15:04:05.999999` で十分 (末尾 0 は `.9` で
  落ちるので秒のみは `12:34:56` 表示)
- **CSV の base64 padding `=` 文字**: CSV body の unquoted
  フィールドで `=` は許容されるが、一部下流パーサ (特に古い
  Excel import) では trailing `=` を formula trigger とみなす。
  `encoding/csv` が常に quote する経路で送るため defensive
  escape 不要

---

## 6. Rejected alternatives

### 6.1 サーバー側で `SELECT col::VARCHAR` 強制

すべての read query を全列 VARCHAR cast に書き換える。**却下**:
型情報をすべてテキストに潰す (tool result の typed number / UI
の `time.Time` を失う)、また任意ユーザークエリの `SELECT *` には
適用不可

### 6.2 `utf8.Valid(b)` のみで判定

`[]byte` を valid UTF-8 のときだけ string 化、それ以外はバイト
のまま流す。**却下**: 16 バイト UUID バイナリは偶然 valid UTF-8
である頻度が高い (バイト範囲が ASCII printable + Latin-1 と重なる)。
ユーザー再現データはたまたま化けて見えただけで、別の UUID では
plausible-but-wrong テキストがサイレントに通る。型ディスパッチが
唯一の deterministic 解

### 6.3 DuckDB の UUID 推論を無効化

`LoadJSON` / `LoadJSONL` に「UUID-shaped 文字列を VARCHAR のまま
にする」パラメータを渡す。**却下**: DuckDB はこの knob を
`read_json` に露出していない (`auto_detect=false` は推論を
**全部** 止めるため、呼出側で完全 schema を渡す必要がある)。
型ディスパッチ修正なら、データは DuckDB に正しく入っているまま
レンダリング層で解決できる

### 6.4 全 `[]byte` を hex / base64 に無条件変換

bytes を常に hex か base64。**却下**: VARCHAR 列 (go-duckdb から
正当に `[]byte` で返る) が hex 文字列になる — UI で無用、CSV で
非可読、LLM トークンの無駄遣い

### 6.5 go-duckdb を先に v2.4.3 に upgrade

driver を bump して新版で binary-UUID 癖がないことを期待。
**前提条件としては却下** (follow-up としては still on the table):
v1 → v2 移行はそれ自体がリスクのある変更 (full API audit、
DuckDB を触る全 caller、sandbox image bump、analysis テスト全体
の regression sweep)。型ディスパッチ修正は小規模で self-contained、
どの driver 版でも有効。修正を先に shipping し、upgrade は後で評価

### 6.6 Phase 1 + Phase 2 を同一リリースで

LIST / STRUCT / JSON 表示改善と TIMESTAMPTZ 対応も v0.6.4 に
混ぜる。**却下**: review 範囲と regression リスクを膨らませる。
Phase 1 はデータ正当性 (ユーザーが今まさに誤データを見せられて
いる)、Phase 2 は表示品質 (見栄えが悪いだけ)。Phase 1 を速く
ship、Phase 2 は設計が固まったら別途

---

## 7. Compatibility & rollout

- **永続化フォーマット**: 変更なし。ColumnType 情報や rendering
  の選択はディスクに乗らない。変換は結果取得時のみ
- **LLM observable**: tool 結果中、以前は base64-of-UUID-bytes
  だったところが canonical UUID 文字列になる。DECIMAL / INTERVAL /
  MAP / TIME も同様。これは strict improvement (LLM が UUID を
  認識できる) だが、既存の pinned-context セッションでは LLM
  挙動が変わる可能性がある (壊れた文字列ではなく正しい文字列を
  見るため)。CHANGELOG に明記
- **CSV observable**: 既存の下流パイプラインが UUID/BLOB/DECIMAL
  列の CSV を読んでいた場合、以前はガベージバイトを受け取って
  いた。挙動変化は壊れた → 正しい。もしガベージを何らかの
  形でパースしているスクリプトがあれば、それは壊れる。before /
  after の例を CHANGELOG に明示
- **UI observable**: Data パネルで UUID 列が文字化けしていた
  ところが canonical 形になる。フロントエンド変更不要
- **Rollout**: Phase 1 を v0.6.4 として ship、test sweep を
  assertion 付きに格上げ。Phase 2 は別 ADR (番号未定) と別の
  point release

---

## 8. References

- ユーザー報告: 2026-05-14、「load-data で大きなサイズの JSON
  をロードする際に、一部のデータが壊れている」(UUID 列を含む
  17 MB JSON ファイルでデータ破損表示)
- DuckDB 型システム リファレンス:
  https://duckdb.org/docs/sql/data_types/overview
- go-duckdb v1.8.5 driver:
  https://github.com/marcboeker/go-duckdb (v1 line)
- Discovery sweep test:
  `app/internal/analysis/engine_typesweep_test.go`
- 既存影響箇所:
  - `app/internal/analysis/engine.go:449-456` (Preview)
  - `app/internal/analysis/engine.go:374-388` (csvFormat)
  - `app/internal/analysis/engine.go:539-547` (QuerySQL)

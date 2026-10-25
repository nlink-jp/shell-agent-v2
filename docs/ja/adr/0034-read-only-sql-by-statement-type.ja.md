# ADR-0034: 読み取り専用の SQL を DuckDB の文の種別で判定する

- Status: Accepted
- Deciders: magi
- Related: ADR-0013 (保存クエリテーブル — その `isReadOnlySQL` ゲートを本 ADR で置き換える)、ADR-0033 (duckdb-go ドライバ)

## 1. コンテキスト

query-sql・quick-summary・export-sql-to-csv は読み取りツール (MITL 承認なし)
で、create-from-query はテーブルを追加するだけである。これらが実行する SQL は
LLM が書く。これまで `isReadOnlySQL` は、テキストが 9 個のキーワードのどれかで
*始まらない*ことで読み取り専用かを判定していた。

2026-10-26 に v0.18.0 で実測した結果:

- `SELECT 1; DROP TABLE t` が判定を通り、テーブルが消えた。ドライバは複数文の
  クエリを準備する際に最後以外の文を実行するため、判定が見たのは先頭の `SELECT`
  だけだった。
- `WITH c AS (…) INSERT …`、`COPY t TO '<file>'`、`ATTACH`、`SET`、`CALL`、
  `EXPLAIN ANALYZE <書き込み>` も通った — どれも一覧のキーワードで始まらない。
  DuckDB 2.0 ではこの種の文がさらに増える。

判定は無限の領域 (クエリテキスト) の上で行われ、次の綴りを足し続ける必要があった。

## 2. 決定

DuckDB のパーサーに判定させる。`requireSelect` はドライバの `Prepare` — 複数文の
クエリは先頭の文を実行せずに拒否する — でクエリを準備し、パーサーが付ける有限の
列挙である文の種別を読む。クエリはちょうど 1 文で、その種別が SELECT でなければ
ならない。判定と実行は、確保した同じ接続の上で、同じ文字列に対して行う。

- 許可 (種別 SELECT): `SELECT`、`FROM t`、`WITH … SELECT`、`VALUES`、`DESCRIBE`、
  `SUMMARIZE`、`SHOW`、`PRAGMA table_info`。
- 何も実行される前に拒否: 2 つ目の文、`INSERT` / `UPDATE` / `DELETE` (`WITH` の
  有無を問わず)、`CREATE`、`ALTER`、`DROP`、`COPY`、`ATTACH`、`SET`、`INSTALL`、
  `LOAD`、`CALL`、`EXPLAIN` (`ANALYZE` を含む)、動的 `PIVOT` (複数文になる)。
- `query('<sql>')` は、引数が単一の SELECT でなければ DuckDB 自身が拒否する。
- create-from-query は本体を同じ方法で判定し、その接続で
  `CREATE TABLE <name> AS <本体>` を実行する。書き込みは作るテーブルだけになる。

構文エラーや名前解決のエラーは DuckDB 自身のもので、従来どおり `query:` /
`save query:` を付けて返す。ゲート自身の拒否だけが専用のメッセージを使う。

## 3. 結果

- 「一覧のキーワードで始まらない書き込み」というクラスが閉じる。判定はクエリの
  綴りに依存しなくなる。
- 旧判定が通していた `EXPLAIN` と動的 `PIVOT` は拒否される。値を明示した
  `PIVOT … ON … IN (…)` と通常の SELECT で同じことができる。
- テストは、拒否する各形を 3 つの読み取り経路と create-from-query のすべてに通し、
  エラーが返ることだけでなく、その後もデータベースが変わっていないことを確かめる。

## 4. 受け入れた残余: ファイルの読み取り

SELECT は今もファイルや URL を読める (`read_csv('<path>')`、`read_parquet(…)`)。
これを塞ぐ案は実測したうえで却下した:

- DuckDB の `enable_external_access = false` は実行中に緩められない (無効にした後は
  `allowed_paths` を変更できない)。利用者のファイルを `read_csv_auto` で読む
  load-data が動かなくなり、同じ実測で一時ディレクトリへの退避も out-of-memory で
  失敗した。
- エージェントは読み取り系シェルツールで、承認なしにすでにファイルを読める。SQL から
  だけ取り上げても、エージェントにできることは変わらず、場所が移るだけである。

このため、DuckDB が修正を出すまで、CVE-2026-111497 (同梱 Brotli 1.1.0) への
parquet 経路は到達可能なまま残る (v0.18.0 の CHANGELOG を参照)。

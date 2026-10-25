# ADR-0034: エージェントの SQL の読み取り専用をエンジンで強制する

- Status: Accepted
- Deciders: magi
- Related: ADR-0013 (保存クエリテーブル — その `isReadOnlySQL` ゲートを本 ADR で置き換える)、ADR-0033 (duckdb-go ドライバ)

## 1. コンテキスト

query_sql・quick_summary・analyze_data・export-sql-to-csv は読むだけで、
save_query (create-from-query) はテーブルを追加するだけである。SQL は LLM が
書く。query_sql・analyze_data・save_query は既定で MITL 承認を求めるが、
quick_summary は求めず、どれも Settings で承認を外せる。これまで
`isReadOnlySQL` は、テキストが 9 個のキーワードのどれかで*始まらない*ことで
読み取り専用かを判定していた。

2026-10-26 に v0.18.0 で実測した結果:

- `SELECT 1; DROP TABLE t` が通り、テーブルが消えた。ドライバは複数文の
  クエリを準備する際に最後以外の文を実行するため、判定が見たのは先頭の
  `SELECT` だけだった。
- `WITH c AS (…) INSERT …`、`COPY t TO '<file>'`、`ATTACH`、`SET`、`CALL`、
  `EXPLAIN ANALYZE <書き込み>` も通った。

最初の置き換え — DuckDB の文の種別が単一の SELECT であること — も、
リリース前レビューの実測で破られた:

- トップレベルのデータ変更 CTE
  `WITH d AS (DELETE FROM t RETURNING *) SELECT count(*) FROM d` は種別が
  SELECT で、実行されて行が消えた。同じ本体を save_query に渡すと、自分の
  テーブルを作りながら別のテーブルにも挿入した。
- テーブル関数の通常の SELECT がデータベースの外に書き込む:
  `enable_profiling(save_location='<file>')` は既存のファイルを上書きし、
  `enable_logging(storage='file', …)` はログのディレクトリを作り、
  `read_csv('https://…')` は httpfs 拡張をインストールした。

どちらの判定も「書き込まない」を代用品 (テキスト、次いで文の種別) から
決めていた。代用品はどれも、自分が表していない書き込みを通した。

## 2. 決定

エンジンで強制する。3 つの層が、互いに閉じられないものを閉じる:

1. **文の種別** (`requireSelect`)。クエリをドライバの `Prepare` — 複数文の
   クエリはどの文も実行せずに拒否する — で準備し、種別が SELECT の 1 文で
   あることを求める。`INSERT` / `UPDATE` / `DELETE`、`CREATE`、`ALTER`、
   `DROP`、`COPY`、`ATTACH`、`SET`、`INSTALL`、`LOAD`、`CALL`、`EXPLAIN`、
   動的 `PIVOT` は何も実行される前に、エージェントが直せるメッセージで
   拒否される。`SELECT`、`FROM t`、`WITH … SELECT`、`VALUES`、`DESCRIBE`、
   `SUMMARIZE`、`SHOW`、`PIVOT … IN (…)` は種別が SELECT である。
2. **読み取り専用トランザクション** (`beginRead`)。読み取り経路は確保した
   接続の上で `BEGIN TRANSACTION READ ONLY` の中でクエリを実行し、終わったら
   ロールバックする。その中では、データ変更 CTE を含め、DuckDB がデータベース
   への書き込みを拒否する。ROLLBACK に失敗した接続は、読み取り専用のまま
   プールに戻さず破棄する。
3. **設定のロック** (`lockConfiguration`、Open 時)。拡張機能の自動インストール
   と自動ロードを切り、`lock_configuration` を設定する。すると DuckDB は
   どのクエリからの `enable_profiling` / `enable_logging` も拒否し、URL を
   読んでも拡張機能はインストールされない。エンジンは Open の後に設定を
   変えず、load-data が読む形式 (CSV、JSON) は組み込みである。

save_query は書き込む必要があるので、本体を層 1 で判定し、
`CREATE TABLE <name> AS SELECT * FROM (<本体>)` を実行する。入れ子にすると
本体はサブクエリとして束縛され、そこでは DuckDB がデータ変更 CTE を拒否する。
テーブル関数は層 3 が覆う。

構文エラーや名前解決のエラーは DuckDB 自身のもので、従来どおり `query:` /
`save query:` を付けて返す。層 1 の拒否だけが専用のメッセージを使う。

## 3. 結果

- エージェントの SQL が書き込めるかは、クエリの綴りではなく、DuckDB の
  トランザクションと設定の仕組みが決める。
- 旧判定が通していた `EXPLAIN` と動的 `PIVOT` は拒否される。
  `PIVOT … ON … IN (…)` と通常の SELECT で同じことができる。
- テストは、拒否する各形 — 複数文の先頭に置いた書き込み、データ変更 CTE、
  ファイルを書くテーブル関数を含む — を 3 つの読み取り経路と save_query の
  すべてに通し、その後のテーブル・アタッチされたデータベース・ファイルを
  確かめる。層 2 と層 3 を外した対照実行ではテストが失敗する。

## 4. 受け入れた残余: ファイルの読み取り

SELECT は今もローカルファイルを読める (`read_csv('<path>')`、
`read_parquet(…)`)。これを塞ぐ案は実測したうえで却下した:

- DuckDB の `enable_external_access = false` は実行中に緩められない (無効に
  した後は `allowed_paths` を変更できない)。利用者のファイルを
  `read_csv_auto` で読む load-data が動かなくなり、同じ実測で一時
  ディレクトリへの退避も out-of-memory で失敗した。
- エージェントは読み取り系シェルツールで、承認なしにすでにファイルを読める。
  SQL からだけ取り上げても、エージェントにできることは変わらず、場所が移る
  だけである。

自動インストールを切ったので、URL の読み取りは拡張機能が既にインストール
されていない限り失敗する。CVE-2026-111497 (同梱 Brotli 1.1.0) への parquet
経路は、DuckDB が修正を出すまでローカルファイルについて到達可能なまま残る
(v0.18.0 の CHANGELOG を参照)。

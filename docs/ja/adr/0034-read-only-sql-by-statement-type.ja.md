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
決めていた。代用品はどれも、自分が表していない書き込みを通した。続く 2 回目の
レビューでは `READ ONLY` トランザクションも破られた:
`json_execute_serialized_sql(json_serialize_sql('<DELETE …>'))` は呼び出し元の
トランザクションの外にある内部接続で SQL を実行し、行を消した。save_query では、
静的リンクされた `dbgen` / `dsdgen` (TPC-H/DS) が、保存するテーブルの横に 8 個と
24 個のテーブルを作った。

## 2. 決定

エンジンで強制する。3 つの層が、互いに閉じられないものを閉じる:

1. **文の種別** (`requireSelect`)。クエリをドライバの `Prepare` — 複数文の
   クエリはどの文も実行せずに拒否する — で準備し、種別が SELECT の 1 文で
   あることを求める。`INSERT` / `UPDATE` / `DELETE`、`CREATE`、`ALTER`、
   `DROP`、`COPY`、`ATTACH`、`SET`、`INSTALL`、`LOAD`、`CALL`、`EXPLAIN`、
   動的 `PIVOT` は何も実行される前に、エージェントが直せるメッセージで
   拒否される。`SELECT`、`FROM t`、`WITH … SELECT`、`VALUES`、`DESCRIBE`、
   `SUMMARIZE`、`SHOW`、`PIVOT … IN (…)` は種別が SELECT である。
2. **読み取り専用のデータベース** (`openReaderLocked`)。エージェントの SQL は
   読み書き用インスタンスでは決して実行しない。読み取りはそれを閉じ、セッション
   ファイルをいつもの名前で `READ_ONLY` として ATTACH して既定にした、使い捨ての
   インメモリインスタンスでクエリを実行する。そこではデータベース自体が読み取り
   専用なので、DuckDB はどの接続からの書き込みも拒否する — データ変更 CTE も、
   `dbgen` も、`json_execute_serialized_sql` が実行する SQL も同じである。それでも
   書けるものは使い捨てインスタンス自身のインメモリカタログに落ち、捨てられる。
   次の書き込みが読み書き用インスタンスを開き直す。ファイルを開いている
   インスタンスは、エンジンのミューテックスの下で常に 1 つだけである。
3. **設定のロック** (`lockConfiguration`、全インスタンス)。拡張機能の自動
   インストールと自動ロードを切り、`lock_configuration` を設定する。すると
   DuckDB はどのクエリからの `enable_profiling` / `enable_logging` も拒否し、
   URL を読んでも拡張機能はインストールされない。エンジンはその後に設定を
   変えず、load-data が読む形式 (CSV、JSON) は組み込みである。

save_query は書き込む必要があるので、本体を同じ方法で実行する: 新しいステージング
ファイルを書き込み可能で ATTACH したリーダーの上で
`CREATE TABLE stage.result AS <本体>` とする。その後、読み書き用インスタンスが
ステージングファイルを `READ_ONLY` で ATTACH してその 1 テーブルだけを写し
(`CREATE TABLE <name> AS SELECT * FROM stage.result`)、ステージングファイルは
削除する。列の型はそのまま写る。本体がそれ以外に書いたものは、使い捨ての
インスタンスか削除したステージングファイルに残るだけである。

構文エラーや名前解決のエラーは DuckDB 自身のもので、従来どおり `query:` /
`save query:` を付けて返す。層 1 の拒否だけが専用のメッセージを使う。

## 3. 結果

- エージェントの SQL が書き込めるかは、クエリの綴りや呼ぶ関数ではなく、
  DuckDB のアクセスモードと設定のロックが決める。
- 読み取りは読み書き用インスタンスを閉じ (チェックポイント)、次の書き込みが
  開き直す。クエリが続く間は切り替え 1 回、読み書きが交互ならそのたびに 1 回。
- 旧判定が通していた `EXPLAIN` と動的 `PIVOT` は拒否される。
  `PIVOT … ON … IN (…)` と通常の SELECT で同じことができる。
- テストは、拒否する各形 — 複数文の先頭に置いた書き込み、データ変更 CTE、
  `dbgen` / `dsdgen`、`json_execute_serialized_sql`、ファイルを書くテーブル関数 —
  を 3 つの読み取り経路と save_query のすべてに通し、その後のテーブル・アタッチ
  されたデータベース・ファイルを確かめる。セッションファイルを書き込み可能で
  ATTACH した対照実行や、ロックを外した対照実行ではテストが失敗する。2 つ目の
  テストは、読み取りの後の書き込みが読み書き用インスタンスを開き直すこと
  (Reset は閉じたハンドルで早期 return していた) と、save_query が列の型を保ち
  ステージングファイルを残さないことを固定する。

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

# ADR-0033: DuckDB ドライバを duckdb-go (DuckDB 2.0) へ移行する

- Status: Accepted
- Deciders: magi
- Related: ADR-0010 (DuckDB 結果のレンダリング — §6.5 で本移行を保留)、ADR-0011 (TIMESTAMPTZ のレンダリング)

## 1. コンテキスト

分析エンジンは `github.com/marcboeker/go-duckdb` v1.8.5 (DuckDB v1.1.3
同梱) をリンクしていた。このモジュールパスにはもうリリースが出ない。
ドライバは `github.com/duckdb/duckdb-go/v2` へ移り、`marcboeker` の
v1 / v2 系列は凍結された。旧パスのままでは DuckDB の修正が
shell-agent-v2 に届くことはない。

きっかけは CVE-2026-111497 (Brotli デコーダの境界外書き込み)。DuckDB の
parquet 拡張は Brotli 1.1.0 を同梱し、分析エンジンは LLM が書いた SQL を
プロセス内で実行するため、細工したファイルへの `read_parquet` で到達しうる。
2026-10-25 時点で DuckDB の全リリース (v1.4.5 LTS、v1.5.6、v2.0.0 LTS) が
まだ 1.1.0 を同梱し、上流の修正 (duckdb/duckdb#27791) は未マージ —
修正済みのドライバはまだ無い。

保守されているパスの候補 (同梱 DuckDB 別):

| duckdb-go | DuckDB | サポート |
|---|---|---|
| v2.5.6 | v1.4.5 LTS | 2026-11-17 まで |
| v2.10506.0 | v1.5.6 | 2027-03 まで |
| v2.20000.0 | v2.0.0 LTS | 少なくとも 2027-10 まで |

## 2. 決定

`github.com/duckdb/duckdb-go/v2` v2.20000.0 (DuckDB v2.0.0 LTS) へ移行する。
サポートが最も長く、上流の Brotli 修正の向き先 (DuckDB `main`) でもあるので、
CVE の修正はパッチ版の更新で入る。

これに伴う変更:

- **MAP** は `duckdb.Map` ではなく `duckdb.OrderedMap` に Scan されるように
  なった。`renderScalar` が `mapToStringKeyed` で変換する (最上位と、LIST /
  STRUCT の中の両方) ので、UI・LLM へのツール結果・CSV 出力の値は従来どおり。
  型スイープテストのアサーションがこれを検出した。ログ出力では他の Go の値に
  変化は無く、ドライバ側で見えた唯一の違い (JSON 列の `DatabaseTypeName` が
  `VARCHAR` から `JSON` になった) はどのディスパッチ項目にも当たらない。
- **Arrow は opt-out から opt-in (`-tags duckdb_arrow`) になった**ので、
  Makefile から `no_duckdb_arrow` タグを外した。Arrow はバイナリに入らない
  ままである。
- **データベースファイル。** 本リリースより前に作られたセッション DB は、
  DuckDB 2.0 が書き込んだ後も旧版で開ける (実測: v1.1.3 と v1.4.1 でそれぞれ
  作り v2.0.0 で書いたファイルを、作成した版で再度開けた)。DuckDB 2.0 が
  *新規作成*した DB は DuckDB 1.x では開けない (両版で実測) ため、本リリースで作ったセッション — およびそれを
  含む `.shellagent` バンドル — は v0.17.x では開けない。

## 3. 結果

- DuckDB が Brotli 修正を出すまで CVE は未解決のまま。CHANGELOG にそう書く。
  取り込みは次の v2.2000x リリースへの `go get` で済む。
- DuckDB 2.0 は既定で新しい PEG パーサを使う。テストは無変更で通る。旧パーサが
  受け付け新パーサが拒む LLM 生成 SQL は、エージェントが再試行できるクエリ
  エラーとして現れる。
- 新しいセッションを作った後のダウングレードはサポートしない (§2)。

## 4. 却下した案

- **go-duckdb v1.8.5 に留まる。** DuckDB v1.1.3 で凍結、修正が入る道が無い。
- **duckdb-go v2.5.6 (DuckDB 1.4 LTS)。** サポートが 3 週間後に終わる。
- **duckdb-go v2.10506.0 (DuckDB 1.5)。** 2027-03 までサポートされるが、
  Brotli 修正がバックポートされるかは不明で、数か月以内に 2.0 への再移行が
  どのみち必要になる。

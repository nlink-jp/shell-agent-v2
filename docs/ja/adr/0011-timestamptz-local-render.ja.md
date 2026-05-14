# TIMESTAMPTZ レンダリング — ローカル TZ 表示 — 設計ノート

**Status:** Design draft (2026-05-14); 承認待ち。
**Target:** v0.6.5 (v0.6.4 上の point release)
**Supersedes:** ADR-0010 §2 非ゴール (TIMESTAMPTZ 後送り部分のみ
supersede。LIST / STRUCT / JSON-as-VARCHAR の表示品質は依然 deferred)
**報告者:** ユーザー — DuckDB テーブルの TIMESTAMPTZ 値が全結果
経路で UTC (`Z` サフィックス) として流れ、ユーザーが期待する
wall-clock 表現を喪失する

このノートは `internal/analysis` の `renderScalar` ヘルパーで
`TIMESTAMPTZ` 列を扱う方針を定める: ランタイムの `time.Local`
ロケーションに変換し、明示的オフセット付き RFC 3339 形式で出力。

---

## 1. Problem

DuckDB の TIMESTAMPTZ 型は格納時に全値を UTC 正規化する。
`database/sql` 経由で Go コードに `time.Time` として返る時点で
元のタイムゾーン情報は失われ、復元可能なのは絶対 UTC 時刻のみ。

ADR-0010 ではこれを「保持には ingest 時の sidecar が必要」と
判断して後送りした。リリース後にユーザーが「実際の痛みは
表示」と明確化した: 東京で分析作業をしているユーザーが、原データが
`2026-05-14 12:34:56+09:00` だった値を `2026-05-14T03:34:56Z` で
見せられても、絶対時刻は正しいが wall-clock 認識は 100% 失われる。

複数 TZ にまたがるデータセットから *元の* TZ をレンダリング層で
復元することは不可能 (schema 変更か sidecar 列が必要)。だが
*useful* な wall-clock 表現を復元することは平易: UTC `time.Time` を
ランタイムのローカル TZ に変換し、明示オフセット付きで format する。
データが自分の TZ で作られている場合 — shell-agent-v2 のローカル
ファースト方針における圧倒的多数ケース — これは元の wall-clock と
完全一致する。

---

## 2. Goals

1. **TIMESTAMPTZ が `time.Local` で表示**: 3 経路 (Preview /
   QuerySQL / QuerySQLToCSV) すべてで明示的数値オフセット
   (`Z` でなく `+09:00`) 付きで出力
2. **UTC 絶対時刻は変化しない**: 表示形式変換のみで絶対時刻
   保持
3. **`TIMESTAMP` (TZ なし) は変更なし**: DuckDB は wall-clock
   (TIMESTAMP) と instant (TIMESTAMPTZ) を意図的に区別、これを尊重
4. **テストは TZ 非依存**: CI ホストが任意 TZ でも assert が
   通ること (host の `time.Local` に関係なく)

Non-goals:

- **設定可能な display TZ** (`analysis.display_timezone` 等):
  簡単な follow-up だが今回スコープ外。`time.Local` がデフォルト
  として正しく、Go 標準ライブラリの慣習にも沿う
- **マルチ TZ データセットの source TZ 保持**: schema 変更なしには
  本質的に不可能、対象外

---

## 3. Design

`internal/analysis/render.go` の `renderScalar` に dispatch エントリ
1 つ追加:

```go
case dbTypeName == "TIMESTAMPTZ":
    if t, ok := v.(time.Time); ok {
        return t.In(time.Local).Format(time.RFC3339Nano)
    }
```

挙動:

- `time.RFC3339Nano` はマイクロ秒精度を保持しつつ末尾 0 を切る
  (秒精度の値は `2026-05-14T12:34:56+09:00` で `...+09:00.000000`
  にはならない)
- `time.In(time.Local)` 変換は表示変更のみ、絶対時刻は保持
- フォールスルー (`if t, ok` ガード失敗) は値をそのまま返すので、
  将来の driver 版が string 戻りに変わっても安全

`TIMESTAMP` (TZ なし) と `DATE` は引き続きデフォルトの `time.Time`
経路を通り、標準ライブラリの `MarshalJSON` (RFC3339Nano UTC) で
marshal される。それぞれ「wall clock」と「カレンダー日」のセマン
ティクスがあり、ローカル TZ 変換の恩恵がない。

---

## 4. Testing

### 4.1 type sweep に TIMESTAMPTZ assertion

`engine_typesweep_test.go` の `wants` テーブルに
`timestamptz_col` エントリを追加。CI ホストごとの差を排除するため:

- テスト開始時に `time.Local` を固定 TZ (`Asia/Tokyo`, +09:00) に
  オーバーライド。test 終了時に save/restore
- 期待値: `2026-05-14T12:34:56+09:00`。元のフィクスチャは
  `TIMESTAMPTZ '2026-05-14 12:34:56+09'` だったので、UTC 正規化
  された格納値 (`03:34:56Z`) を +09:00 で表示すれば元の wall-clock
  に戻る

### 4.2 TIMESTAMP は変更なしを固定

`timestamp_col` エントリを既存の `2026-05-14T12:34:56Z` 形で
追加。誤って TIMESTAMP までローカル TZ ロジックを波及させない
ためのガード。

### 4.3 テスト用 TZ オーバーライドパターン

```go
prevLocal := time.Local
time.Local, _ = time.LoadLocation("Asia/Tokyo")
defer func() { time.Local = prevLocal }()
```

`time.Local` はパッケージレベル変数。`go test` のデフォルト
シーケンシャル実行下では mutate 安全 (analysis パッケージは
parallel test を使っていない)。

---

## 5. Risks

- **CI TZ オーバーライドの race**: 将来の analysis パッケージ
  内テストが parallel 化されると、上書きした `time.Local` が
  漏れる可能性。緩和: analysis テストはシーケンシャル既定、
  `t.Parallel()` を追加しない。defer で復元
- **`time.Local` 変換でマイクロ秒精度喪失**: なし — `time.In`
  はナノ秒フィールドを保持
- **`time.LoadLocation` 失敗**: システム tzdata 必要。macOS と
  Linux は標準同梱、Windows は Go 内蔵コピー。失敗時は `t.Fatalf`
  で clean に止まる。プロダクションコード経路は `time.Local` を
  直接使う (Load なし) のでテストのみの懸念

---

## 6. Rejected alternatives

### 6.1 DuckDB セッション `SET TimeZone` を使う

DuckDB のセッション TZ を設定して TZ-aware 文字列出力に頼る。
**却下**: `database/sql` 経由 go-duckdb は session 設定に関わらず
`time.Time` を返すため、session TZ はこちらの経路に伝播しない

### 6.2 サーバー側で VARCHAR cast

`SELECT col::VARCHAR` で DuckDB に format させる。**却下**: 全列
を text に潰す (React 側が使う Go `time.Time` 型情報を失う)、
任意のユーザー `SELECT *` クエリには retrofit 不可

### 6.3 設定可能な display TZ を今回追加

`analysis.display_timezone` config knob。**v0.6.5 では却下**
(v0.7.x の follow-up としては still on the table): UI / config
スキーマ / マイグレーション面を増やすが、デフォルトで圧倒的多数
ケースを既にカバー

### 6.4 source TZ を sidecar 列で保持

JSON source からオフセット文字列を抽出して `<col>_tz` 列に格納。
**却下**: LoadJSON / LoadJSONL で Go 側で JSON を pre-parse する
必要があり (`read_json_auto` では不可能)、テーブルスキーマ変更で
ユーザーが学ぶことになる、また本製品方針では稀なマルチ TZ データ
セットケースのみを対象とする

---

## 7. Compatibility & rollout

- **永続化フォーマット**: 変更なし
- **LLM observable**: TIMESTAMPTZ 列の tool 結果 JSON が
  `2026-05-14T03:34:56Z` から `2026-05-14T12:34:56+09:00` (host の
  ローカル TZ 次第) に変化。絶対時刻は同一。既存 pinned-context
  セッションでは LLM 挙動が wall-clock 文字列の違いで微変化
  しうる
- **CSV observable**: 同変化。RFC 3339 を使う下流パーサは `Z` も
  数値オフセットも natively 扱える。「`2026-05-14` 接頭辞 match」
  のような素朴 string match は source TZ と display TZ が一致する
  限り維持
- **UI observable**: Data パネル サマリがローカル TZ wall-clock
  になる。ユーザーが自分の TZ のデータを扱う場合は strict
  improvement
- **Rollout**: v0.6.5 として ship。CHANGELOG に before / after の
  実例を明示

---

## 8. References

- ADR-0010 §2 (本 ADR が supersede)
- ユーザー報告: 2026-05-14、「TZ 保持については優先的に対応する
  必要があると考えられる」
- DuckDB TIMESTAMPTZ セマンティクス:
  https://duckdb.org/docs/sql/data_types/timestamp
- Go `time.Local`:
  https://pkg.go.dev/time#Local
- 影響箇所 (ADR-0010 から増えていない):
  - `app/internal/analysis/render.go` (renderScalar dispatch)
  - `app/internal/analysis/engine_typesweep_test.go` (sweep
    assertion + TZ override)

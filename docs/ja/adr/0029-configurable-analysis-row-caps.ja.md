# ADR-0029: データ分析の行数上限を設定可能にする（query + export）

- Status: Accepted
- Deciders: magi
- Related: ADR-0005 (analyze-data の行数上限 — §5 で本 knob を予告), ADR-0004 (sandbox uid mapping), `docs/ja/reference/...` (分析エンジン)

## 1. コンテキスト

GitHub issue #14（ユーザー報告）:

> データ分析において SQL で抽出してサンドボックスに引き渡したりする際に
> 設定されている行数制限が 10000 行となっていて、この制限によく抵触して
> データ分析が実行できない場合があります。上限値がハードコードされている
> ため、設定で変更したりすることができず、エージェントに対して、何らか
> データ分割せよというような余計な指示をする必要が発生したり、エージェント
> が自動で制限を回避するために、複雑な対応を提案したりすることがあり、
> データ分析の体験があまり良くありません。

該当する経路は `export-sql-to-csv`（`sandbox_tools.go` →
`Engine.QuerySQLToCSV`）。SELECT を実行し、その行をセッション毎のサンド
ボックス `/work` ディレクトリに CSV として書き出す。これがインタラク
ティブなチャット出力ツールと同じ `MaxQueryRows = 10000` 定数で制限されて
いる:

```go
// app/internal/analysis/engine.go
const MaxQueryRows = 10000          // チャット出力用の上限
...
// QuerySQLToCSV
for rows.Next() {
    if rowCount >= MaxQueryRows {   // ← export 経路がチャット用上限を共有
        return columns, rowCount, fmt.Errorf("query result exceeds %d rows; ...", MaxQueryRows)
    }
```

## 2. 根本原因

行取得の経路は概念的に**3つ**あるが、上限は2つしかない:

| 経路 | メソッド | 行がチャットに入る? | 現在の上限 |
|------|--------|----------------------|-----------|
| インタラクティブ query（`query-sql`, `query-preview`, `quick-summary`） | `QuerySQL` | **入る** — tool result に JSON 化 | `MaxQueryRows` = 10,000 |
| スライディングウィンドウ分析（`analyze-data`） | `QuerySQLForAnalyze` | 入らない — window 毎の LLM 呼び出しに分割 | `MaxAnalyzeRows` = 1,000,000 |
| サンドボックスへの CSV エクスポート（`export-sql-to-csv`） | `QuerySQLToCSV` | **入らない** — `/work` 内のファイルに書く | `MaxQueryRows` = 10,000 ❌ |

ADR-0005 が既に原則を確立している: *10,000 行の上限は、無制限な SELECT が
LLM コンテキストを溢れさせるのを防ぐために存在する。行がチャットに入る経路に
のみ妥当である。* CSV エクスポート経路はこの原則に反している — 行がチャットに
入らないのに、チャット用上限で絞られている。これは ADR-0005 が `analyze-data`
について修正したのと同種のバグであり、export ツールが ADR-0005 より後に
追加されたため export 経路では未修正のまま残った。

つまり2つの別問題がある:

1. **上限の誤共有（潜在バグ）。** `export-sql-to-csv` は `analyze-data` と
   同様の backstop を使うべきで、チャット用上限を使うべきではない。
2. **設定不可。** チャット用上限ですらパワーユーザーには時に低すぎる。
   ADR-0005 §5 は field report が来るまで config knob を明示的に保留して
   いた。issue #14 がその report である。

## 3. 決定

両方の上限を設定可能にし、export 経路は高い backstop をデフォルトにする
（config を触らなくてもユーザーの痛点が消えるように）。

### 3.1. 設定サーフェス

確立済みの `AgentConfig.MaxToolRounds` パターン（0 → resolved default）に
従い、`config.Config` に `analysis` ブロックを追加:

```go
// app/internal/config/config.go
type AnalysisConfig struct {
    // MaxQueryRows はインタラクティブなチャット出力 query
    // (query-sql / query-preview / quick-summary) を制限。0 → DefaultMaxQueryRows。
    MaxQueryRows int `json:"max_query_rows,omitempty"`
    // MaxExportRows は export-sql-to-csv（サンドボックス /work に書かれ、
    // チャットには入らない行）を制限。0 → DefaultMaxExportRows。
    MaxExportRows int `json:"max_export_rows,omitempty"`
}

const DefaultMaxQueryRows  = 10_000     // チャット出力: 不変
const DefaultMaxExportRows = 1_000_000  // サンドボックス受け渡し: MaxAnalyzeRows と同値

func (a AnalysisConfig) MaxQueryRowsResolved() int  { /* 0 → default */ }
func (a AnalysisConfig) MaxExportRowsResolved() int { /* 0 → default */ }
```

`Config` に `Analysis AnalysisConfig json:"analysis,omitzero"` を追加。
ブロックが無い旧 config はデフォルトに解決される — migration 不要。

### 3.2. エンジン: パッケージ定数ではなくインスタンス毎の上限

`MaxQueryRows` はパッケージデフォルト `DefaultMaxQueryRows` になり、各
セッションのエンジンを個別に設定できるよう `Engine` がインスタンス上限を
持つ:

```go
type Engine struct {
    ...
    maxQueryRows  int // 0 → DefaultMaxQueryRows
    maxExportRows int // 0 → DefaultMaxExportRows
}

// SetRowCaps は query / export 上限を設定。0 はデフォルト維持。
func (e *Engine) SetRowCaps(maxQuery, maxExport int)

func (e *Engine) MaxQueryRows() int  // resolved
func (e *Engine) MaxExportRows() int // resolved
```

- `QuerySQL` → `e.MaxQueryRows()` で制限（旧: 定数）。
- `QuerySQLToCSV` → `e.MaxExportRows()` で制限（**変更**: 旧 `MaxQueryRows`）。
  これが本修正の核心。
- `QuerySQLForAnalyze` → 不変。`MaxAnalyzeRows` パッケージ変数を維持
  （テスト seam `setMaxAnalyzeRowsForTesting` も維持）。

`MaxAnalyzeRows` は本 ADR では意図的に config に**畳み込まない** — ユーザー
が意味を判断しづらい純粋なメモリ backstop であり、ADR-0005 §7 が証拠が
出るまでスコープ外としている。issue #14 は export と query 経路の話で
あって analyze ではない。

### 3.3. 配線

- `bindings.go::switchAnalysis` がセッション毎エンジン構築後に
  `b.analysis.SetRowCaps(cfg.Analysis.MaxQueryRowsResolved(),
  cfg.Analysis.MaxExportRowsResolved())` を呼ぶ。新しいセッションの
  エンジンは config を尊重する。
- `SaveSettings` は**現在ライブな**エンジンにも新上限を適用し、セッション
  切替やアプリ再起動なしで反映される（logger level や sandbox config が
  既にライブ反映するのと一貫）。

### 3.4. Settings UI

General タブに *Data analysis* セクションを追加（*Agent loop* の隣）。
数値入力2つ:

- **Max rows per query result** → `max_query_rows`、デフォルト 10,000。
  ヒント: チャットに返る行数。上げると LLM により多くのデータを送り、
  コンテキストを多く使う。
- **Max rows for CSV export to sandbox** → `max_export_rows`、
  デフォルト 1,000,000。ヒント: サンドボックス内のファイルに書かれる行。
  チャットには入らないので、上限はコンテキストではなくメモリ。

`SettingsData`（bindings DTO）に `MaxQueryRows` と `MaxExportRows` を追加し、
`frontend/src/types.ts` の `Settings` にミラー。

### 3.5. export 上限を 1,000,000 にする理由

export 経路は `analyze-data` とバイト単位で同性質である: 行はチャットでは
なくファイルに行くので、唯一効く上限はメモリ。ADR-0005 §3.3 が既にこの
regime を分析し、`analyze-data` の backstop として 1,000,000 を選んだ。
同じ値を再利用することで2つのファイル指向経路が一貫し、ユーザーの
27,000 行ワークロード（issue #14 が示唆する規模）が config なしで通る。
`analyze-data` と違い、`QuerySQLToCSV` は行を `[]map[string]any` に
materialise せず1行ずつ `encoding/csv` にストリームするので、行あたりの
メモリコストは analyze 経路よりはるかに低い — 1,000,000 行でも余裕。

## 4. 検討した代替案

- **export デフォルトを 1,000,000 に上げるだけ、config なし。** 報告された
  痛点は直るが、明示的な「設定で変更したい」要望を無視する。却下。
- **全経路共通の単一 `max_rows` knob。** チャットコンテキストの懸念とメモリ
  の懸念を混同する。export 上限を上げたユーザーが次の `query-sql` で
  LLM コンテキストを意図せず溢れさせる。却下 — 2つの上限は別資源を守る。
- **ツールへの per-call `max` パラメータ（LLM 供給）。** 上限をモデルの問題に
  してしまう。これはまさに issue が訴える「余計な指示 / 複雑な対応」その
  もの。却下。
- **config ファイルのみ、UI なし。** `MaxOutputBytes` /
  `MaxToolCallArgsBytes` と同様。本件では却下: ユーザーはこれにインタラ
  クティブに、分析の最中に抵触する。GUI knob が適切な affordance（報告者と
  合意）。

## 5. 互換性

- **オンディスク config**: 追加のみ。`analysis` ブロックが無ければ
  デフォルト。migration なし。
- **公開 Go API**: `MaxQueryRows` 定数を `DefaultMaxQueryRows` にリネーム。
  `QuerySQLToCSV` の上限が 10,000 から resolved export 上限（デフォルト
  1,000,000）に変わる。影響を受けるのは repo 内の呼び出し元（agent,
  bindings, tests）のみで、同一変更内で更新。
- **ユーザーに見える挙動**: `export-sql-to-csv` が 10,001〜1,000,000 行の
  結果セットでデフォルトで成功する。`query-sql` / `query-preview` /
  `quick-summary` はデフォルトでは不変。両上限とも Settings → General →
  Data analysis で調整可能。
- **後方互換オプション**（組織規約準拠）: 旧来の 10,000 export 挙動は
  config または Settings で `max_export_rows: 10000` を設定すれば回復可能 —
  挙動の削除はなく、デフォルトを上げただけ。

## 6. テスト

- `config`: `TestAnalysisConfig_Resolved` — 0 → デフォルト; 明示値はその
  まま通る; JSON marshal/unmarshal を往復。
- `analysis` エンジン:
  - `TestQuerySQLToCSV_AllowsBeyondQueryCap` — >10,000 行をロードし、
    `QuerySQLToCSV` が全部書くことを assert（誤共有上限の回帰ピン）。
  - `TestQuerySQLToCSV_RespectsExportCap` — `SetRowCaps` で小さい export
    上限を入れ、query 上限ではなく export 上限で row-limit エラーが出ることを
    assert。
  - `TestQuerySQL_StillCapsAtQueryDefault` — チャット上限が
    `DefaultMaxQueryRows` で不変であることを保証。
  - `TestSetRowCaps_ZeroKeepsDefault` — 引数 0 はパッケージデフォルトに
    解決される。
- リネームした `MaxQueryRows` 定数の既存参照を更新
  （`security_test.go`, `engine_analyze_test.go`）。

export 上限テストは百万行を materialise せず `SetRowCaps` で小さい上限を
注入する（`setMaxAnalyzeRowsForTesting` と同じ精神）。

## 7. スコープ外

- **`MaxAnalyzeRows` の設定化** — 依然純粋なメモリ backstop。ADR-0005 §7 の
  立場は不変。
- **テーブル毎 / クエリ毎の上書き** — 報告された必要にはグローバル config で
  十分。
- **analyze 経路の DuckDB ストリーミング** — 直交。ADR-0005 §7 参照。

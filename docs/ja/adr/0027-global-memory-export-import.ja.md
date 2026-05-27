# ADR-0027: グローバルメモリの Export / Import

- Status: Accepted
- Deciders: magi
- Related: ADR-0001 (セッション import/export — ファイルダイアログ + エンベロープの先例), ADR-0019 (LLM 駆動メモリツール), ADR-0028 (未使用 provenance フィールドを削除し、本機能がサニタイズすべきクロスマシンハザードを除去), `docs/ja/reference/memory-model.ja.md` (4-facility モデル)

## 1. コンテキスト

グローバルメモリは唯一の**クロスセッション**メモリ facility である。
ユーザのアイデンティティ的な preference と恒久的な decision を
`{DataDir}/global_memory.json` に `GlobalMemoryEntry` の JSON 配列として
永続化する (`app/internal/memory/global_memory.go`)。セッションをまたいで
存続し、全システムプロンプトに注入される。

現状、グローバルメモリをマシン外へ持ち出す手段がない。セッションは
`.shellagent` バンドルとして export/import できる (ADR-0001) が、その経路は
グローバルメモリを**意図的に除外**している (ADR-0001 §5.3)。ユーザから
**グローバルメモリをバックアップし別マシンへ持ち運びたい**との要望があった。

既存ストアの制約:

- エントリは `Fact` テキストをキーに dedup（完全一致）。ID も embedding も
  無い（import 時に再生成すべきものが無い）。
- `SourceTime` / `CreatedAt` はユーザ可視（"learned YYYY-MM-DD"）。
- ADR-0028 後、エントリのフィールドは `Fact`・`NativeFact`・`Category`・
  `SourceTime`・`CreatedAt`・`Source`・`ToolOriginated` のみ — すべて移植可能で、
  サニタイズすべきマシンローカルなセッション back-reference は残っていない。
- `GlobalMemoryStore.Add` は既に `Fact` で dedup し、ゼロ値タイムスタンプを
  `time.Now()` で打ち、`MaxEntries` 超過で FIFO evict する
  (`global_memory.go:126`)。

## 2. 決定

セッションバンドルとは別に、専用の**グローバルメモリ export/import** を
追加し、サイドバーの Memory タブに2つのボタンとして配置する。

### 2.1 ファイル形式 — バージョン付きエンベロープ

エントリを自己記述エンベロープで包んだ単一 JSON ファイル（デフォルト拡張子
`.json`）:

```json
{
  "kind": "shell-agent-v2-global-memory",
  "schema_version": 1,
  "exported_at": "2026-05-27T10:00:00Z",
  "exported_by_app_version": "0.15.0",
  "entries": [ { /* GlobalMemoryEntry をそのまま */ } ]
}
```

| フィールド | 用途 |
|-------|---------|
| `kind` | 判別子。`shell-agent-v2-global-memory` 以外を import 拒否し、セッションバンドルや任意 JSON の誤読込を防ぐ。 |
| `schema_version` | `1`。それ以外は拒否（"unsupported global-memory export schema version: N"）。v1 に migration ロジックは持たない。 |
| `exported_at` | RFC3339 UTC。情報用。 |
| `exported_by_app_version` | 生成元アプリバージョン。情報用。 |
| `entries` | `GlobalMemoryEntry` スライスを**そのまま**。provenance 含む全フィールドを保持し、export は忠実なスナップショットとなる。 |

生 `[]GlobalMemoryEntry` 配列（却下案）よりエンベロープを選ぶ理由:
`kind`/`schema_version` の組により誤ファイルに対して fail fast かつ可読に
失敗でき、将来の形式変更の余地も残る。コストはラッパオブジェクト1つで許容範囲。

### 2.2 Export

`GlobalMemoryStore.All()` をエンベロープにシリアライズし、Wails の
`SaveFileDialog`（ADR-0001 のパターン）でユーザ選択パスへ書き出す。
デフォルトファイル名 `global-memory-<YYYYMMDD-HHMMSS>.json`。状態機械ゲートは
不要 — グローバルメモリはセッションスコープでなく、analysis エンジンに開かれて
保持されることもなく、読み取りは安価なインメモリスナップショット。

### 2.3 Import — マージ・重複スキップ

エンベロープを parse + 検証し、各エントリを既存の `Add` セマンティクスで
ストアに畳み込む: **新規 fact は追加、`Fact` テキストが既存なら skip**。
これは最も安全な方針（overwrite と全置換より優先）で、import が既存 fact を
破壊・改変しないため、同じファイルの再 import や複数ファイルの連続 import が
冪等かつ非破壊になる。

import 時のエントリ別処理:

- **Dedup**: 現ストアに対する `Fact` 完全一致 → skip（`skipped` にカウント）。
- **タイムスタンプ**: ファイルの値を保持。`Add` はゼロ値のみ打刻するため、
  忠実な `SourceTime`/`CreatedAt` は残る。タイムスタンプ無しで export された
  エントリは import 時刻で打刻される。
- **Category**: `ValidGlobalMemoryCategories` に無ければ `decision` へ矯正
  （`Set` と同様）。ストア不変条件を維持。
- **空 `Fact`**: skip（`skipped` にカウント）。空 fact は使えるメモリでない。
- **残る全フィールドは移植可能**（ADR-0028 がマシンローカルなセッション
  back-reference を削除済み）なので、import は各エントリをそのまま畳み込むだけで
  サニタイズ対象は無い。`Source`（ひいては `[user-stated]`/`[derived]` の trust
  タグ）と `ToolOriginated` はそのまま保持する。

import は `{ added, skipped }` のサマリを返し、UI が「N 件追加（M 件重複
スキップ）」と報告できる。import 後はストアを atomic に `Save()` し、
フロントは `GetGlobalMemories()` で再取得する（直接編集 UI が既に使う
リフレッシュ経路と同じ。新規イベント不要）。

### 2.4 UI

サイドバーのグローバルメモリセクションヘッダ
(`frontend/src/sidebar/Sidebar.tsx`)、既存の一括操作 UI の隣に2ボタン:

- **Export** → `Bindings.ExportGlobalMemory()` → 保存ダイアログ。
- **Import** → `Bindings.ImportGlobalMemory()` → 開くダイアログ → binding が
  結果（「Imported N facts, M skipped」）または拒否理由をネイティブの
  `wailsRuntime.MessageDialog` で報告 → フロントが一覧をリフレッシュ。
  （実装注: JS `window.alert()` は Wails v2 WKWebView で確実に表示されない
  ため——アプリは同じ理由で他所もネイティブダイアログ/インラインUIを使う——
  フィードバックは Go binding 側からネイティブ表示する。）

## 3. 結果

- グローバルメモリがバックアップ・クロスマシン移行で持ち運び可能になり、
  ADR-0001 が残したギャップを埋める。
- import は非破壊・冪等。import で既存 fact を失う経路はない。
- エントリがセッション back-reference を一切持たない（ADR-0028）ため、ここでは
  クロスマシン衝突の問題が発生しない — 誤処理し得るマシンローカルなデータが無い。
- 新規ランタイムイベント無し。フロントのリフレッシュは `GetGlobalMemories()`
  を再利用。
- import 中も `MaxEntries` FIFO eviction が適用される — cap 超過 import は
  通常の `Add` 同様に古いものから evict。

## 4. 実装

- `app/internal/memory/global_memory.go`
  - `GlobalMemoryExport` 構造体（エンベロープ）+ `GlobalMemoryExportKind`,
    `GlobalMemoryExportSchemaVersion` 定数。
  - `MarshalGlobalMemoryExport(entries, appVersion string) ([]byte, error)`
    — エンベロープ構築、`exported_at = time.Now().UTC()`。
  - `ParseGlobalMemoryImport(data []byte) ([]GlobalMemoryEntry, error)` —
    `kind` と `schema_version` を検証し entries を返す。
  - `(s *GlobalMemoryStore) Import(entries []GlobalMemoryEntry) (added, skipped int)`
    — category 矯正 + 空 fact skip + `Add`（dedup/打刻/FIFO）。
- `app/internal/agent/agent.go`
  - `GlobalMemoryExportJSON(appVersion string) ([]byte, error)` →
    `MarshalGlobalMemoryExport(a.globalMemory.All(), …)`。
  - `GlobalMemoryImportJSON(data []byte) (added, skipped int, err error)`
    → parse → `a.globalMemory.Import(...)` → `a.globalMemory.Save()`。
- `app/bindings.go`
  - `ExportGlobalMemory() (string, error)` — `SaveFileDialog`
    (`*.json`, デフォルト名)、`0600` で書込（ストア自身の perms と同じ。
    個人的 fact を含み得るため）。
  - `ImportGlobalMemory() (GlobalMemoryImportResult, error)` —
    `OpenFileDialog` (`*.json`), `os.ReadFile`, agent import; `{Added, Skipped int}`
    を返す。ダイアログキャンセル → ゼロ値結果・nil error。
- `app/frontend/src/bindings.ts`, `types.ts` — 2メソッド宣言 +
  `GlobalMemoryImportResult` 型を追加。
- `app/frontend/src/sidebar/Sidebar.tsx` — Global Memory セクションヘッダに
  Export/Import ボタン（常時表示の `.memory-io-actions`。hover のみの
  `.session-actions` ではない）。`App.tsx` がハンドラ配線と import 後の一覧
  リフレッシュを行い、結果/エラーのダイアログは Go binding がネイティブ表示
  （§2.4 参照）。

### テスト（必須）

- `global_memory_test.go`:
  - Marshal → Parse ラウンドトリップでタイムスタンプ・provenance 含む全
    フィールドが保持される。
  - `Import` dedup: 既存ストアと重なるエントリを import すると新規のみ追加。
    `added`/`skipped` カウントが正しい。
  - `Import` が非ゼロタイムスタンプを保持し、ゼロ値を打刻する。
  - `Import` が不正 category を `decision` に矯正、空 `Fact` を skip。
  - `ParseGlobalMemoryImport` が誤 `kind`・誤 `schema_version`・非 JSON を
    別個のエラーで拒否。
  - 大量 import 後も `MaxEntries` で FIFO eviction が効く。
- `agent` テスト: `GlobalMemoryImportJSON` が永続化（再 `Load` でマージ結果が
  見える）し、正しいカウントを返す。

## 5. スコープ外

- overwrite / 全置換 import モード（v1 はマージ・スキップのみ）。
- export ファイルの暗号化。
- 選択 export（fact のサブセット）。
- セッションバンドルとの統合（グローバルメモリは別ファイルのまま）。
- スラッシュコマンド版（`/export-memory`）。v1 はボタンのみ。

## 6. 手動スモークチェックリスト

1. グローバルメモリが入った状態で export → 選択パスにファイル生成。開いて
   エンベロープ形状と全 fact の存在を確認。
2. その export したファイルを import → "0 added, N skipped"（全て重複）。
   ストア不変。
3. いくつか fact を削除し再 import → 削除した fact だけが戻り、他は skip。
4. category が無効値の fact を含むファイルを import → `decision` として出現。
5. セッション `.shellagent` バンドルや任意 JSON を import → 可読な
   "not a global-memory export"/schema エラーで拒否。ストア無変更。
6. クロスマシン（可能なら）: A で export, B で import → fact 出現、trust タグ
   ([user-stated]/[derived]) 保持。
7. 保存/開くダイアログをキャンセル → no-op、エラートースト無し。

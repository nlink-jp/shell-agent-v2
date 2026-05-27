# ADR-0028: グローバルメモリの未使用 provenance フィールドを削除する

- Status: Accepted
- Deciders: magi
- Related: ADR-0027 (グローバルメモリ export/import — 本変更で簡素化される), `docs/ja/reference/memory-model.ja.md` (スキーマ)

## 1. コンテキスト

`GlobalMemoryEntry` (`app/internal/memory/global_memory.go`) には、**どの
コードも読まない** provenance フィールドが3つある:

| フィールド | set 箇所 | read 箇所 |
|-------|-----------|------------|
| `SessionID` | 4 (`agent.go:1804`, `agent.go:1849`, `tools.go:286`, `agent_extract.go:273`) | **0** |
| `SourceTurnIndex` | 1 (`agent_extract.go:274`) | **0** |
| `PromotedFromID` | **0** | **0** |

`SessionID` と `SourceTurnIndex` は *write-only*（昇格/抽出時に打刻されるが
消費されない）。`PromotedFromID` は完全な dead — 構造体に宣言されているだけで
書き込みすらされない。

これらは**セッション名前空間**（マシンローカルで永続性が無い）への参照である:

- セッションIDはタイムスタンプ文字列（`sess-<unixMilli>`, `bindings.go:492`）
  で UUID ではない。マシンをまたぐと一意でなく（2台は壁時計のミリ秒空間を共有）、
  新規作成経路に衝突ガードも無いため、単一マシンでも back-reference は信頼できる
  キーにならない。
- `SourceTurnIndex` は*特定*セッションの record 列へのインデックスで、その
  セッションが無ければ無意味。別マシンでは二重に無意味。

未読データを保持するコストはゼロではない: ADR-0027 に import 時のサニタイズ
（外来IDをクリアし、将来の provenance 利用機能が衝突した外来IDをローカル
セッションと誤認しないようにする）を追加させた。これは**使っていないデータ**の
ためだけの複雑性である。

### Findings への影響は？

これらの死蔵フィールドは、ほぼ確実に **Findings** ストア（歴史的に
`OriginSessionID`/`OriginSessionTitle` を持っていた）からの類推で生まれた。
その経緯ゆえ、波及を明示的に否定しておく価値がある。波及は存在しない:

- `findings` パッケージが `internal/memory` から使う symbol はただ1つ、
  `memory.SessionDir()`（`findings.go:93`、パスヘルパ）。`GlobalMemoryEntry`
  や本 ADR が削除するフィールドは一切参照しない。`memory` は `findings` を
  import しない（一方向・最小結合）。
- `Finding` 構造体（`findings.go:52`）は既にクリーン: `ID`・`Content`・`Tags`・
  `CreatedAt`・`CreatedLabel`・`Source`・`ToolOriginated`。セッション由来
  フィールドは findings が per-session ファイル化した時（v0.2.0）に削除済みで、
  説明コメントが残るのみ。よって Findings 側に並行作業は無い — 既に済んでいる。
- promote-finding 経路（`agent.go:~1844`）は Finding から `f.Content` と
  `f.ToolOriginated` のみ read する。`GlobalMemoryEntry.SessionID` 削除は代入を
  消すだけで、Finding フィールドの read を消すわけではない。
- frontend でも `Finding`（`types.ts:76`）は `session_id` を持たない。他の
  `session_id` は無関係な型（`LLMStatus`・`ObjectInfo`）のもの。本 ADR が触るのは
  `GlobalMemory` interface だけ。

結論: 変更はグローバルメモリに限定される。Findings は疎結合で、既に目標形。

## 2. 決定

**`SessionID`・`SourceTurnIndex`・`PromotedFromID` を `GlobalMemoryEntry`
から削除する。** 全 set 箇所での代入も落とす。

残すもの:

- `Fact`・`NativeFact`・`Category`・`SourceTime`・`CreatedAt` — コア。dedup/
  表示/プロンプト予算で read される。
- `Source` — `globalTrustTag`（`global_memory.go:228`）が **read** して
  `[user-stated]`/`[derived]` タグを描画する。`GlobalSourcePromotedFrom*`
  定数も残す。`Source` はエントリが*どう*生まれたかを記録する移植可能な enum で
  あり、マシンローカルなポインタではない。
- `ToolOriginated` — 文脈に依存しない bool。名前空間ハザード無し。今回の trim の
  対象外として据え置く。

根拠: YAGNI。将来本当に由来 provenance が必要になったら、現状のマシンローカルで
未読のフィールドではなく、**クロスマシン同一性を扱える設計**（安定したグローバル
一意の由来トークン）で改めて導入すべき。マシンをまたぐと壊れる参照を今持つのは、
持たないより悪い — 利益ゼロの潜在リスクである。

## 3. 結果

- グローバルメモリのクロスマシン衝突懸念が**スキーマレベルで消滅**する:
  セッション参照を持たないエントリは、ローカルセッションと衝突も誤認もし得ない。
  データが無いので将来のどの機能もつまずきようがない。
- ADR-0027（export/import）が簡素化される: import 時にサニタイズすべき
  マシンローカル要素が無く、エントリはそのまま移植可能。
- **後方互換・マイグレーションコード不要。** `Load` は素の `json.Unmarshal`
  （`global_memory.go:107`, `DisallowUnknownFields` 無し）なので、`session_id`・
  `source_turn_index`・`promoted_from_id` キーを含む既存 `global_memory.json` も
  問題なくロードされる — 未知キーは無視され、次回 `Save` で黙って消える。
- 損失: 現在ディスク上にある履歴的な由来セッション値は失われる。許容範囲 —
  誰も読まず、マシンをまたぐと信頼できない。

## 4. 実装

- `app/internal/memory/global_memory.go` — `GlobalMemoryEntry` から3フィールド
  （と「Promotion back-reference」コメントブロック）を削除。
- `SessionID:` / `SourceTurnIndex:` 代入を削除:
  - `app/internal/agent/agent.go:1804`（promote-from-session-memory）,
    `:1849`（promote-from-finding） — `SessionID` を落とす。
  - `app/internal/agent/tools.go:286`（remember-fact ツール） — `SessionID`
    を落とす。
  - `app/internal/agent/agent_extract.go:273-274`（グローバル抽出） —
    `SessionID` と `SourceTurnIndex` を落とす。（`:299` の SessionMemoryEntry
    リテラルは自身の `SourceTurnIndex` を保持 — 対象外。本 ADR はグローバル
    メモリのみ。）
- `app/internal/agent/extract_memories_test.go:96-97` — グローバルエントリの
  `SessionID` への assertion を削除。
- `app/bindings.go` — `GlobalMemoryData` DTO（`:1928` 付近）から `SessionID` を
  削除し、`GetGlobalMemories` の `SessionID: f.SessionID` マッピングも削除。
  構造体の doc コメントも更新（現状 `SessionID` がバッジを駆動と書かれているが
  stale — サイドバーは描画していない）。
- `app/frontend/src/types.ts` — `GlobalMemory` interface から `session_id?` を
  削除（無関係な `LLMStatus.session_id`・`ObjectInfo.session_id` は残す）。
  サイドバー描画の変化なし（未表示のため）。
- `docs/en/reference/memory-model.md` + `docs/ja/reference/memory-model.ja.md`
  — `GlobalMemoryEntry` スキーマブロックから3フィールドを削除。

### テスト

- 1件の `SessionID` assertion を落とせば既存の memory/agent テストは通る。
- 後方互換テストを追加: レガシー `session_id`/`source_turn_index`/
  `promoted_from_id` キーを含む `global_memory.json` を `Load` → 成功。`Save`
  後はそれらのキーがファイルから消え、インメモリのエントリは無傷
  （Fact/Category/Source 保持）。

## 5. スコープ外

- `SessionMemoryEntry.SourceTurnIndex`（セッションスコープ・別ストア）。
- `ToolOriginated`（保持）。
- セッションID生成の堅牢化（`sess-<unixMilli>` の一意性） — 別の懸念。
  グローバルメモリのセッションID依存を消すこと自体には不要。

## 6. 手動スモークチェックリスト

1. Session Memory エントリと Finding をグローバルメモリへ昇格 → 両方出現。
   `global_memory.json` に `session_id`/`source_turn_index`/`promoted_from_id`
   キーが無い。trust タグ（[user-stated]）は正しいまま。
2. remember-fact ツール使用 → エントリ追加、削除済みフィールドのキー無し、
   エラー無し。
3. 既存の `global_memory.json`（旧キー入り）を開く → ロード成功。fact 表示は
   不変。何か変更を加えると旧キーはディスクから消える。

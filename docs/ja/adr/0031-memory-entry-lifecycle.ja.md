# ADR-0031: メモリエントリのライフサイクル（状態・関連度減衰・touch によるリフレッシュ・統合）

- Status: Accepted
- Deciders: magi
- Related: `docs/ja/reference/memory-model.ja.md` (スキーマ), ADR-0028 (provenance trim; 本 ADR の前提), ADR-0019 (remember-fact tool)。本 ADR を前提とする後続: ADR-0032 (context compaction v2, 計画中), ADR-0033 (recall tool / memory router, 計画中)。

## 1. コンテキスト

### 1.1 症状

長いセッション（典型的には 30 turn を超えたあたり）でエージェントの状況認識が劣化する: 直前の発言を認知せず、突然セッション序盤の話題に再アンカーする現象。仮説は — 該当コードを追って確認済み — `GlobalMemoryStore`・`SessionMemoryStore`・findings `Store` を毎ターン system prompt にフルテキスト dump するため、セッション長が伸びるほど古い事実への固着が強まる、というもの。

### 1.2 3 つの増幅源

`internal/chat/chat.go:196`・`internal/memory/session_memory.go`・`internal/agent/agent_extract.go`・`internal/contextbuild/builder.go` を読んで特定した相互強化メカニズム:

1. **System prompt の累積**: 毎ターン最大 ~48 KiB の事実箇条書きが注入される（3 ストア × 16 KiB cap）。減衰なし・関連度ランキングなし。それぞれ FIFO で 100/50/100 件 cap だが、cap に達するまでは抽出された事実が**セッションの一生分**残り続け、毎リクエストで再露出される。
2. **Assistant ターン抽出のドリフト**: `extractMemories`（`agent_extract.go:41`）は直近 4 件の *non-tool* レコードを読み、これに assistant ターンも含む。アシスタントの一過性の脱線が `fact`/`context` として抽出されて以降全ターンに再注入され、モデルが抵抗すべきはずのドリフトを**自己強化する記憶**として固定化する。
3. **要約のアンカリング**: 会話末尾サマライザのプロンプト（`agent.go:889`）が `"Preserve key facts, decisions, and context."` と指示している。会話が伸びるほどサマリは序盤のアンカーを保存し、system block と直近 records の間に配置される — 注意重みが強い位置である。

本 ADR は **(1)** — system prompt の累積 — に対処する。(2) は副作用として一部緩和される（統合により assistant ドリフト由来の近似重複が 1 entry に潰れる）が根治は別 ADR。(3) は ADR-0032（context compaction v2）の対象で、直後の後続として計画している。

### 1.3 現状で欠けているもの

`GlobalMemoryStore` / `SessionMemoryStore` の API を辿ると、メモリモデルが持つもの:

- `Add` による作成（+ cap 超過時の FIFO eviction）
- `Delete` / `DeleteByFacts` による削除
- 追加時の `Fact` 完全一致 dedup

持っていないもの:

- 「まだ load-bearing」か「stale」かを区別するエントリ状態
- 関連度（relevance）や経時的な aging
- 参照追跡（この事実は再言及されたか? LLM が使ったか?）
- 近似重複の統合
- cap-hit FIFO 以外の eviction policy
- 状態遷移や eviction の監査ログ

FIFO 50/100 は無制限成長を防ぐ安全弁であって、品質管理ではない。

## 2. 決定

**Global Memory と Session Memory** にエントリ単位のライフサイクルを導入する。Findings は本 ADR の対象外（§5）。ライフサイクルは 4 状態:

```
fresh ──┬─→ active ──┬─→ dormant ──→ archived ──→ (evicted)
        │            │
        └── touch ───┘
```

- **fresh** — 直近 `FreshTurns` turn 以内に作成（デフォルト 3）。Relevance = 1.0。system prompt に常時描画。
- **active** — relevance ≥ `ActiveThreshold`（デフォルト 0.4）。system prompt に描画。
- **dormant** — relevance が `ActiveThreshold` を下回ったが `ArchiveThreshold`（デフォルト 0.1）より上。system prompt には**描画しない**。touch 対象としては残る。
- **archived** — relevance ≤ `ArchiveThreshold`。描画しない。extractor の露出対象外。cap pressure 発生時に最優先で eviction。

`State` は load/save 時および touch/decay 後に **`Relevance` から導出**される。エントリにマテリアライズされるのは UI バッジと監査ログを駆動するためで、独立した authoritative source として扱われるわけではない。

### 2.1 Relevance と decay

各エントリは `[0.0, 1.0]` の `Relevance` を持つ:

- 作成時 `1.0`。
- 各 user turn 完了後に `Relevance *= DecayRate`（デフォルト `0.93`）。デフォルト値では、参照のない entry は 1.0 から ~0.4（active/dormant 境界）まで ~12 turn、archive 閾値（~0.1）まで ~32 turn で到達する。
- touch は `Relevance` を `1.0` に戻し、`LastTouchedAt` / `LastTouchedTurn` を更新する。
- `TouchCount` は touch 毎にインクリメント。v1 では状態導出に使わないが、監査と将来の統合ヒューリスティクスのために記録する。

Decay は**ターン単位**であり壁時計ではない。一晩おいたセッションが記憶を失うのは不適切で、80 turn を駆け抜けるセッションは速く減衰すべきだから。壁時計減衰は検討して却下した（§5）。

### 2.2 Touch（参照検出）

2 経路の和集合:

1. **Lexical フォールバック**（常時、追加コストほぼゼロ）: user turn が追記された直後、turn の content と各 entry の `Fact` の token-set Jaccard を計算する。`TouchJaccardThreshold`（デフォルト 0.3）以上のエントリを touch する。閾値は `findings.DedupJaccardThreshold = 0.5` より低い — false-positive な touch（実は参照されていない entry を温存する）は、false-negative な aging（まだ関連する entry を dormant に落とす）よりはるかに害が少ないため。
2. **Extractor 由来の touch**（主信号、追加 LLM コール無し）: `extractMemories` は既に直近 4 turn + 既知 fact リストを LLM に見せている（`agent_extract.go:117-126`）。その出力に `touched:` 行を追加させ、会話末尾で参照された fact-text フィンガープリントを列挙させる。それをパースして該当 entry をリフレッシュする。

Extractor 経路は lexical が取り逃した参照（言い換え・意味的参照）を捕捉でき、 lexical は extractor が何も返さなかった時の安全網。

### 2.3 Consolidation（近似重複の統合）

`Add` は現状 `Fact` 完全一致で dedup している。これを Jaccard ベースの統合に置き換える:

- 新しい entry に対し既存 entry を走査。 `Fact` の Jaccard が `ConsolidationJaccardThreshold`（デフォルト 0.5 — findings と一致）以上なら、append せず**既存 entry を touch する**として扱う。
- 既存の `Fact` テキストを保持（先行する確立された表現）。`TouchCount` を増加、`Relevance` を `1.0` にリセット。
- これは §1.2(2) の assistant ドリフト自己強化ループを断つ: 同じドリフト事実の言い換え群が 1 entry にまとめられて aging するようになり、N 件の近似重複がそれぞれ 32 turn 生存して system prompt に積み上がる現象を防げる。

### 2.4 Eviction policy

cap-hit FIFO を**最低 relevance 優先**に置き換える:

- `len(Entries) > MaxEntries` の時、`Relevance` が最も低い entry を evict する。 tie は `LastTouchedAt` が古い方が優先。
- 古いが touch されている entry は、新しいが touch されていない entry より生き残る — メモリ品質として正しい順序。
- `archived` entry は、`LastTouchedAt` が `active` より新しくても eviction の優先対象（状態が recency より優先される）。

### 2.5 System prompt 注入

`FormatForPrompt` は `dormant` と `archived` を出力から除外する。16 KiB 予算は防御層として残るが、ライフサイクル導入後は実質的な制約として機能することはほぼない。

注入される出力フォーマットは不変（`[fresh]` / `[active]` のような状態タグを LLM に見せることはしない — §2.7）。

### 2.6 監査ログ

全ての状態遷移・touch・consolidation・eviction を、既存 `internal/logger` 経由で `Info` レベルに記録する:

```
memory: session-memory entry "User wants Q1 sales analysis…" fresh→active (relevance 0.79)
memory: session-memory entry "Three datasets loaded" active→dormant (relevance 0.39, last touched turn 12, now turn 27)
memory: session-memory evicted "Spurious tangent about CI…" (relevance 0.08, state archived, last touched turn 4)
memory: session-memory touched "User wants Q1 sales analysis" (relevance 0.62 → 1.0, source: extractor)
memory: session-memory consolidated new entry into existing "User has three datasets loaded" (Jaccard 0.71, source: assistant_turn)
```

「なぜこの事実が消えたか / なぜ LLM が古い話題に固着し続けるか」をログから追えるようにする — ライフサイクル議論でデバッグ性の問題として明示された点。

### 2.7 LLM と UI が見るもの

- **LLM**: スキーマ変更は不可視。 `FormatForPrompt` の出力から dormant/archived の bullet が落ちる以外、フォーマットは同一。`[fresh]` / `[active]` などの注釈がモデルに漏れることはない。
- **UI**: 既存の Memory サイドバーに、行毎の状態バッジ（`fresh` / `active` / `dormant` / `archived`）と relevance hint（行の右端に薄いバー）を追加する。archived は `Show archived (N)` で折り畳む。UI 差分はこれだけ — リスト並び替えなし、新タブなし。

v1 では UI から relevance / state を編集できない（機械的に導出される）。delete はどの状態にも有効。

## 3. 結果

### 3.1 振る舞い

- 長セッションでも system prompt に ~48 KiB の事実が積み上がらない — `fresh` + `active` のみ生き残る。期待: 本 ADR を起こす動機となった「古い話題への再アンカー」症状の顕著な減衰。
- Assistant ドリフトの事実は consolidation により 1 entry に潰れ、N 件の近似重複として残らない。
- Eviction 品質が改善: touch されている安定事実は生き残り、一過性のノイズは aging してまず再利用される。
- 実際は重要だが ~30 turn 参照されなかった事実は dormant 落ちしうる。**削除はされない** — dormant/archived はディスク上に残り、サイドバーから検索可能。dormant 事実を LLM の視界に戻す経路は後続 ADR（recall tool / memory router）で扱う。

### 3.2 ディスクスキーマ

`GlobalMemoryEntry` と `SessionMemoryEntry` に 5 フィールド追加:

```go
Relevance       float64   `json:"relevance,omitempty"`
LastTouchedAt   time.Time `json:"last_touched_at,omitempty"`
LastTouchedTurn int       `json:"last_touched_turn,omitempty"`
TouchCount      int       `json:"touch_count,omitempty"`
State           string    `json:"state,omitempty"` // derived; UI consumer 用に永続化
```

`omitempty` により fresh entry のディスクサイズ増は無視できる程度。

### 3.3 後方互換性

ライフサイクル以前のビルドで書かれた既存 `global_memory.json` / `session_memory.json` の entry は 5 フィールドを持たない。`Load` 時に silent に埋める:

- `Relevance == 0`（ゼロ値）を「legacy」と認識して `1.0` に置換。
- `LastTouchedAt` と `LastTouchedTurn` はそれぞれ `CreatedAt` と `0` をデフォルトに。
- `TouchCount = 0`。
- `State` は `Relevance` から再計算（legacy entry は `active` として入る — fresh window はターン数依存で per-session 概念、load 時には決まらない）。

migration コード無し、ファイルのバージョン bump 無し。`Save` 時に新フィールドが書かれる。ユーザーが downgrade しても `json.Unmarshal` が unknown key を無視する（ストアは `DisallowUnknownFields` を使わない）。

この silent-fill アプローチは ADR-0028 のパターンと整合する。

### 3.4 テスト影響

既存の `Add` / `Delete` / `FormatForPrompt` テストは legacy-fill 挙動でそのまま通る。

新規テスト:

- N turn の decay で entry が `dormant`、次いで `archived` に落ちる。
- Touch が relevance を 1.0 に戻す。 lexical / extractor 両経路を verify。
- Consolidation: 近似重複の追加で既存 entry の `TouchCount` が増え、append されない。
- Eviction: 最低 relevance が最初に evict、`archived` は recently-touched な `active` よりも優先される。
- Legacy ファイル（ライフサイクルフィールド無し）が load し、正しく挙動し、次回 `Save` で新フィールドが書かれる。
- `FormatForPrompt` の出力から `dormant` / `archived` entry が逐語的に除外される。

### 3.5 Config つまみ（デフォルト付き）

全閾値は既存 JSON config の `Memory.Lifecycle.*` で公開する（再ビルド無しで調整可能だが、デフォルトは妥当でほとんどのユーザーは触らない）:

```json
{
  "Memory": {
    "Lifecycle": {
      "DecayRate": 0.93,
      "FreshTurns": 3,
      "ActiveThreshold": 0.4,
      "ArchiveThreshold": 0.1,
      "TouchJaccardThreshold": 0.3,
      "ConsolidationJaccardThreshold": 0.5
    }
  }
}
```

## 4. 実装

### 4.1 Memory パッケージ

- `app/internal/memory/lifecycle.go`（新規）。pure functions: `DecayedRelevance(r, rate)`・`DeriveState(r, freshUntilTurn, currentTurn, thresholds)`・`JaccardScore(a, b)`・`ConsolidationMatch(entries, newFact, threshold) -> (index, ok)`。状態導出を一箇所に集中させ、entry ストアから呼ぶ。
- `app/internal/memory/global_memory.go`:
  - `GlobalMemoryEntry` を §3.2 の 5 フィールドで拡張。
  - `Load` から各 entry に対し（`Relevance == 0` のもの）`legacyFill` を呼ぶ。
  - `Add` の dedup ロジックを `ConsolidationMatch` 経由に置換。merge 経路で `TouchCount` を増加、`Relevance` をリセット、監査ログ行を emit。
  - `Touch(matchFn func(GlobalMemoryEntry) bool, currentTurn int, source string) int` を追加。touched 件数を返す。touch 毎に監査ログ。
  - `DecayAll(currentTurn int)` を追加。`fresh` 以外の全 entry の relevance を乗算し、状態を再計算し、遷移に監査ログを emit。
  - `Add` 内の FIFO eviction を `evictLowestRelevance` ヘルパに置換 — 状態優先（archived → dormant → active → fresh）。
  - `FormatForPrompt` を `dormant` / `archived` をスキップするように更新。
- `app/internal/memory/session_memory.go` — 同じ変更をミラー。

### 4.2 Agent ループフック

- `app/internal/agent/agent.go`: user turn 追記の後、LLM コール前（`buildMessagesV2` の前）に `globalMemory.DecayAll(currentTurn)` と `sessionMemory.DecayAll(currentTurn)` を呼ぶ。Decay は entry 数に対し O(n) で軽量。注入の前に走らせれば `FormatForPrompt` が直前計算済の状態を見られる。
- `app/internal/agent/agent.go`: user turn 追記後に lexical touch:
  ```go
  pred := lifecycle.LexicalTouchPredicate(userContent, cfg.Memory.Lifecycle.TouchJaccardThreshold)
  globalMemory.Touch(pred, currentTurn, "lexical_user_turn")
  sessionMemory.Touch(pred, currentTurn, "lexical_user_turn")
  ```
- `app/internal/agent/agent_extract.go`:
  - Extractor の system prompt を拡張し、会話末尾で参照された fact-text フィンガープリント（短いハッシュ or 先頭 N token）を `touched:` 行で列挙させる。
  - `touched:` 行をパース、ストア側で同じフィンガープリントを再計算しマッチした entry を `Touch(matchFn, currentTurn, "extractor")` する。
  - `Add` が consolidation 経路を返した場合（既存 entry を touch、新規 append 無し）、agent 側で追加ログは不要 — ストアが自分で emit する。

### 4.3 Bindings と frontend

- `app/bindings.go` — `GetGlobalMemories` / `GetSessionMemories` の DTO に `state`・`relevance`・`last_touched_at`・`touch_count` を追加。既存フィールドは不変。順序は挿入順を維持し、UI 側で視覚的グルーピングする。
- `app/frontend/src/types.ts` — DTO 追加をミラー。
- `app/frontend/src/` — サイドバー行に状態バッジ（`fresh` / `active` / `dormant` / `archived`）と細い relevance バーを追加。`Show archived (N)` の折り畳みグループ。レイアウト大改修なし。

### 4.4 Events

既存の `global_memory:updated` / `session_memory:updated` イベントは、 `DecayAll` 後に entry の状態が一つでも flip したら fire する。これでサイドバーは polling なしで再描画される。agent ループが `DecayAll` を呼ぶ同じフックからイベント送出する（ストアパッケージを Wails 依存から自由に保つ）。

## 5. スコープ外

- **Findings**。アクセスパターンが異なる — データ分析の発見はユーザーが明示的に参照することが多く、フィルタ/検索用にタグ付けされ、 `load-data` semantics で per-session に bound されている。ここでライフサイクルを足すコスト/効果が不明。Findings 固有の症状が出る、または ADR-0032 で統合する強い理由が現れるまで保留。
- **Context compaction v2**。会話末尾サマライザの再設計（anchor 保護・多層 summary・dead-topic drop）は ADR-0032 — 直後の自然な後続で、§1.2 の 3 つ目の増幅源。
- **Recall tool / memory router**。`dormant` / `archived` entry を on-demand fetch や parallel scene-keyed selector で LLM の視界に戻す経路は ADR-0033 / ADR-0034 の領域。本 ADR のライフサイクル整備が前提 — 「今は関係ない」と分類できる状態が無ければ「どう呼び戻すか」という問い自体が形を成さない。
- **事実単位の embeddings / ベクター検索**。本 ADR のライフサイクル作業外。touch 検出は lexical + extractor で済ませ、プロジェクトの「外部依存ゼロ」性を保つ。
- **壁時計減衰**。検討して却下: ユーザー痛点は active セッション内の per-turn ドリフトであって複数日に渡る staleness ではない。cross-session Global Memory の衛生問題が顕在化したら再検討する。
- **UI から relevance / state を編集**。v1 は機械的に動かす — ユーザーは delete か keep のみ、 in-between はシステムが管理する。編集 surface を v1 で開くと audit-driven デバッグ物語を壊す ad-hoc チューニングを招く。

## 6. 手動 smoke checklist

1. 新セッションを開始。トピック A について 4 turn 対話。関連する session-memory bullet が `FormatForPrompt` 出力に現れることを確認（app.log またはサイドバーバッジ `fresh`）。
2. トピック B に切り替え。A を参照せずに ~10 turn B を進める。トピック A の bullet が `dormant` に落ちる（サイドバーバッジ変化・app.log に `active→dormant` 行）ことと、system prompt から消えることを確認。
3. 1 turn だけ A を持ち出す。dormant entry が touch され `active` に戻り、次のリクエストの system prompt に再出現することを確認。
4. Consolidation を起こす: アシスタントに近似事実を再生成させる（"the user is analysing Q1 sales" vs "User wants Q1 analysis"）。2 回目の抽出が `consolidated` としてログされ、既存 entry の `TouchCount` が増え、新規 entry が append されないことを確認。
5. Session memory の cap（50）を超えるまで push。Eviction が `archived` を最優先、次に最低 relevance の `dormant` を選び、`active` は決して選ばないことと、app.log の eviction 行で選ばれた entry が fact text で識別できることを確認。
6. ライフサイクル以前の `session_memory.json` を load（または新フィールド無しの entry を書いて模倣）。load 成功、 `active` で `relevance` 1.0 として表示、次回 `Save` で新フィールドが書かれることを確認。
7. `Memory.Lifecycle.DecayRate` を 0.5 に変更して再起動、 ~3 turn 以内に減衰が目で見える速度で進むことを確認。

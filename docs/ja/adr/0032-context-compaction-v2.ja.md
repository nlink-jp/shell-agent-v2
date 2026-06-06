# ADR-0032: Context compaction v2 (層化サマリ + アンカー保護 + ライフサイクル駆動の dead-topic drop)

- Status: Accepted
- Deciders: magi
- Related: ADR-0031 (memory entry lifecycle — 本 ADR の topic-drop シグナルが依存)、`docs/ja/history/memory-architecture-v2.ja.md` (本 ADR が拡張する contextbuild モデル)、`docs/ja/reference/memory-model.ja.md` (スキーマ)。

## 1. コンテキスト

### 1.1 ADR-0031 の症状と未完了部分

ADR-0031 §1.2 は「長いセッションで古い話題に再アンカーする」症状の背後に 3 つの相互強化メカニズムを特定した。 ADR-0031 はそのうち (1) — system prompt の累積 — をエントリライフサイクル導入で対処した。 (2) — assistant ターン抽出のドリフト — は Jaccard 統合の副作用として一部緩和された。 本 ADR は最後の (3) — **要約のアンカリング** — を閉じる。

会話末尾サマライザ (`agent.go:889`) は LLM に *"Summarize the following conversation segment concisely. Preserve key facts, decisions, and context."* と指示する。 セッションが伸びるほどサマリは序盤のアンカーを逐語保持し、一過性の content は圧縮される。 そのサマリブロックは system prompt と直近 raw records の間に注入される — 注意重みが強い位置だ。 結果として、 ADR-0031 が効いていてもサマリ経由で古い話題が LLM の視界に再進入して注意を引き戻しうる。

### 1.2 現状サマライザの何が問題か

`internal/contextbuild/builder.go` と `agent.go:883-897` を読むと:

1. **単層圧縮**。 raw records 窓の外にある全 record が 1 サマリブロックに折り畳まれる。 セッションが伸びるほど要約対象 span は無制限に伸びるが、サマリ自身が予算超過時に再要約されることはない。
2. **Range keyed cache**。 サマリキャッシュキーはサマリがカバーする record indices のスライス (`cache.go:ComputeRangeKey`)。 新しい user ターンごとに range がシフトし、 content が変わらなくても再生成を強制する。
3. **「Preserve key facts」プロンプト指示**。 これが load-bearing 病理。 LLM は序盤の決定を load-bearing として扱い、再生成 summary 毎に保持し続ける — 我々が排除したい序盤アンカーの強化そのもの。
4. **トピック認識なし**。 サマリは会話が先に進んだことを知らない。 turn 5 のトピック A はサマリに永続し、 turn 60 のトピック Z と同じ salience を保つ。
5. **アンカー区別なし**。 「key fact」と「アンカー的瞬間」が混同される。 ユーザが宣言した preference (「Go が好き」) と一過性のコメント (「テーブル見栄え OK」) が同じ保存圧力をプロンプトから受ける。

### 1.3 これが次の一手として正しい理由

ADR-0031 は本 ADR が再利用できる 2 つの成果を生んだ:

- **各 Session Memory entry のライフサイクル状態**。 entry が dormant / archived の時、 ADR-0031 は既に「この話題は現在の会話で load-bearing ではない」と結論している。 compaction も同じシグナルを使える: dormant / archived な topic に紐づく records は逐語要約しなくてよい。
- **カテゴリで分割された Global Memory ストア**。 `preference` と `decision` entry は durable でアンカリングされている。 そのいずれかの fact を生んだ record はアンカーそのものなので、逐語テキスト保持が明確に正しい選択。

この 2 つを使えば、 compaction はサマライザ LLM の判断 — drift 問題の出元 — に頼らず**ライフサイクル駆動**になる。

## 2. 決定

context compaction を 5 軸で再構築する。

### 2.1 2 層サマリ

単一サマリブロックを system prompt と raw records の間の 2 層に置き換える:

```
[System Prompt]                       ← ADR-0031 ライフサイクルフィルタ済みメモリ
[Far Summary]   (古い session, ~5%)    ← トピック箇条書き、アンカーは別出し
[Near Summary]  (中盤 session, ~15%)   ← トピック箇条書き、アンカーは別出し
[Anchored Records]  (逐語)             ← decision/preference 由来ターン
[Raw Records]   (直近, ~80%)
```

トークン予算配分 (デフォルト; `ContextBudget.*` で調整可):

- Raw records: 利用可能予算 (OutputReserve と SystemPrompt 控除後) の 80%
- Near summary chunk: 利用可能予算の ~15%。 raw 窓を過ぎた直後のチャンクが入力。 1 回要約される。
- Far summary chunk: ~5%。 near 窓より古い全部。
- Anchored records: near/far span のどこからでも抽出され (§2.2)、サマリ群と raw records の**間**に逐語で描画される。

far tier 自身が予算超過する場合 (超長セッション)、再 compaction が発動: far tier の古い部分が「このセッションがこれまで何だったか」のメタサマリに折り畳まれ、 1 行サイノプシスを生む。

### 2.2 Global Memory クロスリファレンスによるアンカー保護

**anchor record** は、ある record の content と `decision` / `preference` カテゴリの Global Memory entry の `Fact` テキストとの lexical Jaccard が `AnchorJaccardThreshold` (デフォルト `0.4`) を超えるもの。

アンカー検出は ADR-0031 の既存 `memory.JaccardScore` / `TokenSet` を流用する — 新規 lexical 機構なし。 チェックは compaction 時に全 Global Memory entry の fact テキスト和集合に対して走るので、 pin 直後の decision / preference もすぐ拾える。

アンカー record は専用ブロックで raw records の直前に**逐語描画**される。 同時にそれぞれのサマリ入力にも残る (サマリが言及してもよい) が、 LLM が見る canonical 形は逐語ブロック。

これは設計議論で言う §Q2-A: アンカーは extractMemories が既に decision / preference を Global Memory に routing している副作用として派生する。 LLM に呼ばせる**新規ツールは無い** (`mark_anchor` を検討して却下 — ローカル LLM の tool 呼出信頼性が不確実すぎる)。

### 2.3 ライフサイクル駆動の dead-topic drop

**dead record** は、 record content と*いずれかの* `dormant` / `archived` Session Memory entry の `Fact` の lexical Jaccard が `DeadTopicJaccardThreshold` (デフォルト `0.4`) を超え、 かつ `fresh` / `active` Session Memory entry のいずれにもマッチしない record。

dead records はサマリ入力から**完全に drop** — トピック箇条書きとしても言及されない。 サマリブロックは drop された records 数を 1 つの省略マーカで emit する (`[N dead-topic turns suppressed]`)。

これが ADR-0031 との最も強いシナジー: 「この topic はもう関係ない」シグナルが system prompt から fact を外すのと同時に、会話サマリからも外す。 両 ADR が反発でなく**相互強化**する。

dead records は `session.Records` 上に残る — drop は per-compaction で破壊的ではない。

### 2.4 内容ハッシュ cache key

range-based キャッシュキー (`internal/contextbuild/cache.go:ComputeRangeKey`) を内容ハッシュキーに置き換える:

```
key = sha256(
    SummarizerID
    || sha256(concat(record.Role + record.Content for each input record))
    || sha256(sorted(deadTopicFingerprints))
    || sha256(sorted(anchorRecordIndices))
    || tier  // "near" or "far"
)
```

これにより cache は:

- **ターン追加に対し安定**: 新ターンが tier の入力を変えなければ tier の cache hit は維持される。
- **関連するライフサイクル変化に invalidate**: Session Memory entry が dormant に遷移すると (新規 dead-topic drop が起きる)、影響を受ける tier のキーが変わりサマリが再生成され drops が反映される。
- **Per-tier**: 各 tier が自分の cache slot を持ち、片方の churn が他方を invalidate しない。

ディスク上の cache フォーマット (`summaries.json`) は本キーで `SummaryEntry` 行を蓄積する。 古い (range-keyed) entry は古いキーで読まれ、 hit 時に新内容ハッシュキーで書き直される。

### 2.5 新サマライザプロンプト

現行プロンプトを置換。 新設計は tier 毎に 2 つの相補的指示を持つ:

```
Summarize the following conversation segment as a list of topic bullets,
one line per distinct topic discussed. Format:
  - <topic>: <one-line summary of what was said about it>

Important rules:
- The user's preferences, decisions, and key facts are already preserved
  verbatim in a separate block. Do NOT repeat them; just list the topic
  they belong to.
- Discussions that did not lead anywhere should still be listed but
  marked: "- <topic>: discussed but not concluded".
- Dead-end / dropped topics have already been removed from this segment;
  do not invent topic bullets for content that is not here.
- Be terse. Aim for 1-3 sentences per bullet at most.
- Use the same language as the conversation.
```

序盤アンカー強化の load-bearing 元だった *"Preserve key facts, decisions, and context"* 行を落とす。 サマライザはもうアンカー保持の責任を持たない (アンカー record ブロックがそれを担う) ので、唯一の仕事は凝縮。

## 3. 結果

### 3.1 振る舞い

- 古いトピックがサマリブロック経由で LLM を再アンカリングしない。 そのトピックが decision/preference アンカリングされている (逐語 record がプロンプトにあり明示的に historical として配置) か、 dead (完全省略) のどちらか。
- Session Memory の dormant / archived 状態が意味ある第二の効果を持つ — system prompt から entry を落とすだけでなく、会話末尾サマリから同 topic の言及を落とす。
- サマリブロックは「何が**話された**か」ではなく「何が**議論された**か」の高レベル要約として読める。 人間がノートを取る方法に近く、 LLM が自分の位置を取りやすい。
- アンカー record が別ブロックになることで「ユーザは何を decide したか」がサマリ散文をスキャンせずに答えられる。
- 超長セッション (数百 turn) が context window 飽和なしで実用可能になる。 far tier が予算超過で再 compaction されるため。

### 3.2 後方互換性

- アップグレード後の初回実行で `summaries.json` キャッシュファイルは range-keyed entry を含む。 load は通るが、新内容ハッシュキーにマッチせず orphan 化し、既存 FIFO cap で GC される。 migration コード不要。
- `agent.go:889` のサマライザクロージャを新プロンプトで置換。 ディスク上 in-flight セッションは無影響 — 次の user ターンが新ルールで compaction する。
- ADR-0031 ライフサイクル状態が dead-topic drop の発火に必要。 ADR-0031 以前のセッション (Session Memory entry が legacy default `state=active`) は、ライフサイクル減衰が dormant entry を生み始めるまでは dead-topic drop をスキップする。 安全な degradation: その場合 「drop 無し、 2 層サマリのみ」にフォールバックするが、これでも純改善。

### 3.3 テスト影響

- `internal/contextbuild/builder_test.go`: budget walking、 summary 包含、 cache hit/miss の既存テストはそのまま有効。 2 層分割、 anchor 抽出、 dead-topic drop、 ターン追加への内容ハッシュキャッシュ安定性の新規テストを追加。
- `internal/contextbuild/cache_test.go`: 後方互換テストを追加 (legacy range-keyed entry がエラー無く load し、ただし match しない)。
- Agent レベルテスト: dormant な session-memory topic を参照する Records を持つセッションが「drop 数明示 + その topic への逐語言及なし」の summary を生む。

### 3.4 Config つまみ

`ContextBudget` に新規追加 (既存フィールドを拡張、置換ではない):

```json
{
  "ContextBudget": {
    "FarSummaryShare":           0.05,
    "NearSummaryShare":          0.15,
    "AnchorJaccardThreshold":    0.4,
    "DeadTopicJaccardThreshold": 0.4
  }
}
```

`MaxContextTokens`、 `MaxToolResultTokens`、 `OutputReserve` の形と意味は不変。

`FarSummaryShare + NearSummaryShare` 控除後の残りの ~80% が raw records、アンカー record、 system prompt 用。 アンカー record は raw シェアに数える (同じ「逐語」描画パスを共有)。

### 3.5 削除されるコードパス

- `assembleSummary` の単一ブロックパスは `assembleNearSummary` と `assembleFarSummary` に置換。 `Build` のシグネチャは不変、 result は引き続き `Messages`・`TotalTokens` 等を持つ。
- 「サマライザ無し → 古い tail を silent drop」分岐を削除。 2 層化で「どちらの tier を drop するか」が曖昧になるため。 代わりにサマライザ未提供時は drop 件数を明示する省略マーカをサマリブロックの位置に出す。

## 4. 実装

### 4.1 Anchor + dead-topic 検出ヘルパ

`internal/memory/lifecycle.go` に追加:

```go
// LookupTokenSet returns a TokenSet ready for repeated comparison.
// Memoised by the caller — Build() builds it once per anchor source.
//
// (Already provided by TokenSet; this is just a label.)

// AnchorRecord reports whether a record matches any of the supplied
// anchor texts at Jaccard ≥ threshold. anchorTokenSets is precomputed
// once per Build to avoid re-tokenising every Global Memory entry per
// record.
func AnchorRecord(content string, anchorTokenSets []map[string]struct{}, threshold float64) bool

// DeadTopicRecord reports whether a record matches any dormant /
// archived session-memory fact at Jaccard ≥ threshold AND does not
// also match any active / fresh session-memory fact at the same
// threshold. The second clause is the safety net: a record that
// references both a live topic and a dead one is kept.
func DeadTopicRecord(
    content string,
    deadTokenSets, liveTokenSets []map[string]struct{},
    threshold float64,
) bool
```

両者 pure functions。テストは既存ライフサイクルテストの隣。

### 4.2 Builder 再構築

`internal/contextbuild/builder.go`:

- 「newest → oldest を walk し raw 予算を埋める」ループを `selectRawWindow` に抽出。
- `partitionForTiers` を追加: raw 窓より古い records をシェアつまみに基づき token 数で `nearInput` と `farInput` に分割。
- `liftAnchors` を追加: nearInput + farInput の和集合を走査、 §4.1 で anchor record を識別、サマリ入力から除去、逐語描画用のスライスを返す。
- `dropDeadTopics` を追加: 残りのサマリ入力を走査、 dead records を除去、 drop 件数を返す。
- `assembleSummary` を `assembleTier(name, input, droppedCount, cache, opts) (block string, fromCache bool)` に置換。 2 回呼ばれる。
- 最終アセンブリ順: system → far summary → near summary → anchored records → raw records。

### 4.3 Cache 再構築

`internal/contextbuild/cache.go`:

- `ComputeRangeKey` を `ComputeContentKey(records, deadFingerprints, anchorIndices, summarizerID, tier)` に置換。
- `Get` と `Put` は新キー形を受ける。ディスクスキーマに forward-compat の `kind: "content_v2"` フィールド追加。
- Legacy range-keyed entry は読めるが hit 時に無視 (miss として扱う)。次回 save で drop される。

### 4.4 サマライザ

`internal/agent/agent.go`:

- `summarize` クロージャの system prompt を §2.5 のテキストに置換。
- クロージャはオプションで `tier` ヒントと language hint (既存 `detectUserLanguageHint` を再利用) を取る。検出言語の挙動は不変。

### 4.5 Build options への配線

`BuildOptions` に追加:

- `AnchorSources []string`  — 典型的にはすべての `decision` / `preference` Global Memory entry の `Fact` 文字列。
- `DeadTopicSources []string` — すべての dormant / archived Session Memory entry の Fact 文字列。
- `LiveTopicSources []string` — すべての fresh / active Session Memory entry の Fact 文字列。
- `FarSummaryShare`、 `NearSummaryShare`、 `AnchorJaccardThreshold`、 `DeadTopicJaccardThreshold` (config をミラー)。

`agent.buildMessagesV2` がライブストアからこれらを populate する。 agent 層が型変換、 memory 層は contextbuild に依存しない。

## 5. スコープ外

- **Findings**。 ADR-0031 と同じ理由: アクセスパターンが異なる、保留。
- **Recall tool / memory router**。 ADR-0033 / ADR-0034 領域。 compaction が dead topic で再アンカリングしなくなったので、「必要時に dormant fact を LLM の視界に戻す」問題が well-formed になる。
- **ベクトルベースのトピック類似度**。 短い fact テキストと「外部依存ゼロ」原則 (ADR-0031 が踏襲) を考えると、 anchor / dead-topic ゲートには lexical Jaccard で十分。
- **ユーザ向けサマリ描画**。 compacted サマリはバックエンドのみのアーティファクトのまま。 chat UI は引き続き raw Records を描画する。
- **Streaming compaction**。 far tier 再 compaction は Build 呼び出し毎に同期実行。 将来 latency が問題化したらバックグラウンドゴルーチンに移す ADR を立てる。 現行デフォルト予算では far tier 再生成は稀。
- **Record スキーマへの Anchor フラグ**。 設計議論で Q2-C として検討し却下。 Global Memory の Fact テキストに対する lexical Jaccard クロスリファレンスで Record スキーマ変更を回避し、 anchor シグナルを既存データから派生可能に保つ (import 後の事後再構成も自動で動く)。

## 6. 手動 smoke checklist

1. **2 層描画**: 30 ターンセッションで app.log が 2 つのサマリブロック (`tier=near` と `tier=far`) を emit し、 LLM transcript に system → far → near → anchored → raw の順で組み立てられていることを確認。
2. **アンカー保護**: セッション序盤に明確な preference (「I prefer Go over Python」) を述べ、追加で 25 ターン進行、元の preference ターンが anchored-records ブロックに逐語表示されることを確認 — サマリ箇条書きでパラフレーズされるだけではないこと。
3. **Dead-topic drop**: 5 ターン topic A を議論、 15 ターン topic B に切り替え (topic A の session memory が dormant に落ちる時間)、次の summary 再生成で `[N dead-topic turns suppressed]` が emit され、 topic A の名詞が含まれないことを確認。
4. **ターン追加に対する cache 安定性**: 20 ターンセッションで、要約 span に何も変化が無い turn で `buildMessagesV2: ... cache_hit=true` を観察。 全ターン miss だった旧挙動と比較。
5. **ライフサイクル変化での cache invalidate**: topic A が active なセッションを用意。 (例: `DecayRate=0.5` 一時設定で) topic A を強制 dormant 化。 次のターンでサマリが再生成 (`cache_hit=false`) し topic A 内容が drop されていることを確認。
6. **超長セッション再 compaction**: 200 ターンセッションで far tier 自身が `FarSummaryShare * budget` 内に収まることを確認 — 閾値クロス時に `app.log` が再 compaction 行を emit。
7. **Legacy cache load**: ADR-0032 以前ビルドの `summaries.json` をセッションにコピー、再起動、エラー無く load し次の compaction が新内容ハッシュキー entry を書く (legacy entry は参照されず最終的に FIFO で drop) ことを確認。
8. **サマライザ無しフォールバック**: サマライズできないバックエンドを一時設定 (または `nil` Summarize)、 build がサマリ tier の位置に省略マーカを emit し、 crash しないことを確認。

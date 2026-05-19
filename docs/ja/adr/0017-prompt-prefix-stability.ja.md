# Prompt prefix の安定化による KV cache 再利用 — 設計ノート

**Status:** Proposed (2026-05-20)。
**Target:** v0.13.0 (マイナーバンプ — 内部メッセージ組み立ての変更のみ、
スキーマ移行なし、Wails バインディング署名の変更なし)。
**Reported by:** ユーザー — 「ローカル LM Studio で shell-agent-v2 を
動かすと、各ターンの PROMPT PROCESSING 時間が支配的。トークン生成は
数秒で終わる。体感的には毎ターン全プロンプトを再 tokenize している
感じ」。

本ノートは **プロンプトの prefix を安定化させたメッセージ組み立て** を
規定する。セッション内のリクエスト間で system prompt と会話履歴を
byte 同一に保つことで、LM Studio (および llama.cpp ベースのサーバ全般)
の KV cache prefix 再利用を発動させる。load-bearing な変更は、
temporal context (日時) を system prompt から外し、各レコードの
`Timestamp` フィールドから render する per-record prefix に移すこと。
実測 ([ベンチレポート](../history/llm-cache-bench-2026-05-20.ja.md))
では shell-agent-v2 の実プロンプトサイズで 2 ターン目以降に約 25 倍の
速度向上を確認している。

---

## 1. 問題

LM Studio の OpenAI 互換 `/v1/chat/completions` エンドポイント (および
裏側の `llama-server`) は、新リクエストと前回のキャッシュ済みデータの
最長 byte 同一 prefix について KV cache を再利用する。Prefix が
system block 全部一致すれば、サーバはそれらの token の再 tokenize +
再 attention を skip する — 5K token 規模のプロンプトで数秒の節約。

shell-agent-v2 の現状のメッセージ組み立てはこの再利用を破壊している。
`chat.BuildSystemPrompt` (`internal/chat/chat.go:128`) で system prompt
は以下の順に組み立てられる:

```
[base systemPrompt]
[+ System Rules block (存在時)]
[+ Current date and time: 2026-05-20 12:34:56.789]  ← 毎回再構築
[+ Yesterday: 2026-05-19]                            ← 毎回再構築
[+ Location: …]
[+ sandbox guidance (有効時)]
[+ Global Memory block]
[+ Session Memory block]
[+ Findings block]
```

この内 2 行が毎ターン変わる:

1. **Current date and time** — ミリ秒精度。毎リクエストで異なる。
2. **Yesterday** — 現在時刻から算出。日跨ぎでのみ変わる (稀)。

volatile な timestamp が system prompt の **中央** にあるため、byte
prefix は前リクエストとその行付近から異なる。それ以降 — sandbox
guidance、3 つの memory ブロック、system prompt の残り全部 — が毎ターン
再 tokenize + 再 attention される。system block の KV cache 再利用は
一切発動しない。

5K token 程度のプロンプトでは、実測
([llm-cache-bench-2026-05-20.ja.md](../history/llm-cache-bench-2026-05-20.ja.md)
T7) で **毎ターン約 6.5 秒の prompt-processing コスト** がかかる。
system prompt が安定なら (T8) 同じハードウェアで初回後 **約 250 ms** で
完了 — 25 倍の節約。

ユーザー体感: SEND を押してからアシスタントが返答を始めるまで、
普通の短い follow-up ターンでも 6-10 秒のスピナー表示。トークン生成
自体は速い。prompt processing が体感レイテンシを支配している。

---

## 2. ゴール

1. **セッション内の連続リクエストで system prompt が byte 安定** —
   同じ memory 状態 → 同じ bytes。 (memory 状態の変化は Phase 1 では
   スコープ外、§9 参照)。
2. **会話履歴が byte 安定** — 各履歴レコードは毎ターン同じ bytes に
   render される。
3. **per-turn の temporal context は依然としてモデルに届く** —
   「ユーザーは今 T 時に質問している」情報は prompt に残す必要あり
   (相対日付クエリ「昨日何があった?」を model が正しく解釈するため)。
4. **レコードフォーマットの変更なし、マイグレーションなし** —
   `chat.json` スキーマはそのまま。
5. **`cache_prompt` 等の拡張パラメータに外部依存しない** — ベンチで
   LM Studio が大規模 prefix に対して自動的にキャッシュすることを
   確認済。明示パラメータは不要、かつ他の OpenAI 互換サーバから拒否
   されるリスクあり。

ノンゴール:

- **Vertex AI Gemini 最適化。** Vertex は明示的 context caching API
  (per-call `cache_id`) と独自の料金体系を持つ。本 ADR ではスコープ外。
  cost / latency の状況次第で別案件化。
- **Memory ブロックの安定化** (Phase 2)。Memory ブロックは
  `extractMemories` が新事実を抽出したターンで変わる。先に効果を測定
  (§6) してから、memory を volatile 領域に move するか、抽出を間引くか
  決める。
- **Tool descriptor の安定性。** Tool list は sandbox 利用可化、データ
  ロード等で変わる。同一アクティビティ内のセッションでは概ね安定。
  後回し。
- **streaming vs 非 streaming。** streaming は ADR-0015 §1 で別の理由
  (Gemini Thought-leak) により無効化済。本 ADR では触らない。

---

## 3. 設計

### 3.1 temporal context の置き場所

`buildTemporalContext()` の出力を **system prompt から外し**、
**各レコードの保存済 `Timestamp` から message-build 時に render する
per-record prefix** に移す。

具体的には:

- `chat.BuildSystemPrompt` は `Current date and time` 行と
  `Yesterday` 行を出力しなくなる。
- 新しいヘルパ `chat.RenderRecordTemporalPrefix(ts time.Time) string`
  が、以前 system prompt にあった行を、`time.Now()` ではなく指定の
  timestamp から render して返す。
- `contextbuild` (メッセージアセンブラ) は各 `user` role レコードの
  content を以下のように render する:

  ```
  [Time: 2026-05-20 12:34:56 (Tuesday) (UTC+09:00) — Yesterday: 2026-05-19 (Monday)]

  <保存済 content>
  ```

  prefix は短く (~20 tokens)、人間可読、レコードの `Timestamp`
  フィールドから生成 — 再生時に byte deterministic。

`Timestamp` フィールドは `memory.Record` に既存で、レコード作成時に
populate される。新しい場所で読むだけ。

### 3.2 なぜ per-record アプローチ (「最新 user message のみに prepend」ではなく)

素朴な fix — 最新 user message のみにリクエスト時 prepend — は同じ
cache miss 問題を会話の 1 つ後ろの位置で再発させる。turn 2 の場合:

- Turn 1 が送信したのは `[system, "Time: T1\n\n" + u1]`。
- Turn 2 が送信するのは `[system, ??? + u1, "Time: T2\n\n" + u2]`。

turn 2 における履歴 `u1` に `"Time: T1\n\n"` prefix が欠けている場合
(もはや最新ではないため)、サーバキャッシュは system block 末まで hit、
そこから `u1` で miss。Prefix 再利用は現在の `BuildSystemPrompt` と
同じ場所で破綻する、ただし 1 レコード後ろにずれただけ。

temporal prefix を *レコード自身の Timestamp* から render —
**`time.Now()` ではなく** — すれば、すべての履歴 user message の
render はターン間で byte 安定。Cache 再利用は会話履歴の全長を通じて
延伸。今打った user message の新規 token のみが処理対象。

### 3.3 system prompt に残るもの

変更後の `BuildSystemPrompt` が出力するもの:

```
[base systemPrompt]
[+ System Rules block]
[+ Location: … (設定時)]               ← セッション内安定
[+ sandbox guidance (有効時)]          ← セッション内安定 (toggle 稀)
[+ Global Memory block]                ← extraction で変化 (Phase 2)
[+ Session Memory block]               ← extraction で変化 (Phase 2)
[+ Findings block]                     ← promote-finding で変化
```

これは **同一 memory 状態のリクエスト間で安定**。Phase 1 で memory を
system prompt に残す理由:

1. memory ブロックが実際ユーザーターン間でどれくらい変わるかを実機で
   まだ知らない (大半のターンで抽出は 0 件の可能性)。
2. memory を system prompt に置く配置は既存コードベースの確立された
   attention pattern — 動かすとモデルがそれらの fact を前景化する
   挙動が変わる。独立の計測パスが妥当。

§6 で Phase 2 判断のための計測を規定する。

### 3.4 render パス

```
agent.buildMessagesV2(ctx, budget)
  ├─ systemPrompt := chat.BuildSystemPrompt(...)
  │     (temporal 行なし)
  └─ contextbuild.Build(ctx, session, cache, BuildOptions{
       SystemPrompt: systemPrompt,
       RenderUserContent: func(rec memory.Record) string {
           return chat.RenderRecordTemporalPrefix(rec.Timestamp) +
                  "\n\n" + rec.Content
       },
       ...
     })
```

`contextbuild` はレコードを iterate して `llm.Message` 配列を生成済。
新しい `RenderUserContent` オプション (または既存 API の同等品) で
各 user role レコードの content を temporal prefix でラップする。
tool result と assistant レコードは未変更 (構造化フィールドや
ツール出力テキスト内に既にタイムスタンプを含む)。

### 3.5 トークンコスト

各 user レコードに ~20 token の prefix を追加するコスト:

- ~20 tokens × セッション内の user メッセージ数 N
- 50 ターンのセッション: 約 1000 余分なトークン
- 典型的なセッションコンテキスト (5K-50K tokens) との比較で 2% 未満
  のオーバーヘッド

25 倍の wall-clock 節約に対して無視可能。

---

## 4. エッジケース

1. **timestamp が空のレコード。** 防御的: renderer は `ts.IsZero()` を
   チェックし、その場合 prefix 行を skip。通常作成のレコードでは起こり
   得ないが、古いバンドルからの session import やテストフィクスチャで
   timestamp が欠ける可能性。

2. **非常に古い timestamp のレコード。** モデルは履歴 user message の
   前に「Time: T1 (数か月前の日付)」を見ることになる。意味的に正しい —
   ユーザーは実際にその時間に発言した。turn N で「昨日」と参照した時、
   turn-N レコードの prefix は現在時刻を反映するので、モデルは正しく
   resolve できる。

3. **tool-result content (role=tool)。** temporal prefix は追加しない。
   tool result は「ユーザー入力」ではないので、その時刻は会話の参照枠
   ではない。

4. **assistant content (role=assistant)。** temporal prefix なし。
   同じ理由。

5. **セッション中の System Rules 編集。** 一度きりの invalidation —
   system prompt が 1 回変わり、次のリクエストでキャッシュミス、その後
   はまたヒット。許容。

6. **ターン間に memory extraction が発火。** Phase 1 領域: extraction
   で新規 fact が追加されると、system prompt の Global / Session /
   Findings ブロックが数行成長する。サーバ側キャッシュはそのブロック
   までヒット、新 fact 行で miss。正味効果: base prompt + rules +
   location + sandbox guidance ではヒット、memory + history で miss。
   現状の「timestamp 以降全部 miss」よりは依然マシ。Phase 2 で残余の
   miss が実用上問題になるか測定する。

7. **セッション最初のターン。** 前のキャッシュなし、cold call。今と
   同じ。最適化は 2 ターン目以降から効く。

8. **セッション中の Profile switch。** `/profile <name>` 切替は
   backend client を再構築し、再接続でサーバ側状態がリセットされる。
   キャッシュ無効化の想定。許容 — `/profile` は稀な明示アクション。

9. **LM Studio 側のモデル swap。** ユーザーが LM Studio で load 中
   のモデルを変えるとキャッシュ無効化。本アプリの制御外。

10. **MLX 系 / sliding-window モデル。** ベンチ参照文献で、これらは
    prefix 安定にかかわらず silent に full re-process にフォールバック
    する。shell-agent-v2 の最適化は best-effort: サポート外モデル
    ファミリーでは高速化が見られないがレグレッションもなし。

---

## 5. 却下した代替案

### 5.1 リクエスト body に `cache_prompt: true` を送る

却下。ユーザーは過去に余分なパラメータがサーバから拒否された経験あり。
ベンチ T5 では現行 LM Studio はパラメータを受理するが、キャッシュは
どちらにせよ自動発動 (T1 vs T5 が同等)。パラメータ追加は zero-benefit、
non-zero-risk: 将来の LM Studio バージョンや別の OpenAI 互換サーバが
余分なフィールドで 4xx を返す可能性。

### 5.2 最新 user message のみに temporal を prepend

却下 (§3.2)。会話の 1 つ後ろの位置で cache miss 問題を再発させる。
per-record render が厳密に優れる。

### 5.3 最新 user message の直前に合成「コンテキスト」 system message
を挿入

却下。ほとんどの OpenAI 互換サーバは配列の先頭にひとつだけ system
message を受け付ける。複数 system message の挙動は実装依存。バック
エンド間の subtle なプロンプト解釈差異リスク。

### 5.4 レコード作成時に temporal context をレコード content に永続化

却下。表現 (render された temporal prefix) を保存 (record.Content) に
混ぜると、レコード形式の進化を困難にし、異なるモデルバックエンド向け
の render を変えるのも不可能になる。保存はクリーンに、render は
message-build 時に。

### 5.5 temporal context を完全に削除

却下。モデルは「今日が何か」を知って相対日付クエリを resolve する
必要が正当にある。system tools に `resolve-date` がある (複雑なケース
用) が、inline hint が「昨日」「明日」「曜日計算」等の一般ケースを
カバーする。

### 5.6 memory 出力を agent 層でキャッシュ

Phase 2 用に検討。アイデア: agent は memory 状態が変わっていなくても
毎ターン `FormatForPrompt` を re-render するので、同一状態でも微妙
に違う bytes (例: fact 順序のジッタ) を生む可能性。render 済文字列
をキャッシュし、extraction が実際の変更を signal した時のみ
re-render すれば、抽出が空振りしたターンでも byte 安定が保証される。
測定対象 (Phase 2)。

---

## 6. 計測 / 検証

Phase 1 のリリースには既存 2 つの計測ツールと新規テスト 1 つを伴う:

### 6.1 ベンチハーネス (既に tree 内)

`app/cmd/llm-cache-bench/` がコントロールされたシナリオで wall-clock
per request を計測。2026-05-20 の実行で Phase 1 仮説を検証済
([llm-cache-bench-2026-05-20.ja.md](../history/llm-cache-bench-2026-05-20.ja.md))。
実装後に再実行して、ハーネスと同じ挙動が本番コードパスでも出ることを
確認するべき。

### 6.2 Phase 2 memory 安定性テスト (実測済)

ベンチハーネスのシナリオ T9 で本 ADR 承認前に memory volatility の
残余コストを測定済。結果
([llm-cache-bench-2026-05-20.ja.md §T9](../history/llm-cache-bench-2026-05-20.ja.md#t9))
は、プロンプト残部がキャッシュヒットしている状態で **memory 1 件
変化あたり約 200ms のペナルティ**。典型的な抽出はターンあたり 0-3
fact 追加なので、ワーストケースで約 600ms — Phase 1 が解決する
6 秒の痛みより遥かに小さい。

これは下記 §9 に直接反映: **Phase 2 はスケジューリングではなく
defer 判断**。この規模では memory render キャッシングや memory
relocation の実装複雑度はペイしない。ユーザーが残余レイテンシを
報告するか、抽出が想定 (0-3 fact/turn) より多く発火するように
なった場合に再評価。

### 6.3 Go テスト (新規)

- `TestBuildSystemPrompt_NoTemporal` — 新 `BuildSystemPrompt` が
  出力する system prompt に `Current date and time:` 行も
  `Yesterday:` 行も含まれないことを確認。
- `TestRenderRecordTemporalPrefix_ByteStable` — 同じ `time.Time` を
  与えると renderer は同じ bytes を生成する。
- `TestContextbuild_TemporalPrefixOnUserRecords` — user / assistant /
  tool レコード混在のセッションを与え、user レコードのみが組み立て
  済メッセージ配列内で temporal prefix を持つことを確認。

### 6.4 本番テレメトリ (deferred — Phase 3)

`agent:activity` イベントに `prompt_processing_ms` フィールドを追加
すれば、実本番の高速化を watch できる。LM Studio は usage に
`cached_tokens` を返さない (実証済) ので、LLM round の wall-clock
計測に頼る。本 ADR 初回コミットのスコープ外 — ユーザー要求があれば
別 track。

---

## 7. 互換性

### 非破壊的

- `chat.json` レコード未変更。保存済レコードは natural content のみ
  持ち、temporal prefix は render 時。
- `BuildSystemPrompt` の呼び出し側は引き続き system prompt 文字列を
  受け取る — 型同じ、非キャッシュ用途では semantic 等価。
- Wails バインディング未変更。新イベントなし、新エンドポイントなし。

### 後方観察

v0.12.x で開いたセッションは v0.13.0 で開いたセッションとユーザー
視点で同じに見える。挙動の変化点は prefix caching をサポートする
ローカル LLM 上での高速ターンのみ。

### 前方観察

将来、別バックエンドの tokenizer 向けにレコード content を render
し分けたい場合、`RenderRecordTemporalPrefix` フックは clean に
一般化: `contextbuild.Build` に別 renderer を渡すだけ。

---

## 8. フェージング

コミット案:

1. `refactor(chat): factor out temporal context renderer`
   — 純粋なコード移動: `buildTemporalContext` 本体を新規
   `RenderRecordTemporalPrefix(ts time.Time) string` に抽出。
2. `feat(chat): remove temporal from BuildSystemPrompt`
   — `BuildSystemPrompt` から `Current date and time` /
   `Yesterday` 行を削除。system prompt は同一 memory ターン間で
   安定に。
3. `feat(contextbuild): inject temporal prefix into user records`
   — `BuildOptions` に per-user-record render フック追加。
   `contextbuild.Build` が各 user レコード content にそれを呼ぶ。
4. `feat(agent): wire temporal renderer into buildMessagesV2`
   — agent 層でステップ 1-3 を接続。
5. `test(chat,contextbuild): byte-stability invariants`
   — §6.3 の 3 つのテスト。
6. `docs: ADR-0017 status update + CHANGELOG v0.13.0 +
   architecture note`
   — ADR Status → Implemented; CHANGELOG エントリ;
   architecture.md に prefix-stability 原則の短い追記。

各コミットは独立にビルド + テスト可能。

---

## 9. スコープ外

- **Memory ブロックの volatility (Phase 2) — defer。** ベンチ計測
  (§6.2 / [T9](../history/llm-cache-bench-2026-05-20.ja.md)) で
  残余ペナルティは中程度 (~200ms / memory 変化 1 回) と判明したので、
  memory キャッシングや relocation の実装複雑度は現時点では正当化
  されない。ユーザーがレイテンシを surface してきた場合、または抽出
  が現在の ~0-3 fact/turn より多くなった場合に再評価。
- Vertex AI context caching API 統合。
- LM Studio `cache_prompt: true` 等の拡張パラメータ (§5.1)。
- Streaming レスポンス (ADR-0015 §1 の却下は引き続き有効)。
- Tool descriptor 順序の安定性。

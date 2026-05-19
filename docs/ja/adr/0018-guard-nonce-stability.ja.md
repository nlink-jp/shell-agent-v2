# Guard nonce 安定化による KV cache 再利用 — 設計ノート

**Status:** Proposed (2026-05-20)。
**Target:** v0.13.1 (パッチ — v0.13.0 リリース後に発見された
regression クラスの性能バグへの対処。ユーザー可視の修正で
スキーマ移行なし、Wails バインディング署名の変更なし)。
**Reported by:** ユーザー — 「v0.13.0 をローカル LM Studio で
試したが、最適化が効いているかはっきりわからなかった」。ログ
を確認したところ、新規セッションの run 1-3 で各ターン 27-28 s、
v0.13.0 前と全く変わらない挙動だった。

本ノートは、`nlk/guard` が untrusted な user / tool content を
ラップする時に使う guard nonce に対する **scan-and-rotate**
ポリシーを規定する。v0.13.0 では `BuildSystemPrompt` 呼び出しの
たびに nonce が回転していて、これが production で会話履歴の
KV cache 再利用を silent に無効化していた — ADR-0017 の成果を
production で潰していた。提案ポリシーはセッション内では nonce
を維持し、現在の nonce 文字列が untrusted な record content 内に
実際に出現した時だけ rotate する (nonce 漏洩から injection
attack が成立する厳密な条件)。

ベンチハーネス
([T10a / T10b / T10c](../history/llm-cache-bench-2026-05-20.ja.md))
により、本設計が ADR-0017 が目指していた ~93 % 速度向上を取り戻し、
かつ稀な rotate イベントでは 1 ターン分のコストで済むことを確認済。

---

## 1. 問題

ADR-0017 の `BuildSystemPrompt` 書き直しは per-call の volatile な
timestamp を system prompt から除去したので、セッション内の連続
リクエスト間で system block は byte 同一になっている。ベンチ T8 で
production プロンプトサイズで 96 % 高速化を確認した。

実 LM Studio での production テストでは全く高速化なし。ログ:

```
Turn 1: duration=28.0 s, prompt 14976 tokens
Turn 2: duration=27.3 s, prompt 15089 tokens
Turn 3: duration=27.9 s, prompt 15252 tokens
```

原因は `internal/chat/chat.go:138`:

```go
func (e *Engine) BuildSystemPrompt(...) string {
    e.guardTag = guard.NewTag()  // ← 毎コール回転
    ...
}
```

新タグの名前は cryptographic nonce (16 hex bytes:
`user_data_<32 chars>`)。後続の `contextbuild.renderRecordContent`
が各 user / tool record の content を
`<user_data_XXX>...</user_data_XXX>` でラップする。nonce が毎ターン
変わるので:

- Turn 1: user record #1 を `<user_data_AAA>hello</user_data_AAA>` で送信
- Turn 2: 同じ user record #1 を `<user_data_BBB>hello</user_data_BBB>` で送信

Turn 2 のフル prompt の byte prefix は Turn 1 のキャッシュから
system block 直後で diverge (system block は唯一の安定 byte)。KV
cache 再利用は会話履歴に延伸できない — 履歴が 15 K tokens で
prompt の大部分を占めている。

**ベンチでの証拠**
([report](../history/llm-cache-bench-2026-05-20.ja.md)):
- T10a (per-turn nonce、現行 production) → 1545 ms/turn flat、0%
  高速化。
- T10b (per-session-stable nonce、本 ADR の通常ケース) → 初回以降
  106 ms/turn、**93 % 高速化**。
- T10c (turn 3 で rotate-on-detection) → rotate ターン以外は
  cache-warm (~1500 ms の 1 ターン)、その後また cache-warm。

### なぜ per-turn rotation が最初から入っていたか

`nlk/guard` の godoc:

> A new Tag must be generated for every LLM call (turn).
> Reusing the same Tag across multiple turns is unsafe because
> a previous LLM response may echo the tag name, allowing
> prompt injection in subsequent turns.

攻撃シーケンス:

1. LLM が応答内にタグ名そのものを含める (例: `<user_data_AAA>` を
   prose で言及する)。
2. ユーザー (または、ユーザーが貼り付けるコンテンツの作者) が
   漏洩した nonce を見る — 応答内に直接、もしくはログ / ネットワーク
   経由で間接的に。
3. 攻撃者が次の user input に `</user_data_AAA>` を含めて、ラップを
   早期に閉じる。合成 close 以降の content が system 指示ゾーンに
   位置する。

Per-turn rotation は、turn N+1 のラップに使う nonce が turn N 時点で
未知であることを保証 — LLM が turn N の nonce を echo しても、攻撃者
は turn N+1 の nonce を予測できない。

防御として妥当だが、攻撃ウィンドウから見るとそのメカニズム (毎コール
回転) は overkill。

---

## 2. ゴール

1. **通常運用で会話履歴の KV cache prefix 再利用が発火する。**
   ローカル LLM ターンで ADR-0017 の 93 % wall-clock 節約を回復。
2. **prompt-injection 防御能力の実質的弱化なし。** Nonce は漏洩を
   見ていない攻撃者から依然予測不能であり、ラップは漏洩 nonce 攻撃に
   依然耐える。
3. **スキーマ移行なし、Wails バインディング署名の変更なし。** 修正は
   chat / contextbuild 配線の内部のみ。

ノンゴール:

- **guard wrap を別の防御メカニズムで置き換える。** スコープ外。wrap
  は確立されたパターンで機能している。
- **セッション間での nonce 再利用。** 各セッションが独自 nonce を
  持つ。プロセス全体 or グローバル静的 nonce は §5.2 で却下。
- **LLM 側の nonce echo をアシスタント応答から検知。** 決定的な瞬間
  は漏洩 nonce が untrusted (user / tool) content に実際に出現した
  時 — そこが唯一の攻撃 landing 地点。LLM echo を先に検知するのは
  eager rotation で安全性は同等以下。

---

## 3. 設計

### 3.1 脅威モデルの整理

攻撃は **2 つのイベントが両方発生した時のみ** 成功する:

1. LLM が現在の nonce 文字列を攻撃者が読めるコンテキストに echo する。
2. 攻撃者が同じ nonce 文字列 (典型的には `</user_data_XXX>` の形) を
   モデルにフィードバックされる untrusted content に置く。

どちらか単独では benign。(LLM echo 単独では nonce が漏れるだけで
injection しない; 予測不能な文字列を含む攻撃者テキストはラップを
escape できない。)

現行の per-turn rotation はイベント (2) を防御するために、(1) が前
ターンで起きていても次ターンの nonce を予測不能にする。だが同じ
結果は **イベント (2) を直接検知** することで達成できる — 使用直前に
untrusted content 内に現在の nonce を探し、その瞬間にだけ rotate する。

検知は precise (16-byte hex string は benign な user 散文では実用上
ゼロの false-positive)、回転ウィンドウは脅威が顕在化する瞬間そのもの。

### 3.2 アルゴリズム

```go
// chat.Engine (もしくは既存 guardTag 回転点をラップする形)
//
//   PrepareWrap(session *memory.Session) — 各 LLM round で
//   BuildSystemPrompt / contextbuild.Build の前に呼ぶ。
//
// 挙動:
//   1. 現在の guardTag が空 (セッション最初の build) なら、
//      新規生成して return。
//   2. 全 user / tool record の保存済 content をスキャンして、
//      現在の nonce 文字列があるか調べる (平文 substring、または
//      conservative に open / close タグ形)。
//   3. どれかの record が含むなら、新規 nonce 生成。
//   4. なければ現在の nonce を維持。
//
// 事後条件: 本 build で render される全 record が同じ nonce を
// 使う — 生存した方、または新規 rotate された方のどちらか。
// build 内で nonce が混在する出力は発生しない。
```

回転は **lazy**: 本当に必要な時だけ発火。clean なセッション (nonce
漏洩なし) では nonce はターンを跨いで維持され、prompt prefix は
byte 安定。

### 3.3 何をスキャンするか

guard tag は `user_data_a1b2c3d4e5f6...` のような名前を持つ。攻撃者
ペイロードは `</user_data_a1b2c3d4e5f6...>` でラップを escape する。
Conservative なスキャンは以下のいずれかをチェック:

1. `<` + name + `>` (open tag) — benign content に出現するのは
   astronomical に稀。
2. `</` + name + `>` (close tag) — 同上。
3. `name` (山括弧なしの substring) — 同上。

3 つすべて conservative にスキャン。コストは record あたり数 O(n) の
`strings.Contains` 呼び — tokenize や LLM round-trip と比べて無視可能。

### 3.4 どこをスキャンするか

Untrusted content は以下に存在:

- `Role == "user"` の `Record.Content` (タイプ入力、ペースト、
  D&D 添付がテキスト化されたもの)。
- `Role == "tool"` の `Record.Content` (web fetch や shell 結果等を
  含み得る tool 出力)。

以下はスキャンしない:

- assistant record — これは LLM の出力。モデルが nonce を echo した
  ならここに着地。echo 単独では rotate しない (それ自体は脅威でない)
  が、ユーザーがその assistant メッセージをコピペすれば次の user
  record の `Content` がスキャンで検知されて rotate する。
- システムプロンプト — 漏洩 nonce が memory block に焼き込まれる
  ケース (稀、攻撃者制御 user content の fact 抽出経由) はあり得る
  が、モデルがそれを読むことは attack surface ではない。次の user
  入力こそが surface。

### 3.5 build 内での rotate atomicity

`PrepareWrap` はスキャンを **build の頭で 1 度だけ** 実施、record の
render 前。rotate した場合、本 build 内の以降の
`WrapUserToolContent` 呼び出しはすべて新 nonce を使う。build の出力
は内部一貫 (nonce 混在なし)。

次の build (次 round / 次ターン) は `PrepareWrap` を再走。新コンテンツ
(例: tool 結果) が新 nonce を含んでいれば、もう 1 度 rotate が発火。
実用上きわめて稀。

### 3.6 配線箇所

```
agent.buildMessagesV2
  ├─ a.chat.PrepareWrap(a.session)        ← 新規
  ├─ systemPrompt := a.chat.BuildSystemPrompt(...)
  │     (内部で rotate しなくなる — PrepareWrap へ移動)
  └─ contextbuild.Build with WrapUserToolContent 不変
```

ミューテーションは `BuildSystemPrompt` から新 `PrepareWrap` に
移動する。`BuildSystemPrompt` は guard-tag 非依存になり、現在の
タグを使うだけになる。

### 3.7 既存の "shouldAnnotate" workaround への影響

`contextbuild/render.go:31-41` の `shouldAnnotate` コメントには
system block の temporal context に紐付いた別の gemma-2.5 historical-
event misread が記録されている。ADR-0017 で system prompt から
temporal context を除去したことでその条件は既に解消済。ADR-0018 の
nonce 安定化は再導入しない。

---

## 4. エッジケース

1. **セッション最初の build。** `e.guardTag` が空; `PrepareWrap` が
   初回 nonce を生成。現行の初回ターンと同じ。

2. **単一 SEND 内の multi-round agent loop。** 各 round が
   `buildMessagesV2` を呼ぶので `PrepareWrap` も各 round 走る。通常
   round 間で rotate は起きない (round 間で増えるのは tool 結果
   のみ; 異常事態でない限り tool 結果に nonce は含まれない)。
   単一 SEND 内の round はすべて end-to-end で cache 再利用。

3. **セッション中の `/profile` 切替。** agent は backend client を
   再構築; LM Studio のサーバ側状態は profile が同じ endpoint を
   指していれば維持される可能性も。手元の nonce は無関係 — サーバ
   キャッシュがリセットされていれば次の build は cold で当然。特別
   対応なし。

4. **セッション切替。** `LoadSession` は agent の `chat.Engine` (と
   それが持つ `guardTag`) をそのまま保持。新セッションのレコードは
   旧セッションの nonce とは無関係。次の build で PrepareWrap は
   現 nonce を保つ (新セッションでまだ漏洩なし) か、rotate するか
   どちらか。いずれにせよ正しい挙動。オプション: session load 時に
   tag をリセットして新セッションを cold スタートにする — nonce を
   per-session に保つ。推奨される isolation。コストは session 切替時
   の cold ターン 1 回。

5. **tool 結果が偶然 nonce 文字列を含む。** 稀だが可能 (例:
   `sandbox-run-shell` が以前のラップ済レコードを echo するケース)。
   PrepareWrap が次の build で検知して rotate。1 ターンの遅さの後
   通常に戻る。

6. **system_rules.md の編集に nonce が含まれる。** ユーザーが
   `system_rules.md` を編集。rules は system prompt の一部で、
   records ではない。PrepareWrap はスキャンしない。nonce が system
   prompt に入ることは「LLM への漏洩」だが attack surface (次の
   user 入力) ではない。後でユーザーが同じ文字列を user 入力に
   含めれば、その時 PrepareWrap が検知。

7. **PrepareWrap と並行 agent activity の race。** 各
   `buildMessagesV2` 呼び出しは agent loop 内で順次実行。並行性の
   懸念なし。

8. **partial nonce-like substring を多数含む長文 content。**
   `strings.Contains` は O(n); 200 KB tool 結果でもスキャンは
   サブミリ秒。問題なし。

---

## 5. 却下した代替案

### 5.1 per-turn rotation を維持し、cache 損失を受容

却下。production で ADR-0017 を完全に台無しにする。ベンチ T10a で
0 % 高速化、production ログで 15 K-token セッションの 27-28 s/ターン
を確認済。

### 5.2 プロセス全体の静的 nonce (グローバル)

却下。現状の shell-agent-v2 threat model で実世界の攻撃チェーンが
顕在化していないとしても、全ユーザー / 全時間の全セッションで単一
nonce を維持するのは不必要に広い範囲。セッションスコープの回転で
同じ cache 効果が得られ、isolation も良い。

### 5.3 一切 rotate しない (アプリ install ごとに単一 nonce)

却下、5.2 と同じ理由 + 「nonce が一度漏れたら永久回復不能」という
明らかな failure mode。

### 5.4 session ID から導出する deterministic nonce

検討。`HMAC(session_secret, session_id)` で計算可能、同じセッション
のリロードを跨いで安定。Pros: replay-safe; 各セッションが独自 nonce;
再現性。

却下 (本 ADR では) — 漏洩ケースに対応しない: 漏洩した nonce は
セッション寿命の間維持され、攻撃者は安定したターゲットを得る。
scan-and-rotate は漏洩を自然に処理する; ここに determinism を加える
安全性向上はゼロ。

将来の ADR で、強い再現性ニーズがあれば、検知時の手動 version bump
と組み合わせた `HMAC(session, version)` を取り上げ得る。

### 5.5 検知した LLM echo で rotate (eager rotation)

検討。各 assistant 応答後に現 nonce をスキャンし、見つかれば次の
user 入力処理前に proactively rotate。

却下: scan-and-rotate-on-untrusted-content アプローチ (本 ADR) は
同じ脅威を **決定的瞬間** で捕捉 — nonce が実際に攻撃者制御可能な
content に出現した時。echo に対する eager rotation は、ラップ構造
について model が説明する benign なターンを含めて、LLM が ラップ
構造に言及するすべてのターンで cache miss を加える。同等のセキュリティ
で strictly less performant。

### 5.6 平文ラップの代わりに cryptographic seal

検討。`<nonce>...</nonce>` を境界が HMAC や signature で detectable な
ラップに置換 — nonce 既知でも攻撃者は closing tag を forge できない。

却下 (本 ADR では) — はるかに大きいアーキテクチャ変更、LLM が seal
を認識するように学習されている必要があり、既存の nlk/guard 実装
との互換性が崩れる。検討するなら別 ADR。

---

## 6. テスト / 計測

### 6.1 ベンチ検証 (実施済)

`docs/en/history/llm-cache-bench-2026-05-20.md` の T10a / T10b /
T10c が本 ADR 承認前に本設計の効果を定量化済。実装には追加の
ベンチシナリオ不要。

### 6.2 Go テスト (新規)

- `TestPrepareWrap_FirstCallGeneratesTag` — 空 engine は初回呼び出し
  で新規 tag 生成。
- `TestPrepareWrap_NoLeakKeepsTag` — nonce substring を含まない
  records を持つセッションで連続呼び出しは同じ tag を維持。
- `TestPrepareWrap_LeakInUserContentRotates` — 現タグ名を content に
  含む user record が rotation を発火。新 tag は別物。
- `TestPrepareWrap_LeakInToolContentRotates` — tool records で同上。
- `TestPrepareWrap_LeakInAssistantIgnored` — タグを含む assistant
  content 単独では rotation を発火しない (§3.4)。
- `TestPrepareWrap_ConservativeMatchForms` — `<name>`、`</name>`、
  裸の `name` を含む content いずれも rotation を発火。
- `TestPrepareWrap_ByteStableNormalCase` — 同じセッション (漏洩なし)
  で N 連続呼び出し、`WrapUserToolContent("hello")` の render 出力が
  byte 同一であること。
- `TestPrepareWrap_LoadSessionRotates` — `LoadSession` でセッション
  切替時に tag リセット (defensive isolation)。

### 6.3 production 検証

v0.13.1 リリース後、v0.13.0 regression レポートのローカル LLM
smoke テストを反復:

- ローカル profile で新規セッションを開く。
- substantive な 3 ターン。
- `app/log` で `llm: local.Chat done duration=...` 行を確認。
- 期待: turn 1 は ~10-30 s (cold)、turns 2-3 は十分に 10 s 未満
  (cache 発火)。

turns 2-3 が cold 同等のまま残るなら、deploy 済コードが正しいもので
ない — 設計でなく build artefact を確認。

---

## 7. 互換性

### 非破壊的

- `chat.json` レコード未変更。
- `BuildSystemPrompt` の文字列出力は通常ケースで byte 同一。
- Wails バインディング署名未変更。
- `WrapUserToolContent` 外部 API 未変更。

### 後方観察

v0.13.0 で開いたセッションは v0.13.1 で開いたセッションとユーザー
視点で同じに見える。挙動の変化点はローカル LLM ターンが大幅に
高速化することのみ。

### 前方観察

`PrepareWrap` フックは将来の nonce 戦略 (例: ADR-0018 §5.4 の
deterministic nonce など欲しければ) の clean な拡張ポイント —
`BuildSystemPrompt` やラップ実装を触らずに済む。

---

## 8. フェージング

単一 PR。コミット案:

1. `feat(chat): PrepareWrap scan-and-rotate guard nonce`
   — `Engine.PrepareWrap(session *memory.Session)` を追加、
   `BuildSystemPrompt` から `e.guardTag = guard.NewTag()` 行を
   削除。self-contained な chat-package 変更。
2. `feat(agent): call PrepareWrap before buildMessagesV2`
   — 新フックを `agent.buildMessagesV2` と (defensive isolation
   のため) `agent.LoadSession` に配線。
3. `test(chat): PrepareWrap byte-stability + leak-detection invariants`
   — §6.2 の 8 つのテストケース。
4. `docs: ADR-0018 status update + CHANGELOG v0.13.1 + bench report cross-link`
   — ADR Status → Implemented; CHANGELOG エントリ。

各コミットは独立にビルド + テスト可能。

---

## 9. スコープ外

- 平文 XML ラップに対する cryptographic-seal 代替案 (§5.6) —
  検討したければ別 ADR。
- rotation イベント周りの telemetry / observability (スキャンで
  rotate した時に `bg-task` か `agent:activity` イベントを emit)。
  将来のチューニングに有用だが、本 fix には不要。
- `nlk/guard` パッケージの godoc 保証と本 consumer の厳密な
  scan-and-rotate 挙動の整合性 audit。per-turn rotation についての
  godoc 警告は、漏洩検知を自前でやらない呼び出し側にとっては正確な
  まま。本 ADR は shell-agent-v2 専用の代替案を文書化している。

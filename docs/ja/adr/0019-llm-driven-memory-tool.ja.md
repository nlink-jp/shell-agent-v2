# ADR-0019: LLM 主導の記憶ツール + 抽出トグル

- Status: Implemented in v0.13.2 (2026-05-20)
- Deciders: magi
- 関連: ADR-0015 (遅延抽出), ADR-0017 (prompt prefix 安定), ADR-0018 (guard nonce 安定)

## 1. 背景

ローカル LM Studio (gemma-4-26b-a4b) の ~15 K トークンプロンプトで、
ADR-0017 と ADR-0018 で prompt の byte 安定化を行った後も、ターン毎
~28 秒の応答時間が改善しなかった。LM Studio に直接 curl で同一 body
を 2 回投げると、cold 26.9 秒 → warm 0.44 秒 (98 % 高速化) と、
キャッシュ自体は完璧に効いていることが確認できた。

`SHELL_AGENT_DEBUG_LLM=1` のリクエストダンプと追加 curl 実験で
決定的な証拠が得られた:

| ステップ | Body | 経過時間 |
|---|---|---:|
| A — production body | req1.json (15 K) | 0.70 s |
| B — 小さな無関係リクエスト (extraction 模倣) | extract.json (~150 tok) | 0.81 s |
| C — production body 再投入 | req1.json (15 K) | 25.90 s |

ステップ A でキャッシュ温まったにもかかわらず、C で完全 cold に
戻った。原因はステップ B が llama.cpp の **唯一の prefix-cache スロット
を上書き** したこと。実セッションでは shell-agent-v2 は毎ターン後に
自動抽出 LLM call を実行する (ADR-0015 で応答経路から切り離した遅延
抽出) — これがステップ B と同じ役割を果たし、ターン間で会話 prefix
を毎回 evict する。ADR-0017 と ADR-0018 は prompt prefix から volatility
を排除したが、保護しようとしたキャッシュが直後の LLM call で wipe
されていた。production の turn-2 レイテンシが cold run と同じになる
のはそのため。

クラウド backend (Vertex AI) ではこの問題は起きない: KV キャッシュは
プロジェクト/モデル単位の共有 + リクエストストリーム毎に分離されて
おり、同一モデルへの補助 call が main セッションのキャッシュを evict
しない。これは **ローカル固有の問題**。

## 2. 決定

連動する 2 つの変更:

**(a) Backend 毎の自動抽出トグル。** `AutoExtractEnabled` を
`LocalConfig` と `VertexAIConfig` に追加。デフォルトは
`local=false`, `vertex=true`。無効時、`postResponseTasks` は
`extractMemories` call を完全にスキップ (goroutine も
`agent:extraction:*` イベントも出さない)。ADR-0015 の遅延抽出
送信キューは「待つべき in-flight 抽出が無い」ため自然に no-op に。

**(b) ビルトイン `remember-fact` ツール。** `builtinDescriptors()`
に新規追加。アシスタントがターン内で明示的に事実をメモリに保存できる
ようにし、自動抽出器の役割を代替する。スキーマは既存の抽出分類体系
をそのまま使うので、`session/global memory` 側のルーティングロジックを
無改修で再利用できる:

```json
{
  "name": "remember-fact",
  "description": "Save a fact about the user to memory ...",
  "parameters": {
    "fact":     { "type": "string", "description": "Concise statement" },
    "category": { "type": "string", "enum": ["preference","decision","fact","context"] }
  }
}
```

ルーティング: `preference` / `decision` → `GlobalMemory`, `fact` /
`context` → `SessionMemory` (`extractMemories` と同一)。
`IsSelfReferential` フィルタも同じく適用し、THINK 漏れ系の事実を防御。

**(c) 排他制御。** `AutoExtractEnabled = true` の時、`remember-fact`
ビルトインは LLM 提示用ツールリストから **省く**。両者は同じニーズ
(事実をメモリにルートする) を満たすので、両提示は重複リスクと prompt
肥大を招く。自動抽出器のプロンプトは recall 重視、tool 側は
アシスタント判断重視でチューニング方針も異なる; 混在は両方を曇らせる。

排他は descriptor 構築時 (`builtinDescriptors()`) で行い、runtime
ゲートにはしない。ユーザーは Settings の既存「ツール毎 toggle」で
ビルトインを手動 off にすることは引き続き可能。

## 3. 設計詳細

### 3.1 Config スキーマ追加

```go
type LocalConfig struct {
    // ... 既存フィールド ...

    // AutoExtractEnabled は毎ターン後の自動抽出 LLM call (ADR-0015)
    // を gate する。ローカル backend の default は false: 抽出 call
    // が llama.cpp の唯一の prefix-KV-cache スロットを evict し、
    // 次ターンが履歴全体の cold 再エンコードを強いられるため
    // (ADR-0019 §1 参照)。latency より recall を優先したいユーザーは
    // opt-in 可能。
    AutoExtractEnabled bool `json:"auto_extract_enabled"`
}

type VertexAIConfig struct {
    // ... 既存フィールド ...

    // AutoExtractEnabled — LocalConfig 参照。Vertex の default は
    // true: サーバー側 KV キャッシュはリクエストストリーム毎で、
    // 補助 call で evict されないため。
    AutoExtractEnabled bool `json:"auto_extract_enabled"`
}
```

`config.Load` でディスク JSON にフィールドが無い時に default を適用。
`bool` のゼロ値は `false` なのでローカル default は無料だが、Vertex
セクションは loader 側で明示的に `if !present { ... = true }` を
入れる必要がある。

### 3.2 Agent への統合

`Agent.postResponseTasks` (agent.go:2123) で:

```go
extractFn := a.extractMemoriesOverride
if extractFn == nil {
    if !a.autoExtractEnabled() {
        return // 完全 skip; goroutine もイベントも出さない
    }
    extractFn = a.extractMemories
}
```

`autoExtractEnabled()` は **アクティブプロファイル** (ADR-0016) の
backend セクションからフラグを解決するので、プロファイル単位の
オーバーライドが正しく効く。会話途中で local → Vertex に切り替えた
セッションは、セッション開始時ではなく切替時点で挙動が切り替わる。

### 3.3 ビルトインツール実装

`builtinDescriptors()` に `remember-fact` を追加。ただし
`!autoExtractEnabled()` の時のみ。ハンドラ:

1. `fact` (必須、trim 後非空) と `category` (必須、
   `ValidExtractionCategories` 内) をパース。
2. `IsSelfReferential` フィルタ適用 — マッチしたら tool error で
   返す。LLM がフィードバックを受けて、正当なユーザー事実が誤分類
   されていた場合は言い換えできる。
3. `GlobalMemory` (preference/decision) または `SessionMemory`
   (fact/context) にルート。`extractMemories` と同じ writer を使用。
4. 保存 ID を含む 1 行成功メッセージを返す。後続ターンで LLM が更新
   参照できるように。

System prompt ガイダンス (既存ビルトインドキュメントブロック末尾に
追加):

```
ユーザーが安定した好みを表明したとき、明示的な判断を下したとき、
または後のセッションで意味を持つ自分自身に関する事実を共有した
ときは、remember-fact ツールを使って永続化すること。一時的な
コンテキスト、中間推論、会話履歴から明らかな情報には使わないこと。
1 セッションあたり数回までを目安に。
```

### 3.4 Settings UI

プロファイルエディタの backend セクション毎に 1 行追加:

> **毎ターン後に記憶を自動抽出する**  (トグル)
> *ローカル default: off。有効時、アシスタントは `remember-fact`
> ツールを使えない (自動抽出器が代わりに処理する)。*

排他のコピー文は重要 — ユーザーがこの toggle で tool が隠れる理由を
理解できるように。

## 4. 検討した代替案

- **2 個目の LM Studio インスタンスで抽出実行**。キャッシュ evict
  は解消するがモデル RAM が倍 + セットアップ複雑化。default として
  却下; ハードウェア予算のあるユーザーは既存の secondary profile を
  別エンドポイントに向けることで自分で実現可能。
- **main backend に関わらず常に小型クラウドモデル (Gemini Flash)
  で抽出**。外部依存追加 + ローカルユーザーの「デフォルトで
  プライバシー」ストーリーが崩れる。却下。
- **セッションクローズ時にまとめて抽出**。会話中のキャッシュは保護
  できるが、Wails アプリ強制終了やクラッシュで未クローズ終了した
  セッションのクロスセッション事実が失われる。将来の ADR 候補として
  残置、本 ADR スコープ外。
- **自動抽出を local on のまま保持し、抽出後に prompt cache を
  再 prime する workaround**。脆弱 + 毎ターン ~5 K トークンの捨て
  処理が増える。却下。

## 5. 影響

**ポジティブ:**

- ローカルの turn-2 以降のレイテンシが warm prompt で ~28 s →
  数 100 ms に低下 (見積もり: §1 curl 実験で示した 98 % 高速化が
  ADR-0017 §3 のメカニズムで再現)。
- LLM 主導の記憶は会話トランスクリプトから監査可能 — 保存された
  すべての事実に対して可視 tool call がある。バックグラウンド自動
  抽出器の判断はメモリストアでしか見えなかったのと対照的。
- 「2 つの抽出経路 + 明示的排他」は「1 つの経路 + モードフラグ」
  より単純。

**ネガティブ:**

- 自動抽出器が拾えた事実を LLM が `remember-fact` 呼び忘れる可能性。
  System prompt ガイダンスで緩和 + recall を latency より重視する
  ユーザーは toggle on で従来挙動を維持できる。
- LLM が `remember-fact` を過剰呼び出しする可能性 (gemma 系は
  tool call 積極的)。v0.13.2 では rate limit を入れない — ストレージ
  コストは低く、過剰保存は UI で削除する方が、保存漏れを後から復旧
  するより容易だから。production ログで病的な呼び出し回数が観測
  された場合のみ再検討。
- backend 間で挙動がユーザー可視に異なる。Settings コピー文と
  CHANGELOG で明示する必要あり。

## 6. 実装計画

1. Config スキーマ + loader default (Commit 1)
2. `postResponseTasks` の `extractMemories` call を gate (Commit 2)
3. `remember-fact` ビルトイン + ハンドラ + descriptor gating
   (Commit 3)
4. Settings UI トグル、両プロファイル (Commit 4)
5. テスト: 抽出 off 経路、descriptor 排他、tool handler ルーティング
   + self-ref フィルタ (Commit 5)
6. CHANGELOG v0.13.2、ADR Status → Implemented、JA ミラー、
   docs-mirror-check (Commit 6)

検証: §1 の curl 実験を local プロファイルの GUI 経由で再現し、
同じ 15 K プロンプトの turn-2 レイテンシが秒未満になることを確認。
Vertex プロファイル回帰チェック: 事実を含む 3 ターン会話で、自動
抽出された record が引き続き保存されることを確認。

# ADR-0020: タイトル生成トグル (ヒューリスティック fallback)

- Status: Implemented in v0.13.2 (2026-05-20)
- Deciders: magi
- 関連: ADR-0015, ADR-0017, ADR-0018, ADR-0019

## 1. 背景

ADR-0019 で毎ターン後の自動抽出 LLM call を gate し、フォローアップの
tools-sort ホットフィックスで `tools` 配列を byte 安定化した結果、
ローカル LM Studio の turn-3 以降のレイテンシはようやく期待された秒未満
レンジに到達した (turn 4 round-1 → round-2: 29 s → 2 s、2026-05-20 実測)。
同じデバッグログに残る最後の問題は **turn 2**: 15 K トークンプロンプトで
依然 ~28 s の cold 再エンコード。

原因はタイトル生成 LLM call。`generateTitleIfNeeded` が
`postResponseTasks` 内のバックグラウンドタスクとして、turn 1 の応答配信
後に小さなプロンプト (`Generate a very short title…` + ユーザーの最初の
メッセージ) を同じ backend に送信し、`session.Title` に書き込む。自動
抽出と同じメカニズム: 単一の補助 LLM call が llama.cpp の唯一の prefix-
KV-cache スロットを evict するので、**次の** main turn (turn 2) が
全会話履歴の cold 再エンコードを強いられる。

影響は限定的 — タイトル生成はセッション毎に最大 1 回 — だが、これは
まさに新規セッションでユーザーが体感する turn 2 のレイテンシ。

## 2. 決定

ADR-0019 と同じパターンを踏襲: backend 毎のトグル + backend で defaults
を分け、LLM 経路が off の時は決定的ヒューリスティックに fallback。

**(a) `AutoTitleEnabled` フラグ。** `LocalConfig` と `VertexAIConfig`
に追加。defaults:
- Local: **off** (ヒューリスティック使用; llama.cpp ではキャッシュ保護が勝つ)
- Vertex: **on** (サーバー側キャッシュはリクエストストリーム毎で
  補助 call で evict されない — AutoExtractEnabled と同じ根拠、ADR-0019 §3.1)

**(b) off の時は fallback しない。** AutoTitle が off のとき、
`generateTitleIfNeeded` は単に return; `session.Title` は空のままで、
UI はセッションを untitled として表示 (現在も LLM call ウィンドウ中に
表示している transient state と同じ)。ユーザーは既存の Sessions list
右クリック → リネーム アクションでいつでも改名可能。

ヒューリスティックタイトル fallback は意図的に却下。トグルの動機は
「prefix キャッシュを守るために補助 LLM call を skip する」こと;
合成タイトルを捻り出すのは機能的利得なしに製品面を増やすだけ。
Untitled で十分 — UI は既に対応済み。

**(c) UI は AutoExtract と平行配置。** 各プロファイルの backend
セクション下に 2 個目のチェックボックス行を配置。同じ排他コピー文体
(ただしこちらは見た目だけで、隣接ツールを隠すわけではない)。
セッション一覧での手動リネーム機能は影響を受けず、どちらの経路で生成
されたタイトルでもオーバーライド可能。

## 3. 設計詳細

### 3.1 Config スキーマ

```go
type LocalConfig struct {
    // ... 既存フィールド ...
    AutoTitleEnabled *bool `json:"auto_title_enabled,omitempty"`
}

type VertexAIConfig struct {
    // ... 既存フィールド ...
    AutoTitleEnabled *bool `json:"auto_title_enabled,omitempty"`
}

const LocalAutoTitleDefault  = false  // ADR-0020 — キャッシュ優先
const VertexAutoTitleDefault = true   // サーバー側キャッシュは平気

func (c LocalConfig) AutoTitle() bool { ... }
func (c VertexAIConfig) AutoTitle() bool { ... }
```

`AutoExtractEnabled` と同じ `*bool` 表現で、ディスク JSON で
「ユーザーが明示的に false」と「フィールド未存在 → default 使用」を
区別。DTO (`LocalProfileFields` / `VertexProfileFields`) には対応する
`auto_title_enabled bool` を追加; bindings は常に `*bool` で書き込み、
ユーザー選択を永続化。

### 3.2 Agent への統合

`Agent.generateTitleIfNeeded` (postResponseTasks から起動される
goroutine) で:

```go
if a.session == nil || a.session.Title != "" {
    return nil
}
if !a.autoTitleEnabled() {
    return nil  // ADR-0020: タイトル空のまま; UI が untitled を扱う
}
// ... 既存の LLM 駆動経路 ...
```

`autoTitleEnabled()` はアクティブプロファイルのアクティブ backend から
フラグを解決、`autoExtractEnabled()` と同一パターン。

### 3.4 Settings UI

既存の `auto_extract_enabled` 行の直下に 2 個目のチェックボックス行を、
Local と Vertex の両セクションに追加:

> **セッションタイトルを自動生成する**  (トグル)
> *ローカル default: off。off の時、リネームするまでセッションは
> untitled のまま (1 セッションあたり 1 回の LLM call を節約)。*

ADR-0019 の行と同じ `.checkbox-row` スタイル、CSS 追加不要。

## 4. 検討した代替案

- **常にヒューリスティック** (トグルなし)。よりシンプルだが、Vertex
  ユーザーは LLM 生成タイトルを諦める動機がない (キャッシュ影響なし)。
  ユーザーが override できる、backend 毎 default で「正しいことを
  自動的にやる」設計に寄せて却下。
- **より小型 / 別モデルでタイトル生成**。1 回限りの装飾機能のために
  モデル管理面を増やす。却下。
- **session-close / idle 時にタイトル生成を遅延**。会話中のキャッシュは
  保護できるが、セッション一覧表示で空タイトルが出る + クラッシュで
  タイトルが永久消失。ヒューリスティックの方が厳密に勝る。
- **タイトル生成を turn 1 応答ストリーム並行で走らせ、turn 1 終了時の
  キャッシュ状態 == turn 2 開始時のキャッシュ状態 にする**。保証が
  難しい — 特に初回のコールド・モデルロード時にタイトル生成完了が
  ユーザーの turn 2 送信より遅れる可能性。脆弱として却下。

## 5. 影響

**ポジティブ:**

- ローカルの turn 2 レイテンシが ~28 s → 数 100 ms に低下 (見積もり:
  ADR-0019 事後測定で実証された turn-N round 遷移の 14 倍高速化と同じ
  prefix-cache メカニズム)。
- `auto_extract_enabled` × `auto_title_enabled` の結合なし — 独立した
  LLM call に対する独立したつまみ。メンタルモデル: 「ターン間の補助
  LLM call はすべてキャッシュを evict する; 諦められるものはフラグで
  切れ」。

**ネガティブ:**

- セッションは手動リネームするまで untitled のまま。セッション一覧の
  視覚スキャンに依存するパワーユーザーは local でその使い勝手を失う。
  緩和策: 手動リネームは常に利用可能; タイトル気になるユーザーは
  トグル on に。
- backend 毎にコンフィグフィールドが増える。プロファイルエディタが
  サイドあたり 2 個目のチェックボックス行を持つことに。ADR-0019 で
  既に確立したパターンの繰り返しなのでメカニカル、アーキテクチャ的な
  追加ではない。

## 6. 実装計画

1. Config スキーマ + loader default (ADR-0019 Commit 1 と平行)
2. `generateTitleIfNeeded` を AutoTitle で gate + DTO + Settings UI
   トグル
3. テスト: gate が LLM call を skip、DTO round-trip、default 値
4. CHANGELOG v0.13.2 追記 + ADR Status → Implemented + JA ミラー +
   docs-mirror-check

検証: ADR-0019 と同じデバッグログワークフロー。ローカルで 3 ターン
セッションを再生し、`Generate a very short title…` の JSONL エントリが
出ないこと + turn 2 レイテンシが秒未満になることを確認。Vertex プロファ
イル回帰確認: 3 ターンセッションは引き続きタイトル生成 LLM call を発火
させ、洗練されたタイトルを生成すること。

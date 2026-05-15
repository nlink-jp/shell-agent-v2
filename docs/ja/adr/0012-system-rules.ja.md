# System Rules — ユーザー定義のエージェント指示 — 設計ノート

**Status:** Design draft (2026-05-15); 承認待ち。
**Target:** v0.7.0 (v0.6.6 からのマイナーバンプ — 新規ユーザー向け
サブシステム + config schema 拡張、破壊的変更なし)。
**報告者:** ユーザー — Global Memory とは別に、全セッションにわたって
有効な「エージェントが従うべきルール」を設定として宣言的に定義
できる機能が欲しい。Claude Code 等の `AGENTS.md` / `CLAUDE.md`
スタイルのプロジェクトルールの精神。

このノートは新サブシステム **System Rules** を定義する: ユーザー
がオーサリングするシングルファイルの Markdown を、毎ターンごとに
system prompt の冒頭近くへ注入し、Settings パネル (または任意の
外部エディタ) から編集する。

---

## 1. Problem

shell-agent-v2 は既に 4 つのメモリ施設を持つ (Records、Session
Memory、Findings、Global Memory; ADR / memory-model 文書参照)。
ただし 4 つすべて *ランタイム中に学習* される性質のもの:

- Session Memory: 会話中にエージェントが抽出した fact/context
- Global Memory: ユーザーが明示的に promote した preference /
  decision 形状の項目
- Findings: データ分析の発見
- Records: 会話トランスクリプト

**ユーザーが事前にオーサリングする宣言的・恒久的な指示** —
全セッションでのエージェント挙動を *形作る* もの、Claude Code
における `AGENTS.md` / `CLAUDE.md` に相当するもの — を受け取る
仕組みは存在しない。現状の代替策はどれも歪んでいる:

1. 長文の指示テキストを Global Memory にピン留めする: しかし
   Global Memory のカテゴリ語彙は `preference` / `decision` で
   モデルは「ユーザーについて学習した事実」。「ユーザーから受け
   取った指示」とは方向が真逆で、promotion ロジックと UI 表示が
   混乱する。
2. ベース system prompt を編集する: しかしこれはコード内
   (`internal/chat/chat.go` の `defaultSystemPrompt`) でリビルド
   必須。
3. 毎セッションの最初のユーザーメッセージで繰り返す: 面倒で
   忘れやすい。

「ユーザーが、一度、恒久的に、エージェントは常にこう振る舞え、
と言った」を保持する仕組みが無い。

---

## 2. Goals

1. **グローバル 1 ファイル** を `<dataDir>/system_rules.md` に
   配置。ユーザーがオーサリングする。内容はフリーフォーム、
   スキーマなし、パース不要。
2. **Settings UI で編集** — 新セクション「System Rules」、
   textarea、文字数 / 推定トークン表示、Save ボタン。ファイル
   パスも表示し、外部エディタ運用も可能にする。
3. **system prompt の冒頭近くに注入** — base prompt 直後、
   temporal context の前。明示マーカーブロックで囲む。空ファイル
   → 注入なし (ヘッダもマーカーも出さない)。
4. **Save 時にホットリロード** — アプリ再起動不要。次のユーザー
   ターンから新ルールが適用。
5. **トークン予算を意識** — Settings UI に推定トークン数を表示。
   現役バックエンドの context budget の有意な割合を消費しそうな
   場合はソフトウォーニング。
6. **config migration を生き残る** — System Rules は `config.json`
   の外、専用ファイルに置く。将来の config schema 変更で破壊・
   消失しない。

非ゴール:

- **セッション単位オーバーライド**。将来「セッションプロファイル」
  施設を上に積める設計余地は残すが、v0.7.0 ではグローバルのみ。
- **複数ルールファイル / プロジェクト別ルール**。shell-agent-v2
  は GUI アプリで「プロジェクトディレクトリ」概念が無い。
  Claude Code の CLAUDE.md 探索パターンは綺麗に写像しない。
- **テンプレート / 変数展開**。プレーン Markdown のみ。
  `{{date}}` / `{{user}}` 置換は行わない。temporal context は
  別途 base system prompt が注入済み。
- **過去ルールのバージョン管理 / 履歴**。ファイルはユーザー所有。
  バージョン管理したければ自分の git に置く。
- **マシン間の同期**。ローカルファーストツールにおいて対象外。

---

## 3. Design

### 3.1 保存

- ファイル: `<dataDir>/system_rules.md`
  (macOS では
  `~/Library/Application Support/shell-agent-v2/system_rules.md`)。
- 形式: UTF-8 Markdown、frontmatter 無し、スキーマ無し。Save
  時に末尾改行を 1 個に正規化。
- エンコーディング安全性: ファイルはエージェントパイプライン
  内で opaque text として扱う。正規化は `strings.ReplaceAll(s,
  "\r\n", "\n")` と `strings.TrimRight(s, "\n") + "\n"` のみ。
- ファイル欠落: 空コンテンツと等価。エラーは表面化しない。
- アトミック書込: `internal/atomicio.WriteFileAtomic` (tmp +
  rename + 親ディレクトリ fsync)、他の全 JSON 状態ファイルと
  同じパターン。モード 0600。

### 3.2 新パッケージ `internal/sysrules/`

小規模・単一責任:

```go
package sysrules

type Store struct {
    path    string
    content string       // in-memory キャッシュ
}

func NewStore() *Store                        // path = config.SystemRulesPath()
func NewStoreAt(path string) *Store           // test helper
func (s *Store) Path() string
func (s *Store) Load() error                  // disk → キャッシュ; ファイル無 → 空
func (s *Store) Save(content string) error    // 正規化 + atomic 書込 + キャッシュ更新
func (s *Store) Get() string                  // キャッシュ取得
```

`Load` はアプリ起動時に 1 回呼ぶ。`Save` は Settings で Save が
押されたときバインディング経由で呼ばれる。Store は **内部
ミューテックスを持たない** — 全アクセスが `Agent` 経由で `a.mu`
により直列化される。既存 `internal/memory/global_memory.go` の
`GlobalMemoryStore` パターンに合わせる。

トークン推定は `internal/memory/tokens.go`
(`EstimateTokens(s string) int`) に既存。フロントエンドからは
直接呼ばずローカル近似で十分 (§3.6)。

### 3.3 `chat.Engine.BuildSystemPrompt` への注入

既存 3 メモリチャネル (`globalMemoryContext` /
`sessionMemoryContext` / `findingsContext`) は `BuildSystemPrompt`
に **パラメータ** として渡される設計で、`Engine` 構造体に
mutable field として保持されない。System Rules も同じパターンに
従い、4 つ目のパラメータとして追加する。Engine への mutable field
追加は **しない**。

これにより、turn goroutine が `a.mu` 非保持で
`BuildSystemPrompt` を呼ぶ既存パターンと、Settings goroutine が
`a.mu` 配下で field 書込する案との間で起こり得る field race を
構造的に排除する。`Engine` は入力に対する準・純関数モジュールに
留まり、mutable state は agent 層が所有する。

```go
func (e *Engine) BuildSystemPrompt(
    globalMemoryContext, sessionMemoryContext, findingsContext, systemRules string,
) string {
    e.guardTag = guard.NewTag()
    timeContext := buildTemporalContext()
    if e.location != "" {
        timeContext += "\nLocation: " + e.location
    }
    full := e.systemPrompt
    systemRules = strings.TrimSpace(systemRules)
    if systemRules != "" {
        full += "\n\n" +
            "The user has defined the following standing instructions. " +
            "Treat them as high-priority rules that override the default agent behaviour unless they conflict with safety or security guidelines.\n\n" +
            "<system_rules>\n" + systemRules + "\n</system_rules>"
    }
    full += "\n\n" + timeContext
    if e.sandboxEnabled {
        full += sandboxGuidance
    }
    // ... 既存の global / session / findings 注入はそのまま
    return full
}
```

マーカー (`<system_rules>` / `</system_rules>` エンベロープ) の
根拠:

- LLM が trust boundary をはっきり認識できる (ユーザーがオーサ
  リングした高優先度の指示、データではない)。
- デバッグ / ログレビュー時のレンダリングが安定。
- タグは固定 (ユーザーデータ guard のような nonce ローテーション
  はしない)。これは **意図的**: system rules はユーザーからの信頼
  済みコンテンツで、injection 防御が必要な untrusted data ではない。
  信頼方向が逆。

位置の根拠 (再掲):

- **base prompt の後**: base prompt がエージェントのコア
  アイデンティティ・ロール・ツール使用プロトコルを定義する。
  System Rules はその上に乗せるユーザー定義の *調整* なので、
  ロールが確立した直後・コンテキストデータが入る前に置くのが
  自然。
- **temporal context の前**: temporal context はデータ (「今日は
  …」)、指示ではない。ルールは指示と一緒に並べる。
- **sandbox guidance とメモリブロックの前**: これらはランタイム
  状態から派生するもの。ルールは安定。

これは `feedback_prompt_injection_position` (防御指示は冒頭) を
一般化したもの: *全ての信頼済み指示面* を *全てのデータ面* の
前に置く。

### 3.4 バインディング

Wails バインディング層 (`app/bindings.go`) に薄いデリゲート 2 つ:

```go
func (b *Bindings) GetSystemRules() string
func (b *Bindings) SetSystemRules(content string) error
```

両方とも agent を経由する (`agent.SystemRules()` /
`agent.SetSystemRules(content)`)。agent は `a.mu` 配下で Store を
操作する。ホットリロードは自動: 次ターンで `buildMessagesV2` が
`a.sysRules.Get()` を読み、その値が `BuildSystemPrompt` の 4 番目
引数として流れる。

`feedback_in_memory_disk_sync` ルール「per-session 状態は必ず
agent 層経由」の精神をここにも適用: bindings から
`sysrules.Store` 直叩きは禁止。

### 3.5 トークン推定

Settings UI は textarea フッタに「~N tokens」のライブ助言を
表示する。フロントエンドでローカル近似:
`Math.max(content.length/4, words*1.3)`。バックエンド
`memory.EstimateTokens` のアルゴリズム
(`feedback_token_estimation_json`: `max(chars/4, words×1.3)`)
と十分近く、キー入力ごとの RPC 往復を避ける。

### 3.6 Settings UI

`frontend/src/components/settings/` 配下に新セクション、既存の
memory / sandbox / tools セクションと並ぶ:

```
┌─ System Rules ─────────────────────────────────────┐
│ 全セッションでエージェントが従う恒久的な指示。      │
│ プレーン Markdown。保存先:                          │
│ ~/Library/Application Support/shell-agent-v2/     │
│   system_rules.md                                  │
│                                                    │
│ ┌──────────────────────────────────────────────┐  │
│ │ <textarea, monospace, ~12 行表示>            │  │
│ │                                              │  │
│ └──────────────────────────────────────────────┘  │
│                                                    │
│ 1,247 chars · ~310 tokens     ⚠ none / advisory   │
│                                                    │
│           [ Reload from disk ]  [ Save ]          │
└────────────────────────────────────────────────────┘
```

- **Reload from disk**: ファイルを再読込 (外部エディタで変更した
  あと有用; 編集中は textarea の状態が真実)。
- **Save**: ファイルを書き出してエンジンに伝播。
- **トークン助言**: 現役バックエンドの `MaxContextTokens` の
  `< 5%` で緑、`5%–20%` で黄、`> 20%` で赤。閾値は助言であり
  強制ではない — system rules はユーザー意図的なもので、長い
  ファイルの保存を拒否はしない。

textarea は v0.6.6 で文書化された ChatInput / Sidebar /
MITLDialog パターンと同じ IME composition ハンドリングを持つ。

### 3.7 マイグレーション無し

マイグレーション対象なし。v0.6.x には等価施設が無い。新規
インストールも v0.6.x からのアップグレードも、`system_rules.md`
不在 → 空ルールと等価 → ユーザーが内容を書くまで挙動変化なし。

---

## 4. テスト

### 4.1 `sysrules.Store` 単体テスト

- ファイル欠落時の `Load` は err nil、`Get` は `""`。
- `Save` → `Load` のラウンドトリップで内容保持 (CRLF → LF 正規化
  も検証)。
- `Save` のアトミック性: 読み取り専用ディレクトリ書込で書込中
  パニックを模擬し、ターゲットファイルが不変であることを検証。
- goroutine から `Save` + `Get` 並列実行で race detector に
  ひっかからないこと。
- 末尾改行正規化: 入力 `"a\nb"` → ファイル `"a\nb\n"`; 入力
  `"a\nb\n\n"` → ファイル `"a\nb\n"`。

### 4.2 `chat.BuildSystemPrompt` 注入

3 パラメトリックケース:

- 空ルール: prompt 中に `<system_rules>` マーカーも前置文も無し。
  現行挙動とバイト等価。
- 短いルール: マーカー有り、タグ間の内容が入力と一致、前置文
  存在、位置が base prompt の後・temporal context の前である
  こと (substring offset 順で assert)。
- 周囲に空白がついた複数段落ルール: `SetSystemRules` で
  TrimSpace されてから注入される (`<system_rules>` ブロック内に
  先頭・末尾改行が出ないこと)。

### 4.3 バインディングのラウンドトリップ

`bindings_test.go` — `SetSystemRules("…")` → `GetSystemRules()`
が同じ内容を返す。続く `BuildSystemPrompt` 呼出に新内容が含まれる
(agent 伝播経路を検証)。

### 4.4 構造テスト

新規 `internal/chat/chat_structural_test.go` (または既存構造
テストと同居) で順序不変条件を assert: ルール非空 prompt 中で、
`<system_rules>` の substring offset が base prompt 先頭行より
大きく、`Current date is` 行 (temporal context) より小さいこと。
将来の drift で注入位置がずれるのを機械的に防ぐガード。

### 4.5 トークン推定器の同等性

`sysrules.EstimateTokens` が共有コーパスに対して
`memory.EstimateTokens` と同値を返すこと (再エクスポートなので)。
sanity test を 1 個。

---

## 5. Risks

- **ルール内容経由の prompt-injection**. System Rules の内容は
  *信頼済み* — ユーザーから来たもの、データではない。
  `<system_rules>` エンベロープは clarity のためで防御では無い。
  悪意ある actor が disk のファイルを直接編集する状況は既に
  ファイルシステムレベルのアクセスがあり、この機能の脅威モデルの
  外。**緩和**: 「system rules は全幅信頼で読まれる」と文書で
  明示。
- **トークン予算枯渇**. 極端に長いルールファイルがローカル
  バックエンドの context window をほぼ使い切る可能性 (デフォルト
  `MaxContextTokens` 16,384)。**緩和**: Settings UI でパーセント
  助言を出す。silent truncation はしない (ユーザーが書いた指示を
  黙って切り詰めるのは驚き)。コストはユーザーが意図的に払う。
- **base system prompt または sandbox guidance との衝突**. ユーザー
  がベースプロンプトや sandbox guidance と矛盾するルールを書く
  可能性 (「ツールは決して使うな」「常に sandbox X を使え」)。
  **緩和**: 前置文で「override default behaviour *unless* they
  conflict with safety or security guidelines」と枠付けし、コア
  機能をブロックするようなルールは LLM が無視してよい余地を残す。
  ユーザーが自分の足を撃つ余地は受容する; これは opt-in 設定。
- **ターン進行中の Save によるホットリロード競合**. ターン実行中
  に Settings で Save しても prompt 組み立てとは race しない:
  `BuildSystemPrompt` は `systemRules` を関数引数で受け取る
  (ターン入口で agent から snapshot された文字列値)。共有の
  `Engine` フィールドではない。進行中ターンはその snapshot を
  使い続け、次ターンが新しい snapshot を取る。**Agent.mu が
  Store の読み書きを直列化していれば追加緩和は不要**。

---

## 6. 却下した代替案

### 6.1 `config.json` 内の string フィールドに保存

`Config.Agent.SystemRules string`。**却下**: JSON 内の長文 Markdown
は編集が辛い (エスケープ改行、ハイライト無し); 将来の config
schema 変更で破損リスク; AGENTS.md / CLAUDE.md のメンタル
モデルは「オーサリングするファイル」であって「config knob」では
無い。独立ファイルが正しい形。

### 6.2 Global Memory に専用カテゴリで pin

System rules を category `"rule"` の Global Memory エントリと
して扱う。**却下**: Global Memory の promotion ロジック・UI・
抽出パイプラインは「エージェントがユーザーについて学んだこと」
を前提にしている。ユーザーがオーサリングする恒久指示は方向が
逆 — 宣言的で、学習ではない。両方を 1 施設に押し込むとモデルと
UI が濁る。

### 6.3 プロジェクトディレクトリの `AGENTS.md` 自動探索

「現在のプロジェクトディレクトリ」から上に向かって `AGENTS.md`
を集めて連結する、Claude Code の `CLAUDE.md` 方式。**v0.7.0
では却下**: shell-agent-v2 は GUI チャットアプリで「現在のプロジェクト
ディレクトリ」概念が無い。これを足すこと自体がより大きな製品
判断 (マルチワークスペース、プロジェクト切替、ファイル継承
スコープ)。System Rules を単一グローバルファイルにすれば
価値の 95% を一握りの複雑度で取れる。後で重ねれば良い。

### 6.4 system prompt の末尾に注入

末尾配置が LLM 遵守度を上げると主張されることがある。**却下**:
ユーザー指示を sandbox guidance・メモリブロック・findings の
*後* に置くと構造の見通しが悪くなる (指示がデータをサンドイッチ
する形)、また `feedback_prompt_injection_position` の「指示が先、
データが後」原則と矛盾する。現代の Gemini / OpenAI 互換モデル
における遵守度差はこの種の standing-rules コンテンツではノイズ
範囲。

### 6.5 各セッションの最初のユーザーメッセージとして注入

ルールをユーザーターン偽装で先頭に入れる。**却下**: チャット
トランスクリプトを汚染、Records 履歴を食う、base prompt の
ロール設定と合成できない、履歴書換なしには取り消せない。
system-prompt 位置が自然な居場所。

### 6.6 v0.7.0 でセッション単位オーバーライド

セッション別ルールをグローバルの上に重ねる「セッションプロ
ファイル」施設。**v0.7.0 では却下**: 要求スコープ外、編集面が
2 つになる、「今何が有効か」のメンタルモデルが複雑化する。
需要があれば後から平易に追加可能 — Store は
`sessions/<id>/system_rules.md` を持ち、engine は連結し、UI は
セッション別タブを得る。

---

## 7. 互換性 & ロールアウト

- **永続化形式**: 加算的 — 新ファイル `system_rules.md` を
  `<dataDir>` に追加、既存 JSON 状態ファイルへの変更なし。
- **Config schema**: `config.json` に変更なし。Settings UI セク
  ションは専用バインディング経由で `sysrules.Store` と話す。
- **LLM 観測**: ファイルが存在し空でない場合、system prompt
  冒頭近くに `<system_rules>…</system_rules>` ブロックが入る。
  空 / 欠落 → v0.6.6 とバイト等価 (テスト 4.2 ケース 1 で
  検証)。
- **UI 観測**: 新 Settings セクション。チャットペイン・サイド
  バー・既存ダイアログには変更なし。
- **Import / Export**: セッション import/export はセッション内
  コンテンツ。System Rules はグローバル。bundle 形式の **対象外**、
  export しない。(後から opt-in フラグで bundle に含める拡張は
  可能だが意図的に後送り。)
- **ロールアウト**: v0.7.0 としてリリース。CHANGELOG に新サブ
  システムを利用例付きで明記。README と README.ja に System
  Rules の短いサブセクションを追加; 詳細リファレンスは
  `docs/{en,ja}/reference/system-rules.md`。

---

## 8. References

- ユーザー報告: 2026-05-15、「グローバルメモリとは別に、システム
  設定として Shell-Agent が従うべきルールのようなもの設定で
  定義できるような機能を追加したい (AGENTS.md みたいなかんじ)」。
- `feedback_prompt_injection_position` — 防御指示はプロンプト
  冒頭。「全ての信頼済み指示面を全てのデータ面の前」に一般化。
- `feedback_in_memory_disk_sync` — per-session 状態は必ず agent
  層経由、bindings → memory 直叩きは禁止。グローバル状態である
  engine `systemRules` フィールド経路にもこの原則を適用。
- `feedback_token_estimation_json` — word-count 単独では
  JSON / Markdown を 4-5 倍過小評価; `max(chars/4, words×1.3)`
  推定器を `internal/memory` から再利用。
- 既存メモリモデル: `docs/ja/reference/memory-model.md` —
  4 施設 (Records, Session Memory, Findings, Global Memory)。
  System Rules は 5 つ目のメモリ施設では **無い**; system prompt
  という同じ宛先に流れる *設定* 面である。文書ではこの分類を
  明示する。
- 影響箇所 (本 ADR で新規):
  - `app/internal/sysrules/` (新パッケージ — Store、内部 mutex 無し)
  - `app/internal/config/config.go` (`SystemRulesPath()` ヘルパ)
  - `app/internal/chat/chat.go` (`BuildSystemPrompt` に 4 番目
    引数 `systemRules` を追加; Engine の新フィールドは無し)
  - `app/internal/agent/agent.go` (`sysRules *sysrules.Store`
    フィールド、init 時 load、`SystemRules()` /
    `SetSystemRules()` メソッド、`buildMessagesV2` が snapshot を
    4 番目引数として渡す)
  - `app/bindings.go` (`GetSystemRules`, `SetSystemRules`)
  - `app/frontend/src/dialogs/SettingsDialog.tsx` (新 "rules" タブ)
  - `app/internal/chat/chat_test.go` (BuildSystemPrompt ケース)
  - `app/internal/sysrules/sysrules_test.go` (新規)
  - `app/bindings_test.go` (ラウンドトリップ)

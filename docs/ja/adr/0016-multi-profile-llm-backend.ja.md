# マルチプロファイル LLM バックエンド — 設計ノート

**Status:** Approved (2026-05-19)。
**Target:** v0.12.0 (マイナーバンプ — config スキーマ移行、新しい
セッションレベル永続状態、新しい `/profile` コマンド、新しい
Settings タブ。Wails API の破壊的削除はなし)。
**Reported by:** ユーザー — 「業務内容ごとに Vertex AI の課金を別の
GCP プロジェクトに振り分けたい。あとローカル LLM エンドポイントも
複数持ちたい (自宅と職場ノート PC)。現状の shell-agent-v2 は
`config.json` に `(Local, VertexAI)` の組を1つしか保持できず、
ファイル直編集なしに切り替える手段がない」。

本ノートは **profile-as-pair** モデルを規定する。*プロファイル* とは
`(LocalConfig, VertexAIConfig)` の名前付きペア + `default_backend`
フラグの束である。ユーザーは複数プロファイルを定義でき、各セッションは
ちょうど1つのプロファイルを参照し、`/model` はそのセッションの
プロファイル *内* で Local↔Vertex を切り替える形を維持する。
v0.11.x の「2 つのバックエンドは常に存在する」不変条件は維持され、
ユーザーが選んだバックエンドが一方的に消えるセッションが発生する
ことはない。

---

## 1. 問題

現状 (`app/internal/config/config.go:99-104`):

```go
type LLMConfig struct {
    DefaultBackend LLMBackend     `json:"default_backend"`
    Local          LocalConfig    `json:"local"`
    VertexAI       VertexAIConfig `json:"vertex_ai"`
}
```

設定全体で `Local` と `VertexAI` は各 1 つきり。これで実害が出ている
点が 2 つある:

1. **Vertex AI の課金 / プロジェクト分離不可能。** GCP プロジェクト A
   に課金すべき有償のクライアント案件と、個人プロジェクト B に
   課金すべき個人実験を同じユーザーがやる場合、セッションごとに
   `config.json` を手編集するしかない。セッションごとの課金帰属も
   ないし、「プロジェクト A を使う」というのをアプリ再起動後も生き残る
   セッションのプロパティにする手段もない。

2. **マルチエンドポイントのローカル LLM が表現できない。** 自宅 LM Studio
   (`localhost:1234`) と職場ノート PC (同僚のサーバー `10.0.0.5:8000`)
   が共存できない。コンテキストスイッチのたびに Settings を開かされる。

ユーザーの初手の発想 (「Local も VertexAI も独立に複数許せばいい」) は
v0.11.x の `/model` がペアを前提にしている点と衝突する。独立した複数
だと「どの Local?」が曖昧になり、さらに最悪、セッションが永続化した
バックエンドを config 編集で削除されてしまう可能性がある。

### より単純な案を却下した理由

- **`Local2` / `VertexAI2` フィールドの追加。** 2 つまでしかスケールせず、
  スキーマに「2」が刻まれる。
- **`Local` / `VertexAI` をそれぞれ独立した `[]Local` / `[]VertexAI` に置換。**
  v0.11.x の「Local と VertexAI が常に各 1 つ存在する」不変条件が崩壊。
  `/model` が曖昧 (「どの Local?」)。特定の Local をスペシフィックに
  参照するセッションが config 編集でその参照を失う。
- **バックエンド抽象を捨てて、各セッションがエンドポイント + プロジェクトの
  任意のタプルを直接持つ。** 現状コード (`llm.Backend`,
  `ContextBudgetFor(backend)`, バックエンドタイプごとの retry policy)
  が頼っている Local/VertexAI 二分法が崩れる。ユーザーの実需に対して
  リファクタが大き過ぎる。

---

## 2. ゴール

1. **複数の `(Local, VertexAI)` プロファイル。** ユーザーは Settings で
   N 個のプロファイルを定義でき、それぞれが固有の Local config と
   固有の VertexAI config を持つ。
2. **セッションごとのプロファイル参照を永続化。** 各セッションは作成
   時にどのプロファイルに対して作られたかを記憶する。アプリを閉じて
   開き直しても束縛は維持される。
3. **`/model` のセマンティクスは変えない。** `/model local` / `/model
   vertex` は、そのセッションのプロファイル内の Local と Vertex を
   プロセス内で切り替える。`/model` の永続化挙動は変えない。
4. **新しい `/profile <name>` コマンド。** 現セッションのプロファイル
   参照を切り替える。`session.json` に永続化される。切替後、次の
   `/model` は新プロファイルのペアに対して作用する。
5. **デフォルトプロファイルの不変条件。** ちょうど 1 つのプロファイルが
   デフォルト。UI から削除できない。新規セッションはデフォルト
   プロファイルが付与される。セッションが参照するプロファイルが
   削除されたら、デフォルトにフォールバックする。
6. **v0.11.x → v0.12.0 のクリーンな移行。** 既存の `config.json` は、
   従来の Local + VertexAI を保持する単一の「Default」プロファイルに
   自動アップグレード。`session.json` を持たない既存セッションは
   ロード時にデフォルトプロファイルへ解決される。
7. **セッション設定とチャットレコードの分離。** プロファイル束縛は
   `chat.json` に埋め込まずに、隣接する新規ファイル `session.json` に
   置く。会話トランスクリプトファイルは設定変更で書き換えない。
8. **GUI 上で発見可能なセッションコントロール。** 現セッションの
   プロファイルとアクティブバックエンドは常時ステータスバーに表示
   され、バッジクリックでインライン **Session Control Popover** が
   開き、チャットを離れずにプロファイル切替 + Local↔Vertex トグルが
   できる (Settings ダイアログを開いて戻る往復は不要)。パワー
   ユーザーは `/profile` / `/model` を続けて使い、カジュアル
   ユーザーはワンクリックで操作できる。

ノンゴール:

- **セッションごとの `active_backend` 永続化。** v0.11.x の `/model` は
  プロセスローカルの ephemeral な切替であり、本 ADR ではそれを維持。
  セッションは *プロファイル* を覚えるが、プロファイルのどちら側が
  現在アクティブかは覚えない。要望が出たら将来別 ADR で追加。
- **プロファイル単位の memory / sandbox / tools オーバーライド。** スコープ外。
  プロファイルは `(Local, VertexAI, default_backend)` のトリプルのみで、
  `Config` の残部はグローバル据え置き。
- **マシン間でのプロファイル import/export。** スコープ外。プロファイルは
  ローカル限定設定。バンドル export/import (`ExportSession`) はソース
  プロファイル参照を持ち運ばず、import 先マシンのデフォルトプロファイルに
  束ねる。
- **プロファイル単位のモデルリスト、モデル取得 UI 等。** スコープ外。
  プロファイルは現状の `Model` フィールド (各サイド 1 つ) のみを保持。

---

## 3. 設計

### 3.1 データモデル

`LLMConfig` のベタな `Local` / `VertexAI` を新しい `LLMProfile` 型に
置換:

```go
// LLMProfile は (Local, VertexAI) のペアと、このプロファイルに
// 対してセッションが最初にロードされた際に /model が着地するサイドを
// 表す default_backend を 1 つに束ねた単位。
type LLMProfile struct {
    ID             string         `json:"id"`              // UUID v4 (不変)
    Name           string         `json:"name"`            // 表示名 (可変、UI は一意を推奨するが強制はしない)
    DefaultBackend LLMBackend     `json:"default_backend"` // "local" | "vertex_ai"
    Local          LocalConfig    `json:"local"`
    VertexAI       VertexAIConfig `json:"vertex_ai"`
}

// LLMConfig はプロファイルのリスト + デフォルトポインタになる。
type LLMConfig struct {
    DefaultProfileID string       `json:"default_profile_id"` // Profiles の要素を参照すること
    Profiles         []LLMProfile `json:"profiles"`
}
```

ローダが強制する不変条件:
- `Load()` 後に `len(Profiles) >= 1` (空なら移行時に合成生成)。
- `DefaultProfileID` は既存プロファイルを参照する (ぶら下がっていれば
  ロード時に `Profiles[0].ID` に自動修復)。
- 各プロファイル内で `Local` も `VertexAI` も常に構造体として存在
  する (たとえ空 / 未設定でも)。v0.11.x の不変条件をそのまま継承。

### 3.2 session.json

セッションごとの新規ファイル、`chat.json` の隣:

```
~/Library/Application Support/shell-agent-v2/sessions/<id>/
├── chat.json              # 据え置き (records, title, private)
├── session.json           # NEW (本 ADR)
├── session_memory.json
├── findings.json
├── summaries.json
└── work/
```

`session.json` スキーマ (v1):

```json
{
  "schema_version": 1,
  "profile_id": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
}
```

これだけ。フィールド 2 つ。`title` も `private` も入れない (後方互換
のため `chat.json` のまま据え置き — 移動すると既存セッション全てが
リライト対象になり、本 ADR が明示的に避けたいケース)。
`active_backend` も入れない (§2 ノンゴール参照)。`profile_name_hint`
も入れない (デバッグ専用フィールドは load-bearing でないため却下)。

`schema_version` は前方互換のフック。v1 が本 ADR の唯一の定義。
将来フィールド追加時にバンプ予定。

I/O は atomic (tmp + rename + fsync)、`chat.json` (`memory.go:Save`)
と同じパターン。

### 3.3 セッションロード時のプロファイル解決

```
LoadSession(sessionID):
  1. chat.json を読む → Session struct (据え置き)
  2. session.json を読む:
     2a. 存在しない (v0.11.x セッション、または新規セッションで初回保存前)
         → profile_id = config.LLM.DefaultProfileID を採用
         → その profile_id で session.json を書き出し (遅延マイグレーション)
     2b. 存在 & パース OK → ファイル内の profile_id を採用
     2c. 存在するが破損 → warn ログ + デフォルトにフォールバック + 書き直し
  3. profile_id が config.LLM.Profiles 内にあるか検証:
     3a. ある → そのプロファイルを使う
     3b. ない (プロファイルが削除された) → info ログ + profile_id =
         DefaultProfileID にセット + session.json リライト (フォールバックを永続化)
  4. profile.DefaultBackend に従って agent.backend をインスタンス化
     (LocalConfig / VertexAIConfig は解決済プロファイルから取得)
```

(2a) の遅延マイグレーションにより、すべての v0.11.x セッションは
v0.12.0 で初めて開かれた際に `session.json` を書く。一括移行ステップ
不要、flag day もなし。後続のロードでは idempotent。

### 3.4 /model と /profile

`/model local` と `/model vertex` は v0.11.x の挙動を維持: `agent.backend`
のプロセスローカル切替。変化点は、それらがインスタンス化する新しい
`Local` / `VertexAI` config が、もはやトップレベルのペア (削除済) ではなく
現セッションの *プロファイル* のペアから来るという点だけ。

新しい `/profile <name>`:

```
/profile                  → プロファイル一覧、現在のは • マーク
/profile <name>           → 現セッションのプロファイルを <name> に切替
/profile <name> default   → <name> をグローバルデフォルトにも設定
```

プロファイル切替 (`/profile <name>`):
1. `<name>` を大文字小文字無視のマッチでプロファイル UUID に解決。
   防御的: 曖昧 (同名のプロファイルが 2 つ — Settings Save パスが
   自動 disambiguation するため (§3.5)、手編集された `config.json`
   経由でしか到達できない) ならエラー、部分 UUID プレフィックスを
   要求。
2. session.json を新 `profile_id` で更新。Atomic write。
3. `agent.backend` を新プロファイルの `default_backend` サイドに
   再インスタンス化 (`/model` トグルを新プロファイルのデフォルトに
   リセット — つまりセッション途中での `/profile` は `/model` より
   強い文ステートメントであり、ユーザーは新プロファイルが好む側に
   オプトインしている)。
4. `agent:profile:changed` Wails イベントを emit してステータスバー
   バッジを更新。

プロファイル切替は `state == StateBusy` 中や `extractionInFlight` 中は
許可しない (セッション切替と同じゲート — ADR-0015 §4.5)。流用機構:
`/profile` は `/model` と同じパスでチャット入力パーサが捌くので
既存の busy-state ガードが自然に効く。

### 3.5 Settings 経由のプロファイル CRUD

Settings ダイアログに新タブ「LLM Profiles」を追加し、既存の「Local」+
「Vertex AI」タブを置換:

```
┌─ LLM Profiles ────────────────────────────────────────────┐
│                                                           │
│  Profiles:                                                │
│    ● Default               (default)    [Edit] [Delete*]  │
│    ○ Production GCP                     [Edit] [Delete]   │
│    ○ Personal Lab                       [Edit] [Delete]   │
│                                                           │
│    [+ New profile from template ▼]                        │
│         ├─ Default (LM Studio + gemini-2.5-flash)         │
│         ├─ Clone of: Production GCP                       │
│         └─ Empty                                          │
│                                                           │
│  Selected: Production GCP                                 │
│  ──────────────────────────────────────────────────────── │
│  Name:            [Production GCP        ]                │
│  Default side:    ( ) Local  (•) Vertex AI                │
│  Set as default profile:  [ ]                             │
│                                                           │
│  ┌─ Local ─────────────────────────────────────────────┐  │
│  │ Endpoint, model, API key env, context budget, …    │  │
│  └────────────────────────────────────────────────────┘  │
│  ┌─ Vertex AI ────────────────────────────────────────┐  │
│  │ Project ID, region, model, context budget, …       │  │
│  └────────────────────────────────────────────────────┘  │
│                                                           │
│  [Cancel]                              [Save changes]     │
└───────────────────────────────────────────────────────────┘
```

\* デフォルトプロファイルの Delete ボタンは disabled (tooltip:
「これはデフォルトプロファイルです。先に別のプロファイルをデフォルトに
設定してください」)。別のプロファイルをデフォルトに設定すると、
旧デフォルトの Delete が再度 enable される。

Edit フロー: プロファイルフィールドはインプレースで編集; Save で
バリデート + `config.json` リライト (atomic、既存 `Save()`)。
メモリ内の他のセッションには影響なし — 次のセッションロード時に
変化が見える (プロファイル解決は LoadSession 時)。

**Name の自動 disambiguation。** ユーザーが Save (作成 or リネーム)
しようとした Name (大文字小文字無視) が他のプロファイルの Name と
衝突する場合、Save は通過するが、Name は ` (N)` を末尾に付加して
自動書換 — N は free な名前を生む最小の整数 (≥ 2)。macOS Finder の
重複ファイル名コピーと同じ規約。ブロックしない黄色インライントーストが
書換を通知: `「Production GCP」は既に使われているため「Production
GCP (2)」にリネームしました。`

アルゴリズム:
```go
// DisambiguateName は、desired が他のプロファイル (selfID で識別
// される自分自身は除く) の Name (大文字小文字無視) と衝突しなければ
// desired を返す。衝突するなら、衝突を解消する最小の整数 N ≥ 2 を
// 使って "<desired> (N)" を返す。macOS Finder の重複ファイル名
// suffix 規約と一致。
func DisambiguateName(profiles []LLMProfile, desired, selfID string) string
```

リネームはプロファイル UUID を保ったまま行われるため、リネーム後の
プロファイルを参照するすべてのセッションは suffix の有無にかかわらず
束縛を維持する。

これにより「同名プロファイル 2 つ」というケースは通常 UI 経路では
到達不能になる。§3.4 の `/profile <name>` の ambiguity 分岐は、
ユーザーが `config.json` を手編集して重複を作った時にだけ発火する
防御コードに格下げされる。

Delete フロー: 確認ダイアログが「X セッションがこのプロファイルを
参照しています。次回ロード時にデフォルトにフォールバックします」と
警告。フォールバックは遅延 (§3.3 ステップ 3b) で、即時ではない —
すべてのセッションファイルを開いてリライトしたりはしない。各
セッションは次にロードされた時に自分の `session.json` をリライトする。

### 3.6 v0.11.x からの移行

`config.Load()` で、パース済 `LLMConfig` が旧形 (トップレベルの
`default_backend` / `local` / `vertex_ai` フィールドが存在、または
`profiles` 配列が欠落) なら、単一プロファイルを合成生成:

```go
LLMProfile{
    ID:             uuid.New().String(),
    Name:           "Default",
    DefaultBackend: old.DefaultBackend,
    Local:          old.Local,
    VertexAI:       old.VertexAI,
}
```

`LLMConfig.DefaultProfileID` をその UUID にセット。即座に永続化
(`config.json` リライト)。古いトップレベルフィールドは移行後、
オンディスク JSON から *削除* されるので、二重形式の曖昧さは残らない。
旧フィールドを `json:"-"` でインメモリ構造体に残す必要はない — Go の
デフォルト unmarshal は未知フィールドを暗黙に捨てるので、一発移行で
十分。

検出: 両形式をオプショナルとして持つ過渡構造体にパース:

```go
type migrationLLMConfig struct {
    // 新形式
    DefaultProfileID string       `json:"default_profile_id,omitempty"`
    Profiles         []LLMProfile `json:"profiles,omitempty"`
    // 旧形式
    DefaultBackend LLMBackend      `json:"default_backend,omitempty"`
    Local          *LocalConfig    `json:"local,omitempty"`
    VertexAI       *VertexAIConfig `json:"vertex_ai,omitempty"`
}
```

`len(Profiles) == 0 && (Local != nil || VertexAI != nil ||
DefaultBackend != "")` なら合成生成を実行。それ以外は新形式そのまま。

セッションは移行ステップ不要。`session.json` を持たない v0.11.x
セッションは初回ロード時に書かれる (§3.3 ステップ 2a)。指す先は
新規合成済「Default」プロファイル。

### 3.7 Export / Import

Export (`bindings.ExportSession`) は現状、`chat.json` +
`session_memory.json` + `findings.json` + `summaries.json` + `work/`
をバンドル。`session.json` をバンドルに追加するか? しない:

- プロファイル UUID はマシン間で安定しない。
- `profile_id` をエクスポートすると、インポート先で (1) バリデーション
  失敗、または (2) 同名だが別物のプロファイルへ暗黙束縛するリスク
  (後者の方が悪い)。
- プロファイルレベル config (エンドポイント、プロジェクト ID、
  API キー環境変数名) はマシン依存のローカルデータの典型例で、
  持ち運ばせるべきではない。

なので: export はバンドルから `session.json` を省略。import は
import 先マシンの現在の `DefaultProfileID` を指す `session.json` を
新規生成。v0.11.x バンドルを v0.12.0 へ import した時と同じ動作。

`ExportSession` にドキュメント注記を追加: 「セッションバンドルは
プロファイル非依存。Import するマシンが現在のデフォルトプロファイルに
束ねる」。

### 3.8 バックエンドインスタンス化のリファクタ

`agent.setBackend(LLMBackend)` (`agent.go:2388`) は現状 `Config.LLM.Local`
/ `Config.LLM.VertexAI` を直接読む。プロファイル束縛 config を受け
取る形に変更:

```go
// Before
func (a *Agent) setBackend(b LLMBackend) error {
    switch b {
    case BackendLocal:
        return a.useLocalBackend(a.config.LLM.Local)
    case BackendVertexAI:
        return a.useVertexAIBackend(a.config.LLM.VertexAI)
    }
}

// After
func (a *Agent) setBackend(b LLMBackend) error {
    profile, err := a.config.ResolveProfile(a.session.ProfileID)
    if err != nil { return err }
    switch b {
    case BackendLocal:
        return a.useLocalBackend(profile.Local)
    case BackendVertexAI:
        return a.useVertexAIBackend(profile.VertexAI)
    }
}
```

`Config.ResolveProfile(id)` はマッチするプロファイルを返す。id が
空 or 不明ならデフォルトを返す。

`Agent` 構造体には新フィールドを追加しない — profile_id は
セッションレベル状態に住む。`Agent.session` が信頼源になり、
`agent.session.ProfileID` は `session.json` から LoadSession 時に
セットされる (§3.3)。

### 3.9 Wails イベント

新イベント:

| Event | Payload | When |
|-------|---------|------|
| `agent:profile:changed` | `{profile_id, profile_name, default_backend}` | `/profile <name>` の後、`SwitchSessionProfile` の後、または `agent.session.ProfileID` と異なるプロファイルに解決されたセッションロードの後 |
| `agent:backend:changed` | `{backend}` | `/model` または `SwitchSessionBackend` で現プロファイル内のアクティブバックエンドが切り替わった後 |
| `config:profile:list_changed` | `{profile_ids: []string}` | Settings の Save がプロファイルを追加 / 削除 / リネームした後 |

既存イベントは変更なし。`agent:profile:changed` は、削除されたために
アクティブセッションがデフォルトへフォールバックした場合にも発火する。
`agent:backend:changed` はステータスバーのバックエンドバッジと Session
Control Popover のラジオ状態を駆動する。現状の `agent.backend` トグル
には専用イベントがなく、フロントエンドは `Bindings.GetState()` ポーリングで
状態を推定しているが、本 ADR では popover と将来のリスナー双方のために
明示イベントを追加する。

### 3.10 ステータスバー + Session Control Popover

ステータスバーは現セッションのプロファイルとバックエンドを横並びで
表示し、両方がインタラクティブ:

```
[Profile: Production GCP ▾] [Vertex AI · gemini-2.5-flash ▾]
```

どちらのバッジをクリックしてもステータスバー下にアンカーされた
インライン **Session Control Popover** が開く。重要な点として、これは
*Settings ダイアログそのものではなく*、頻繁に行う 2 操作 (プロファイル
切替 + Local↔Vertex トグル) を会話から離れずにできるためのチャット
ローカルなアフォーダンス。

モック (プロファイルバッジをクリックした状態):

```
┌─ Session Control ──────────────────────────────┐
│  Profile                                       │
│  [Production GCP                          ▾]   │
│    └ default side: Vertex AI                   │
│                                                │
│  Active backend (このセッション、ephemeral)    │
│  ( ) Local       — google/gemma-4-26b-a4b      │
│  (•) Vertex AI   — gemini-2.5-flash            │
│                                                │
│  [Edit profiles in Settings →]      [Close]    │
└────────────────────────────────────────────────┘
```

セマンティクス:

- **プロファイルドロップダウン。** 定義済全プロファイルを一覧。
  選択は `/profile <name>` と同等: `session.json` をアトミックに
  リライト、`agent:profile:changed` を emit、アクティブバックエンドを
  新プロファイルの `default_backend` サイドにリセット (§3.4 と
  同じ "stronger statement than /model" セマンティクス)。
- **Active backend ラジオ。** `/model local` / `/model vertex` と同等:
  現プロファイル内のエフェメラルなプロセス状態トグル。
  `session.json` には永続化されない (§2 ノンゴールと整合)。各
  オプション横のモデル名は profile の `Local.Model` / `VertexAI.Model`
  でここでは読み取り専用。
- **「Edit profiles in Settings →」リンク。** Settings ダイアログの
  LLM Profiles タブを開き、現プロファイルを選択状態にする。プロファイル
  CRUD の唯一の入口で、popover 自体はプロファイル内容を編集しない。
- **Close / アウトサイドクリック / Esc。** Popover を閉じる。

Busy-state ゲート: `state == StateBusy` か `extractionInFlight == true`
の間は両コントロールが disabled 描画 + tooltip「Busy — please wait.」
これは `/profile` / `/model` チャットコマンドと同じゲートを再利用
(§3.4)。

実装: 2 つの新 Wails バインディングを追加。両方とも `/profile` /
`/model` が使うのと同じハンドラへの薄いラッパーで、busy-state ガード、
イベント emit、副作用について単一の信頼源を維持:

```go
// Bindings:
func (b *Bindings) SwitchSessionProfile(profileID string) error
func (b *Bindings) SwitchSessionBackend(backend string) error
```

Popover は `Bindings.ListProfiles()` (read-only summary — Settings タブ
と同じバインディング) を呼んでプロファイル名を描画する。現選択は
`agent.session.ProfileID` と `agent.backend` から計算し、既存の
`agent:profile:changed` / `agent:backend:changed` イベントで更新。

設計根拠: ステータスバーバッジは「自分は何で動いているか」のパッシブ情報、
popover は「それを変える」アクティブサーフェス。同じアンカーと同じ
視覚語彙を共有することで、ユーザーは 2 つのアフォーダンスを学ぶ必要が
ない。

---

## 4. エッジケース

1. **同じ `Name` のプロファイル作成要求が 2 つ。** Settings Save パス
   が 2 つ目に ` (2)`, ` (3)`, … (free な最小整数) を自動 suffix
   付与 — macOS Finder の重複ファイル名規約と一致。ブロックしない
   黄色トーストでユーザーに「『Production GCP』が既に使われていた
   ため『Production GCP (2)』にリネームしました」と通知。プロファイル
   UUID は独立なので、セッション束縛には影響しない。§3.4 の
   `/profile <name>` ambiguity-error 分岐は、Settings フロー外で
   `config.json` を手編集して重複を作った時にだけ発火する防御コードに
   格下げされる。

2. **アクティブセッションのプロファイルを Settings から削除。** メモリ
   内のセッションは旧プロファイル config (`agent.useLocalBackend` /
   `useVertexAIBackend` のクロージャ状態にロード済) で動作継続。
   次回セッションロード (切替後 or アプリ再起動後) で §3.3 ステップ
   3b のフォールバックが効く。現在のチャットへの即時的影響なし。

3. **プロファイルのリネーム。** UUID は安定、`Name` のみ変更。参照
   しているすべてのセッションは引き続き正しく解決される。
   `config:profile:list_changed` イベントで UI がプロファイル名を
   再取得しバッジが更新される。

4. **引数なしの `/profile`。** 全プロファイル一覧 + 現セッションの
   プロファイルにマーク。状態変更なし。

5. **`/profile <存在しない名前>`。** エラーメッセージ、状態変更なし。

6. **`StateBusy` 中や extractionInFlight 中の `/profile`。** `ErrBusy`
   でリジェクト。`Agent.handleCommand` / `Agent.handleModelCommand`
   の既存 busy-state ガードを流用。

7. **現セッションが旧デフォルトを使用中に新しいデフォルトを設定。**
   現セッションへの影響なし (まだ旧デフォルトの UUID を参照中)。
   変更が効くのは:
   - 新規作成セッション (新デフォルトを取得)
   - 参照プロファイルが削除されたセッション (新デフォルトへフォールバック)

8. **最後の 1 個になったプロファイル。** 削除不可 (UI ゲート)。
   概念的に: 削除したら次の `/model` トグルは何に作用する? 最低
   1 つのプロファイルを強制。

9. **どのセッションも参照していないプロファイルの削除。** ストレート
   削除、フォールバック作業なし。

10. **N セッションが参照しているプロファイルの削除。** 確認:「N 個の
    セッションがこのプロファイルを参照しています。次回ロード時にデフォルト
    プロファイルを使用します。続行?」遅延フォールバック (§3.3 ステップ 3b)。

11. **v0.11.x の export バンドルを v0.12.0 へ import。** バンドルに
    `session.json` はない。Import は import 先マシンのデフォルト
    プロファイルを指す session.json を生成。上記 (2a) のケースと同じ。

12. **プロファイルが未設定の側を `/model` で指定 (例: VertexAI ProjectID 空)。**
    現状と同じ — バックエンド側の readiness check (`useVertexAIBackend`
    が空 ProjectID を拒否) がユーザーにエラーを surface する。
    プロファイル抽象は新規 failure mode を追加しない。

13. **Config save とセッションロードの並行。** セッションはユーザー
    アクションでロード (サイドバークリック)、Config save は Settings
    Save クリック。両方ユーザー駆動で実用上シリアル。防御的ロック:
    `config.LLM.Profiles` は `Load()` 返却後 read-only で、Settings
    Save が新たな `Load()` サイクルをトリガするまで変わらない。
    v0.11.x の既存パターン。新規ロック不要。

14. **破損した旧 config.json の移行。** `applyBackendInheritance()`
    が per-backend tokens を 0 にした場合、移行プロファイルはその 0
    を継承 — 現状と同じ挙動。移行は構造変換のみで、値の正規化はしない。

---

## 5. 却下した代替案

### 5.1 独立した `[]Local` + `[]VertexAI`

却下 (§1)。`/model` のペア不変条件が壊れる。特定の Local-by-id を
参照するセッションが config 編集で消える可能性。

### 5.2 `chat.json` に `profile_id` を埋め込む

却下。会話トランスクリプトと設定状態を混ぜる。既存 chat.json 全部が
フィールド追加対象になり、後方互換読み込みが複雑化。ユーザーは
セッションレベル設定を `session.json` として明示分離する案を選好
(本 ADR §3.2)。

### 5.3 `active_backend` (/model トグル) を `session.json` に永続化

検討。Pros: ユーザーが最後 Local/Vertex のどちらにいたか覚える;
3 日後に開いても同じサイドに着地。Cons: v0.11.x からの挙動変更で
`/model` が ephemeral でなくなる; ユーザーは session.json を最小
フィールドにと明示要望; `/model` 呼出ごとに書き込みが増える。
据え置き。将来 session.json schema_version を 2 にバンプして別 ADR で
追加可能。

### 5.4 memory / sandbox / tools オーバーライドもプロファイル化

検討。Pros: コンテキストごとの完全分離 (プロファイルごとに別 sandbox、
別 MCP profile、…)。Cons: スコープが大きすぎる、v0.11.x の Memory v2
グローバル設計と衝突、推論しづらい。ユーザーの明示需要は課金 +
エンドポイント分離で、それは LLM config プロファイリングのみで足りる。
無期限据え置き。

### 5.5 UUID なし安定文字列 ID (例: Name から生成した slug)

却下。リネームで参照が暗黙に切れる。UUID v4 は不透明 & 不変、
人間ラベルは `Name` で自由にリネーム可能、参照は壊れない。

### 5.6 サイドバードロップダウンとしてのプロファイル選択

GUI サーフェスの 3 案 (チャットコマンド、サイドバードロップダウン、
ステータスバー popover) の 1 つとして検討。ステータスバー popover
(§3.10) を選んだ理由は以下:
- サイドバーの本務はセッション一覧で、プロファイルドロップダウンは
  視覚的に競合し、「自分はどのセッションにいるか」と「このセッションは
  どの config で動いているか」を混同させる。
- Popover サーフェスは既存のバックエンドバッジと同所にあるため、
  「自分は何で動いているか」を見ようとしているユーザーが変更
  アフォーダンスをワンクリック先に発見できる。
- Popover サーフェスは将来 ADR の追加のセッション単位コントロール
  (例: セッションレベル sandbox トグル) に自然にスケールする —
  それぞれにサイドバースロットを与える必要がない。

`/profile` はパワーユーザー向けに `/model` の前例とマッチ; ステータスバー
popover はカジュアルユーザー向け。Settings ダイアログはプロファイルの
作成 / 編集 / 削除の入口で、これらは意図的に重い操作で popover に置く
べきでない。

---

## 6. テスト / 不変条件

### 6.1 バックエンド (Go)

**`internal/config/`**:
- `TestMigrate_V011LegacyShape_CreatesDefaultProfile` — 旧
  `{default_backend, local, vertex_ai}` JSON が合成 UUID + Name "Default"
  の 1 プロファイルに移行。
- `TestMigrate_V011LegacyShape_PreservesAllFields` — 移行後
  プロファイルの Local + VertexAI が旧値と deep-equal。
- `TestMigrate_AlreadyMigrated_NoChange` — `profiles[]` 既存の config が
  変更なしで通過。
- `TestMigrate_DanglingDefaultProfileID_RepairsToFirst` —
  `default_profile_id` が欠落 UUID を指す → `profiles[0].ID` に修復、
  警告ログ。
- `TestResolveProfile_KnownID_ReturnsProfile`
- `TestResolveProfile_UnknownID_ReturnsDefault`
- `TestResolveProfile_EmptyID_ReturnsDefault`
- `TestDisambiguateName_NoCollision_ReturnsDesired`
- `TestDisambiguateName_OneCollision_AppendsSuffix2`
- `TestDisambiguateName_ChainOfCollisions_FindsLowestFree` —
  既存名が `["Foo", "Foo (2)", "Foo (3)"]` の状態で "Foo" を要求
  → "Foo (4)"。
- `TestDisambiguateName_GapInChain_FillsLowestFree` — 既存名が
  `["Foo", "Foo (3)"]` ("Foo (2)" なし) の状態で "Foo" を要求 →
  "Foo (2)" (Finder 規約)。
- `TestDisambiguateName_CaseInsensitive` — 既存名が `["Foo"]` の
  状態で "foo" を要求 → "foo (2)"。
- `TestDisambiguateName_SelfIDExcluded` — 既存プロファイルの
  リネームで、現在の Name が要求 Name と一致する場合は no-op
  (desired をそのまま返す)。

**`internal/memory/`**:
- `TestSessionConfig_LoadMissing_ReturnsZero` — session.json なし →
  `SessionConfig{}` + エラーなしを返す (呼び出し側がデフォルトを補う)。
- `TestSessionConfig_LoadMalformed_ReturnsError` — 破損 JSON →
  エラーを surface、リカバリは呼び出し側で判断。
- `TestSessionConfig_SaveAtomic` — 並行 save でファイルが裂けない
  (`TestSession_Save_Atomic` と同じパターン)。

**`internal/agent/`**:
- `TestAgent_LoadSession_NoSessionJSON_CreatesDefault` — v0.11.x
  セッションを v0.12.0 で読み込み → デフォルトプロファイルを指す
  session.json が書かれる。
- `TestAgent_LoadSession_DeletedProfile_FallsBackToDefault` —
  session.json が config にない UUID を参照 → フォールバック +
  session.json リライト。
- `TestAgent_NewSession_GetsDefaultProfile` — `NewSession` /
  `NewPrivateSession` がデフォルトプロファイル付きセッションを生成。
- `TestAgent_ProfileCommand_Switch` — `/profile foo` が session.json
  を更新 + `agent:profile:changed` を emit + 新プロファイルの
  default サイドにバックエンドをリセット。
- `TestAgent_ProfileCommand_AmbiguousName_Errors`
- `TestAgent_ProfileCommand_UnknownName_Errors`
- `TestAgent_ProfileCommand_DuringBusy_Rejected`
- `TestAgent_ProfileCommand_DuringExtraction_Rejected`
- `TestAgent_SetBackend_UsesActiveProfilesPair` — `/profile B` の後
  の `/model vertex` が B の VertexAI を使う (A のではなく)。

**`bindings.go`**:
- `TestBindings_ExportSession_OmitsSessionJSON` — バンドルに
  session.json エントリなし。
- `TestBindings_ImportSession_AssignsDefaultProfile` — import 後の
  セッションの新規 session.json が現デフォルトを指す。

### 6.2 フロントエンド (手動スモーク)

- Settings → LLM Profiles タブを開く。「Clone of Default」で 2 つ目の
  プロファイル作成。VertexAI ProjectID を変更。Save。
- ステータスバーバッジにプロファイル名 + バックエンド + モデル表示。
- プロファイルバッジクリック → Session Control Popover 開く;
  プロファイルドロップダウンに両方表示; バックエンドラジオに現選択。
- Popover でドロップダウンから別プロファイルを選択 → ステータスバー
  即時更新; チャット入力は同じセッションのまま; 次回 LLM 呼び出しが
  新プロファイルの config を使う (`/model vertex` 経由 tool 呼び出しが
  新 ProjectID にヒットして検証)。
- Popover でバックエンドラジオ Local ↔ Vertex 切替 → `/model` と
  同じ効果; popover とステータスバー両方が変化反映。
- 「Edit profiles in Settings →」リンククリック → popover 閉じ、
  Settings ダイアログが LLM Profiles タブで現プロファイル選択状態で
  開く。
- Agent ターン中 (Busy or extractionInFlight) に popover を開く →
  コントロールが disabled で tooltip 表示; クリックブロック。
- Popover オープン中に Esc → popover 閉じる; 状態変化なし。
- `/profile` (引数なしチャットコマンド) で両プロファイル一覧、
  アクティブにマーク — popover ドロップダウンと同じデータ。
- `/profile <other>` でチャット経由切替; popover ドロップダウン
  (表示されていれば) も変化反映。
- Settings → 別所でアクティブセッションが参照中の非デフォルト
  プロファイルを削除 → 確認ダイアログ; 削除; 次回そのセッションを開く
  時にデフォルトへフォールバック + ステータスバーバッジ +
  popover ドロップダウン反映。
- デフォルトプロファイル削除試行 → ボタン disabled。
- 非デフォルトをデフォルトに設定; 旧デフォルトが削除可能になる。

### 6.3 構造

- `Load()` 返却後 `len(Config.LLM.Profiles) >= 1`。
- `Load()` 返却後 `Config.LLM.DefaultProfileID` が `Profiles` の
  要素を解決。

---

## 7. 互換性

### 破壊的

- **`config.json` スキーマ。** 移行後はトップレベル `llm.default_backend`
  / `llm.local` / `llm.vertex_ai` が消える。v0.12.0 後にファイルを
  手編集するユーザーは新しい `llm.profiles` 形を使う必要あり。
  一発移行は v0.12.0 初回ロード時に走る。v0.12.0 で移行済 config に対して
  v0.11.x を実行すると、`llm` を空としてパースしてしまう (旧形式の
  フィールドが欠落)。`LLMConfig` ゼロ値で Local/VertexAI 非アクティブ。
  **v0.12.0 → v0.11.x のダウングレードは手動 config ロールバックなしには
  サポートしない**。CHANGELOG エントリに明記。

### 非破壊的

- **`chat.json` / `session_memory.json` / `findings.json` /
  `summaries.json` スキーマ:** 変更なし。
- **Wails バインディング署名:** 変更なし。`Send`, `Abort`,
  `LoadSession`, `NewSession`, `ExportSession`, `ImportSession`,
  `IsBusy` すべて現状の署名のまま。
- **新規バインディング:**
  - プロファイル CRUD (Settings → LLM Profiles タブが使用):
    `ListProfiles() []ProfileSummary`,
    `CreateProfile(req CreateProfileReq) (ProfileSummary, error)`,
    `UpdateProfile(id string, req UpdateProfileReq) error`,
    `DeleteProfile(id string) (DeleteProfileResult, error)`,
    `SetDefaultProfile(id string) error`。
  - セッションコントロール (Session Control Popover が使用、
    `/profile` / `/model` チャットコマンドと同じハンドラを共有):
    `SwitchSessionProfile(profileID string) error`,
    `SwitchSessionBackend(backend string) error`。

### 移行

- v0.11.x `config.json` の v0.12.0 初回ロード時に一発移行。Idempotent
  (2 回目以降のロードは no-op)。
- セッションごとの遅延 `session.json` マイグレーション (各 v0.11.x
  セッションが v0.12.0 で初回ロードされた時に生成)。
- データロス不可能 — 移行は構造再編成で値変換ではない。

---

## 8. フェージング

単一 PR (または順序付きコミットの単一ブランチ)。コミット案:

1. `feat(config): LLMProfile type + multi-profile schema + migration`
   — config 層純粋変更。migrate + resolve のテスト。
2. `feat(memory): SessionConfig type + session.json IO`
   — memory 層純粋変更。load/save テスト。
3. `refactor(agent): plumb profile through setBackend + LoadSession`
   — agent がロード時に session.json を読み、解決プロファイルのペアから
   バックエンドをインスタンス化。fallback + 新規セッションデフォルト
   のテスト。
4. `feat(agent): /profile command`
   — チャットコマンドパーサ + ハンドラ + busy-state ガード。`/profile`
   全パスのテスト。
5. `feat(bindings): profile CRUD + ExportSession omits session.json`
   — Wails バインディング + イベント。CRUD + export 省略 + import
   デフォルト割当のテスト。
6. `feat(frontend): LLM Profiles tab + status badges + Session Control Popover`
   — Settings UI 書き直し (LLM Profiles タブが従来の 2 バックエンド
   タブを置換) + ChatStatusBar プロファイル / バックエンドバッジ +
   Session Control Popover コンポーネント + `agent:profile:changed`
   / `agent:backend:changed` / `config:profile:list_changed` ハンドラ。
7. `docs: ADR-0016 status update + CHANGELOG v0.12.0 +
   architecture reference`
   — ADR Status → Implemented; architecture §2 にプロファイル解決を
   追記; CHANGELOG エントリ。

---

## 9. スコープ外

- セッションごとの `active_backend` 永続化 (`/model` トグル)。
  session.json schema_version=2 で v0.12.x 内追加可能性として記録。
- memory / sandbox / tools / MCP プロファイル単位の上書き。それぞれ
  独立した設計圧力を持つ別案件。本 ADR を膨張させる。
- マシン間プロファイル同期 / 共有。プロファイルはローカル限定設定。
- プロファイル単位のモデルリスト or モデル取得 UI。各サイド 1 つの
  `Model` フィールドは据え置き。
- どのプロファイルが最も使われているかのテレメトリ。プライバシー
  優先のデフォルト: 一切のテレメトリ収集なし。

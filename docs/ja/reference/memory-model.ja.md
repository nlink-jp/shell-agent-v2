# メモリモデル — shell-agent-v2

> 日付: 2026-05-03
> ステータス: **v0.2.0 設計 (リリース前)** — メジャーバージョン
> アップ。下記アーキテクチャは v0.1.x モデルを全面的に置換。
> データ移行なし: v0.1.x の `pinned.json` / `findings.json` は
> v0.2.0 初回起動時に無視される (意図的リセット; §11
> 「破壊的変更」参照)。
> 対象: contributor / オペレータ。

shell-agent-v2 v0.2.0 設計では **4 つの異なるメモリ機能** が
ある。本書はその唯一のエントリポイント: それぞれが何で、どう
作成され、どこに保存され、システムプロンプトにどう集約される
かを示す。

> 本ドキュメント外: **System Rules** (v0.7.0+)。System Rules は
> *設定面* — エージェントが恒久指示として扱う、ユーザーが
> オーサリングする Markdown ファイル。同じく system prompt に
> 流れるが、メモリ施設ではない (下記 4 つはランタイム学習、
> System Rules は宣言的)。
> [`system-rules.ja.md`](system-rules.ja.md) と
> [ADR-0012](../adr/0012-system-rules.ja.md) 参照。

詳細な設計根拠:

- [memory-architecture-v2.ja.md](memory-architecture-v2.ja.md) —
  Records / contextbuild 設計 (v0.1.x から不変)
- [memory-injection-hardening.ja.md](memory-injection-hardening.ja.md) —
  脅威モデルと防御 (v0.1.26 Security Round 3、v0.2.0 では
  Global Memory に適用)

---

## 1. 4 つの機能の俯瞰

| 機能 | スコープ | 保存場所 | 内容 | 設定者 |
|---|---|---|---|---|
| **Records** | session 内 | `sessions/<id>/chat.json` (+ `summaries.json`) | 会話履歴の逐語ログ (immutable, append-only) | エージェントループ (各ターン) |
| **Session Memory** | session 内 | `sessions/<id>/session_memory.json` | 自動抽出された session 文脈 (`fact` / `context` カテゴリ) | `extractMemories` 各アシスタントターン後 |
| **Findings** | session 内 | `sessions/<id>/findings.json` | データ分析からの発見: 異常値、パターン、構造化された観察 | `promote-finding` ツール (LLM、`hasData` ゲート)、`analyze-data` 自動昇格 |
| **Global Memory** | 跨セッション (唯一の global 機能) | `global_memory.json` | ユーザー identity / 決定 (`preference` / `decision` カテゴリ) | `extractMemories` 各アシスタントターン後 + Session Memory / Findings からの手動 Pin |

命名整理:

- **「Pinned」は廃止**。「Pin (する)」というユーザー操作は残る
  (session-scoped を Global Memory に昇格する操作の名前)。
  ストア名としては Global Memory に統一。
- **`Pin to Global Memory`** が Session Memory 行と Findings
  行に表示される UI ボタン。

LLM コンテキストへの流入経路:

- Records → user/assistant/tool メッセージ (必要に応じ
  `contextbuild` で要約)
- Session Memory + Findings + Global Memory → **システム
  プロンプト**に注入 (Session Memory と Findings はその
  セッションがアクティブな時のみ)

---

## 2. Records (会話履歴)

user / assistant / tool メッセージとメタデータの逐語ログ。
セッションの single source of truth。

**永続化**: `~/Library/Application Support/shell-agent-v2/sessions/<id>/chat.json`
に append-only JSON。`internal/atomicio.WriteFileAtomic` 経由
の atomic write。

**圧縮モデル**: records は不変。LLM コンテキスト予算超過時、
古い raw records は派生要約として `sessions/<id>/summaries.json`
(content-keyed cache) に折り畳まれる。record が書き換えられる
ことはない。v0.1.x の `Memory.UseV2` opt-in フラグは
**v0.2.0 で削除**: contextbuild 経路が唯一の経路となる。
レガシーの破壊的圧縮コード (`compactIfOverBudget`,
`compactMemoryIfNeeded`) は no-op 化ではなく**完全削除**。

**時間マーカー**: `[YYYY-MM-DD HH:MM TZ]` 接頭辞は意味のある
境界 (>30 分ギャップ、tool/report ロール) のみ。要約ブロックは
range header 付き。

**Tier フィールド**: v0.2.0 で削除。(v0.1.26 Memory v2 時点で
既に vestigial だった。)

**プライバシーフラグ (v0.3.0)**: `Session.Private bool` は
`chat.json` に `omitempty` JSON タグ付きで永続化される — フィー
ルドのない legacy セッションは `Private = false` として load
される。`true` のとき、セッションは跨セッション promotion から
opt-out される: `extractMemories` パイプラインは `preference` /
`decision` fact を drop (Global Memory にルーティングされない)、
`PromoteSessionMemoryToGlobal` と `PromoteFindingToGlobal`
ハンドラはサーバ側で reject、frontend は ★ Pin ボタンを hide
し、サイドバー行と chat-pane バナーに 🔒 indicator を表示する。
プライバシーはセッション作成時に固定 (mid-session トグル不可)
され境界が曖昧にならない。詳細設計:
[privacy-controls.ja.md](privacy-controls.ja.md) §2。

**Bundle ポータビリティ (v0.4.0)**: セッションはサイドバーの
Export アイコンまたは `/export` スラッシュコマンドから 1 つの
`.shellagent` ZIP にパッケージ可能。Bundle はこの `chat.json`
に加えて session memory、findings、summaries、サンドボックス
`work/`、analysis DuckDB、セッションが所有する全 objstore
object を運ぶ。Re-import されたセッションは fresh sess-id と
fresh object ID を取得 (in-record の全参照は
`internal/sessionio` rewriter で書き換え)。プライバシーフラグ
は逐語保持される。詳細設計:
[session-import-export.ja.md](session-import-export.ja.md)。

**詳細**: [memory-architecture-v2.ja.md](memory-architecture-v2.ja.md)。

---

## 3. Session Memory (自動抽出された session 文脈)

*現在の*会話に関する自動抽出事実。跨セッションのユーザー
identity に値しないもの。「ユーザーは Q1 売上を分析中」「グフ
モデル画像を添付した」「レポートは 3 セクション希望」など。

**スキーマ** (`internal/memory/session_memory.go`):

```go
type SessionMemoryEntry struct {
    Fact            string    // 英語形式
    NativeFact      string    // ユーザー言語
    Category        string    // fact | context  (4 カテゴリの subset)
    SourceTime      time.Time
    CreatedAt       time.Time

    // Provenance
    SourceTurnIndex int
    Source          string    // user_turn | assistant_turn
    ToolOriginated  bool

    // Lifecycle (ADR-0031)。State は Relevance と CreatedTurn
    // から各 mutation で導出される; サイドバーバッジが閾値を
    // 再計算せずに描画できるよう永続化される。
    Relevance       float64
    CreatedTurn     int
    LastTouchedAt   time.Time
    LastTouchedTurn int
    TouchCount      int
    State           string    // fresh | active | dormant | archived
}
```

`SessionID` は暗黙 (ファイルが per-session)。`Source = manual`
は無し (会話文脈の手動エントリは use case にならない —
ユーザーが直接タイプしたものは Records に既にある)。

**自動抽出**: Global Memory と同じ `extractMemories` パスで
処理。LLM が category 付きで facts を出力、カテゴリ
`fact` / `context` がここに routing される。

**システムプロンプトレンダリング** (現セッションのみ):

```
- [user-stated] [context] User is analysing 2025 Q1 sales data (ユーザーは2025年Q1売上データを分析中) (learned 2026-05-03)
- [derived] [fact] Three datasets are loaded: sales, customers, returns (3つのデータセットがロード済み) (learned 2026-05-03)
```

**キャパシティ**: per-session FIFO cap (デフォルト 50; per-
session ノイズを厳密に bound するため Global より低い)。

**ライフサイクル**: セッションと一緒に削除。
`sessions/<id>/` ディレクトリごと削除される。

**Pin (昇格)**: サイドバー Session Memory 節の各行に
「Pin to Global Memory」ボタン。クリックで category 確認
ダイアログ (デフォルト `decision`、`preference` に変更可) →
新規 Global Memory entry 作成。元 Session Memory entry は
残存。

---

## 4. Findings (データ分析の発見)

**データ分析特化**の洞察 — ロード済データセットの異常値、
統計的パターン、`analyze-data` からの構造化観察。会話一般
からの自動抽出はしない: `promote-finding` が唯一の LLM 経路で、
ロード済データのあるセッションに gate される。

**スキーマ** (`internal/findings/findings.go`):

```go
type Finding struct {
    ID              string    // f-YYYYMMDD-NNN[-hex]
    Content         string
    Tags            []string  // 自由形式; severity tag は色付き badge
    CreatedAt       string    // RFC3339
    CreatedLabel    string    // "2026-05-03 (Friday)"

    // Provenance
    Source          string    // llm_promoted | analyze_data
    ToolOriginated  bool
}
```

`SessionID` / `OriginSessionTitle` は削除 (ファイルが per-
session)。Source `manual` も削除 (v0.2.0 では `/finding` slash
コマンドなし)。

**作成経路** (v0.2.0):

- **`promote-finding` ツール** — LLM 呼び出し可能、
  `hasData == true` のセッションでのみ提供 (`load-data` で
  少なくとも 1 テーブル ロード済み)。MITL デフォルト ON:
  昇格ごとに content 確認ダイアログ。
- **`analyze-data` 自動昇格** — sliding-window 分析が結果に
  構造化 `Finding` を surface したら一括追加。

**v0.2.0 で削除**:
- `/finding <text>` slash コマンド (手動エントリ経路) — Pin
  ワークフロー + Session Memory が代替。

**キャパシティ**: per-session FIFO cap (デフォルト 100/
セッション)。

**ライフサイクル**: セッションと一緒に削除。

**システムプロンプトレンダリング** (現セッションのみ):

```
- [derived] [2026-05-03] 2025-03-09 大阪 Widget-C: 1850個販売 (週平均の50倍) — データ入力エラーまたは大口取引の可能性
- [derived] [2026-05-03] 東京 Widget-A は全週で一貫して 130 個前後の安定した出荷量
```

**Pin (昇格)**: 各 Finding 行に「Pin to Global Memory」ボタン
(Session Memory と同じ UI affordance)。分析的発見が実は
「跨セッションで覚える価値ある決定」だった場合に有用 (例:
「Widget-C の需要は予測不能と判断」)。

**専用 UI パネル**: Findings は chat-pane エリアに専用ペイン
を持つ (Data 開示の隣)、サイドバーではない。理由: Findings は
ロード済データに密結合 — 発見一覧は元データセットの隣で見るのが
最も自然。フラットリスト + filter / search 表示。

---

## 5. Global Memory (跨セッションのユーザー identity)

エージェントが全セッションを跨いで記憶する長期事実。v0.2.0
で**唯一の**跨セッション永続機能。

**スキーマ** (`internal/memory/global_memory.go`):

```go
type GlobalMemoryEntry struct {
    Fact            string    // 英語形式
    NativeFact      string    // ユーザー言語
    Category        string    // preference | decision  (4 カテゴリの subset)
    SourceTime      time.Time
    CreatedAt       time.Time

    // Provenance (移植可能)。マシンローカルなセッション back-reference
    // (SessionID, SourceTurnIndex, PromotedFromID) は ADR-0028 で削除
    // — read されず、マシンをまたぐと安全でないため。
    Source          string    // user_turn | assistant_turn | manual | promoted_from_session_memory | promoted_from_finding
    ToolOriginated  bool

    // Lifecycle (ADR-0031)。SessionMemoryEntry と対称。Global
    // Memory と Session Memory は同じ state machine を共有する。
    Relevance       float64
    CreatedTurn     int
    LastTouchedAt   time.Time
    LastTouchedTurn int
    TouchCount      int
    State           string    // fresh | active | dormant | archived
}
```

**カテゴリ** (`preference` / `decision` のみ):

| Category | 用途 | 例 |
|---|---|---|
| `preference` | ユーザーの好み・癖 | "User prefers Go over Python" |
| `decision` | 設計 / 構成上の決定 | "Chose DuckDB over SQLite for analysis" |

`fact` / `context` は session-scoped 概念 (Session Memory)。
ここへの auto-routing はできない。

**作成経路**:

- **自動抽出** (`extractMemories`): カテゴリ `preference` /
  `decision` でタグされた fact が自動 routing される
- **Session Memory からの手動 Pin**: Session Memory 行で
  「Pin to Global Memory」をクリック、`preference`/`decision`
  に再分類可
- **Findings からの手動 Pin**: 同じ flow を Findings パネル
  から
- **直接手動エントリ**: settings UI / API (`Set` メソッド)

**システムプロンプトレンダリング** (常時注入):

```
- [user-stated] [preference] User prefers Go over Python (learned 2026-05-03)
- [user-stated] [decision] Chose DuckDB over SQLite (promoted from Finding, 2026-05-02)
```

**キャパシティ**: FIFO cap (デフォルト 100)。v0.1.x の "Pinned"
に適用されていた 100 より tight (Global Memory が
`preference`/`decision` のみに絞られたため)。

**サイドバー表示**: Memory タブ → 上部 "Global Memory" 節。
各行に fact、category badge、trust badge、learned date。
「Demote to Session Memory」ボタン (希少。global entry が実は
session-bound だったと気付いた時用)。

---

## 6. サイドバー & UI レイアウト

**Memory サイドバー** (既存タブを再構成):

```
┌── Sidebar / Memory tab ──────────┐
│                                  │
│ [Global Memory]                  │
│  • [user-stated] User prefers Go │
│  • [user-stated] Chose DuckDB    │
│  …                               │
│                                  │
│ [Session Memory]                 │
│  • [user-stated] User analysing  │
│    Q1 sales data                 │
│  • [derived] Three datasets…     │
│  • [Pin to Global Memory] btn    │
│  …                               │
│                                  │
└──────────────────────────────────┘
```

既存 Memory サイドバータブ内に 2 セクション — Global Memory
(上) + Session Memory (下)。各節に bulk-select + delete。
Session Memory 行に Pin ボタン。

**Findings パネル** (新規、chat-pane エリア):

```
┌── Chat pane ─────────────────────┐
│ 会話                              │
│                                  │
│ ┌── Data ──────────────────────┐ │
│ │ Tables / objects / /work     │ │
│ └──────────────────────────────┘ │
│ ┌── Findings ──────────────────┐ │ ← 新規専用パネル
│ │ • [analyze-data] Q1 大阪異常 │ │
│ │ • [promote-finding] 東京…    │ │
│ │ Filter: [all|critical|…]     │ │
│ │ [Pin to Global Memory] btn   │ │
│ └──────────────────────────────┘ │
└──────────────────────────────────┘
```

Data 開示の隣に配置。リストビュー + severity tag フィルタ +
search + Pin ボタン (各行)。

---

## 7. システムプロンプト集約

`chat.Engine.BuildSystemPrompt(globalMemoryContext,
sessionMemoryContext, findingsContext)` (`internal/chat/chat.go`):

```
{base system prompt}

{temporal context}
{Location: ... 設定時}
{sandbox guidance、Sandbox.Enabled 時}

Important facts you remember about the user:
{global_memory.FormatForPrompt()}

Notes about the current session:
{session_memory.FormatForPrompt()}

Analysis findings in this session:
{findings.FormatForPrompt()}
```

これが LLM コール時の `system` メッセージになる。Records は
**ここを通らない** — `BuildMessages` (`agent.buildMessagesV2`)
から user/assistant/tool 別メッセージとして入る。

**空のセクションは丸ごと省略** (content なしのヘッダは出さない)。

**ライフサイクルフィルタ (ADR-0031)**: `FormatForPrompt` は
Global Memory と Session Memory の `dormant` / `archived`
状態の entry をスキップする。該当 entry はディスクに残り
サイドバー UI からは見えるが、 touch でリフレッシュされる
までは LLM のプロンプトに乗らない。Findings は従来通り
(ライフサイクルフィルタなし)。

**トークン予算**: 各 `FormatForPrompt` は内部で **16 KiB** に
bounded、最新優先で含めて省略マーカ付き。最悪 system prompt
追加分の合計 ~48 KiB。ライフサイクルフィルタ導入後は実質的な
制約として機能することはほぼない。

---

## 8. 自動抽出 (`extractMemories`)

`extractPinnedMemories` から改名 (broader routing を反映)。
各アシスタントターン後の post-response バックグラウンドタスク
として実行。

**パイプライン**:

1. 直近 4 件の hot records を取得
2. `nlk/guard.Tag` でラップ (prompt-injection 隔離)
3. 同じ LLM に
   `category|turn-N|english fact|native expression` 形式で抽出
   依頼
4. 各返却行について:
   - `{preference, decision, fact, context}` allowlist 外の
     category は drop
   - `IsSelfReferential(fact)` ヒット (the assistant / model /
     system prompt / THINK タグ / tools 等への言及) は drop
   - `parseTurnToken` + content overlap refinement で source
     判定
   - **カテゴリで routing**:
     - `preference` / `decision` → `globalMemory.Add(...)`
     - `fact` / `context` → `sessionMemory.Add(...)`
5. atomic save、`global_memory:updated` または
   `session_memory:updated` イベント発火 → UI refresh

抽出プロンプトは LLM に明示的に伝える:

> カテゴリ `preference` と `decision` は長期的なユーザー
> identity を表し、全セッションで永続する。カテゴリ `fact` と
> `context` は現在の会話の状態を表し、セッション終了で消える。
> 意図する永続性に合うカテゴリを選んでほしい。

これにより LLM のカテゴリ選択が scope 意図を担う。

---

## 9. Provenance & Trust Tags

(v0.1.26 と同じモデルを Global Memory と Session Memory に適用。)

| Source 値 | Trust tag | 意味 |
|---|---|---|
| `user_turn` | `[user-stated]` | user role record から抽出 |
| `manual` | `[user-stated]` | UI 経由でユーザーが pin |
| `promoted_from_session_memory` | `[user-stated]` | Session Memory entry を Pin |
| `promoted_from_finding` | `[user-stated]` | Finding を Pin |
| `assistant_turn` | `[derived]` | assistant role record から抽出 |
| `llm_promoted` | `[derived]` | `promote-finding` ツールで promote |
| `analyze_data` | `[derived]` | `analyze-data` ツールで自動昇格 |
| 空 (legacy) | `[derived]` | 安全側 default |

Findings: 同じ Source enum (subset)。防御 (self-referential
filter, category allowlist, `nlk/guard` wrap) は抽出時に適用。

---

## 10. キャパシティ & リテンション

**Per-store soft cap:**

| Store | デフォルト | Config key |
|---|---|---|
| Global Memory | 100 | `Memory.MaxGlobalMemory` |
| Session Memory | 50 (per session) | `Memory.MaxSessionMemoryPerSession` |
| Findings | 100 (per session) | `Memory.MaxFindingsPerSession` |

**Render 時予算**: 各 `FormatForPrompt` は 16 KiB で clip、
最新優先。

**ライフサイクル減衰 (ADR-0031)**: Global Memory と Session
Memory の entry は `Relevance` ([0, 1]) を持ち、各 user
ターン完了時に `Memory.Lifecycle.DecayRate` (デフォルト
0.93) で乗算される。`ActiveThreshold` を割ると `dormant`
(注入されない)、`ArchiveThreshold` を割ると `archived`
(eviction 候補) になる。touch (user 発話との lexical match
または extractor 出力の `touched|` 行) で relevance は
1.0 に戻る。cap 圧迫時の eviction は最低 relevance を選び、
`archived` は recently-touched でも優先的に削られる。
Findings は従来通り FIFO で、ライフサイクル減衰の対象外。

**ターン単位減衰 vs 壁時計減衰**: ライフサイクル減衰は user
ターン単位で測られ、実時間ではない。一晩 paused された
セッションが記憶を失うことはなく、80 turn を駆け抜ける
セッションは速く aging する。詳細は ADR-0031 §5。

---

## 11. 破壊的変更 (v0.1.x → v0.2.0)

これは**破壊的アーキテクチャ変更**:

- `Memory.UseV2` config フラグ — **削除**。contextbuild 経路
  (immutable Records + 派生要約 cache) が唯一の経路に。
  レガシー v1 destructive-compaction コードは flag で gate
  するのではなく**削除** — 「v1 モード」はもう存在しない。
- `pinned.json` (global ファイル) — **起動時に無視**。古い
  preferences/decisions は失われる。会話または settings UI
  経由で再 pin。
- `findings.json` (global ファイル) — **起動時に無視**。古い
  跨セッション findings は失われる。
- セッションファイル (`sessions/<id>/chat.json` +
  `summaries.json`) — **保持**。古い会話を browse 可能;
  Session Memory / Findings は付かない (v0.1.x で存在しなかった
  facility なので)。
- `/finding` slash コマンド — **削除**。chat-pane Findings
  パネル + Pin ボタンを使用。
- `PinnedFact` 型 — `GlobalMemoryEntry` に改名 (Go API)。
- `PinnedStore` → `GlobalMemoryStore`。
- `SetPinnedHandler` → `SetGlobalMemoryHandler` および
  `SetSessionMemoryHandler` (両方)。
- Wails event `pinned:updated` → `global_memory:updated` および
  `session_memory:updated`。
- Bindings 改名: `GetPinnedMemories` → `GetGlobalMemories`;
  `DeletePinnedMemories` → `DeleteGlobalMemories`。新規:
  `GetSessionMemories`, `DeleteSessionMemories`,
  `PinSessionMemory`, `PinFinding`。

初回起動 banner で v0.1.x ストアが無視されたことを通知 (tip:
`~/Library/Application Support/shell-agent-v2/pinned.json` は
disk に保持されているので手動回収可能)。banner は dismissible。

---

## 12. ストレージレイアウト

```
~/Library/Application Support/shell-agent-v2/
├── global_memory.json                    # Global Memory (NEW 命名; was pinned.json)
├── sessions/
│   └── <session-id>/
│       ├── chat.json                     # Records (不変)
│       ├── summaries.json                # contextbuild SummaryCache (不変)
│       ├── session_memory.json           # NEW: Session Memory
│       └── findings.json                 # MOVED: per-session Findings
├── objects/                              # objstore (不変)
└── config.json
```

disk に残るレガシーファイル:
- `pinned.json` — opt-in 回収ツールでのみ read access
  (v0.2.0 では out of scope; ユーザーが手動 `cat` で確認可能)
- `findings.json` — 同上

新規書き込みはすべて `internal/atomicio.WriteFileAtomic` 経由。

---

## 13. 脅威モデル (v0.2.0 update)

v0.2.0 以前の攻撃: 悪意ある CSV セルがアシスタントによって
引用 → 自動抽出 → グローバル pin → 全将来セッションに権威ある
context として再注入。

**v0.1.26 hardening の上に v0.2.0 の追加緩和**:

| メカニズム | 攻撃への効果 |
|---|---|
| `fact` / `context` は Session Memory に routing、Global Memory に行かない | CSV セル経由の注入成功でも由来セッションのみ汚染、全将来セッションには波及しない |
| Session 削除 ⇒ Session Memory + Findings 削除 | セッション終了で攻撃 window が閉じる |
| Global Memory は `preference` / `decision` のみ受領 | 攻撃者は payload を user preference としてラベルさせる必要あり (`context` より難しい) |
| `promote-finding` `hasData` ゲート | data ロード済セッション外では呼べない |
| `/finding` slash 削除 | LLM 影響下の手動 surface が 1 つ減る |
| Pin は明示ユーザークリック必須 | session-scoped → global の自動昇格なし |

v0.1.26 の self-referential filter, category allowlist,
`nlk/guard` wrap は **両方の**自動抽出ストリーム (Global Memory
と Session Memory) に適用。

**詳細**: [memory-injection-hardening.ja.md](memory-injection-hardening.ja.md)。

---

## 14. 用語集

- **Records** — 会話ターンの逐語; per-session
- **Session Memory** — 自動抽出された session 文脈
  (`fact`/`context`); per-session
- **Findings** — データ分析の発見; per-session
- **Global Memory** — 跨セッションのユーザー identity
  (`preference`/`decision`); 唯一の global 永続機能
- **Pin** / **Pin to Global Memory** — Session Memory または
  Findings entry を Global Memory に昇格する UI 操作
- **Source / Provenance** — 由来ラベル、trust tag 導出元
- **Trust tag** — `[user-stated]` (高) or `[derived]`
  (LLM 経由)

---

## 15. 参照

- [memory-architecture-v2.ja.md](memory-architecture-v2.ja.md) —
  Records / contextbuild (不変)
- [memory-injection-hardening.ja.md](memory-injection-hardening.ja.md) —
  Pinned/Findings セキュリティモデル (v0.1.26)
- `internal/memory/global_memory.go` — GlobalMemoryStore (NEW)
- `internal/memory/session_memory.go` — SessionMemoryStore (NEW)
- `internal/findings/findings.go` — per-session findings store
- `internal/contextbuild/` — Memory v2 ContextBuilder
- `internal/agent/agent.go:extractMemories` — 自動抽出 routing
- `internal/chat/chat.go:BuildSystemPrompt` — 集約

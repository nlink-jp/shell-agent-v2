# メモリモデル — shell-agent-v2

> 日付: 2026-05-03
> ステータス: v0.1.28 時点
> 対象: shell-agent-v2 のメモリ周りの全体像を理解する必要が
> ある contributor / オペレータ。詳細設計文書は各節からリンク。

shell-agent-v2 には **3 つの異なるメモリ機能** があり、初見
ではこれらを混同しがち。本書はその唯一のエントリポイント:
それぞれが何で、どう作成され、どこに保存され、システム
プロンプトにどう集約されるかを示す。

詳細な設計根拠:

- [memory-architecture-v2.ja.md](memory-architecture-v2.ja.md) —
  Records / contextbuild 設計
- [memory-injection-hardening.ja.md](memory-injection-hardening.ja.md) —
  脅威モデルと防御 (v0.1.26 Security Round 3)

---

## 1. 3 つの機能の俯瞰

| 機能 | スコープ | 保存場所 | 内容 | 設定者 |
|---|---|---|---|---|
| **Records** | セッション内 | `sessions/<id>/chat.json` (+ `summaries.json`) | 会話履歴の逐語ログ (immutable, append-only) | エージェントループ (各ターン) |
| **Pinned Memory** | 跨セッション | `pinned.json` | ユーザーに関する長期事実 (preference, decision, context) | `extractPinnedMemories` 各アシスタントターン後 (自動)、`Set()` (手動) |
| **Findings** | 跨セッション | `findings.json` | 跨セッションで再利用価値ある分析洞察 (異常値、パターン、データに関する判断) | `promote-finding` ツール (LLM)、`/finding` slash (ユーザー)、`analyze-data` 自動昇格 |

3 つとも LLM コンテキストに流れ込むが、**経路が異なる**:

- Records → user / assistant / tool メッセージターン
  (必要に応じ `contextbuild` で要約)
- Pinned + Findings → **システムプロンプト**に注入

---

## 2. Records (会話履歴)

user / assistant / tool メッセージとメタデータ (タイムスタンプ、
添付画像 object ID、ツールコール記録) の逐語ログ。セッションの
single source of truth。

**永続化**: `~/Library/Application Support/shell-agent-v2/sessions/<id>/chat.json`
に append-only JSON。`internal/atomicio.WriteFileAtomic` 経由
の atomic write。

**圧縮モデル** (v0.1.26+ デフォルト `Memory.UseV2: true`):
records は **不変**。summary record で置換されることはない。
LLM コンテキスト予算を超過したら、古い raw records は別
キャッシュ (`sessions/<id>/summaries.json`) 内の **派生要約**
に折り畳まれる。content hash で keyed されているのでラン跨ぎ
バックエンド跨ぎで再利用可能。レガシー v1 経路 (in-place
`Tier` mutation, `PromoteOldestHotToWarm`) は古いセッション
ファイルのために残存するが、書き込みでは触らない。

**時間マーカー**: 各 record に `[YYYY-MM-DD HH:MM TZ]` 接頭辞が
付くのは意味のある境界 (>30 分ギャップ、tool/report ロール) のみ。
要約ブロックには `[Summary of N earlier turn(s) — from … to …]`
の range header。

**Tier フィールド**: レガシー遺物。新コードは書かず v2 経路は
読まない。`Tier=hot/warm/cold` を含む古いセッションファイルは
load して render される。

**詳細**: [memory-architecture-v2.ja.md](memory-architecture-v2.ja.md)。

---

## 3. Pinned Memory (跨セッションのユーザー事実)

エージェントが全セッションを跨いで記憶する長期事実。セッション
削除後も残る。

**スキーマ** (`internal/memory/pinned.go`):

```go
type PinnedFact struct {
    Fact            string    // 英語形式 (分析用)
    NativeFact      string    // ユーザー言語 (表示用)
    Category        string    // preference | decision | fact | context
    SourceTime      time.Time // 元会話の発生時刻
    CreatedAt       time.Time // pin された時刻

    // Provenance (v0.1.26+)
    SessionID       string    // 由来セッション ID
    SourceTurnIndex int       // そのセッション Records 内の index
    Source          string    // user_turn | assistant_turn | manual
    ToolOriginated  bool      // 周辺窓に tool 出力を含んだか
}
```

**カテゴリ** (LLM が割り当て、allowlist 強制):

| Category | 用途 | 例 |
|---|---|---|
| `preference` | ユーザーの好み・癖 | "User prefers Go over Python" |
| `decision` | 設計 / 構成上の決定 | "Chose DuckDB over SQLite" |
| `fact` | 事実コンテキスト | "User is in Tokyo" |
| `context` | 状況把握 | "User analyses Q1 sales data" |

**自動抽出** (`extractPinnedMemories`、各アシスタントターン後):

1. 直近 4 件の hot records (user/assistant 混在) を取得
2. `[turn N|role]:` で番号付け、`nlk/guard.Tag` でラップして
   データ扱い
3. 同じバックエンドの LLM に
   `category|turn-N|english fact|native expression` 形式で抽出
   依頼
4. 各返却行について:
   - allowlist 外の category は drop
   - `IsSelfReferential(fact)` ヒット (the assistant / model /
     system prompt / THINK タグ / tools 等への言及) は drop
   - source 判定: `turn-N` パース → 由来 role。パース不可 or
     由来が assistant でも fact の英語キーワード / 日本語
     trigram が user 発話と重複していれば `user_turn` に格上げ
     (content-based attribution refinement)
5. `pinned.Add()` で追加、`Fact` 完全一致で dedup、
   `MaxPinnedFacts` (デフォルト 100) 超過で FIFO evict
6. atomic write で保存、`pinned:updated` イベント発火 →
   サイドバー即時 refresh

**手動操作**:

- `Set(key, content)` — UI 手動 pin、`Source: manual` stamp
- `Delete(key)` / `DeleteByKeys([]string)` — UI 手動削除

**システムプロンプトレンダリング** (`FormatForPrompt`):

```
- [user-stated] [preference] User prefers Go over Python (ユーザーはPythonよりGoを好む) (learned 2026-05-03)
- [derived] [context] User is currently analysing Q1 sales data (learned 2026-05-03)
```

行頭の `[user-stated]` / `[derived]` は **trust tag**、`Source`
から導出。§6 参照。

**サイドバー表示**: 各行に fact、category badge、trust badge、
`learned YYYY-MM-DD` (v0.1.28+)。

**詳細**: filter と脅威モデルは
[memory-injection-hardening.ja.md](memory-injection-hardening.ja.md)。

---

## 4. Findings (跨セッション分析洞察)

データ分析で得られた、セッション跨ぎで surface する価値ある
洞察。`OriginSessionID` を持つので「どこから来たか」を追跡
可能。

**スキーマ** (`internal/findings/findings.go`):

```go
type Finding struct {
    ID                 string   // f-YYYYMMDD-NNN[-hex]
    Content            string
    OriginSessionID    string
    OriginSessionTitle string
    Tags               []string // 自由形式; severity tag ("critical", "high", …) は色付き badge
    CreatedAt          string   // RFC3339
    CreatedLabel       string   // "2026-05-03 (Friday)"

    // Provenance (v0.1.26+)
    Source         string  // llm_promoted | manual
    ToolOriginated bool
}
```

**作成経路**:

- **`promote-finding` ツール** — LLM 呼び出し可能。MITL デフォルト
  **ON** (v0.1.26 hardening): 昇格ごとに content 確認ダイアログ。
  Source: `llm_promoted`。
- **`/finding <text>` slash command** — ユーザー手動。
  Source: `manual`。
- **`analyze-data` 自動昇格** — sliding-window 分析が結果に構造化
  `Finding` レコードを surface したら一括追加。
  Source: `llm_promoted`。

3 経路すべて `findings:updated` (v0.1.28+) でフロントエンドに
通知 → サイドバー即時 refresh (セッション切替不要)。

**キャパシティ**: `MaxFindings` (デフォルト 200) で FIFO eviction。
日次 ID カウンタは 999/日 超過で `f-YYYYMMDD-NNNNNN-<6 hex>` に
roll over。

**システムプロンプトレンダリング**:

```
- [derived] [2026-05-03 (Friday)] 2025年Q1において大阪のWidget-Cで… (from: Sales Analysis, session: sess-1234)
- [user-stated] [2026-05-02 (Thursday)] User identified anomaly in… (from: Manual Review, session: sess-5678)
```

**サイドバー表示**: content、trust badge、日付、由来セッション
タイトル、severity 色分け tag badge。

---

## 5. システムプロンプト集約

`chat.Engine.BuildSystemPrompt(pinnedContext, findingsContext)`
(`internal/chat/chat.go:110`) が唯一の集約点:

```
{base system prompt}

{temporal context}
{Location: ... 設定時}
{sandbox guidance、Sandbox.Enabled 時}

Important facts you remember about the user:
{pinned.FormatForPrompt()}

Analysis findings from other sessions:
{findings.FormatForPrompt()}
```

これが LLM コール時の `system` メッセージになる。Records は
**ここを通らない** — `BuildMessages` (v2 経路では
`agent.buildMessagesV2`) から user/assistant/tool 別メッセージ
として入る。

**副作用**: `BuildSystemPrompt` は `nlk/guard.Tag` の nonce を
ローテートする。同一ターン内の続く `WrapUserToolContent`
コールは新しい nonce を使う。

**トークン予算**: 各 `FormatForPrompt` は内部で **16 KiB** に
bounded、最新優先で含めて省略マーカ
(`(N earlier facts elided to fit budget)`) 付き。Pinned/Findings
ストアの肥大が会話予算を圧迫しないようにしている。

---

## 6. Provenance & Trust Tags

各 Pinned / Finding entry は `Source` フィールドを持つ。trust
tag は導出 (`pinned.go:trustTag`, `findings.go:trustTag`):

| Source 値 | Trust tag | 意味 |
|---|---|---|
| `user_turn` | `[user-stated]` | user role record から抽出 (or content override で user 認定) |
| `manual` | `[user-stated]` | UI / slash command 経由でユーザーが pin/promote |
| `assistant_turn` | `[derived]` | assistant role record から抽出 — content は LLM 経由 |
| `llm_promoted` | `[derived]` | LLM tool call で promote — content は LLM 分析由来 |
| 空 (legacy) | `[derived]` | v0.1.26 以前 entry に Source なし — 安全側 |

**なぜ重要か**: LLM 生成ターンを通過した content には攻撃者
影響下バイトが含まれうる (引用 CSV セル、MCP 応答、画像 OCR、
取得 Web ページ)。`[derived]` タグは将来 LLM コンテキストに
「user 発言とは限らない」と警告する。§9 参照。

---

## 7. キャパシティ & リテンション

現状: **FIFO cap のみ**。時間ベース decay なし、Category 以上
の importance 評価なし。

| 設定 | デフォルト | 場所 |
|---|---|---|
| `Memory.MaxPinnedFacts` | 100 | `internal/config/config.go` |
| `Memory.MaxFindings` | 200 | 同上 |
| `PinnedFormatBudget` | 16 KiB | `internal/memory/pinned.go` |
| `FindingsFormatBudget` | 16 KiB | `internal/findings/findings.go` |

**Eviction policy**: `Add` overflow で最古から (FIFO)。将来 (B)
importance score / (C) per-category TTL は memory plan に記載
あり、未実装。

`PinnedFormatBudget` / `FindingsFormatBudget` はレンダー時に
適用; ストアに 100 件あっても、システムプロンプトに出るのは
16 KiB に収まる最新分のみ。

---

## 8. ストレージレイアウト

```
~/Library/Application Support/shell-agent-v2/
├── pinned.json            # PinnedStore — 跨セッション global 事実
├── findings.json          # findings.Store — 跨セッション global 洞察
├── sessions/
│   └── <session-id>/
│       ├── chat.json      # session.Records (immutable log)
│       └── summaries.json # contextbuild SummaryCache (派生、再生成可)
├── objects/               # objstore: 画像、レポート、blob (16-byte ID)
└── config.json
```

JSON 書き込みはすべて `internal/atomicio.WriteFileAtomic`
(tmp + rename + 親 dir fsync) 経由 — 書き込み中クラッシュでも
前のファイルは無傷 (security-hardening-2.md C4 / H10)。

---

## 9. 脅威モデル (要約)

Pinned facts と findings はすべての将来セッションのシステム
プロンプトに権威ある context として再注入される。これは構造
的なプロンプトインジェクション経路: アシスタントターンに一度
でも現れた content (アシスタントが要約したツール出力を含む) は
`extractPinnedMemories` または `promote-finding` LLM コールで
拾われ、将来全セッションを操舵しうる。

**v0.1.26 の防御** (Security Round 3):

| 防御 | レイヤ |
|---|---|
| Provenance attribution | 各 fact が `Source` を持ち trust 復元可能 |
| Self-referential filter | `IsSelfReferential` が「the assistant / THINK / system prompt / …」系 fact を抽出時に drop |
| Category allowlist | `preference|decision|fact|context` のみ生存 |
| `nlk/guard` ラップ | 抽出プロンプトが会話テールをデータ扱い |
| `promote-finding` MITL ON | LLM 昇格 finding にユーザー確認 |
| FIFO retention cap | ストア成長を bound |
| 16 KiB FormatForPrompt 予算 | プロンプト成長を bound |

自動抽出自体は ON のまま (pin 単位 MITL は chat UX 破壊)。
自動抽出経路の残留リスクはサイドバー監査 + 一括削除で回収。

**詳細**: [memory-injection-hardening.ja.md](memory-injection-hardening.ja.md)。

---

## 10. 設計 — 2-tier scope (提案中、v0.1.28 以降)

> ステータス: **設計のみ**、未実装。

現モデルは pinned fact と finding を *すべて* **global** 扱い —
全将来セッションのシステムプロンプトに出る。動作はするが
global pool を圧迫: タスク文脈アイテム (「Q1 分析用に 3
データセット load 済」) が identity アイテム (「Go を好む」)
と同じ予算を消費。

提案する進化は `Scope` フィールドの導入:

- **`global`** — 現挙動。全セッションに毎回注入。
- **`session`** — 由来セッションがアクティブな時のみ注入。
  セッション削除と一緒に auto-delete。

**カテゴリ別デフォルトマッピング** (Pinned):

| Category | デフォルト scope |
|---|---|
| `preference` | global |
| `decision` | global |
| `fact` | session |
| `context` | session |

**Findings**: 全件デフォルト `session` (分析はケース依存)。
ケース跨ぎ価値があれば手動 global 昇格。

**手動オーバーライド**: サイドバー各行に `[global]` /
`[session]` badge と「Pin to global」ボタン (session→global)、
「Unpin」トグル (global→session)。

**抽出プロンプト改訂**: LLM に category→scope マッピングを
教え、scope 意図を反映した category 選択を促す。

**後方互換**: 既存 entry (Scope フィールドなし) は `global`
default — データロスなし、現挙動維持。

**未解決の問い** (次の planning round で詰める):

- 自動昇格はあるべきか (例: 同じ fact が N セッションで再出現
  → global へ自動昇格)
- ストレージ: 単一ファイル + `Scope` フィールド vs ファイル分離
- サイドバーレイアウト: 2 サブ節 (Global / This Session) vs
  単一リスト + badge filter

実装時にこの節は実装結果と open questions の解消で更新する。

---

## 11. 用語集

- **Records** — 会話ターンの逐語; セッション内
- **Pinned Memory** — ユーザーに関する跨セッション事実
- **Findings** — 跨セッション分析洞察
- **Pin to global** — (提案) session-scoped を global へ昇格
- **Scope** — (提案) `global` (跨セッション) or `session`
  (セッション束縛)
- **Source / Provenance** — 由来ラベル (`user_turn` 等)、
  trust tag 導出元
- **Trust tag** — `[user-stated]` (高信頼) or `[derived]`
  (LLM 経由、攻撃者影響下バイト含みうる)
- **Tier** — Records 上のレガシー hot/warm/cold フィールド、
  Memory v2 では vestigial
- **Memory v2 / `UseV2`** — contextbuild 経路: records 不変、
  要約は content-keyed cache から per-call で導出

---

## 12. 参照

- [memory-architecture-v2.ja.md](memory-architecture-v2.ja.md) —
  Records, contextbuild, summary cache 設計
- [memory-injection-hardening.ja.md](memory-injection-hardening.ja.md) —
  Pinned/Findings セキュリティモデル (v0.1.26)
- `internal/memory/pinned.go` — PinnedStore 実装
- `internal/findings/findings.go` — findings.Store 実装
- `internal/contextbuild/` — Memory v2 ContextBuilder
- `internal/agent/agent.go:1820+` — `extractPinnedMemories`
- `internal/chat/chat.go:110` — `BuildSystemPrompt` 集約

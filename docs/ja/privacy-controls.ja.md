# プライバシー制御 — 設計ノート

**Status:** 設計確定 (open questions 解消 2026-05-06); 実装着手可能。
**Targets:** v0.3.0 (v0.2.5 後継)

本書は関連 2 機能の設計を扱う:

1. **私的セッション (Private session)** — セッション単位の opt-in
   モード。跨セッション記憶 (Global Memory) への昇格を抑止し、
   セッション内体験は通常通り。
2. **ログプライバシー制御** — `app.log` のデフォルト冗長度を絞り、
   ユーザー発話・LLM 応答・ツール引数がディスクに漏えいする
   既定動作を停止。診断時のみ DEBUG opt-in。

両者に共通する主旨は: **ユーザーが意図せず保持してしまう
会話内容の永続化を防ぐ** こと。

---

## 1. 脅威モデル

shell-agent-v2 は single-user ローカルファースト app。ネットワーク
露出は最小限。実用上の漏えい経路は次の 3 つ:

- **`global_memory.json`** — 長寿命 JSON ファイル。一度入った
  fact は将来の全セッションのシステムプロンプトに再注入され、
  メモリ画面 / サイドバー screenshot で可視化される。
- **`app.log`** — install の寿命を持つ file-based log。再起動を
  跨いで永続。デフォルトで user message snippet, LLM 応答 head,
  ツール引数, 抽出 LLM reply (記憶候補 fact 文字列を逐語含有) を
  含む。
- **Session JSON** (`chat.json`, `session_memory.json`,
  `findings.json`) — per-session、UI で削除単位として消去可能。
  セッション内コンテンツとして許容範囲。

スコープ内: 上記 1, 2 の表面積を縮小。
スコープ外: ディスクレベルの暗号化、クラウド同期、外部 LLM
provider 側のデータ保持 (Vertex AI は GCP 側で本アプリと独立に
リクエストを log している)。

---

## 2. 私的セッション

### 2.1 ユーザー視点モデル

新規 chat の起動方法を 2 通りにする:

- **+ New Chat** (既存) — 通常セッション。auto-extraction で
  preference / decision を従来通り Global Memory に routing する。
- **+ New Private Session** (新規) — 上と同じだが私的フラグが
  立つ。Global-Memory 経路が抑止される。Session Memory と
  Findings は通常通り動作 (どちらも per-session でセッション
  削除と一緒に消える)。

UI で私的状態を **2 箇所** に surface する:
- **サイドバーセッションリスト** — タイトルの隣に **🔒 鍵
  アイコン**。
- **チャットペイン** — アクティブ会話の上部に **🔒 アイコン**。
  ユーザーがフォーカスを外さずに見える位置に。

二重表示は意図的: privacy は信頼上クリティカルな状態であり、
片方の indicator は見落とされうる (collapsed sidebar / scroll
位置 / etc.)。

セッションの私的化は **作成時に固定**。途中の toggle は
**不可** — 「私的で始めたから何も漏れない」と user が依存できる
よう、境界を曖昧にしない。

### 2.2 データモデル

```go
// memory/Record.go
type Session struct {
    ID      string
    Title   string
    Private bool          // NEW; default false; chat.json に persist
    Records []Record
}
```

`Private` は `chat.json` に persist されるので、復元したセッション
も privacy 設定を保持する。

### 2.3 Routing 変更

`agent.extractMemories` は既にカテゴリで routing している:

- `preference` / `decision` → GlobalMemoryStore
- `fact` / `context` → SessionMemoryStore

私的ゲート追加:

```go
isGlobal := category == "preference" || category == "decision"
if isGlobal && a.session.Private {
    logger.Debug("extractMemories: dropping global-route fact in private session: %q", fact)
    continue
}
```

Findings の扱い:

- **Auto-promote** (analyze-data の sliding-window 経由) → per-
  session findings に行く。影響なし (元々 per-session で、
  セッション削除と一緒に消える)。
- **Pin to Global Memory** (Finding / Session Memory entry に
  対する UI 操作) → user-initiated promotion。私的セッション
  では UI が ★ Pin ボタンを **非表示** にし、**binding 側でも
  reject** (server-side check) する。両方やる理由: stale UI /
  直接 binding 呼び出しが bypass しないように。これがないと
  「私的」のラベルが misleading になる。

### 2.4 Bindings + frontend

- `Bindings.NewSession() string` (既存) — 非私的セッション作成。
- `Bindings.NewPrivateSession() string` (新規) — `Private: true`
  でセッション作成。
- frontend に返す `SessionInfo` に `Private bool` を追加 →
  サイドバーが鍵アイコンを描画できるように。
- サイドバー bottom-nav: 既存の **+ New Chat** に隣接して新規
  **+ New Private Session** ボタン (鍵アイコン付き) を配置。

### 2.5 Edge cases

- **Busy 中のセッション切替**: 既存の Idle/Busy ゲートが適用
  される。変更なし。
- **stale ID で promote**: Pin handler は server-side で
  `a.session.Private` を確認する必要あり — UI 隠蔽だけでは
  不足 (binding は直接呼べる)。
- **Legacy セッション** (本リリース前に書かれたもの) は
  `Private` field を持たないが、JSON unmarshal で `false` (非
  私的) にデフォルトされる。マイグレーション不要。

### 2.6 監査ログ

私的セッションの作成と load 時に INFO レベルで log entry を
出力し、user が `app.log` で検証可能に:

```
[INFO] session created: id=<id> private=true
[INFO] session loaded:  id=<id> private=true
```

非私的セッションも対称性のため `private=false` で log する。
lifecycle event 自体は会話内容を漏らさない。トレードオフ
(「ログを読む attacker が『このセッションは私的だった』を
推測できる」) は許容: (a) single-user local app である、
(b) ログ自体が会話データと同じディスク上にある、(c) 検証性
の価値が speculative leak リスクを上回る。

---

## 3. ログプライバシー制御

### 3.1 現状の漏えい源 audit

`app.log` 出力のうち、ユーザー可視コンテンツを含むもの:

| 出力箇所 | 現 level | 内容 |
|----------|---------|------|
| `extractMemories: LLM reply (N chars): "..."` | INFO | 抽出 LLM の生 reply、記憶候補 fact 文字列を逐語埋込 |
| `agentLoop: response content=...` (truncate 200) | DEBUG | アシスタント応答 head |
| `agentLoop: message=...` (truncate 90) | DEBUG | ユーザー message head |
| `agentLoop: tool_call args=...` (truncate 200) | DEBUG | ツール引数 (パス / クエリ / プロンプトを含みうる) |
| `agentLoop: tool_result name=... result=...` (truncate 200) | DEBUG | ツール結果 head |
| `vertex parseResponse part[0]: ... textHead=...` | DEBUG | Vertex 応答 head |

DEBUG 出力は現状常時 disk に書かれている — level filter なし。
INFO 経由でも `extractMemories: LLM reply` が漏れている。

### 3.2 設計

**(a) `internal/logger` に level filter を追加。** Logger に
threshold (Debug / Info / Warn / Error) を設定可能に。閾値未満
の出力は disk に書かれずに drop。

**(b) Default level = Info。** DEBUG 出力 (会話内容の主体)
は user が opt-in しない限り抑制される。

**(c) 漏えい源の INFO を Debug に降格。** 具体的には:

- `extractMemories: LLM reply` → Debug

Lifecycle / event 系の INFO 呼び出し (`bg-task ...: start/done`,
`agentLoop: tool_call name=... args_len=...` (length only),
`Bindings.Abort: invoked`, `Agent.Abort: ...`) は INFO のまま
維持 — 「何が起きたか」を漏えいなく可視化できる、有用な情報。

**(d) 設定 surface。**

- `cfg.Logger.Level: "debug" | "info" | "warn" | "error"`
  (default `"info"`)
- Settings UI: General タブに "Log verbosity" select dropdown
  を追加。help text は「デフォルト `info` は user message,
  LLM 応答, ツール引数を `app.log` から除外します。問題再現の
  時のみ `debug` に切り替えてください」。
- 起動時に有効 level を INFO で 1 行 announce (user が確認可
  能なように)。

**(e) 範囲外 (将来作業)。**

- log file size cap / rotation。現状 `app.log` は無限に成長
  する。実問題だが本件と直交 — 0 byte log でも漏れていれば
  bad。別 PR で対処。
- lifecycle INFO 内の file path / IP 等の redaction。既存
  INFO surface は既に minimal、特定の漏えいが見つかったら
  再訪。

### 3.3 互換性

- 既存 logger `Init(dir string) error` API は不変。Level は
  Info にデフォルト。
- 新規 `SetLevel(Level)` を export し、`main.go` で `Init`
  後に config を適用できるように。
- 既存の `logger.Info` / `logger.Debug` / `logger.Error`
  呼び出しは全て as-is で動作; threshold を尊重するだけ。

---

## 4. 実装フェーズ

独立した 2 スライス。順序は任意。

### Phase A — ログプライバシー

1. `internal/logger`: `Level`, `SetLevel`, `Info` / `Debug`
   内で threshold check を追加。
2. `internal/config`: `LoggerConfig{ Level string }` を追加
   (default `"info"`)。
3. `main.go`: `Init` 後に `cfg.Logger.Level` を parse + 適用。
4. 既存 INFO 呼び出しを audit; `extractMemories: LLM reply`
   を Debug に降格。ツール引数を INFO で length-only に維持
   していることを確認。
5. Settings UI: "Log verbosity" select を追加。
6. テスト: logger threshold の unit test; Settings 変更の
   manual smoke。

### Phase B — 私的セッション

1. `memory.Session`: `Private bool` を追加; chat.json に
   persist。
2. `Bindings.NewPrivateSession() string` を追加; `ListSessions`
   が返す `SessionInfo` に `Private` を公開。
3. `agent.extractMemories`: privacy gate を追加。
4. `Bindings.PinSessionMemory` / `PinFinding`: アクティブ
   セッションが私的なら reject (UI が surface できるよう
   error 返却)。
5. 監査ログ: session create / load 時に INFO で `private=`
   フラグ付きに emit (§2.6)。
6. Frontend `Sidebar.tsx`: bottom-nav に **+ New Private
   Session** ボタンを追加 (**+ New Chat** の隣、鍵アイコン)。
7. Frontend session list: 私的行に 🔒 indicator を表示。
8. Frontend chat pane: アクティブ会話の上部に 🔒 indicator
   を表示 (active-session header 領域)。
9. Frontend Memory tab + Findings panel: 私的セッション中の
   行で ★ Pin ボタンを非表示 (Session Memory section +
   FindingsPanel の両方)。
10. テスト: 私的フラグ下での extraction routing と Pin handler
    reject の unit test; UI smoke (manual)。

---

## 5. 解決済み議題

(本来 "open questions" だったもの — 2026-05-06 解消)

1. **私的セッションでの Pin ボタン** — UI 非表示 + binding
   reject 両方やる (§2.3 / §2.4 を更新)。
2. **視覚識別** — サイドバー 🔒 indicator AND チャットペイン
   の上部にも 🔒 indicator (§2.1 を更新)。
3. **デフォルトログレベル** — `info`。シンプル、保守的、
   会話内容を `app.log` から既定で除外 (§3.2 (b) は as-is)。
4. **ログレベルの Settings UI** — Yes、General タブに "Log
   verbosity" select を追加 (§3.2 (d) は as-is)。
5. **私的セッションの監査ログ** — Yes、session create / load
   を INFO で log (`private=true|false`)。§2.6 を追加。

---

## 6. Non-goals

- `chat.json` / `session_memory.json` / `findings.json` の
  ディスク暗号化。OS レベルの data dir パーミッション (`0700`)
  + セッション削除時消去がストレージ保護。暗号化を加えるのは
  別の大きな議論 (鍵管理、recovery、performance)。
- per-message プライバシーマーカー。セッション単位の境界の
  方が reasoning 容易で user 期待 (「この chat 全体が private」)
  にも合う。
- privacy mode 有効化時の既存 `global_memory.json` 内容の
  retroactive 削除。privacy mode は将来動作のみ制御;
  retroactive purge は別の user action (サイドバー Global
  Memory の bulk delete が既存)。

---

## 7. 解決計画

§5 の resolved questions の上で、unit test (extraction
routing, Pin handler rejection, logger threshold) を書き、
本書の `Status` を `実装中` → ship 時に `実装完了 v0.3.0` に
更新。history entry は superseded されたら `docs/en/history/`
に移動; 当面は `docs/en/privacy-controls.md` (英語) /
`docs/ja/privacy-controls.ja.md` (日本語) として現状ドキュメント
の位置に置く。

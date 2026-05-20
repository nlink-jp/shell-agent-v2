# ADR-0021: ステートマシン整合性 — formal FSM + 権威ある Send 応答

- Status: Implemented in v0.14.0 (2026-05-20)
- Deciders: magi
- 関連: ADR-0015 (遅延抽出、本 ADR が改訂する設計)
- 部分的に置き換え: ADR-0015 §3.5 (event ベース UI ゲート) — event は補助に
  なり、Send 応答が権威ある phase を運ぶ

## 1. 背景

v0.13.3 のプロダクション報告: UI が Idle 表示なのにメッセージ Send で
backend が "QUEUED" を返却、UI が不整合な状態表示になり、その後の操作で
ドリフトが拡大する症状。

コード監査により 12 件の独立した invariant 違反を特定。すべての根本原因は、
ADR-0015 が **Wails event をフロントエンド UI state の権威ソースとして
扱った** という設計選択にある。

違反のクラス分類:

**Race condition (4 件)**
- V1: `extractionInFlight = true` を lock 内で書き、unlock 後に
  `extraction:started` event を emit。Send がこの window に来ると backend は
  "QUEUED" 返却するが、frontend はまだ Idle 表示のまま。
- V5: `extraction:done` で逆方向の同じレース。
- V6: `IsBusy()` が 3 つの mu 保護フィールドを 3 回別々の lock 取得で読む
  → 互いに非アトミック。
- (Wails の Go→JS event 境界は async。応答チャネルと event チャネルは
  同期されない。)

**Leak / 未管理 goroutine (3 件)**
- V2: Auto-dispatch goroutine (`go a.SendWithAttachments(...)` in
  postResponseTasks) が `postTasksWg` 未登録。`LoadSession.Wait()` がこれを
  待たずに return する。
- V7: `LoadSession` / `DeleteSession` / `Export` / `Import` がいずれも
  `extractionInFlight` / `queuedSend` をリセットしない。前セッションから
  stale flag が引き継がれる。
- V8: `Abort` は `queuedSend` をクリアするが、extraction context を
  cancel しても `extractionInFlight=true` を残す。

**Robustness (2 件)**
- V4: `extractMemories` の panic が trackBg 後の cleanup を bypass する。
  `defer wg.Done()` だけ走り、`wg.Wait()` は premature return。
- V10: `trackBg` に `defer recover()` なし。Title 生成の panic で同じ
  wg-drain-without-cleanup パターン発生。

**Frontend bug (2 件)**
- V3: `"QUEUED"` 文字列が `else if (response && response.trim())` 分岐に
  落ちて、assistant のチャットバブルとして append される。可視のチャット
  汚染。
- V9: `/model` backend switch が `postTasksWg.Wait()` を呼ばない。in-flight
  の title / extraction が再構築直前の backend を参照する可能性。

**仕様 gap (2 件)**
- V11: state machine の仕様が ADR-0015 の文章と code コメントに散在。
  FSM 図なし、enumerated invariants なし。
- V12: ADR-0015 が flag write と対応 event emit の順序を明文化していない。
  現状の「set → unlock → emit」順序が V1/V5 の温床。

## 2. 決定

「event を権威ソースとする」モデルを **`SendResult` を権威ソースとする**
モデルに置き換え、FSM を formal 化、すべての cleanup 経路を panic /
cross-session leak に対して堅牢化する。

### 2.1 Formal FSM

エージェントは 4 つの observable phase を持つ:

```
       ┌─────────┐  user Send  ┌──────┐
       │  Ready  │ ───────────▶│ Busy │
       │         │             │      │
       └─────────┘◀────────────┴──┬───┘
            ▲                     │ agentLoop returns,
            │   extraction        │ autoExtract = false
            │   completes,        │
            │   no queue          ▼
            │              ┌────────────┐
            │              │ Extracting │
            │              └─────┬──────┘
            │      auto-dispatch │ ▲ user Send
            │  ┌─────────────────┘ │
            │  │                   ▼
            │  │              ┌──────────┐
            │  │              │  Queued  │
            │  └──────────────┴──────────┘
            │      extraction completes,
            └──────  queue dispatch creates
                     new turn ─▶ Busy
```

**State** は triple `(state, extractionInFlight, queuedSend!=nil)` で
encode:

| Phase | state | extractionInFlight | queuedSend |
|-------|-------|---------------------|------------|
| Ready | Idle | false | nil |
| Busy | Busy | false | nil |
| Extracting | Idle | true | nil |
| Queued | Idle | true | non-nil |

**不正な組合せ** (絶対に観測されてはならない):
- `(Busy, true, *)` — extraction は agentLoop と overlap し得ない
- `(Idle, false, non-nil)` — extracting せずに queue があるのは無意味
- `(Busy, *, non-nil)` — queue は Extracting 中だけ存在すべき

### 2.2 権威ある Send 応答

`Send` / `SendWithImages` は構造化された `SendResult` を返す:

```go
type SendResult struct {
    // Phase は呼出側が表示すべき agent phase。
    // "completed" | "queued" | "command" | "error" のいずれか。
    Phase string `json:"phase"`

    // Content は Phase=="completed" 時の assistant のテキスト応答。
    Content string `json:"content,omitempty"`

    // CmdResult は Phase=="command" 時のスラッシュコマンド出力。
    CmdResult string `json:"cmd_result,omitempty"`

    // QueuedAt は Phase=="queued" 時に SEND がキューに入った時刻。
    QueuedAt string `json:"queued_at,omitempty"`

    // ErrorMessage は Phase=="error" 時の人間可読エラー。
    ErrorMessage string `json:"error_message,omitempty"`
}
```

frontend の `handleSend` は `Phase` を読んで分岐する。event 待ちなし、
マジック接頭辞の文字列スニッフィングなし。

`extraction:*` / `queued` / `queue_cleared` event は残るが、frontend が
発生させた応答に紐付かない state 変化 (auto-dispatch がユーザータイピング
中に発火、等) の補助信号になる。Send 後の UI 更新はこれらに依存しない。

### 2.3 Snapshot API

```go
type AgentSnapshot struct {
    Phase string // "ready" | "busy" | "extracting" | "queued"
    QueuedMessage string // Phase!="queued" のとき空
}

func (a *Agent) Snapshot() AgentSnapshot {
    a.mu.Lock()
    defer a.mu.Unlock()
    return a.snapshotLocked()
}
```

`Snapshot()` は 3 つの state field を 1 度の lock で読み、triple から
phase を導出。`IsBusy()` はこれの薄いラッパーに書き換える。frontend は
mount 時に呼んで、refresh / reconnect 後の state を event リプレイ無しで
復元できる。

### 2.4 Cleanup 経路

**Backend 堅牢化:**

1. `trackBg` が内部 `fn()` を `defer recover()` で包む。panic はログ + error
   変換され、trackBg 後の cleanup code は常に走る。
2. extraction goroutine の flag-clear を straight-line code ではなく deferred
   block 内で実行。順序: defer flag-clear を先に登録、続いて defer wg.Done()。
   defer は LIFO なので flag-clear が wg.Done() より前に走り、両方が任意の
   exit path (panic / normal) で走ることが保証される。
3. Auto-dispatch を `postTasksWg` (または LoadSession から並列に drain される
   専用 `queueDispatchWg`) に登録。lifecycle がセッションポインタを swap する
   前に必ず drain する。
4. `Abort` で `extractionInFlight` と `queuedSend` を一緒にクリア (今は
   queue だけ)。
5. `LoadSession` / `DeleteSession` / `Export` / `Import` / `NewSession` は
   `wg.Wait()` 後に共通の `resetStateMachine()` ヘルパーを呼んで
   `extractionInFlight = false`, `queuedSend = nil`, `state = StateIdle` を
   mu の下で防御的にリセット。正常 cleanup が走った場合は no-op、panic /
   leak 経路から復帰する保険。
6. `/model` (backend switch) は再構築前に `postTasksWg.Wait()` を呼ぶ。
   title / extraction が解放された client を参照することがなくなる。

**Frontend 変更:**

1. `handleSend` は `SendResult.Phase` を消費。`[CMD]` / `QUEUED` の
   magic-string sniffing を撤廃。
2. mount 時に `Snapshot()` を呼んで `state` / `extractionPending` /
   `queuedMessage` を seed。
3. `isBusy` を単一定義に統一: `isBusy = phase !== "ready"`。
   上位の `state==='busy' || postBusy` と ChatInput の
   `state==='busy' || extractionPending` の二重定義を解消。
4. `agent:extraction:*` / `agent:queued` / `agent:queue_cleared` を補助的な
   再読込トリガーとして扱う — 受信時に `Snapshot()` を再読込し、React state を
   piecewise に変更するのをやめる。シンプル化 + per-event mutation バグ回避。

### 2.5 Event 順序

Event は flag を書いたのと **同じ lock の critical section 内で** emit する。
Wails runtime の `EventsEmit` は non-blocking (buffered channel send) なので
contention を導入しない。backend の因果順序は「flag 書込 → event emit →
unlock」になり、次の lock 取得 reader は post-emit state を見る。JS→handler
の async hop は残るが、応答が権威ソースになったので frontend は event 順序に
頼らない。

## 3. 実装計画

12 commits を 2 phase に分けて。

**Phase A: Backend FSM 堅牢化 (プロトコル変更なし)**

1. `refactor(agent): resetStateMachine() helper + extraction cleanup via defer`
2. `feat(agent): trackBg panic recovery`
3. `fix(agent): Abort clears extractionInFlight`
4. `fix(agent): LoadSession / Delete / Export / Import reset FSM`
5. `fix(agent): /model waits postTasksWg before backend rebuild`
6. `fix(agent): auto-dispatch goroutine on postTasksWg`
7. `feat(agent): Snapshot() + atomic IsBusy`

**Phase B: プロトコル + frontend (内部 API breaking)**

8. `feat(agent): SendResult return type (Send / SendWithImages)`
9. `feat(bindings): SendResult DTO + Snapshot binding`
10. `feat(frontend): handleSend consumes SendResult; remove magic-string sniffing`
11. `feat(frontend): Snapshot-driven isBusy / state seeding on mount`
12. `test(agent): FSM invariant tests (各 invalid state を unreachable と assert)`

**Phase C: リリース**

13. `docs: CHANGELOG v0.14.0 + ADR-0021 Implemented`
14. `chore: release v0.14.0`

Minor bump (patch でなく) の理由:
- Send 応答 shape の変更 (内部だが Go/JS 境界で observable)。
- 振る舞い変更: Abort が extraction flag をクリアするように。cancelled
  extraction からの復帰が次ターンを待たず即時に。

## 4. テスト方針

### 4.1 FSM invariant test

各 invalid state combination について、public-API call の任意の sequence で
到達不能であることを assert する unit test:

```go
TestFSM_BusyAndExtractingNeverCoexist
TestFSM_QueuedRequiresExtracting
TestFSM_QueuedNeverDuringBusy
```

既存の `extractMemoriesOverride` テストフック + goroutine 一時停止チャネルで
駆動。

### 4.2 Race 回帰テスト

- `TestSend_DuringExtraction_ReturnsQueuedResult` — `Phase == "queued"` を
  assert、文字列 "QUEUED" ではないこと。
- `TestExtractionPanic_RecoversAndClearsFlags` — panic する extractFn override
  を装着; call 後の snapshot が `ready` であることを assert。
- `TestAbort_ClearsExtractionInFlight` — extraction 開始、abort、snapshot →
  ready。
- `TestLoadSession_ResetsStateMachine` — extractionInFlight=true を手動で
  leak、LoadSession で正規化されることを assert。

### 4.3 Frontend manual smoke

1. Vertex プロファイル + auto-extract on。Send → 応答待ち → 高速で再 Send。
   期待: 2 回目は queued pill 表示、"QUEUED" の assistant バブル無し、ChatInput
   が正しく disabled。
2. extraction 中に Abort。期待: ChatInput 即時 re-enable、stale extracting
   indicator なし。
3. extraction 中に refresh / dev-tools reload。期待: mount 時の snapshot で
   正しい UI が復元される。

## 5. Migration / 互換性

- **Settings ファイル**: スキーマ変更なし。
- **Session ファイル**: スキーマ変更なし。
- **Wails binding シグネチャ**: `Send` / `SendWithImages` が `(string, error)`
  から `(SendResult, error)` に変更。frontend は同 release で更新; 外部
  consumer なし。
- **Event payload**: 変更なし。frontend は引き続き listen するが primary state
  を駆動しない。
- **既存テスト**: 新戻り値型に合わせて更新。`extractMemoriesOverride` フックは
  引き続き動作。

## 6. Out of scope

- Wails event を poll-only model に置換。Event は spontaneous な state 変化
  (auto-dispatch 発火) には依然有用。
- per-session メモリストアや agentLoop 自体の再構築。本 ADR は FSM 整合性に
  特化。
- per-session FSM isolation (1 プロセス複数 agent)。v2 は single-agent;
  multi-session-active が機能要件になったら再訪。

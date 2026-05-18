# 抽出遅延化 + シングルスロット送信キュー — 設計ノート

**ステータス:** v0.11.0 で実装済み (2026-05-18)。
**ターゲット:** v0.11.0 (minor bump — user-visible state-machine
変更、新規 Wails イベント、破壊的 API 変更なし)。
**報告者:** ユーザー — 「Vertex AI を使ってもチャットの受け答えが
非常に遅い」。SEND から次入力可能になるまでの時間が、内容のある
ターンでは 10 秒を恒常的に超え、UI が post-response の memory
extraction LLM 呼び出し中ずっとロックされているため。

本ノートは**抽出遅延化モデル**を仕様化する: 可視レスポンス配信
直後に UI ロックを解除し、`extractMemories` はバックグラウンドで
継続実行。バックグラウンド抽出中に新たな SEND が来た場合は
**シングルスロットでキューイング**され、in-flight 抽出完了後に
キュー上の SEND が発射される。次ターンの `BuildSystemPrompt` は
常に直前ターン抽出 facts を含む memory を読む。Facts は失われない、
抽出は中断されない — **UI gating のみ** が変わる。

---

## 1. 問題

現状のターンライフサイクル (`agent.go` + `bindings.go`):

```
[1] ユーザー入力
[2] SEND  ─→  state = StateBusy  ─→  UI ロック
[3] agentLoop で LLM ラウンド
[4] レスポンス配信 (チャットに表示)
[5] postResponseTasks が title + extractMemories goroutine 発火
[6] postTasksWg.Wait() — 両方完了を待つ
[7] state = StateIdle  ─→  UI ロック解除
```

`extractMemories` (`agent.go:2258`) は直近会話 tail を分析し、
`preference`/`decision`/`fact`/`context` を Global Memory と
Session Memory に振り分ける別 LLM 呼び出し。**毎ターン 3-8 秒の
デッドタイム** を消費する。Title 生成 (初回のみ 3-5 秒) と
agent loop 本体 (tool calls 込で 5-15 秒) を合わせると、SEND から
次入力可能までの典型レイテンシは内容のあるターンで 10-20+ 秒。

composer (作文中のユーザー) が痛みを受ける: 既にレスポンスを読み、
次に何を聞きたいかわかっており、ロックされた入力欄を見ている。
残りの待機時間はすべてユーザーが直接観測しない memory bookkeeping
である。

### より単純な fix が却下された理由

- **新 SEND で抽出 abort** — facts を捨てる。データ分析セッション
  で extractMemories がユーザー preferences (「外れ値除外しない」)、
  暫定判断、派生 context (「このログは部署 X」) を捕捉するため、
  この損失は本質的。memory 抽出はシステムの売りの 1 つ。
- **純粋 fire-and-forget 抽出** — ユーザーが速く SEND すると
  turn N+1 の `BuildSystemPrompt` が turn N の抽出書き込み前に
  memory を読む。records には残るので最終的整合だが、turn N+1 の
  LLM は turn N の facts を見ない。
- **レスポンスのストリーミング** — 意図的に無効化されている
  (`vertex.go:74-83`): Gemini 2.5 Flash が `Thought=false` の text
  Part として `"THOUGHT\n…"`/`"思考\n…"` を返すバグがあり、これらが
  post-stream `CleanResponse` パスの前にチャットバブルに流れ込む。
  Gemini 3 (Part-level `Thought=true` が信頼可能) で再検討可能だが
  本 ADR スコープ外。
- **trivial ターンで抽出 skip** — 直交する最適化として有効だが、
  内容のあるターン (ユーザーが不満を持っているまさにそのターン)
  は助けない。

---

## 2. ゴール

1. **UI ロックは [4] (レスポンス可視化) で解除**、[7] (抽出完了)
   ではなく。composer は次のメッセージを即座に書き始められる。
2. **Facts 損失ゼロ**。抽出は常に完走する。新ターンの
   `BuildSystemPrompt` は常に前ターン抽出 facts を見る。
3. **シングルスロット SEND キュー**。バックグラウンド抽出中の
   SEND は抽出完了まで保持され、その後自動発射される。既にキュー
   にある状態での 2 回目の SEND は前のメッセージを**上書き** (ユー
   ザーは自分で修正できる — most-recent wins)。
4. **既存 Wails バインディング signature は不変**。frontend 挙動
   のみ変化。
5. **キャンセル可能**。現ターン、in-flight 抽出、キュー上 SEND
   すべて既存 Abort affordance 経由でキャンセル可。

非ゴール:

- **マルチスロットキュー**。チャット semantics: 1 ユーザー 1
  pending message。連続 SEND は最新だけ有効。順次発射すると
  最初のメッセージが先に history に消えて応答もないまま 2 番目も
  消える、という混乱したことになる。
- **`extractMemories` 自体の高速化**。本 ADR スコープ外。UI
  gating の話、LLM コスト削減の話ではない。
- **ストリーミング**。§1 の却下理由参照。別途追跡。
- **クロスターン抽出バッチ化**。各ターン依然個別抽出を発火。
  N ターンごとバッチ化は別 ADR で扱う。

---

## 3. 設計

### 3.1 State machine

可視 `agent.State` enum は不変 — `StateIdle` / `StateBusy` は
そのまま。変更は agent struct 内の新フラグ:

```go
type Agent struct {
    // 既存フィールド...
    state State // StateIdle / StateBusy

    // extractionInFlight: あるターンのレスポンス配信時点から
    // そのターンの extractMemories goroutine が return する時点
    // までの間 true。これが true の間、UI はアンロック
    // (state == StateIdle) だが SEND は queuedSend に保持され
    // 即座に開始されない。
    extractionInFlight bool

    // queuedSend: nil でない場合、extractionInFlight 中に受信した
    // 最新の SEND。in-flight 抽出完了直後に発射される。2 回目の
    // SEND はこのフィールドを上書き (single-slot、most-recent-wins)。
    queuedSend *queuedSend
}

type queuedSend struct {
    Message            string
    ImageObjectIDs     []string
    ImageDataURLs      []string
    DocumentObjectIDs  []string
    QueuedAt           time.Time
}
```

両フィールドは `a.mu` (既存 mutex) で保護。

### 3.2 Send エントリポイント変更

`Agent.SendWithAttachments` (`agent.go:682`) に 1 分岐追加:

```go
a.mu.Lock()
switch {
case a.state == StateBusy && !a.extractionInFlight:
    // 真の busy — agentLoop 進行中。従来通り拒否。
    a.mu.Unlock()
    return "", ErrBusy
case a.state == StateIdle && a.extractionInFlight:
    // レスポンス配信済、抽出進行中 — キューへ。
    a.queuedSend = &queuedSend{
        Message: message, ImageObjectIDs: imageObjectIDs,
        ImageDataURLs: imageDataURLs,
        DocumentObjectIDs: documentObjectIDs,
        QueuedAt: time.Now(),
    }
    a.mu.Unlock()
    a.emitQueued()  // §3.4 のイベント参照
    return "QUEUED", nil
case a.state == StateIdle:
    // 通常ケース — 進む。
    a.state = StateBusy
    ctx, a.cancel = context.WithCancel(ctx)
    a.mu.Unlock()
    return a.agentLoop(ctx, message, imageObjectIDs, imageDataURLs, documentObjectIDs)
}
```

frontend は `"QUEUED"` を入力 clear 目的で成功 SEND と同様に扱う:
メッセージはシステムに受け取られた、ただし未処理。

### 3.3 postResponseTasks の変更

現行 (`agent.go:1740`):

```go
a.postTasksWg.Add(2)
go { a.trackBg(ctx, "title", generateTitle) }
go { a.trackBg(ctx, "memory-extraction", extractMemories) }
go {
    a.postTasksWg.Wait()
    // state = StateIdle
}
```

新形:

1. **State は postResponseTasks 入口で即 Idle** に遷移、両 bg
   goroutine launch 前。可視レスポンスは postResponseTasks が
   呼ばれた時点で既に画面表示済み (agentLoop の defer 経由)。
   *いかなる* bg work で Idle を gate しても初回ターンの title-gen
   レイテンシ 3-5 秒が Vertex AI でも残り、本 ADR を motivate した
   元の不満が解消されない。
2. **Title と extraction は両方とも state machine から外れた bg
   goroutine で走る** が `postTasksWg` で追跡され、LoadSession /
   Export / tests が drain 可能。
3. **`a.extractionInFlight` がキュー / UI tint の専用シグナル**、
   `state` とは独立。

```go
// State → Idle を entry で即時。両 bg goroutine はこの後で launch、
// state machine とは独立して走る。
a.mu.Lock()
a.postCancel = cancel
a.extractionInFlight = true
a.state = StateIdle
a.mu.Unlock()
a.emitExtractionStarted()

// Title 生成 — bg goroutine、session.Title が "New Session" の
// ときだけ発火。Vertex は速く local は遅め; いずれにせよ state
// とは decouple。
a.postTasksWg.Add(1)
go func() {
    defer a.postTasksWg.Done()
    a.trackBg(ctx, "title", generateTitle)
}()

// Memory extraction — ユーザーの次メッセージ作文ウィンドウと並行。
// 完了時にキュー上 SEND を dispatch。
a.postTasksWg.Add(1)
go func() {
    defer a.postTasksWg.Done()
    a.trackBg(ctx, "memory-extraction", extractMemories)

    a.mu.Lock()
    a.extractionInFlight = false
    queued := a.queuedSend
    a.queuedSend = nil
    a.mu.Unlock()
    a.emitExtractionDone()

    if queued != nil {
        // キュー上 SEND を自動 dispatch。agentLoop 直接呼びでなく
        // SendWithAttachments を呼ぶことで通常の state machine、
        // MITL hooks、イベント emitter が走る。
        go a.SendWithAttachments(
            a.baseCtx,
            queued.Message,
            queued.ImageObjectIDs,
            queued.ImageDataURLs,
            queued.DocumentObjectIDs,
        )
    }
}()
```

`a.baseCtx` は startup (`Agent.Start`) で捕捉する long-lived
context。per-turn cancellable ctx ではない。キュー上 SEND は
キューイングしたターンより長く生きる必要があるため。

### 3.4 Abort

`Agent.Abort` (`agent.go:725`) にキュークリア責任が追加される。
既存挙動 (現ターン cancel + post-response goroutine cancel) は
維持、新挙動は:

```go
func (a *Agent) Abort() {
    a.mu.Lock()
    cancel := a.cancel
    postCancel := a.postCancel
    queued := a.queuedSend
    a.queuedSend = nil
    state := a.state
    a.mu.Unlock()

    if cancel != nil { cancel() }
    if postCancel != nil { postCancel() }
    if queued != nil {
        a.emitQueueCleared() // §3.5 参照
    }
    _ = state
}
```

抽出中の Abort:
- 抽出 goroutine は ctx cancellation を受信、early return。partial
  抽出 facts は捨てられる。**これは許容**: 明示的 Abort は暗黙的な
  「速度優先」より強いユーザー意図。
- キュー上 SEND があれば消える (ユーザーは会話フロー自体を放棄
  している)。

### 3.5 Frontend 向け Wails イベント

deferred-extraction state を frontend が区別表示するための新イベン
ト 4 種:

| イベント | Payload | タイミング |
|---------|---------|----------|
| `agent:extraction:started` | `{turn: int}` | レスポンス配信後、抽出 goroutine 開始前 |
| `agent:extraction:done` | `{turn: int, success: bool}` | 抽出 goroutine 復帰後 (成功 or エラー) |
| `agent:queued` | `{at: ISO8601}` | SEND がキュースロットに格納された後 |
| `agent:queue_cleared` | `{}` | Abort がキューを drain した後、または auto-dispatch が消費した後 |

既存 `agent:activity` の `tool_start`/`tool_end` 等は新ターン
(キューが dispatch した) でも引き続き発火。frontend は通常 SEND
と同様に処理。

### 3.6 Frontend UX

#### 入力バーの状態

ADR 後、入力欄は意味のある 3 状態:

1. **Ready** — `state == StateIdle && !extractionInFlight`。SEND
   ボタン緑、hint 空。完全に ready。
2. **Extraction in flight** — `state == StateIdle &&
   extractionInFlight && queuedSend == nil`。SEND ボタンは active
   だが tinted (例: amber)、小さなインラインヒント:
   > "⏳ Memory 抽出中 — 次のメッセージは抽出完了後に送信されます"
3. **Queued** — `state == StateIdle && extractionInFlight &&
   queuedSend != nil`。composer エリアは clear (メッセージはキュー
   へ送信済)、入力上部に status pill 表示:
   > "Queued: '<最初の 60 文字>' — memory 抽出完了時に送信 ✕ で
   > キャンセル"
   ✕ は Abort を呼ぶ。

State (3) のビジュアルモック:
```
┌──────────────────────────────────────────────┐
│  [エージェントの直近レスポンス]              │
│                                              │
│  ⏳ Queued: "How about the time series view…" │
│      抽出完了時に送信されます ✕              │
│  ┌──────────────────────────────────────────┐│
│  │ (入力 clear — 次のメッセージ準備可)      ││
│  └──────────────────────────────────────────┘│
└──────────────────────────────────────────────┘
```

pill は `agent:queue_cleared` または turn auto-dispatch で消える。

#### ステータスバー

既存 `input-status-bar` にオプショナルインジケータ 1 つ追加 (小、
右寄せ、backend バッジの隣):

> `⚙ extracting…`

`extractionInFlight && !queuedSend` の間表示、SEND がキュー入りすると
上記の queue pill に置換される。

---

## 4. エッジケース

1. **抽出中の複数 SEND**。シングルスロット、most-recent-wins。
   2 回目 SEND が `queuedSend` を上書き、1 回目は silent drop。
   UI は queue pill を新メッセージで更新。*根拠*: ユーザーは自分を
   修正している。両方表示は雑然。順次発火は驚き。

2. **`StateBusy` (レスポンス生成中) の SEND**。今日と同じく
   `ErrBusy` で拒否。frontend は既に `state === 'busy'` で gate;
   この挙動継続。

3. **抽出中の Abort**。§3.4 参照: 抽出 cancel、キュー clear。
   partial 抽出 facts は捨てられる。Abort されたターンは明示的な
   ユーザー選択 — そこで memory bookkeeping を失うのは OK。

4. **抽出失敗 (timeout / LLM error)**。エラーは log (`trackBg` が
   実施済)。`agent:extraction:done` イベントは `success: false`
   で発火。キュー上 SEND があれば依然 dispatch — 抽出失敗はユーザ
   ーの責任ではない。失敗時点の memory state は LLM 応答パーサが
   失敗前にどこまで処理できたかによる。

5. **抽出中のセッション管理操作 (切替 / 削除 / export / import /
   rename / 新規)**。ブロックする。抽出 goroutine は agent
   フィールド (`a.session.Records`、`a.sessionMemory`、
   `a.globalMemory`、`a.findings`) を参照しており、これらは
   セッション変更で差し替わる。切替を許可するとセッション A の
   tail から導出された facts がセッション B の memory ファイルに
   landing する、または共有 `global_memory.json` で 2 つの抽出
   (A の tail と B の初ターン) が race する。削除はさらに悪化:
   抽出が削除中のファイルに書き込み中になりうる。

   実装: 既存の frontend gate (`handleLoadSession` in
   `App.tsx:510`: `state === 'idle' && bgTasks.length === 0`)
   は抽出 goroutine が `trackBg` で登録され続けていればこれを
   既にカバーする。新設計はその登録を維持 — 抽出は持続中も
   `bgTasks` に居る; ただ `state` 遷移を gate しないだけ。
   セッション管理バインディング (`LoadSession`, `DeleteSession`,
   `ExportSession`, `ImportSession`, `RenameSession`,
   `NewSession`, `NewPrivateSession`) はすべて同じ idiom を使うので
   自動的にブロックを継承。バインディング個別の変更なし。

   ビジュアル: サイドバーのセッション entry は
   `extractionInFlight === true && state === 'idle'` の間
   `cursor: not-allowed` + tooltip "Memory 抽出中…"。チャット
   ペインの `⏳ Memory 抽出中…` インジケータが主シグナル;
   サイドバー disable は誤クリック防止の副 affordance。

6. **抽出中のアプリ終了**。`OnBeforeClose` (`main.go:56`) は既に
   `IsBusy()` で gate。`IsBusy()` を `extractionInFlight` が true
   *もしくは* `queuedSend != nil` でも true を返すよう拡張。抽出
   中の終了は書き込み途中で facts を失う可能性 — clean まで block。

7. **キュー上 SEND auto-dispatch と manual Abort の race**。
   queue-dispatch goroutine は `queuedSend` を `a.mu` 下で読み、
   SendWithAttachments 呼び出し前に lock を release。lock release
   と実 send の間に Abort が発火した場合、`queuedSend` は既に
   dispatch goroutine 自身で cleared なので Abort の clear は no-op。
   in-flight SendWithAttachments が lock を取り、state = Idle (or
   別経路と race して Busy) で進む。デッドロック無し; 最悪ケースは
   ユーザーがキャンセルしたつもりのキュー上 SEND が 1 回発射されて
   しまうが、ユーザーは新ターンを即 Abort 可能。許容。

8. **Trivial turn (tool call 無、短いレスポンス)**。旧モデルでは
   ターンから次入力可能までの総レイテンシは `extractMemories` 支配。
   新モデルでは UI が [4] で解除。ユーザーが抽出より早く作文する
   ならキューが lit up; そうでなければターンは即時に感じる。これが
   主たる狙い。

9. **Long-running turn (多くの tool call)**。体感変化なし — agentLoop
   持続中は `state == StateBusy`、UI ロック、従来通り。新機構は
   *post-response* gate のみ変更、in-loop gate は不変。

10. **Title 生成も state machine の gate から外す**。当初ドラフトは
    「短いし初回のみ」の根拠で title を wg に残していたが、観測された
    初回ターンレイテンシは Vertex AI でも title 単独で 3-5 秒 (ローカル
    LLM はさらに長い)。Title も pure bg goroutine として extraction
    と合流 — title は response 描画後数秒でサイドバーに反映され、
    他のチャットアプリと同じ UX。両方とも `postTasksWg` 上に居続け、
    session state mutation 前に LoadSession / Export / tests が
    drain 可能。

---

## 5. 却下した代替案

### 5.1 新 SEND で抽出 abort (チャットの B-1)

却下 (§1): データ分析セッションは抽出 facts (ユーザー preferences、
派生 context、判断) を重視。ユーザーが速く打ちたいだけで facts を
捨てるのは間違ったトレードオフ。

### 5.2 キューなし純粋 fire-and-forget (B-2)

却下: turn N+1 の `BuildSystemPrompt` が turn N の抽出書き込み前に
memory を読む。records は残るので情報は context-build 経由で技術的
に届くが、圧縮版 Global Memory が cross-session 価値の load-bearing
キャリア。Eventually consistent != consistent within a session。

### 5.3 マルチスロット SEND キュー (FIFO)

却下 (§2 Non-goals): チャット semantics。メッセージ A キュー中に
B を作文するのは稀かつ混乱。シングルスロット most-recent-wins は
他チャット UI が SEND 前再打字に行う挙動と一致。

### 5.4 N ターンごとバッチ抽出

検討。総抽出負荷を ~1/N に削減。しかし:
- memory-extraction trigger ロジックの複雑化。
- 「facts rot」リスク — 抽出可能だった memory が N-1 ターン遅れて
  surface する。
- UI レスポンス性ゴールと直交; N 番目のターン (full extraction
  コスト) は助けない。
- LLM コスト面が問題なら別 ADR。

### 5.5 抽出をストリーミング要約に置換

検討。抽出仕事を agent loop と interleave、コスト amortize。しかし:
- `extractMemories` を partial 会話 tail 消費に再構成必要。
- レスポンスパイプラインに触る、いじり方を間違えるとリスク大。
- 本 ADR の deferred-extraction model は抽出パイプラインに一切
  触らずユーザー可視ゴール (UI レスポンス性) を達成。低リスク、
  シンプル。

---

## 6. テスト / 不変条件

### 6.1 バックエンド (Go)

- **`TestAgent_DeferredExtraction_UIUnlocksBeforeExtraction`**
  — `Send` 駆動 → レスポンス待ち → mock 抽出器が return する
  *前* に `State() == StateIdle` をアサート。
- **`TestAgent_QueuedSend_FiresAfterExtraction`** — Send、
  レスポンス待ち、抽出進行中の 2 回目 Send、2 回目がキュー入りを
  アサート; mock 抽出を完了させる; 2 回目 Send が自動発射、新
  レスポンスを生成することをアサート。
- **`TestAgent_QueueOverwrite`** — 抽出中に 3 連続 Send;
  3 回目のみ発射をアサート; 上書き間に `queue_cleared` イベントが
  NOT 発火することをアサート (上書きは silent)。
- **`TestAgent_AbortClearsQueue`** — Send、抽出中の 2 回目 Send、
  Abort; キュー上 SEND が clear されることをアサート、どちらも
  発射されない。
- **`TestAgent_ExtractionErrorStillDispatchesQueue`** — mock
  抽出器がエラーを返す; キュー上 SEND は発射される。
- **`TestAgent_IsBusyDuringExtraction`** — 抽出 in flight →
  `IsBusy()` が true を返す (OnBeforeClose が quit を gate する
  ため)。

### 6.2 Frontend (手動スモーク + seeder)

`cmd/seed-objlink-smoke` を extractionInFlight 状態に事前
positioning させる? overkill。代わりに:

- 手動スモークステップ: アプリを開き、agentLoop を exercise する
  内容のあるメッセージを送信 (例: `analyze-data` 呼び出し)、観察:
  - レスポンス出現、入力バーが即座に active になる
  - `⏳ Memory 抽出中…` インジケータ可視
  - ウィンドウ中に 2 回目メッセージ送信 → "Queued: …" pill 表示、
    入力欄 clear
  - 待つ → queue pill 消失、新ターン開始
  - キューしたターンの BuildSystemPrompt が実際に前ターン facts を
    見たことを検証 (例: system prompt 内容の debug log 経由)

### 6.3 構造的

新規構造的不変条件なし。state machine 拡張は `a.mu` 下にカプセル化
されており、既存 race-free 保証が保たれる。

---

## 7. 互換性

破壊的変更なし。

- **既存 API**: `Send` / `SendWithImages` / `Abort` / `IsBusy`
  signature 不変。`Send` は今後エージェントの応答テキストでなく
  文字列 `"QUEUED"` を *成功値* として返すことがある。frontend は
  既に入力 clear を成功シグナルとして扱っているので影響なし; 応答
  自体は `agent:activity` / message-list イベント経由で届く。
- **既存イベント**: `agent:stream`、`agent:activity`、
  `session:title` 等不変。新イベント (`agent:extraction:started/done`、
  `agent:queued`、`agent:queue_cleared`) は加算。
- **既存セッション**: スキーマ変更なし; chat.json、global_memory.json、
  session_memory.json 全て不変。
- **既存テスト**: 拡張、置換なし。

---

## 8. フェージング

単一 PR。コミット:

1. `feat(agent): extractionInFlight + queuedSend state + tests`
2. `refactor(agent): move extractMemories off postTasksWg`
3. `feat(agent): dispatch queued SEND when extraction completes`
4. `feat(bindings): IsBusy reflects extractionInFlight; Abort clears queue`
5. `feat(events): agent:extraction:started/done + agent:queued/queue_cleared`
6. `feat(frontend): input-bar tinted state + queue pill + Abort
   wiring + status-bar indicator`
7. `docs: ADR-0015 + CHANGELOG v0.11.0 + reference 更新`

---

## 9. 範囲外

- レスポンス出力ストリーミング (Gemini 3 で Part-level Thought
  シグナリングが安定するまで延期)。
- Trivial ターンの抽出 skip (直交する別最適化)。
- N ターンごとバッチ抽出 (必要なら別 ADR)。
- `extractMemories` LLM モデル/プロンプトチューニング (別問題)。

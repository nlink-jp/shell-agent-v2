# バックグラウンドタスク表示

## ステータス

v0.1.19 でリリース済み。フッタバッジ、post-task Busy ゲート
（post-task 走行中は入力欄 + Sidebar の New / Load / Delete を
ロック）、対称な INFO / ERROR ログ（`done` / `canceled` / `<err>`）
すべて稼働中。本ドキュメント早期改訂で記述された auto-cancel 案は
試行 → 撤回。経緯は §「post-task 完了まで Busy を維持」を参照。

## 課題

チャット応答が画面に返った後、エージェントは `postResponseTasks` で
3 本の goroutine を走らせる:

1. `generateTitleIfNeeded`  — 新規セッションのタイトル生成
2. `compactMemoryIfNeeded`  — warm 履歴のサマリー化
3. `extractPinnedMemories`  — ユーザ向け事実の抽出

いずれも LLM を呼ぶため、数秒〜数分（429 リトライ時はさらに長く）
かかる。今の挙動は **完全に不可視**:

- 応答が返ると UI は即 Idle に戻る
- 裏で何が走っているかの表示がない
- 429 リトライが発生すると、次の `Send` が `postTasksWg.Wait()` で
  止まるが、UX 上は理由不明の「固まり」になる
- 失敗してもログを開かない限り気づけない

`b8ebb2f` で入れた `postCancel` は Abort で中断できるようにした
だけで、不可視性そのものには手をつけていない。

## ゴール

- 何が裏で走っているか・どれくらい走っているかをユーザに見せる
- 次の `Send` で自動的にキャンセルする（手動 Abort 不要）
- 失敗を短く可視化する（しつこくない範囲で）
- 既存の Wails イベント駆動 UI（`agent:activity` /
  `sandbox:build:line` 等）と整合する

## 非ゴール

- 進捗バー（自然な進捗指標がない）
- 過去のバックグラウンドタスク履歴ビュー
- ユーザ向けのリトライ／再開操作

## 設計

### バックエンド — タスク状態のストリーム

`internal/agent` に追加:

```go
type BgTaskEvent struct {
    Name  string // "title" | "memory-compaction" | "pinned-extraction"
    Phase string // "start" | "end"
    Error string // Phase=="end" でタスクが失敗した時のみ
}

type BgTaskHandler func(BgTaskEvent)
```

`Agent` に省略可能な `bgTaskHandler` フィールドを足し、`bindings.go`
が起動時に一度だけ登録する（既存の stream / activity コールバックと
同じパターン）。`postResponseTasks` は各 goroutine をヘルパで包む:

```go
func (a *Agent) trackBg(ctx context.Context, name string, fn func() error) {
    logger.Info("bg-task %s: start", name)
    a.notifyBg(BgTaskEvent{Name: name, Phase: "start"})
    err := fn()
    msg := ""
    switch {
    case err == nil:
        logger.Info("bg-task %s: done", name)
    case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
        // 次 Send による auto-cancel。失敗扱いはしないが、
        // 「ユーザが完了前に入力したからタスクが切られた」という
        // 因果関係を後から追えるようログには必ず残す
        logger.Info("bg-task %s: canceled (next Send)", name)
    default:
        logger.Error("bg-task %s: %v", name, err)
        msg = err.Error()
    }
    a.notifyBg(BgTaskEvent{Name: name, Phase: "end", Error: msg})
}
```

タスクは error を返し、`trackBg` がログ出力 + 通知を担当する。
ログは対称: `start` の後に `done` / `canceled` / エラー行のいずれか
1 件だけ。`canceled` は赤フラッシュは出さないが、ログには必ず残す
（「このセッション、なぜ最後までタイトルが付かないのか」を後で
調べる時、無言の不在ではなくキャンセル痕跡が見えるように）。

### post-task 完了まで Busy を維持

初期案では「次の Send 開始時に古い post-task を auto-cancel」を
試したが、以下 2 点で破綻した:

1. **ローカル LLM は遅い**。post-task が無事終わるより先にユーザが
   次のメッセージを打ってしまうのが普通の対話ペースで起こりうる。
   pinned-fact extraction がこの度に毎回 cancel されると pinned
   store に何も溜まらず、ユーザは気づかぬ間に欠損する。
2. **タスクは全部 idempotent ではない**。pinned-fact extraction は
   直近 hot レコード 4 件しか見ないので、cancel されたら次回に
   持ち越されるのではなく **失われる**。

正解はユーザメッセージ受領から **3 タスクすべての完了** までの
間 `StateBusy` を維持すること。Wails のフロントエンドは Busy で
入力欄を disable するので、その間ユーザは物理的にメッセージも
スラッシュコマンドも打てない。

```go
// SendWithImages — Busy ガート → agentLoop → defer で
// postResponseTasks 起動。完了後 goroutine が state=Idle に戻す。

a.mu.Lock()
if a.state != StateIdle {
    a.mu.Unlock()
    return "", ErrBusy
}
a.state = StateBusy
ctx, a.cancel = context.WithCancel(ctx)
a.mu.Unlock()

return a.agentLoop(ctx, message, objectIDs, dataURLs)
// agentLoop の `defer a.postResponseTasks(ctx)` が success/error
// すべての return path をカバーする。postResponseTasks の末尾
// goroutine が WaitGroup 完了後に state=Idle に戻す。
```

スラッシュコマンドは Busy 窓内で処理して二重実行を防ぐが、
agentLoop には行かないので post-task もトリガしない。直接 state
を Idle に戻して return する。

`Abort` がユーザの逃げ道。`cancel`（in-flight agentLoop）と
`postCancel`（post-task ctx）を両方発火し、postResponseTasks の
末尾 goroutine が成功時と同じく state を Idle に戻す。

### バインディング — Wails イベント橋渡し

`bindings.go::onAgentReady` 付近で:

```go
agent.SetBgTaskHandler(func(e BgTaskEvent) {
    wailsRuntime.EventsEmit(b.ctx, "bg-task:" + e.Phase, map[string]any{
        "name":  e.Name,
        "error": e.Error,
    })
})
```

イベント名は `bg-task:start` と `bg-task:end` の 2 種。既存の
コロン区切り命名規約に揃える。

### フロントエンド — フッタ表示

メインカラム下部（入力欄の上）に細い行を追加。アクティブなタスク
無し・失敗フラッシュ無しの時はそもそも DOM に出さない。

状態（`App.tsx` か小さい dedicated context で保持）:

```ts
type BgTask = { name: BgTaskName; startedAt: number };
type BgTaskFailure = { name: BgTaskName; error: string; at: number };

const [active, setActive] = useState<BgTask[]>([]);
const [failure, setFailure] = useState<BgTaskFailure | null>(null);
```

`bg-task:start` で `active` に push、`bg-task:end` で削除。
`error !== ''` なら `failure` をセットし 5 秒後にクリア。

描画ルール:

- `active` 空 + `failure` 無し → フッタ非表示（DOM なし）
- `active` 有り → 灰色行、`"処理中: タイトル生成, メモリ圧縮"`
  （ラベル表はフロントで i18n）
- `failure` 有り → 赤行、`"失敗: メモリ圧縮 (timeout)"`、5 秒で自動
  フェード。新タスクが立ち上がったら通常の灰色行を優先表示。

ラベル表:

| code              | ja            | en                       |
|-------------------|---------------|--------------------------|
| title             | タイトル生成   | Title generation         |
| memory-compaction | メモリ圧縮     | Memory compaction        |
| pinned-extraction | 注目情報抽出   | Pinned-memory extraction |

### 失敗の扱い

| 結果              | ログ                               | UI                              |
|-------------------|------------------------------------|---------------------------------|
| 成功              | `INFO bg-task <name>: done`        | 静かに消える                     |
| auto-cancel       | `INFO bg-task <name>: canceled`    | 静かに消える                     |
| エラー            | `ERROR bg-task <name>: <err>`      | 5 秒赤表示 + タスク名 + 短文    |

`canceled` はエラー率を汚さないよう INFO で残すが、必ずログに出す。
「このセッションになぜタイトルが付かないのか」を調べる時、不在では
なくキャンセル痕跡として追えるようにするのが目的。

## テスト戦略

- `agent_test.go`:
  - `postResponseTasks` が各タスクに対し `Phase: "start"` /
    `Phase: "end"` を発火する
  - `parentCtx` がタスク途中でキャンセルされた場合、`end` イベント
    の `Error` は空（cancel は失敗扱いしない）
  - cancel 以外の error を返したタスクは `end` の `Error` に
    メッセージが入る
- フロント（jest/Vitest が入っていれば、無ければ手動）:
  - reducer が複数タスクの start/end を正しく集約する
  - 5 秒タイマーで failure がクリアされる

## 明示的に非対象

- 各タスクの LLM トークンコスト表示
- タスク単位の abort ボタン
- 過去ログビュー（既存ログで足りる）

## 影響ファイル

- `app/internal/agent/agent.go`  — ハンドラフィールド、`trackBg`、
  `postResponseTasks` への配線、ヘルパ関数からの error 返却
- `app/bindings.go`  — ハンドラ登録、イベント発火
- `app/frontend/src/App.tsx`  — `bg-task:*` 購読、状態管理
- `app/frontend/src/components/BgTaskFooter.tsx`（新規）  — 描画
- `app/internal/agent/` 配下のテスト

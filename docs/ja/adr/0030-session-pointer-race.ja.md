# ADR-0030: agentLoop と title 生成間の `a.session` ポインタアクセスを同期する

- Status: Accepted
- Deciders: magi
- Related: ADR-0015 (遅延抽出 + auto-dispatch), ADR-0021 (FSM 整合性), Issue #13 (本レースを surface した flaky tests)

## 1. コンテキスト

Issue #13（flaky な `TestAutoDispatch_DrainedByWait` と
`TestQueuedSend_ExtractionErrorStillDispatches`）を調査中に、Go race
detector が本体コードの実データレースを別個に捕捉した:

```
WARNING: DATA RACE
Read at 0x… by goroutine 96 (title 生成):
  agent.go:2920  generateTitleIfNeeded   ← a.session == nil || a.session.Title …
  agent.go:2194  postResponseTasks.func1.1   (title goroutine)

Previous write at same addr by goroutine 98 (auto-dispatch SEND):
  agent.go:1925  agentLoop                ← a.session = &memory.Session{…}
  agent.go:1157  SendWithAttachments
  agent.go:2272  postResponseTasks.func2.1.1   (auto-dispatch goroutine)
```

`postResponseTasks` は title 生成 goroutine を spawn し、同一ターン内で
抽出完了の cleanup が queued SEND を別 goroutine で auto-dispatch しうる。
両者が同期なしで `a.session` を触っている:

- `agentLoop` (1925行): `if a.session == nil { a.session = &memory.Session{...} }`
- `generateTitleIfNeeded` (2920行): `if a.session == nil || a.session.Title != ...`

通常の本番利用では `a.session` は SEND 前に `LoadSession` で必ず初期化される
ので、`agentLoop` の nil-init は防御的 no-op となり race は稀にしか
顕在化しない。しかし `LoadSession` 起源の書き込みと同時並行の title 生成
読み出しの間にも同じ race は存在する — テストパスは単に `LoadSession`
を呼ばないことで決定論的にしているだけ。いずれにせよ無同期のポインタ
read/write は不正。

## 2. 決定

最小の **lock 下スナップショット** パターンを2箇所に適用:

### 2.1. `agentLoop` (1923行付近)

nil-init を `a.mu` 下に移し、書き込みを並行リーダーと同期させる:

```go
func (a *Agent) agentLoop(ctx context.Context, ...) (string, error) {
    a.mu.Lock()
    if a.session == nil {
        a.session = &memory.Session{ID: "default", Records: []memory.Record{}}
    }
    a.mu.Unlock()
    ...
}
```

### 2.2. `generateTitleIfNeeded` (2918行付近)

エントリで lock 下に `a.session` を1度スナップショットし、以降はローカル
コピーで操作:

```go
func (a *Agent) generateTitleIfNeeded(ctx context.Context) error {
    a.mu.Lock()
    sess := a.session
    a.mu.Unlock()
    if sess == nil || sess.Title != "New Session" {
        return nil
    }
    ...
    sess.Title = title
    _ = sess.Save()
    ...
    h(sess.ID, title)
}
```

スナップショットはポインタ。以降の `sess.Title` / `sess.Records` の読みと
`sess.Title` への書きは pointee へ向かう。これがここで許される理由は、
pointee（`memory.Session` 構造体）は title 生成中はその goroutine の所有
として扱われる前提があるため — title 生成の短い window 中に他 goroutine
がその特定 `*Session` を mutate することは無い、という暗黙の所有モデル。
detector が捕捉したのは純粋に `a.session` *フィールド*（ポインタ slot）の
race で、`Session` 構造体内部のフィールド race ではない。

## 3. これで十分な理由

- race detector のレポートは `a.session` フィールドの並行 read/write を
  指摘するもの。ポインタを lock 下でスナップショットすればその field 上の
  data race は除去される。
- `Session` 構造体内部の可変性は別の関心事。今日 `LoadSession` は `a.mu`
  下で `a.session` を新しいポインタにスワップする原子操作なので、旧
  `*Session` をスナップショットした title goroutine は古いセッション上で
  安全に動作し続ける。これは既存の暗黙の所有モデルと合致。
- パッケージ内の全 `a.session` 読み書きを網羅監査するのは本 ADR のスコープ外
  — detector が canonical な signal であり、本修正は detector が報告した
  対をそのまま潰す。後続の `-race` 実行で別の対が surface すればそれぞれ
  別 ADR を起こすか straight bug fix として畳み込む。

## 4. 検討した代替案

- **`a.session` を `atomic.Pointer[memory.Session]` に置換。** ポインタ
  slot 自体は綺麗になるが侵襲的（パッケージ内全 reader/writer 更新）かつ
  contract が微妙に変わる（atomic ポインタは reader が writer と race
  することを許す。mutex 保護版とは違う）。検出された1対の race のための
  オーバーエンジニアリングとして却下。
- **`agentLoop` の nil-init を削除。** テストが必ず `LoadSession` を先に
  呼ぶように強制することになる。魅力的だが多くのテストに波及する広い
  refactor で、deferred。同期さえされていれば防御的 init は無害。
- **`generateTitleIfNeeded` の全 `a.session` アクセスを `a.mu` で囲む。**
  関数は title-check と title-write の間で `a.backend.Chat`（LLM HTTP
  呼び出し、数秒かかりうる）を実行する。その間 `a.mu` を保持すると他の
  全 agent 操作がブロックされる。スナップショットパターンの方が lock
  window が短い。

## 5. テスト

レースは既存の `-race` 実行で既に捕捉済み (例:
`TestQueuedSend_OverwriteMostRecentWins`)。修正後:

- `go test -race -count=1 ./internal/agent/`（フルパッケージ）で当該
  goroutine 対について WARNING: DATA RACE が出ないこと。
- Issue #13 の 2 テスト修正（deadline + barrier パターン）は維持。これらは
  別の test-design race に対する修正。

新規テストは追加しない: detector が回帰検知機構そのものであり、修正を
revert すれば warning が即再発する。

## 6. 互換性

- 純粋に内部同期の変更。API・オンディスク・ユーザー可視挙動の差分なし。
- 新規依存・新規 build tag なし。

## 7. スコープ外

- `a.session` 所有モデルの全体的 refactor（atomic ポインタ、RCU、
  copy-on-write、…）。
- `memory.Session` 構造体自体のスレッド安全性 contract — それを所有する
  パッケージの責務。
- detector が未だ flag していない `a.session` アクセサ群の race。detector
  が surface した時に対処する。

# MCP ツール呼び出し中の Abort — 設計ノート

**ステータス:** ドラフト (2026-05-12); 承認待ち
**対象バージョン:** v0.6.1 (v0.6.0 上のポイントリリース)
**報告:** ユーザー — 「MCP を利用した Tool Calling 実行中に
Chat を Abort できない」

本ノートは、MCP guardian ツール呼び出しが in-flight な間でも
Chat の Abort が効くようにするための実装方針を定める。v0.6.x
時点では、LLM ストリーミング・analysis ツール・sandbox
ツール・shell スクリプトといった他のツールソースは全て
`Abort()` を尊重するが、MCP ツール呼び出しだけは上流応答を
待たされ、Abort が事実上無効になる。

---

## 1. 問題

`internal/agent/agent.go` の MCP ディスパッチ箇所
(v0.6.0 時点で `executeTool` の `mcp__` ブランチ、~line 1792)
は次のとおり:

```go
result, err := g.CallTool(toolName, json.RawMessage(tc.Arguments))
```

二重の欠陥がある:

1. **`mcp.Guardian.CallTool` が `context.Context` を取らない。**
   シグネチャは `(name string, arguments json.RawMessage)`
   (`internal/mcp/mcp.go`、line 208 上のコメントブロック参照)。
2. **内部の `Guardian.call()` が `g.stdout.Scan()` で
   ブロックする。** キャンセル経路がない。パッケージ冒頭の
   コメント (`mcp.go` line 50–62) は、ハング中の `Scan` を
   解除する唯一の手段が `Stop()` による stdin クローズ +
   子プロセス kill であることを明記している。

`Agent.Abort()` は Send 用 context をキャンセルするが、MCP
呼び出しはそれを観測できない。上流 guardian が応答するまで
`Scan` で待ち続ける。ユーザー視点では、レート制限がかかった
API や長時間ジョブを叩いている際、Abort ボタンが壊れている
ように見える。

MCP プロトコル (2024-11-05) 自体には「tool call cancel」
通知が存在しない。よって上流サーバに「止めて」と頼む
graceful な手段はそもそも存在しない。**子プロセスを kill
するのが唯一の確実な中断手段**である。

---

## 2. 目標

- **`Abort()` が MCP ツール呼び出しを中断する** — 他ツール
  ソースでの即時 Abort に対し ≤ 100 ms のオーバーヘッドで。
- **影響範囲は guardian 単位。** 1 つの MCP ツール呼び出しを
  Abort しても、無関係な guardian は落とさない。
- **想定外の再実行を起こさない。** Abort されたツール呼び出しは
  `(Cancelled by user)` という明示的な結果文字列を返し、agent
  の Send を終了させる。他の Abort 経路と同一の挙動。
- **自己回復。** 同じ guardian を狙う次回ユーザーメッセージは
  正常動作すること。`"guardian is stopped"` エラーで突き返さない
  よう、guardian プロセスを事前に再起動する。
- **プロセスツリー衛生。** Abort 後にゾンビ子プロセスや
  in-flight goroutine がリークしないこと。

非目標:

- プロトコル内 graceful cancel (MCP 2024-11-05 では不可能)。
- 上流 MCP サーバが既に開始したサイドエフェクト (外部 API
  呼び出し等) を取り消すこと。サーバ側ではそのまま完了する。
  クライアント側で「待つのをやめる」だけ。

---

## 3. 設計

### 3.1 `Guardian.CallToolContext` の新設

既存 `CallTool` の context 対応ラッパーを `mcp` パッケージに
追加する。goroutine + `Stop()` の相互作用は同パッケージで
すでに整理されているので、キャンセル経路をそこに集中させる。

```go
// CallToolContext は CallTool に context 駆動キャンセルを加えた版。
// 上流が応答する前に ctx.Done() が来た場合、Stop() で子プロセスを
// kill して in-flight stdout.Scan を解除し、ctx.Err() を返す。
// goroutine 側は読み取りエラーで終了する。buffered チャネル経由で
// 後追いで届く結果は無害に捨てられる。
//
// 注意: キャンセルされた CallToolContext は guardian を使用不能に
// する (Stop が g.stopped = true を恒久セット)。呼び出し側は次の
// CallTool 前に guardian を再起動すること — Agent.restartGuardian
// 参照。
func (g *Guardian) CallToolContext(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error)
```

実装スケッチ:

```go
func (g *Guardian) CallToolContext(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
    type res struct {
        body json.RawMessage
        err  error
    }
    ch := make(chan res, 1) // buffered — goroutine が常にブロックなく書ける
    go func() {
        b, e := g.CallTool(name, arguments)
        ch <- res{b, e}
    }()
    select {
    case r := <-ch:
        return r.body, r.err
    case <-ctx.Done():
        _ = g.Stop() // goroutine 内の stdout.Scan を解除
        return nil, ctx.Err()
    }
}
```

なぜ `call()` 自体を ctx 対応の I/O で書き直さないのか:
`bufio.Scanner` API にキャンセル手段がない上、JSON-RPC
機構全体を context-aware な net I/O に持ち上げるのは
コスト過大。「goroutine + Stop」パターンは `mcp.go:50–62`
で文書化済みの既存並行モデルと一貫している。

### 3.2 単一 guardian 再起動ヘルパー

`startGuardians` を単一プロファイル spawn ヘルパーへ分解:

```go
// spawnGuardian は 1 プロファイル分の guardian を構築・起動する。
// 起動済み guardian (失敗時 nil) と記録すべき MCPStatus を返す。
// 呼び出し側は guardiansMu を保持していること。startGuardians
// (起動時) と restartGuardian (Abort 後) の両方が利用する純粋ヘルパー。
func (a *Agent) spawnGuardian(p config.MCPProfile) (*mcp.Guardian, MCPStatus)
```

その上で、agent パッケージ内専用の restart を追加:

```go
// restartGuardian は指定された guardian を現在の config で再 spawn する。
// goroutine から呼んで安全 — guardiansMu を取って map エントリを
// アトミックに差し替える。CallToolContext が context.Canceled を
// 返した後に呼ばれる: キャンセルで子プロセスは kill 済みなので、
// map のエントリは「死んだハンドル」になっている。
func (a *Agent) restartGuardian(name string)
```

`restartGuardian` は `a.mcpStatuses` も同時に更新し、Settings UI
が「restarting」→「running」の遷移を反映できるようにする。

### 3.3 ディスパッチャ変更

`executeTool` の MCP ブランチを以下に置換:

```go
result, err := g.CallToolContext(ctx, toolName, json.RawMessage(tc.Arguments))
if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
    // CallToolContext は子プロセスを kill して Scan を解除した。
    // 当該 guardian を非同期で再 spawn し、次回ユーザーターンで
    // 再利用可能にする。MCP プロトコルに tool-call キャンセル
    // 通知が存在しないため、kill-and-respawn が Abort 即応性を
    // 確保する唯一の手段 (docs/en/mcp-abort.md 参照)。
    go a.restartGuardian(guardianName)
    return "(Cancelled by user)", ActivityStatusError
}
```

`ErrToolFailed` 処理や一般エラーマッピングはそのまま。

### 3.4 ワイヤ可視性

新しい activity-event 型もワイヤフォーマット変更も無し。
キャンセルされたツールのバブルは既存の `ActivityStatusError`
経路で `error` ステータスに遷移し、`(Cancelled by user)` を
detail として表示する。analyze-data / sandbox ツール側の
abort 表現と完全に揃う。フロントエンド変更: 無し。

---

## 4. テスト

### 4.1 `mcp` パッケージ — unit

`internal/mcp/guardian_test.go` に追加:

- **`TestGuardian_CallToolContextRespectsCancel`** — 10 秒
  sleep してから応答する Python スタブを起動し、100 ms 後に
  キャンセルする context で `CallToolContext` を発行。
  ~200 ms 以内に `context.Canceled` を返すこと、`cmd.Process`
  が動作していないことを assert。
- **`TestGuardian_CallToolContextSucceedsWhenFast`** — 既存
  `hello` ツールに対し十分なタイムアウトで `CallToolContext`
  を発行。body が成功で返ること (`CallTool` と等価) を assert。
- **`TestGuardian_CallToolContextDoesNotLeakGoroutine`** —
  キャンセル後に orphan goroutine の終了を待つ
  (`time.Sleep(50ms)`)、in-flight call が終了している
  (チャネルが drain されている) こと assert。`runtime.NumGoroutine`
  差分でも良いがチャネル drain チェックの方が安定。

### 4.2 `agent` パッケージ — integration

`internal/agent/integration_test.go` か新規
`mcp_abort_test.go` に追加:

- **`TestAgent_AbortDuringMCPTool`** — 無限ブロックする
  mock guardian を仕込んで Send を駆動、`Abort()` 呼出、
  Send が ~500 ms 以内に `(Cancelled)` を返すこと、guardian
  map エントリが差し替わっていること (別の `*Guardian`
  ポインタ、または `MCPStatuses()[i].Status == "running"`
  + 新規 PID) を assert。

agent テストでは map に mock guardian を仕込む手段が必要。
`a.guardians` は package-private で同一パッケージから
書き込めるので、test-only ヘルパー `setGuardianForTest(name, g)`
を用意するか、テスト内で直接 map を操作する。build tag 不要。

---

## 5. 却下した代替案

### 5.1 Abort 時に全 guardian を再起動

既存の `RestartGuardians()` を呼ぶ 1 行変更で済む。却下理由:
1 つのツール Abort で関係のない (場合によっては認証済みで
長寿命な) guardian セッションまで全部潰してしまう。単一
guardian 再起動ヘルパーは ~30 LOC のリファクタで済み、
こちらが厳密に上位互換。

### 5.2 guardian を維持し、orphan 応答をバックグラウンドで drain

子プロセスは kill せず、元の呼び出しを goroutine 内で走らせて
上流応答を捨てる「discard bin」チャネルに流す案。だが
guardian の `callMu` は上流応答まで保持されたままなので、
次の呼び出しがブロックする — UX バグが場所を変えただけ。
却下。

### 5.3 `call()` を context-aware な net I/O に書き直す

`bufio.Scanner` を `os.Pipe` + `SetReadDeadline` ループ、
あるいは goroutine が `call()` の select 先チャネルにバイトを
積む方式で置き換える。ctx キャンセルがクリーンに伝播し、
子プロセス kill 不要になる。v0.6.1 では却下 — 変更規模が
大きく、すでにレビュー済みのコード (`security-hardening-2.md`
が参照) を大きく動かす。kill-and-respawn でユーザー要件は
満たせる。MCP がプロトコルレベルでキャンセルを獲得した
(post-2024-11-05) 場合に再検討。

### 5.4 MCP 上に独自の `cancel` 通知を載せる

MCP サーバ実装側で ad-hoc なキャンセル規約を採るところはある。
却下理由: (a) ターゲット仕様に存在しない、(b) cooperating
サーバにしか効かない、(c) そもそもクライアント側の
blocked-Scan 問題を解決しない。

---

## 6. リスクと未解決事項

- **上流の orphan 作業。** 子プロセス kill により、guardian
  が in-flight にしていた外部リクエスト (HTTP, DB) は
  クライアント側で放棄されるがサーバ側では完走する。
  許容する — プロトコルレベル cancel がない以上避けられない。
  CHANGELOG に 1 行注記する (rate-limit 払い戻しはない、等)。
- **再起動レイテンシ。** `mcp.Guardian.Start()` は `initialize`
  + `tools/list` の往復を含み、タイムアウト 15 秒。実測では
  ~50–500 ms が典型だが、遅い guardian だとそのプロファイルが
  期待より長く "restarting" 状態に留まる可能性。再起動は
  非同期なので agent の idle 復帰はブロックされない。**当該
  guardian** 向け次回呼び出しのみが待つ。許容。
- **`a.cfg.Tools.MCPProfiles` の race。** `restartGuardian` は
  プロファイル一覧を config から読む。config 変更は Settings
  経由で別ロックを取るパス。`guardiansMu` 下での read-only
  アクセスで安全。ヘルパーにコメントを残す。
- **Stop ハング時の goroutine リーク。** `cmd.Process.Kill()`
  自体がブロックするケース (macOS/Linux で動作中プロセスに
  対しては通常ありえない)。緩和策なし — Kill が信頼できない
  なら他にも大問題。Start タイムアウト 15 秒が暗黙の安全網。
- **MCP 呼び出し中の MITL プロンプト + Abort。** MITL 承認
  ダイアログ表示中 (`g.CallTool` 起動前) に Abort された場合、
  `requestMITL` 自体が同じ context を見ているので reject
  テキストを返して `CallToolContext` に入らずに抜ける。変更
  不要。

---

## 7. 互換性

- LLM 観測可能な変化: キャンセルされた MCP ツール呼び出しは
  ハングせずに文字列 `(Cancelled by user)` を返すように
  なる。バブルステータスは `error`。既存プロンプトには影響
  しない。system prompt 変更なし。
- API: `Guardian.CallTool` シグネチャ維持。新規
  `CallToolContext` は純粋追加。
- Bindings / frontend: 変更なし。
- Session export/import: 変更なし (キャンセルされたツール
  呼び出しは既に error ステータスでシリアライズ可能)。

---

## 8. ロールアウト

bisect しやすい 1 機能 = 1 commit:

1. `feat(mcp): add CallToolContext for cancellable tool calls`
   — mcp.go + guardian_test.go の追加。
2. `refactor(agent): extract spawnGuardian helper from startGuardians`
   — 純粋リファクタ、挙動変化なし。
3. `feat(agent): restart MCP guardian after aborted tool call`
   — `restartGuardian` + ディスパッチャ変更 + agent test。
4. `docs: describe MCP abort + guardian restart` — 本ノート +
   architecture roll-up + CHANGELOG ドラフト。
5. `chore: release v0.6.1` — リリースコミット + タグ。

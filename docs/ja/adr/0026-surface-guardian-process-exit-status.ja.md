# ADR-0026: MCP guardian の Start 失敗時にプロセス終了ステータスを可視化する

- Status: Accepted
- Deciders: magi
- Related: ADR-0024（guardian バックグラウンド起動）, mcp-guardian ADR-0002, `feedback_dropbox_synced_binary`

## 1. 背景

MCP guardian プロセスが `Guardian.Start()` 中に死ぬと、ユーザに見えるのは
閉じたパイプの症状だけで、原因が見えない:

```
MCP guardian "aws" start failed: initialize: write request: write |1: broken pipe
MCP guardian "slack" start failed: initialize: read response: EOF
```

これらは `call()` のブロッキング `stdout.Scan()` が EOF を返す（プロセスが
stdout を閉じた）か、stdin 書き込みが broken pipe になる＝guardian が
`initialize` に応答する前に終了した、というもの。**なぜ終了したかは不可視**。

実際に起きた事象: ユーザがビルドしたての `mcp-guardian` を `~/bin`（Dropbox
同期配下）へ `cp` で配置 → macOS が `com.apple.provenance` + quarantine を
付与し**起動時に SIGKILL**（`exit 137`）。全プロファイル（stdio の `aws`
含む）で guardian spawn が即死し、アプリ再起動後も失敗し続けた。理由不明の
`read response: EOF` のせいで restart/コードのバグに見え、診断に多大な時間を
要した — エラーに `signal: killed` の一語があれば一目瞭然だった。
（`feedback_dropbox_synced_binary` 参照。）

guardian は子の stderr を debug ログへ排出しているが（`mcp.go`）、SIGKILL
ではプロセスが stderr を出さず、終了ステータス（`signal: killed`）は捕捉も
表示もされない。

## 2. 決定

guardian プロセスの終了ステータスを捕捉し、`Start` の失敗エラーに含める。
`signal: killed` のときは的を絞ったヒントを付ける。

実装は os/exec の「`cmd.Wait()` を pipe 読み取りと競合させてはならない」規則を
守る必要がある。`cmd.Wait()` の唯一の呼び出しを**既存の stderr ドレイン
goroutine**の読み取りループ終了後に置く:

```go
// Guardian に追加:
//   exited  chan struct{}
//   exitErr error

// stderr ドレイン goroutine (Start):
go func() {
    s := bufio.NewScanner(stderrPipe)
    ...
    for s.Scan() { logger.Debug("mcp[%s] stderr: %s", name, s.Text()) }
    // stderr EOF ⇒ プロセス終了（または終了中）。この goroutine が stderr
    // 読み取りを所有し、この時点で init goroutine の stdout 読み取りも同じ
    // EOF で返っているので、Wait はどの pipe 読み取りとも競合しない。Wait は
    // ただ1箇所からのみ呼ばれる。
    g.exitErr = g.cmd.Wait()
    close(g.exited)
}()
```

`Start` の失敗分岐は、init エラーが閉じたパイプ系（EOF / broken pipe /
file already closed）のとき、`exited` を短時間待って返却エラーを enrich する:

```go
case err := <-done:
    if err != nil {
        g.Stop()
        if isPipeClosed(err) {
            select {
            case <-g.exited:
            case <-time.After(2 * time.Second):
            }
            if g.exitErr != nil {
                return fmt.Errorf("guardian process exited before initialising: %v%s",
                    g.exitErr, gatekeeperHint(g.exitErr))
            }
        }
        return err
    }
    return nil
```

`gatekeeperHint` は終了が `SIGKILL`（`signal: killed`）に見えるときだけ
1行のヒントを付ける:

> " — the process was killed on launch; if the binary lives under a
> quarantined or cloud-synced path (e.g. ~/bin via Dropbox), macOS may
> be SIGKILLing it: re-sign with `codesign --force --sign -` or move it
> out of the synced path."

閉じたパイプでないエラー（例: `initialize` の RPC エラー — mcp-guardian
ADR-0002、プロセスは**生存**し JSON-RPC エラーを返したケース）はそのまま
返す。これらでは `exited` を待たない（プロセスは死んでいない）。

### なぜ stderr goroutine が Wait を所有するか

- 既に stderr pipe 読み取りを所有しているので、ループ終了後の `Wait` がその
  読み取りと競合しない。
- stdout 読み取りは init/`call` goroutine にあり、失敗を検査する時点では既に
  返っている（同じ EOF を見て `Scan` が false）ので、`Wait` は stdout 読み取りとも
  競合しない。
- `Wait` の呼び出し箇所はただ1つ ⇒ 二重 `Wait` 無し。`Stop()` は `Kill` のみ
  （`Wait` 無し）を継続; kill で子が終了 → stderr goroutine が EOF を見て唯一の
  `Wait` を行う。
- 生存中の guardian（成功、または ADR-0002 の RPC エラー）では stderr goroutine
  が `Scan` でブロックし続けるので `Wait` は呼ばれず `exited` も閉じない —
  これがまさに正しい挙動。

## 3. 影響

- 起動時に kill された guardian は
  `guardian process exited before initialising: signal: killed — …macOS
  may be SIGKILLing it: re-sign or move it out of the synced path` で失敗し、
  `read response: EOF` でなく原因を直接示す。
- 非ゼロ終了の guardian は `exit status N` を出す。
- 死亡経路は子を reap（`Wait`）するので、失敗ケースでゾンビを残さない。
- ヒント文言は macOS 向けだが他 OS でも無害（`signal: killed` のときだけ付与）。

## 4. 却下した代替案

- **`Start` で専用の `cmd.Wait()` 監視 goroutine を起動。** init goroutine の
  `StdoutPipe`/`StderrPipe` 読み取りと競合し（os/exec の既知の落とし穴）、読み取り
  途中で pipe を閉じるリスク。stderr 読み取り所有者に `Wait` させる方式を採用し却下。
- **`Stop()` で reap（Kill 後に Wait）+ `RestartGuardians` reap。** 魅力的だが、
  今回の restart 問題は SIGKILL されたバイナリが原因で reap レースではない:
  `Kill` された子は未 reap でも死亡時に fd/接続を解放する（ゾンビは PID スロットを
  占めるだけ）。よって広範な reap は正しさには不要。本 ADR は診断性に限定する。
  （ゾンビ蓄積が観測されたら再検討。）
- **子の stderr を auth/kill 文字列でパターンマッチ。** 脆い; SIGKILL された
  プロセスはそもそも stderr を出さない。終了ステータスが信頼できるシグナル。

## 5. スコープ外

- quarantine の自動修正 / 再署名（環境の問題; `feedback_dropbox_synced_binary`
  に文書化）。
- `Stop()` / `RestartGuardians` の変更。
- 15秒 start-timeout 経路の文言（ハングだが生存しているプロセスは別ケース;
  不変 — ただし `Stop()`→kill すれば同じ終了ステータス捕捉が使える）。

## 6. 実装

- `app/internal/mcp/mcp.go`:
  - `Guardian`: `exited chan struct{}` と `exitErr error` を追加; `Start` で
    spawn 前に `exited` を初期化。
  - stderr goroutine: `Scan` ループ後に `g.exitErr = g.cmd.Wait()` → `close(g.exited)`。
  - `Start` 失敗分岐: `isPipeClosed(err)` なら `exited` を待ち（≤2s）`g.exitErr`
    + `gatekeeperHint` を wrap。
  - `isPipeClosed(err) bool`（`io.EOF` / "broken pipe" / "file already closed"
    を判定）と `gatekeeperHint(err) string` を追加。
- `app/internal/mcp/guardian_test.go`: 自身を `kill -KILL $$` する（または非ゼロ
  終了する）スタブ上流 ⇒ `Start` が `signal: killed`（または `exit status N`）を
  含むエラーを返す; 既存の happy-path / timeout テストは green 維持。

検証: `go test ./internal/mcp/ -tags no_duckdb_arrow`; 手動 — プロファイルの
`binary` を quarantine/SIGKILL されるバイナリに向け、MCP 設定のエラーが新文言に
なることを確認。

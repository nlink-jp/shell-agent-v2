# ADR-0024: 非ブロッキング startup と決定論的なセッション復元

- Status: Accepted
- Deciders: magi
- Related: startup 退行報告（ウィンドウ復元が遅い + 起動後にセッション未選択）; ADR-0021（状態機械の一貫性）, ADR-0022（agent ファイル分解）

## 1. 背景

最近のビルドで2つの startup 退行が報告されている:

1. **ウィンドウ復元と入力可能化が遅い。** 起動後しばらくウィンドウが
   デフォルトの 1024×768 のまま・UI 操作不可で、その後に保存サイズへ
   スナップする。
2. **セッションが選択されない。** 起動後サイドバーにセッション一覧は
   出るが、どれも選択されておらず、最後にアクティブだったセッションが
   復元されない不定状態になる。

両者は単一原因に帰着する: **`bindings.startup()`（Wails の `OnStartup`
コールバック）が外部依存の重い処理を同期実行し、UI に必須のステップが
その後ろに置かれている。**

### 1.1. ブロッキング経路

`bindings.startup()`（`app/bindings.go:68`）は `bindings.go:91` で
`agent.New(cfg)`（`app/internal/agent/agent.go:220`）を同期呼び出しする。
`New` 内でタイムアウト無しに外部システムへ到達する呼び出しが2つある:

- `a.startGuardians()`（`agent.go:248` → `agent_mcp.go:37`）は各 MCP
  guardian の**プロセス**を spawn し、`initialize` + `tools/list` の
  ハンドシェイク（`g.Start()`）を同期で待つ。
- `a.maybeStartSandbox()`（`agent.go:249`、定義は `agent.go:331`）は
  コンテナエンジンを probe する: `eng.ImageReady(context.Background(), …)`
  と `eng.StopAll(context.Background())` が **`context.Background()`=
  タイムアウト無し**で podman/docker へ shell out する。停止中の podman
  machine や無応答の Docker daemon でここがハングする。

ウィンドウ復元（`bindings.go:226-231`、`wailsRuntime.WindowSetSize` /
`WindowSetPosition`）は `startup()` の**最後**にあるため、上記ブロッキング
呼び出しが返るまで実行されない。これが症状1。

### 1.2. セッション復元のレース

Wails v2 では `OnStartup` 実行中に webview がロードされ、JS からの binding
呼び出しが別 goroutine で並行ディスパッチされる。frontend の一発初期化
effect（`app/frontend/src/App.tsx:707-724`、deps `[]`）はその窓で走る:

```ts
const s = await window.go.main.Bindings.ListSessions()   // 成功
setSessions(s || [])
if (!s || s.length === 0) { /* 作成 */ }
else {
    const msgs = await window.go.main.Bindings.LoadSession(s[0].id) // 失敗
    setCurrentSessionId(s[0].id)                                    // 到達せず
    setMessages(restoredMessages(msgs))
}
```

- `ListSessions()` → `memory.ListSessions()`（`bindings.go:619`）は
  セッションメタデータを disk から直接読み、`b.agent` に**触れない**ので
  サイドバーは埋まる。
- `LoadSession(s[0].id)`（`bindings.go:465`）は冒頭で
  `if b.agent == nil { return …"agent not initialised yet" }`
  （`bindings.go:466-468`）。`agent.New` がまだブロック中で `b.agent` が
  nil のため reject され、async IIFE が throw し、`setCurrentSessionId`
  に到達しない。
- effect は `[]` deps で**リトライ無し**のため、セッション未選択のまま
  固定される。これが症状2。

startup が遅い（§1.1）ほどレース窓が広がる。これが2症状が同時に現れる
理由であり、startup の順序自体にコード変更が無くても、guardian/sandbox
初期化を遅くする要因（MCP profile の追加、sandbox 有効化、podman machine
停止）と発症が相関する理由でもある。

## 2. 決定

サイズ確定済みで操作可能なウィンドウと前回セッションをユーザに見せるのに
必要な処理を非ブロッキング化し、遅い外部処理はバックグラウンドへ回し、
その完了まではメッセージ入力を gate してユーザ操作が半初期化 agent と
レースしないようにする。5パートを同時にリリースする。

### Part A — ウィンドウサイズは startup 後ではなく生成時に決める

保存済みウィンドウジオメトリを `wails.Run` の**前**に `main()` で読み、
`options.App` の初期 `Width`/`Height`（と位置）として渡す。ハードコード
された `1024`/`768`（`main.go:76-77`）を置換する。startup 後の
`WindowSetSize` / `WindowSetPosition`（`bindings.go:226-231`）は削除する。

これでウィンドウはリサイズのジャンプ無しに正しいサイズで開き、`startup()`
の完了に依存しなくなる。csv-editor のウィンドウ復元（`main()` で config を
読み `wails.Run` に渡す）と同じ方式。初回起動と異常な永続値のために、妥当な
デフォルトと最小寸法フロアは残す。

### Part B — agent を即返し、外部初期化をバックグラウンドへ

`agent.New` は既存の安価なローカル構築（registry scan、chat engine、
memory/sysrules ロード、descriptor 登録）を維持し、2つのブロッキング
呼び出し**抜き**で完全に使える `*Agent` を返す。それらは新設の
`(*Agent).StartBackground(ctx context.Context)` へ移し、`bindings.startup`
が `b.agent` 代入後に goroutine で起動する:

```go
b.agent = agent.New(cfg)        // 高速: b.agent は即 non-nil
b.agent.SetObjects(b.objects)
b.agent.SetHandlers(...)
wailsRuntime.EventsEmit(b.ctx, "app:ready", nil)   // セッション復元を許可
go func() {
    b.agent.StartBackground(b.ctx)                 // startGuardians + maybeStartSandbox
    wailsRuntime.EventsEmit(b.ctx, "tools:ready", nil)  // コンポーザ有効化を許可
}()
```

### Part C — sandbox probe にタイムアウトを付ける

`maybeStartSandbox` の `ImageReady` / `StopAll` probe（`agent.go:367`,
`agent.go:383`）の `context.Background()` を `context.WithTimeout`
（提案 5s）に置換する。バックグラウンドであっても無応答エンジンが
goroutine を永久にハングさせてはならない。タイムアウト時はログを出して
sandbox ツールを非表示のままにする — 既存のエラー復帰経路と同じ挙動。
これは Part D のコンポーザ gate の最悪値も bound する。

### Part D — 2段の readiness シグナル; tools:ready までコンポーザを gate

2つの readiness レベル、2イベント、2帰結:

| シグナル | emit タイミング | frontend の帰結 |
|---|---|---|
| `app:ready` | `b.agent` 代入直後（Part B）、goroutine の前 | セッション復元可; 履歴・サイドバー・セッション切替が使える |
| `tools:ready` | `StartBackground` 完了時（guardian + sandbox 完了/timeout） | メッセージコンポーザ（SEND）を有効化 |

**セッション復元**（`LoadSession(s[0])` 経路）は `app:ready` を起点とする。
`agent.New` が高速化したので実質即時に発火する。イベント取りこぼし / mount
順序への堅牢化: frontend は mount 時に小さな `Ready() bool` binding も呼び
（listener 接続前に `app:ready` が emit 済みのケースを救う）、`LoadSession`
が "agent not initialised yet" で reject した場合は1回だけバウンドリトライ
する。

**メッセージ入力**は `tools:ready` まで「初期化中…」表示で無効化する。これが
「ユーザ入力が init とレース」懸念（§3.2）への決定: SEND が init 途中に着弾
する挙動を解析するより、禁止する。その間も UI は完全に使える（履歴閲覧、
セッション切替、設定）。通常 `tools:ready` は sub-second、sandbox 有効 +
engine 停止の edge では Part C の 5s timeout で bound。取りこぼし対策に
frontend は mount 時に `ToolsReady() bool` binding も問い合わせる。

### Part E — デッドフィールド `LastSession` を削除; `s[0]` で復元

`config.Config.LastSession`（`internal/config/config.go:340`）は全コード
ベースで**自分自身以外に参照が無い** — reader/writer/binding/frontend いずれ
も無し。半端な機能の先端ではなく、孤立した1フィールド（window state の
コミットで投機的に追加され配線されなかった）。**削除する。** `omitempty` +
writer ゼロなので、現存する config.json に `last_session` は無く、削除は安全。

セッション復元の正しさに新機構は不要: レース修正（Part B + D）後、frontend は
`s[0]`（`memory.ListSessions` ソートで最終更新が最新、`store.go:62-64`）を
ロードし、これは通常使用で「最後にアクティブなセッション」と一致する。
「すでに動くなら難しい機構を作らない」方針に従い、`s[0]` ヒューリスティックを
維持し、永続的な last-active ポインタは導入しない。

## 3. init 窓における安全性

### 3.1. Part B が導入する `a.sandbox` data race

現状 `maybeStartSandbox` は `agent.New` 内で同期実行され、並行アクセスの
前なので、ガード無しフィールド `a.sandbox` は安全。**`sandboxMu` は存在
しない** — `a.sandbox` は `agent.go:1449`、`agent.go:2448`、および
`sandbox_tools.go` 全体（`Exec`, `WorkDir`, `RestartSandbox`, …）で
ロック無しに読まれている。

`maybeStartSandbox` を goroutine 化すると、`a.sandbox = eng`
（`agent.go:376`）がそれら読み取りと並行しうる — data race。よって Part B は
**`a.sandbox` の同期化無しには完成しない。** 決定: `sandboxMu sync.RWMutex`
を追加し、全 `a.sandbox` 読み書き（と相棒の `a.chat.SetSandboxEnabled` 呼び
出し）をそれ経由にする。既存の `guardiansMu` パターンに揃い、最も意外性が
少なく（interface 値に対する `atomic.Pointer` より素直）、現状すでに存在する
潜在的無ガード書き込みも直す: `RestartSandbox` は実行時に `a.sandbox` を
ロック無しで変更している（`sandbox_tools.go:262-265`）。

`a.guardians` には既に `guardiansMu`（`agent.go:142`）があり、`startGuardians`
が spawn ループ全体で保持し（`agent_mcp.go:42`）、読み取り側
`buildToolDefs`（`agent.go:2339`）/ `ListTools`（`agent.go:2455`）は `RLock`
を取る。Part B の MCP 側は新規ロック不要。

### 3.2. init 窓でユーザ操作が当たりうるもの

入力を gate しても（Part D）、init 中もセッション切替・設定・ツール一覧は
生きている。列挙すると各々安全:

- **MCP spawn 中の tool-def 構築 / ListTools。** 読み取り側は
  `guardiansMu.RLock` を取り、`startGuardians` が write lock を解放するまで
  ブロックしてから完全なマップを見る。`spawnGuardian` は `g.Start()` 成功
  **後**にのみ guardian をマップへ入れるので、半起動 guardian は決して
  ディスパッチ対象にならない。race も部分ディスパッチも無く、最悪でも短い
  ロック待ち。
- **sandbox 未初期化での sandbox ツール参照。** sandbox descriptor は無条件
  登録され、表示/実行時に `a.sandbox != nil` でゲートする（`agent.go:1449`,
  `agent.go:2448`）。§3.1 の `sandboxMu` で読み取りは一貫し、nil（ツール
  非表示 / 「sandbox 利用不可」）か live エンジンのいずれかを返す。graceful、
  クラッシュ無し。
- **状態機械。** agent は `StateIdle` 開始（`agent.go:239`）。SEND は
  Idle→Busy 遷移でバックグラウンド初期化と独立。`StartBackground` は
  `a.state` に触れない。
- **ブート init 中の Settings からの `RestartSandbox`。** それと
  `StartBackground` の `maybeStartSandbox` が両方 `a.sandbox` を書く。
  `sandboxMu` で直列化され race は無いが、二重初期化は論理的にありうる
  （稀: ユーザが gate 窓内に Settings を開いてトグル）。`StartBackground` の
  sandbox ステップを「ブート sandbox init 完了」フラグでガードし、ユーザ起点の
  restart が綺麗に勝つようにする。

結論: §3.1 を入れれば、init 中の操作の観測可能な影響はバウンドされたロック
待ちと「ツールがまだ無い」だけ — どちらも benign で既存の動的ツール設計と
同じ。Part D のコンポーザ gate は SEND 経路からそれらすら除く。

## 4. 変更しないもの

- agent の Idle/Busy 状態機械、実行ループ、MITL、ツールディスパッチ契約。
- セッションファイル形式と `memory.ListSessions` の順序 / `s[0]` 復元
  ヒューリスティック。
- 最終的に利用可能なツール集合。変わるのは*可視タイミング*が「初回描画前」
  から「`tools:ready` 時点」へ移ることだけ。
- `RestartGuardians` / `RestartSandbox` のユーザ起点経路（ユーザ操作時は
  同期実行のまま。ブートのみバックグラウンドへ）。
- MCP の `mcp__<guardian>__<tool>` エンベロープとディスパッチ。

## 5. 却下した代替案

### 5.1. `WindowSetSize` を `startup()` の先頭へ移すだけ

ウィンドウ復元は早まるが依然 `OnStartup` 内であり、セッション復元レースも
長い入力不可期間も解決しない（agent はなお同期構築）。Part A（生成時に
サイズ確定）が厳密に優れ、リサイズジャンプも完全に消える。

### 5.2. init 中に「degraded」SEND を許す（コンポーザ gate 無し）

SEND を `app:ready` で発火可能にし、ツール未登録なら最初のターンが
MCP/sandbox ツールを欠くだけ。ロック上は安全（§3.2）だが、モデルのツール集合が
最初のターンだけ静かに変動する — 「ある時はある、ない時はない」挙動は説明も
テストも難しい。明示的 gate（Part D）を採用して却下: 短く可視な「初期化中…」の
方が、確率的に劣化する最初のターンより推論しやすい。

### 5.3. 2段 UI（`app:ready` で SEND 有効 + 「ツール読込中」pill）

5.2 と同じ degraded-first-turn の露出を pill 付きで持つだけ。同理由で却下。
gate の方が単純で懸念クラスを根絶する。

### 5.4. guardian/sandbox を初回使用時に遅延初期化

必要な最初のツール呼び出しまで遅延。却下: MCP ツールの*定義*は最初の LLM
ターン前に既知である必要があり（モデルが呼べるように）、Settings → Tools も
起動直後の一覧を期待する。ブート時バックグラウンド goroutine は初回呼び出しの
レイテンシスパイク無しに「起動から1秒以内に利用可能」を与える。

### 5.5. `a.sandbox` ロックを省き goroutine が先に終わるのを当てにする

定義上 data race。`go test -race` が検出し、負荷時に間欠的破損として顕在化。
即却下。

### 5.6. `LastSession` を配線して厳密な last-active セッションを復元

切替ごとに `LastSession` を永続化し起動時に読む。不要として却下: `s[0]` が
通常使用で last-active セッションを既に復元し、当該フィールドに支援機構も
無い。作るのは挙動の利得無しの労力。代わりにフィールドを削除する（Part E）。

## 6. 含意とリスク

- **コンポーザ gate のレイテンシ。** SEND は `tools:ready` まで無効:
  通常 sub-second、sandbox 有効 + engine 停止の edge で ≤5s（Part C で
  bound）。その間も他の UI は応答するので、現状の「未サイズのウィンドウ +
  セッション無し」よりはるかに良い体験。
- **ツール可視性。** Settings → Tools は `tools:ready`（または既存 `agent:*`
  シグナル）で更新し、起動時にそのペインを見るユーザが再オープン無しに
  MCP/sandbox ツールの出現を見られるようにする。
- **`go test -race`** が新 `sandboxMu` で green である必要。`StartBackground`
  模擬中の並行 `a.sandbox` 読み取りを行うテストを追加する。
- **取りこぼし堅牢性。** `app:ready` / `tools:ready` は `Ready()` /
  `ToolsReady()` binding と対にし、emit 後に接続した listener でも解決できる
  ようにする。これが無いと gate が固着しうる。
- **データ移行なし**、セッション形式変更なし。config スキーマ変更は未使用
  フィールドの*削除*のみ（Part E）。

## 7. 実装計画

単一 PR。各パートは相互依存（Part B は §3.1 無しに安全でなく、Part D は
Part B で追加するイベントに依存）。

1. **§3.1 を最初に — `sandboxMu sync.RWMutex` 追加**し、全 `a.sandbox`
   読み書き（`agent.go:1449`, `2448`, `sandbox_tools.go`,
   `maybeStartSandbox`, `RestartSandbox`）をそれ経由に。加えて §3.2 の
   ブート init ガードフラグ。この時点では全て同期のまま。
   `go test -race ./internal/... -tags no_duckdb_arrow` が green を維持。
2. **Part C** — 2つの sandbox probe にタイムアウト。
3. **Part B** — 2つのブロッキング呼び出しを `(*Agent).StartBackground(ctx)`
   へ分離。`bindings.startup` で `b.agent` 代入後に goroutine 起動。
   `app:ready` の後 `tools:ready` を emit。`Ready()` / `ToolsReady()`
   binding を追加。
4. **Part A** — `main()` でウィンドウジオメトリをロードし `wails.Run` へ
   渡す。startup 後の `WindowSetSize`/`WindowSetPosition` を削除。
5. **Part D** — frontend: セッション初期化を `app:ready` / `Ready()` 起点に
   し `LoadSession` を1回バウンドリトライ。コンポーザは `tools:ready` /
   `ToolsReady()` まで「初期化中…」表示で無効化。
6. **Part E** — `LastSession` フィールド削除。
7. **テスト** — `a.sandbox` の `-race` 並行テスト; `agent.New` が外部 probe を
   行わなくなったことを検証する単体テスト（呼び出しを記録する fake）;
   既存スイートを green 維持。
8. **Docs / CHANGELOG** — README/README.ja はユーザ向けに「起動が速く
   なった」以上の記載は不要; CHANGELOG は
   `fix: non-blocking startup — MCP/sandbox 初期化を待たずにウィンドウと
   前回セッションを復元` として記載。

各コミットで `go test ./internal/... -tags no_duckdb_arrow`（および `-race`）が
green であること。

## 8. スコープ外

- 複数 MCP guardian spawn の相互並列化。
- agent の汎用 async 初期化フレームワーク。本 ADR は既知のブロッキング
  ブート呼び出し2つだけを移す。
- Idle/Busy 状態機械の再設計（ADR-0021 の領域）。
- 停止中の sandbox エンジンをユーザの代わりに自動起動すること — ここでは
  probe がブートをブロックするのを止めるだけ。

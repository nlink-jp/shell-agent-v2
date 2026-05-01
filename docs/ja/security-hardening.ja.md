# セキュリティ強化 — 設計ドキュメント

> 日付: 2026-05-01
> ステータス: v0.1.18 でリリース済み（Phase 1 87252c2、Phase 2 56cddeb、Phase 3 2be166a、Phase 4 2743947）。M4 は TODO.md にデファ
> 範囲: 2026-05-01 監査で見つかった HIGH 3 件 / MEDIUM 4 件。
> 各々を独立してレビュー / 戻せるように 4 コミットに分割。

## 1. 背景

2026-05-01 の監査で、Go バックエンド・フロントエンド・
sandbox の 3 観点で以下が浮上:

- **HIGH**:
  - H1 — `/work` バインドマウントを symlink で突破して
    任意のホストファイル read/write 可能
    (`internal/agent/sandbox_tools.go:466-489`,
    `internal/sandbox/cli.go:155`)
  - H2 — `objstore.Store` の map にロックなし。現実的な
    並行ワークロードで Go ランタイムが
    `concurrent map writes` panic
    (`internal/objstore/objstore.go:42-63`)
  - H3 — `:Z` SELinux マウントラベルを無条件付与。
    macOS / docker-desktop で誤動作、Linux+docker では
    親ディレクトリのラベルを破壊する可能性
    (`internal/sandbox/cli.go:154`)
- **MEDIUM**:
  - M1 — `mitlChan` がバッファ付きチャネル使い回し。
    リクエスト未送信時の Approve クリックが buffer に
    残り、次の MITL を無条件承認する
    (`bindings.go:96, 314-335`)
  - M2 — `agent.guardians` map の write が
    `guardiansMu.Lock()` 取らずに行われ、reader と
    data race (`internal/agent/agent.go:417-447`)
  - M3 — SIGKILL/panic 時に sandbox container がリーク。
    コメントで起動時 `StopAll` スイープに言及があるが
    実際には呼び出されていない
    (`bindings.go:114-134`,
    `internal/agent/agent.go:139-165`)
  - M5 — Settings の `Network` 変更が動作中 container に
    反映されず、設定ドリフトが起きる
    (`internal/sandbox/cli.go:111-114`)
  - M6 — `sandbox-load-into-analysis` が stat 後のパスを
    DuckDB に渡すが、DuckDB は内部で symlink 解決する。
    H1 と同じ EvalSymlinks→prefix チェックが必要
    (`internal/agent/sandbox_tools.go:369`)

`M4`（DuckDB `LoadFile` の SQL を `'` ダブル化で防御している
string-concat 構成）は**今回対応外**。現状の防御は機能しており、
parameterize 化は analysis パッケージの構造変更を伴うため、
DuckDB の bind API がスタックで安定してから着手する。

## 2. ゴール / 非ゴール

### ゴール

1. `/work` からの symlink トラバーサル escape 全閉塞
   （H1, M6） — write/read 双方
2. `objstore.Store` の race 解消で並行 agent 動作中の
   ランタイム panic を排除（H2）
3. `:Z` を不適切な engine/host で付与しない（H3）
4. stale な MITL response 排除（M1）
5. `agent.guardians` の write を一貫してロック（M2）
6. 起動時 stale container スイープ、設定ドリフト時の
   sandbox 再起動（M3, M5）

### 非ゴール

- **新 sandbox API なし**。既存 6 ツールそのまま、形も同じ。
  enforcement 側のみ修正
- **SQL parameterize リファクタなし**（M4 deferred）
- **SELinux テスト環境なし**。条件付き `:Z` は flag-builder
  の単体テストで検証、SELinux VM は使わない
- **脅威モデル不変**。LLM 出力 / ツール引数 / MCP server 出力
  が untrusted、ホストユーザは trusted

## 3. 詳細設計

### 3.1 Phase 1 — 並行性正しさ（H2, M2）

純 Go。UI / integration には触れない。

**3.1.1 `objstore.Store` mutex**

`Store` に `sync.RWMutex` 追加。ロックポリシー:

| メソッド | ロック |
|---|---|
| `Store`, `Save` | `Lock` |
| `Delete`, `DeleteBySession` | `Lock` |
| `Get`, `All`, `ListBySession`, `ListByType`, `ReadData` | `RLock` |

`ReadData` は `io.ReadCloser` を返し、ロック解放後も
存在し続ける。メタデータ参照は `RLock` 内、ファイルオープン
は**ロック外**で writer をブロックしない。メタデータの
ポインタはアンロック前にローカルへキャプチャしておくので
caller が破損状態を観測しない。

`Save` は map をイテレートするので `Lock` 配下で実行。
書き込み中に key が削除される race を避けるため、
disk への書き込みも `Lock` 内に含める。

**3.1.2 `agent.guardians` write ロック**

`startGuardians` での map 書き込みを `a.guardiansMu.Lock()` /
`defer Unlock()` で囲む。`RestartGuardians` が既に使っている
パターンと同じ。

**3.1.3 テスト**

- `objstore` パッケージ: `TestStore_ConcurrentStoreAndList` で
  32 goroutine（書き込み 16 / 一覧 16）、panic と torn read
  なしを assert。`-race` で実行
- `agent` パッケージ: 既存 `TestRestartGuardians` を
  `ListTools` と並走させて `-race`

### 3.2 Phase 2 — sandbox isolation（H1, H3, M3, M5, M6）

sandbox 表面に触れるためリスクは高めだが、各サブ修正は
ローカル scoped。

**3.2.1 `safeWorkPath` symlink チェック（H1）**

lexical のみのチェックを以下に置換:

```go
func (a *Agent) safeWorkPath(sid, rel string) (string, error) {
    workDir := a.sandbox.WorkDir(sid)
    cleaned := filepath.Clean(filepath.Join(workDir, rel))
    if !strings.HasPrefix(cleaned, workDir+string(filepath.Separator)) && cleaned != workDir {
        return "", fmt.Errorf("path escapes /work: %q", rel)
    }

    // 親ディレクトリ階層で symlink 解決。leaf は
    // write 操作だとまだ存在しない可能性があるので
    // 親だけ resolve して leaf を rejoin。
    parent := filepath.Dir(cleaned)
    leaf := filepath.Base(cleaned)
    resolvedParent, err := filepath.EvalSymlinks(parent)
    if err != nil {
        if !errors.Is(err, fs.ErrNotExist) {
            return "", err
        }
        return cleaned, nil
    }
    if !strings.HasPrefix(resolvedParent, workDir) {
        return "", fmt.Errorf("path escapes /work via symlink: %q", rel)
    }
    final := filepath.Join(resolvedParent, leaf)

    // leaf 自体が symlink ならば（同一ディレクトリ内
    // でも）拒否。後続の同一 session 操作の攻撃面を
    // 残さない。
    if info, err := os.Lstat(final); err == nil && info.Mode()&os.ModeSymlink != 0 {
        return "", fmt.Errorf("path is a symlink: %q", rel)
    }
    return final, nil
}
```

LLM-controlled なパスを取る全ての `os.Open` / `os.Create` /
`os.WriteFile` / `os.Stat` 呼び出しに適用:

- `toolSandboxWriteFile`（現在 `os.WriteFile`）
- `toolSandboxCopyObject`（現在 `os.Create`）
- `toolSandboxRegisterObject`（現在 `os.Open`）
- `toolSandboxLoadIntoAnalysis`（現在 `os.Stat` と
  DuckDB に渡るパス） — M6 をカバー

`safeWorkPath` 戻り値の解決済みパスを caller がそのまま使う。
`LoadIntoAnalysis` は解決済みパスを DuckDB に渡し、DuckDB は
それ以上 resolve するものがない。

**3.2.2 条件付き `:Z` マウントラベル（H3）**

`cliEngine` に `engineKind`（podman / docker）と `osKind`
（linux / darwin / その他）を保持。`buildRunArgs` で
`engineKind == podman && osKind == linux` のときのみ
`:Z` を付与、それ以外は明示的に `:rw`（現状デフォルト）。
新しい config キーは増やさない — 環境から正解が決まる。

**3.2.3 起動時スイープ + `Close` の信頼性（M3）**

`maybeStartSandbox` で `sandbox.NewCLI` 成功直後、
`EnsureContainer` の前に `eng.StopAll(ctx)` を呼ぶ。
スイープした件数をログに出す（クリーン起動時はゼロ）。
スイープは既存の `label=app=shell-agent-v2` で絞るので
他人の container には触れない。

加えて `main.go` に `signal.Notify(SIGINT, SIGTERM)` を
仕込み、`agent.Close()` 後 `os.Exit(0)`。Wails の
`shutdown` フックは残す — シグナルハンドラは Wails
フックが発火する前に OS が落とすケース（`kill -TERM`
など）の belt-and-braces。

`SIGKILL` は捕捉不可能。これは起動時スイープでカバー。

**3.2.4 SaveSettings での sandbox 再起動（M5）**

`Bindings.SaveSettings` は既に backend 設定変更時に
`RestartLLMBackend` を呼んでいる。同様に
`RestartSandboxIfChanged` を追加: 直前の
`config.SandboxConfig` と新しいものを比較
（`reflect.DeepEqual` で十分、小構造体・map なし）し、
差分があれば `agent.RestartSandbox(ctx)` を呼んで
`StopAll` し、次のツール呼び出しで再生成させる。

`Sandbox.Enabled` が `true` → `false` のときは既存
ツールが unregister される。`false` → `true` のときは
次の `buildToolDefs` でツールが見えるようになる。
進行中の sandbox exec を強制キャンセルはしない —
ユーザはいつでも abort できる。

**3.2.5 テスト**

- `sandbox_tools_test.go`:
  `TestSafeWorkPath_BlocksAbsolute`,
  `TestSafeWorkPath_BlocksDotDot`,
  `TestSafeWorkPath_BlocksSymlinkLeaf`,
  `TestSafeWorkPath_BlocksSymlinkInParent`,
  `TestSafeWorkPath_AcceptsValidNewLeaf`。
  symlink テストは `t.TempDir()` + `os.Symlink` でトラップ構築
- `cli_test.go`:
  `TestBuildRunArgs_ZLabelOnPodmanLinuxOnly` で
  `engineKind`/`osKind` 順列を駆動して argv を assert
- `bindings_test.go`:
  `TestSaveSettings_RestartsSandboxOnChange` で fake
  sandbox engine が `StopAll` 呼び出しを記録

### 3.3 Phase 3 — MITL チャネル強化（M1）

`b.mitlChan chan agent.MITLResponse`（単一・buffer 1・
`*Bindings` ライフタイム）を per-request チャネルに置換:

```go
type Bindings struct {
    ...
    mitlMu  sync.Mutex
    mitlReq *mitlSlot
}
type mitlSlot struct {
    req agent.MITLRequest
    ch  chan agent.MITLResponse
}
```

MITL handler closure（`bindings.go` の `agent.SetMITLHandler`）:

```go
func(req agent.MITLRequest) agent.MITLResponse {
    ch := make(chan agent.MITLResponse, 1)
    b.mitlMu.Lock()
    b.mitlReq = &mitlSlot{req: req, ch: ch}
    b.mitlMu.Unlock()

    wailsRuntime.EventsEmit(b.ctx, "mitl:request", ...)

    resp := <-ch

    b.mitlMu.Lock()
    b.mitlReq = nil
    b.mitlMu.Unlock()
    return resp
}
```

Approve / Reject / RejectWithFeedback は slot 経由で resolve:

```go
func (b *Bindings) ApproveMITL() {
    b.mitlMu.Lock()
    slot := b.mitlReq
    b.mitlMu.Unlock()
    if slot == nil { return } // stale クリック、no-op
    select {
    case slot.ch <- agent.MITLResponse{Approved: true}:
    default: // 既に resolve 済み
    }
}
```

リクエスト未送信時のクリックは `mitlReq == nil` で no-op。
ダブルクリックは 2 回目で `default` 分岐。次の MITL
リクエストには新しい channel が割り当たるので、buffered
stale 値の経路は存在しなくなる。

**テスト**:
`bindings_test.go`:
`TestMITL_StrayApproveBeforeRequest_NoOps`,
`TestMITL_DoubleApproveSameRequest_Idempotent`,
`TestMITL_TwoRequestsInSeries_NoLeakBetween`。

### 3.4 Phase 4 — 範囲外

`M4`（analysis パッケージの SQL parameterize）は今回見送り。

## 4. 影響ファイル

| Phase | ファイル | 変更 |
|---|---|---|
| 1 | `internal/objstore/objstore.go` | RWMutex |
| 1 | `internal/objstore/objstore_test.go` | 並行性テスト |
| 1 | `internal/agent/agent.go` | `startGuardians` ロック |
| 2 | `internal/agent/sandbox_tools.go` | `safeWorkPath` 書き換え + LoadIntoAnalysis にも適用 |
| 2 | `internal/sandbox/cli.go` | 条件付き `:Z`、（既にある） `StopAll` を engine 経由で公開 |
| 2 | `internal/agent/agent.go` | `maybeStartSandbox` 内の起動時スイープ、SaveSettings ドリフト検出 |
| 2 | `main.go` | SIGINT/SIGTERM ハンドラ |
| 2 | `bindings.go` | SaveSettings の sandbox 再起動配線 |
| 2 | テスト各種 |
| 3 | `bindings.go` | mitlReq slot, mutex |
| 3 | `bindings_test.go` | 新規 3 テスト |

## 5. テスト計画

### 単体（外部依存なし）
- objstore 並行ストレステスト（`-race`）
- guardians map race（`-race`）
- safeWorkPath の正常系 / 異常系（symlink 含む）
- buildRunArgs `:Z` 順列
- MITL slot races

### 統合（podman/docker が PATH に必要）
- 既存 `internal/sandbox/integration_test.go` 引き続き pass
- 新規 `TestIntegration_SymlinkAttempt`: container 内で
  `/work/foo → /etc/passwd` を作成、外側から
  `sandbox-write-file path=foo` 実行 → ホスト
  `/etc/passwd` 不変かつツールがエラーを返すことを assert

### 手動
- `podman machine` 未起動でアプリ起動 → スイープが無害なことを確認
- セッション実行 → アプリ強制終了（Activity Monitor）
  → 再起動 → 前 container がスイープされることを確認
- MITL プロンプト出して Approve を高速 2 連打 →
  解決は 1 回のみであることを確認
- MITL pending 無し時の Approve（dev mode で binding
  直接コール）→ no-op であることを確認

## 6. リスクと緩和策

| リスク | 緩和策 |
|---|---|
| `safeWorkPath` 書き換えで正常な dot パスや中間ディレクトリ付きパスが壊れる | `subdir/file.csv`, `./file.csv`, `a/b/c.txt` の正常系テストを追加。新規ディレクトリ作成 write の場合は parent の `os.IsNotExist` を明示的に許容 |
| 条件付き `:Z` のために `Exec` ごとに `os.Hostname` 風 probe が走る | `NewCLI` で 1 回だけ probe、`cliEngine` 構造体にキャッシュ |
| 起動時スイープが、別インスタンスがまだ使っている container を誤って削除 | shell-agent-v2 は単一インスタンス前提（`.app` バンドル）。複数同時起動非対応。サポートする日が来たらリーダー選出哨戒で gate。今回範囲外 |
| MITL 書き換えで `mitl:request` イベントの発火タイミングが変わる | イベント発火は引き続き handler が `<-ch` で blocking する前。UI 挙動同一。テストで順序を assert |
| 既存ユーザの保存セッションが lexical-only チェック前提のパスを含む | ディスク形式不変。次回アクセス時に新しいチェックを通るだけで、stricter になるのは symlink のみ。ユーザセッションが symlink 依存していることはまずない |

## 7. フェーズ分割

順序付き 4 コミット:

1. **chore(security): concurrency locks for objstore and guardians (H2, M2).** 純粋な正しさ修正、最低リスク
2. **fix(security): block symlink traversal in /work and conditional :Z mount label (H1, H3, M6).** 影響最大、sandbox 層に scoped
3. **feat(sandbox): startup container sweep + signal-driven shutdown + restart on config drift (M3, M5).** 信頼性、Phase 2 に依存
4. **fix(security): per-request MITL channel (M1).** Phase 1-3 と独立、いつでも投入可能。能動的脅威としては最小（ユーザ UI レース + プロンプト未送信状態という発火条件があるため、他より起こしにくい）なので最後

各コミットにテスト同梱、フェーズ間で実セッション動作確認。
Phase 4 完了後 v0.1.18 リリース。

## 8. 範囲外

- M4（DuckDB SQL parameterize）— 現状の防御が機能している
- Container `--pids-limit`（LOW）— `--cpus`/`--memory` で
  fork-bomb の影響範囲は既に限定
- ChatInput の SVG フィルタ厳格化 — defense-in-depth のみ。
  別途 cleanup commit で都合のよい時に
- `TestSendReturnsToIdle` の環境 flake — 既に TODO.md に記載

# セキュリティ強化 第2ラウンド — 設計文書

> 日付: 2026-05-02
> ステータス: 提案中
> スコープ: 2026-05-02 監査で確定した 15 件 (Critical 4 / High 11)。
> 5 コミットに分割し、各 Phase 単体でレビュー・revert 可能とする。
> v0.1.18 で出荷した `security-hardening.ja.md` の続編。

## 1. 背景

2026-05-02 に第 2 回セキュリティレビューを実施。エージェント /
ツール層、サンドボックス / MCP 層、LLM / chat / contextbuild、
ストレージ / 解析、Wails バインディング / フロントエンドを対象。

レビュー結果のうち、v0.1.18 で対応済みの項目（per-request MITL
チャネル、symlink 対応 `safeWorkPath` 等）と false positive は §8
に列挙する。本文書は **v0.1.19 現コードに対して再検証してなお残る
finding** のみを対象とする。

## 2. Findings (確認済み)

### Critical

- **C1 — `refreshTableMeta` の SQL injection.**
  `internal/analysis/engine.go:572-573` が
  `SELECT comment FROM duckdb_tables() WHERE table_name = '%s'`
  を文字列連結。同ファイル内の他の呼び出し点は
  `sanitizeIdentifier` 経由だが、ここだけ漏れている。テーブル名
  は `SetTableDescription` 経由で LLM 入力から到達可能。`t' OR
  '1'='1` のような名前でメタデータ漏洩 / クエリ破綻が起こる。
- **C2 — MCP guardian の stderr が未ドレイン.**
  `internal/mcp/mcp.go:77-92` は stdin/stdout pipe を取得するが
  `cmd.Stderr` は nil のまま。guardian が kernel pipe buffer
  (~64 KB) を超える stderr を書くとブロックし、parent は
  `stdout.Scan` で待機しデッドロック。チャットセッション全体が
  ハングする。
- **C3 — Sandbox stdout/stderr 無制限.**
  `internal/sandbox/cli.go:525-528` は両 stream を `bytes.Buffer`
  にキャップなしで取り込む。LLM が `sandbox-run-shell` で
  `cat /dev/zero` 等を流すと、timeout 発火前に出力全量を RAM に
  確保し OOM / 可用性問題に直結。
- **C4 — Summary cache 書込が非アトミック.**
  `internal/contextbuild/cache.go:128` が `os.WriteFile` 直書き。
  クラッシュ最中や 2 writer 競合で torn file が発生する。

### High

- **H1+H2（escalated） — Analysis ツールの MITL gate が UI には
  配線されているが dispatcher で honour されていない.**
  `ListTools()` (`internal/agent/agent.go:710-720`) が全 analysis
  tool を Settings → Tools リストに露出し、UI は per-tool MITL
  トグル（裏側は `cfg.Tools.MITLOverrides[name]`）を描画する。
  ところが analysis tool に対する dispatcher 分岐
  (`internal/agent/agent.go:1227-1243`) は `executeAnalysisTool`
  を直接呼び、**`MITLOverrides` を一切参照しない**。結果:

  | Tool | UI MITL トグル挙動 |
  |---|---|
  | `load-data`, `reset-analysis`, `promote-finding`, `describe-data`, `list-tables`, `query-preview`, `suggest-analysis`, `quick-summary`, `create-report` | ON にしても確認なしで実行 |
  | `query-sql`, `analyze-data` | OFF にしても強制 MITL（`tools.go:230, 251` で hard-code） |

  正規の API (`IsToolMITLRequired`,
  `internal/agent/agent.go:1377`) は `MITLOverrides` を参照する
  実装になっているが、`sandbox-*` と `mcp__*` でしか呼ばれて
  おらず (`agent.go:1247, 1257`)、analysis 系は完全にバイパス。
  実効重大度: ユーザーは Settings UI が示唆する **analysis tool
  の MITL を一切制御できない**。初回レビューで挙げた
  load-data/reset-analysis/promote-finding の欠落は、より大きな
  契約違反の sub-symptom にすぎなかった。
- **H3 — MCP ツール名 parse が脆弱.**
  `internal/agent/agent.go:1262` が
  `strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)` を
  使用。guardian 名・upstream tool 名に `__` が含まれると誤分割。
  guardian 名は config 由来（影響限定）だが、tool 名は MCP
  サーバ提供で制御できない。誤分割で別 guardian にルーティング
  されるか "not found" エラーが出る。
- **H4 — MCP response ID 未検証.**
  `internal/mcp/mcp.go` `call()` がリクエスト ID を発行するが
  応答の `resp.ID` と照合していない。挙動不審 / 悪意ある guardian
  が応答を順序入れ替えで返すと、別呼び出しの結果が混入する。
- **H5 — Sandbox image が mutable tag.**
  `internal/sandbox/engine.go:177` のデフォルトが
  `python:3.12-slim`。`ensureImage` がオンデマンド pull。レジストリ
  侵害（または pull 中の DNS / MITM）で image が差し替わると、
  LLM がその中で任意コード実行できる。ユーザーは override 可能
  だが mutable tag を選ぶ防止策はない。
- **H6 — local backend の `ToolCall.Arguments` 未検証.**
  `internal/llm/local.go:146-151` が `tc.Function.Arguments` を
  生 string で保持。JSON 妥当性・schema 適合・サイズの検証なし。
  数 MB のゴミ Arguments がエージェントループを止め、セッション
  記録を汚染する。
- **H9 — Findings ID race.**
  `internal/findings/findings.go:69` が `len(s.findings)+1` から
  ID を生成。mutex なし。現状はエージェントループからの逐次
  アクセスのみだが、`promote-finding` は dispatcher 経由・post-task
  WG も触りうる。並行化されると重複 ID で `DeleteByIDs` が誤削除
  する。
- **H10 — findings / pinned / objstore index に `fsync` / atomic
  rename なし.** `findings.go:62`, `pinned.go:61`, `objstore.go:106`
  が `os.WriteFile` 直書き。force quit / panic / OS crash で最新
  書込ロスト。チャットが「保存しました」と告げた直後にユーザー
  作業が消える。
- **H11 — 12-char hex object ID (48-bit エントロピー).**
  `internal/objstore/objstore.go:285-289`。birthday bound 16M
  オブジェクトで衝突、~1M で衝突確率 ~0.18%。ヘビーユーザーで
  しか現実的にならないが、衝突時に 2回目の `Store()` が 1回目を
  silent overwrite する（`O_TRUNC` で bytes も含めて clobber）。
- **H12 — Local backend `Chat()` の応答 body 無制限.**
  `internal/llm/local.go:371` `io.ReadAll(resp.Body)` がキャップ
  なし。streaming path はエラー body を 512 B にキャップしている
  が、success path は無制限。不正 / 設定ミスのローカル endpoint
  でアプリ OOM。
- **H14 — Path expansion が symlink を follow.**
  `internal/config/config.go:281-288` が `~/` を expand、
  `internal/analysis/engine.go:228-253` `validateFilePath` が
  `os.Stat`（symlink を follow）で確認。アプリのデータ dir に
  別プロセスが置いた symlink で、解析層が拒否すべき host path に
  リダイレクトされる。

### Low (継続課題)

- **L1 — guard.Wrap silent fallback.** `chat.go:172-174` と
  `contextbuild/render.go:59-61` が、`guard.Tag.Wrap` が err を
  返した場合に静かに unwrapped 文字列で fallback する。
  `nlk/guard` は nonce に `crypto/rand` を使用しており、失敗は
  実質 process 級の致命状態。防御深さで fail-closed に切替。

## 3. Goals / Non-goals

### Goals

1. `refreshTableMeta` の SQL injection を排除 (C1)
2. MCP guardian I/O のデッドロックフリー化 (C2) と応答ルーティング
   の信頼性確保 (H4)
3. Sandbox 出力 (C3) と local backend response body (H12) を固定
   メモリ上限化
4. データ パスの全 JSON store をクラッシュセーフに (C4, H10)
5. 破壊的解析ツールの MITL 欠落を塞ぐ (H1, H2)
6. MCP ツール名 parse を `__` collision に強化 (H3)
7. Sandbox image を digest pin、mutable tag 時に UI 警告 (H5)
8. Local backend からの `ToolCall.Arguments` を JSON 妥当性 +
   サイズで検証 (H6)
9. Findings ID 生成を race-free に (H9)、object ID エントロピーを
   拡張 + 書込時衝突検出 (H11)
10. 解析が使用する path expansion / file open 層で symlink 拒否
    (H14)
11. `guard.Wrap` を fail-closed に (L1)

### Non-goals

- **新サンドボックス API・新ツールなし.** 同じ shape、enforcement
  側のみ修正。
- **on-disk format 変更なし.** JSON store の schema は維持、書き方
  のみ変更。
- **retry policy リファクタなし.** v0.1.18 出荷の retry 層に手を
  加えない。
- **新規脅威モデルなし.** security-hardening.ja.md §1 と同じ:
  非信頼な LLM 出力 / tool args / MCP server 出力、信頼ホスト
  ユーザー、ユーザー編集の Dockerfile / MCP profile path はユーザー
  権威で当方は検証しない。
- **v0.1.18 の修正を再議論しない.** per-request MITL channel,
  symlink 対応 `safeWorkPath`, 条件付き `:Z`, 起動時 container
  sweep, SaveSettings sandbox restart は既に在席、本ラウンドは
  触らない。

## 4. 詳細設計

### 4.1 Phase A — DuckDB metadata, MCP wire, sandbox/local I/O caps (C1, C2, C3, H4, H12)

純バックエンド、UI surface なし。

**4.1.1 C1 — `refreshTableMeta` パラメータ化**

```go
// engine.go
commentRow := e.db.QueryRow(
    "SELECT comment FROM duckdb_tables() WHERE table_name = $1",
    tableName,
)
```

`engine_test.go` に regression を追加：`'`, `%`, NUL byte を含む
名前で `SetTableDescription` を呼び、エラーなくコメントが
round-trip すること。

**4.1.2 C2 — guardian stderr ドレイン**

```go
// mcp.go Start()
stderrPipe, err := g.cmd.StderrPipe()
if err != nil {
    return fmt.Errorf("stderr pipe: %w", err)
}
go func() {
    s := bufio.NewScanner(stderrPipe)
    s.Buffer(make([]byte, 0, 64*1024), 256*1024)
    for s.Scan() {
        logger.Debug("mcp[%s] stderr: %s", g.name, s.Text())
    }
}()
```

ドレイン goroutine は process exit 時の pipe close で自然終了。
追加の shutdown 同期は不要。

テスト: `mcp_test.go` に 1 MB を stderr に出した直後に stdout
で JSON-RPC 応答を返す fake guardian を追加し、timeout 内に応答が
届くことを確認（デッドロック検出）。

**4.1.3 C3 — sandbox 出力に上限**

新規 `internal/sandbox/limitedbuffer.go`:

```go
type limitedBuffer struct {
    buf       bytes.Buffer
    cap       int
    truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
    n := len(p)
    if l.buf.Len() >= l.cap {
        l.truncated = true
        return n, nil // discard, but report success to keep child happy
    }
    remaining := l.cap - l.buf.Len()
    if len(p) > remaining {
        l.buf.Write(p[:remaining])
        l.truncated = true
        return n, nil
    }
    l.buf.Write(p)
    return n, nil
}
```

cap は新規フィールド `cfg.Sandbox.MaxOutputBytes`（デフォルト
8 MB）。`Exec` が両 stream に `&limitedBuffer{cap: ...}` を渡す。
truncate 時は結果文字列に `"\n... [stdout truncated at 8 MB]"`
マーカーを末尾追加し、LLM に状況を伝える。

テスト: `cli_test.go` に `TestExec_TruncatesOversizedStdout`
（大量出力の Python ツール）を追加。

**4.1.4 H4 — MCP response ID を検証**

応答の `json.Unmarshal` 後、`resp.ID` を送信 ID と照合。不一致は
transport error として返す。後の pipelining 拡張に備え、ID 必須
要求に対し ID なし応答も拒否。

現契約は guardian あたり同時 1 in-flight call のみ（pipelining
無し）。pipelining 化には response demux が必要だが、ID チェック
はその前提条件でもある。

テスト: `mcp_test.go` `TestCall_RejectsResponseIdMismatch` —
全リクエストに `id: 999` で応える fake guardian を使用。

**4.1.5 H12 — local Chat() 応答 body 上限**

```go
// local.go Chat()
data, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalResponseBytes))
```

`maxLocalResponseBytes` 定数（16 MB; 現実的な LLM 応答に十分大きく
メモリは bound）。`ChatStream` の success path にも適用（現状は
unbound、512 B キャップは error body のみ）。

テスト: `local_http_test.go` に `httptest.Server` で 32 MB を
stream する `TestChat_RejectsOversizedResponse` を追加。

### 4.2 Phase B — analysis tool MITL を `IsToolMITLRequired` 経由に統一、MCP 名 parse (H1+H2, H3)

エージェント dispatcher に手を入れる。修正は per-tool checklist
ではなく **契約の修復**。

**4.2.1 H1+H2 — UI が既に広告している MITL gate に analysis tool
を通す**

連動する 2 変更:

(a) 既存の shell tool `Category` セマンティクスと同列に、analysis
tool の **MITL デフォルトカテゴリ** を 1 箇所で定義:

```go
// internal/agent/tools.go — 新規
var analysisToolMITLDefault = map[string]bool{
    "load-data":         true,  // host file 取り込み
    "reset-analysis":    true,  // 破壊的
    "promote-finding":   true,  // グローバル状態変更
    "create-report":     false, // ローカル成果物
    "describe-data":     false, // 純 read
    "list-tables":       false,
    "query-sql":         true,  // SQL preview は元々必須
    "query-preview":     false, // NL → SQL、実行なし
    "suggest-analysis":  false,
    "quick-summary":     false,
    "analyze-data":      true,  // analysis plan は元々必須
}
```

(b) `IsToolMITLRequired` を analysis tool 用に拡張、優先順は:

```go
func (a *Agent) IsToolMITLRequired(toolName string) bool {
    if override, ok := a.cfg.Tools.MITLOverrides[toolName]; ok {
        return override // ユーザー override が最優先
    }
    if strings.HasPrefix(toolName, "mcp__") {
        return true
    }
    if strings.HasPrefix(toolName, "sandbox-") {
        return true
    }
    if def, ok := analysisToolMITLDefault[toolName]; ok {
        return def
    }
    // shell tool: caller が tool.NeedsMITL を参照
    return false
}
```

(c) `executeAnalysisTool` 内の hard-coded MITL 呼び出し
（query-sql / analyze-data 分岐）を **削除** し、shell tool と
同じ dispatcher 階層 (`agent.go:1294`) のゲートに統一:

```go
case "load-data", "describe-data", "query-sql", ..., "analyze-data":
    if a.analysis == nil {
        return "Error: no analysis engine available", ActivityStatusError
    }
    if a.IsToolMITLRequired(tc.Name) {
        category := "write"
        if tc.Name == "load-data" || tc.Name == "reset-analysis" {
            category = "execute"
        } else if tc.Name == "query-sql" {
            category = "sql_preview"
        } else if tc.Name == "analyze-data" {
            category = "analysis_plan"
        }
        if rejection := a.requestMITL(tc.Name, tc.Arguments, category); rejection != "" {
            return rejection, ActivityStatusError
        }
    }
    result, err := a.executeAnalysisTool(ctx, tc.Name, tc.Arguments)
    ...
```

カテゴリ文字列 `sql_preview` と `analysis_plan` はフロントが既に
SQL preview ダイアログ / analysis plan ダイアログ用に special-case
している UI コードで、そのまま温存。

`executeAnalysisTool` は内部 `requestMITL` 呼び出しを drop。
MITL は全 tool source について dispatcher の責務となり、二重階層
の split が解消される。

**既存ユーザー MITLOverrides の移行**: 現状の config に
`{"load-data": true}` 等が既にある可能性（Settings UI で保存
された分）。修正後はこれらが **意図通りに動き始める**。移行
コード不要。

**`query-sql` や `analyze-data` の MITL を OFF にして prompt 抑制
を期待していたユーザーへの挙動変更**: 修正後は実際に抑制される。
CHANGELOG の "Fixed" subsection に記録。これが正しい挙動 — 従来は
UI が嘘をついていた。

テスト: `agent_test.go` に
`TestAnalysisTool_MITLOverrideRespected`（override を flip して
prompt が出る/出ないを assert）,
`TestAnalysisTool_DefaultsMatchTable`,
`TestAnalysisTool_HardCodedQuerySQLBypassRemoved` を追加。

**4.2.3 H3 — robust な MCP 名 parse**

guardian / tool 名を登録時に検証：

- guardian 名は `^[a-zA-Z0-9-]+$` 必須。`config.Load` で不正
  プロファイルは log + drop で拒否
- 上流 tool 名は `__` を含むことが正当にあり得るので、dispatcher
  側で `SplitN("__", 2)` の素朴 split に加え、登録 guardian 名の
  最長 prefix match で fallback：

```go
rest := strings.TrimPrefix(tc.Name, "mcp__")
guardianName, toolName, ok := splitMCPName(rest, a.guardians)
if !ok {
    return "Error: invalid MCP tool name format", ActivityStatusError
}
```

`splitMCPName` は素朴 split を試み、結果の guardian が未登録なら
登録 guardian 名を全走査して最長 prefix を採用。map サイズ小なので
最悪 O(n)。

テスト: `agent_test.go` に
`TestSplitMCPName_NaiveSplit`,
`TestSplitMCPName_GuardianContainsDoubleUnderscore`,
`TestSplitMCPName_ToolContainsDoubleUnderscore` を追加。

### 4.3 Phase C — クラッシュセーフ書込 / findings race (C4, H9, H10)

純バックエンド、UI surface なし。

**4.3.1 共通 atomic-write ヘルパ**

新規 `internal/atomicio/writefile.go`:

```go
// WriteFileAtomic は data を tmp+rename で path に書き込む。
// reader は常に旧 or 新ファイルを見、partial を見ない。
// macOS APFS では fsync(file) が directory entry の fsync に
// ならない移植性問題があるため、POSIX 系ではさらに parent dir も
// fsync する。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error
```

利用箇所:
- `objstore.saveLocked`
- `findings.SaveLocked`
- `pinned.Save`
- `contextbuild/cache.Save`
- `memory.Session.Save`（要確認、未対応なら含める）

テスト: `atomic_test.go` で rename happy path、source 既存ケース、
power-fail シミュレーション（rename 前）でも旧ファイルが読めること
を確認。

**4.3.2 H9 — findings ID 生成を lock 下に**

`Store` に `sync.Mutex` を追加。`Add` は `len(s.findings)` 読み・
ID 構築・append・状態変更を 1 ロック内で行う。後続の on-disk save
も同ロックで直列化。

ID format は現行 `f-YYYYMMDD-NNN` を維持しつつ、当日カウントが 999
を超えたら `f-YYYYMMDD-NNNNNN-<6 hex>` に fallback。hex suffix で
legacy ID（dash 2 個）と衝突せず on-disk 移行不要。

テスト: `findings_test.go` `TestStore_AddIsThreadSafe` — 64
goroutine で `Add` を並行呼び出し、ID ユニーク・最終件数一致を確認。

### 4.4 Phase D — MCP image pin warning, ToolCall validation, objstore ID 拡張, symlink 拒否 (H5, H6, H11, H14)

**4.4.1 H5 — image pin advisory**

digest pin の強制はしない（既存設定が壊れる）。代わりに：

1. `imagebuild` に `IsImageDigestPinned(tag string) bool` を追加
   （regex `@sha256:[a-f0-9]{64}$`）
2. `GetSandboxImageStatus` に `ActivePinnedByDigest bool` を追加。
   Settings dialog (Sandbox tab) は active image が mutable tag の
   時に警告バナーを表示：「This image uses a mutable tag. A
   registry or network compromise could replace it. Consider
   pinning by digest.」 image ごとに dismiss 可
   (`cfg.UI.DismissedSandboxWarnings`)

テスト: `imagebuild_test.go` で regex を確認；フロントエンドは
`SettingsDialog.test.tsx`（存在すれば）に snapshot test を追加。

**4.4.2 H6 — `ToolCall.Arguments` 検証**

`local.go` Chat() / ChatStream() の結果組立時に：

```go
// デフォルト 1 MiB。これは garbage / 攻撃の検出閾値であり、
// tight な resource limit ではない（メモリ防御は H12 の
// 16 MiB response body cap 側）。実 tool call（HTML/CSV/Python
// を書く sandbox-write-file、長文 markdown の create-report）は
// 100〜500 KB に普通に到達するため、1 MiB は余裕を確保しつつ
// response cap を十分下回る。backend 毎に
// cfg.LLM.*.MaxToolCallArgsBytes で override 可。
const defaultMaxToolArgsBytes = 1024 * 1024

for _, tc := range msg.ToolCalls {
    args := tc.Function.Arguments
    if len(args) > l.maxToolArgsBytes() {
        return nil, fmt.Errorf("tool call %q arguments exceed %d bytes",
            tc.Function.Name, l.maxToolArgsBytes())
    }
    if !json.Valid([]byte(args)) {
        return nil, fmt.Errorf("tool call %q arguments are not valid JSON",
            tc.Function.Name)
    }
    result.ToolCalls = append(result.ToolCalls, ToolCall{
        ID: tc.ID, Name: tc.Function.Name, Arguments: args,
    })
}
```

`config.LocalConfig` / `config.VertexAIConfig` の双方に
`MaxToolCallArgsBytes int` フィールドを追加（0 → default 1 MiB）。
UI surface には載せず config ファイル直接編集のみ — もっと大きくしたい
稀なユーザー向けの逃げ道として残す。

不正引数はエージェントループに伝播し、既存の LLM エラー分類で
hint を付けて再質問する経路に乗る。新規エラーパスなし。

テスト: `local_test.go` `TestChat_RejectsOversizedToolArguments`,
`TestChat_RejectsInvalidJSONToolArguments`。

**4.4.3 H11 — objstore ID 拡張 + 衝突チェック**

`generateID` を 16 byte（32 文字 hex）に拡張。128 bit 空間で衝突は
天文学的に低確率。既存 12 文字 ID は読み込み可能（read path は長さ
非依存）；新規 ID のみ wider format。

`Store()` に防御チェック：選択 ID が index に既存なら最大 3 回まで
再生成、それでも衝突したらエラー返却。実運用では発火しないが、
将来 ID 空間を縮める変更時の安全網。

テスト: `objstore_test.go` `TestStore_RejectsCollidingID`（test seam
で衝突を強制）, `TestGenerateID_NewIdsAre32Hex`。

**4.4.4 H14 — analysis path validation で symlink 拒否**

`internal/analysis/engine.go` `validateFilePath`:

```go
info, err := os.Lstat(path)
if err != nil {
    return fmt.Errorf("stat: %w", err)
}
if info.Mode()&os.ModeSymlink != 0 {
    return fmt.Errorf("symlinks are not allowed: %q", path)
}
if !info.Mode().IsRegular() {
    return fmt.Errorf("not a regular file: %q", path)
}
```

`config.ExpandPath` の `~/` 展開はそのまま（`~` は現ユーザの home
に展開、これは documented behaviour）。expansion 後に再度 resolve
して、展開先が `DataDir()` か home の外に出る symlink を経由する場合
は拒否。

テスト: `engine_test.go` `TestValidateFilePath_RejectsSymlink` —
`t.TempDir()` に symlink を作って拒否を確認。

### 4.5 Phase E — guard fail-closed (L1)

`chat.go:172-174` と `contextbuild/render.go:59-61`:

```go
wrapped, err := e.guardTag.Wrap(content)
if err != nil {
    // crypto/rand 失敗等の致命状態。続行を拒否し、unwrap された
    // 非信頼コンテンツを system prompt と共に LLM に渡すより
    // caller にエラーを返す方が安全。
    return llm.Message{}, fmt.Errorf("guard wrap: %w", err)
}
content = wrapped
```

`BuildMessages` は `(messages, error)` を返すよう変更。caller は
エラーを伝播。`Engine.WrapUserToolContent` は `(string, error)` を
返し、contextbuild は wrap エラーを build 全体の致命と扱う。

テスト: `chat_security_test.go` に err を返す fake guard を追加し、
`BuildMessages` がエラーと空メッセージを返すことを確認。

## 5. 触るファイル

| Phase | File | 変更内容 |
|---|---|---|
| A | `internal/analysis/engine.go` | `refreshTableMeta` をパラメータ化 |
| A | `internal/analysis/engine_test.go` | SQL escape regression |
| A | `internal/mcp/mcp.go` | stderr drain; response ID 検証 |
| A | `internal/mcp/mcp_test.go` | stderr-flood + ID-mismatch tests |
| A | `internal/sandbox/limitedbuffer.go` (新規) | bounded Writer |
| A | `internal/sandbox/cli.go` | `Exec` で limitedBuffer 使用 |
| A | `internal/sandbox/cli_test.go` | truncation test |
| A | `internal/llm/local.go` | Chat success body に LimitReader |
| A | `internal/llm/local_http_test.go` | oversized-response test |
| B | `internal/agent/tools.go` | `analysisToolMITLDefault` map; query-sql / analyze-data の hard-coded MITL を内部ハンドラから削除 |
| B | `internal/agent/agent.go` | `IsToolMITLRequired` を analysis tool default 対応に拡張、analysis 分岐をこれ経由に変更 |
| B | `internal/agent/tools_test.go` | MITL coverage + override-respected tests |
| B | `internal/agent/agent.go` | `splitMCPName` ヘルパ |
| B | `internal/agent/agent_test.go` | 三 split tests |
| B | `internal/config/config.go` | guardian-name regex on Load |
| C | `internal/atomicio/writefile.go` (新規) | tmp+rename ヘルパ |
| C | `internal/atomicio/writefile_test.go` (新規) | atomicity tests |
| C | `internal/objstore/objstore.go` | atomic save |
| C | `internal/findings/findings.go` | mutex + atomic save + ID overflow format |
| C | `internal/findings/findings_test.go` | concurrency test |
| C | `internal/memory/pinned.go` | atomic save |
| C | `internal/contextbuild/cache.go` | atomic save |
| D | `internal/sandbox/imagebuild/bundle.go` | `IsImageDigestPinned` |
| D | `bindings.go` | `ActivePinnedByDigest` field; 新規 dismissal config |
| D | `internal/config/config.go` | `DismissedSandboxWarnings` |
| D | `frontend/src/SettingsDialog.tsx` | mutable-tag バナー |
| D | `internal/llm/local.go` | argument-size + JSON-validity check |
| D | `internal/llm/local_test.go` | oversized / malformed args tests |
| D | `internal/objstore/objstore.go` | 16-byte ID + 衝突 regen |
| D | `internal/objstore/objstore_test.go` | 幅 + 衝突 tests |
| D | `internal/analysis/engine.go` | `validateFilePath` symlink 拒否 |
| D | `internal/analysis/engine_test.go` | symlink 拒否 test |
| E | `internal/chat/chat.go` | `BuildMessages` がエラー返却; fail-closed wrap |
| E | `internal/contextbuild/render.go` | wrap エラー伝播 |
| E | `internal/contextbuild/builder.go` | wrap エラー伝播 |
| E | `internal/agent/agent.go` | BuildMessages エラーを処理（ユーザーに surface, log） |
| E | `internal/chat/chat_security_test.go` | fail-closed test |

## 6. テスト計画

### Unit (外部依存なし)

- C1: SQL injection 風のテーブル名が安全に round-trip
- C2: 1 MB stderr 直撃で guardian の stdout がデッドロックしない
- C3: 大量 stdout が marker 付きで truncate される
- C4 / H10: torn-write シミュレーション — rename 中に process kill、
  次回起動で前回ファイルが intact
- H4: ID-mismatch 応答を拒否
- H6: oversized / malformed args from local backend を拒否
- H9: 64 goroutine 並行 `Add` でユニーク ID
- H11: ID 幅 = 32 hex; 強制衝突を拒否
- H12: oversized success body を拒否
- H14: validateFilePath で symlink 拒否
- L1: guard.Wrap エラー → BuildMessages がエラー返却

### Integration (podman/docker 必須)

- 既存 `internal/sandbox/integration_test.go` パス継続
- 新規 `TestIntegration_StdoutCapBlocksOOM`: Python tool が cap を
  大幅超過する出力を行い、結果が truncate されエージェントループが
  timeout 内に戻ることを確認

### Manual

- アプリ起動、`load-data` で CSV を ingest；MITL prompt が表示
  されることを確認
- Settings → Sandbox で active image を `python:3.12-slim` に
  したまま、新規 mutable-tag バナーが見えることを確認；digest pin
  に切替えるとバナーが消えることを確認
- `promote-finding` の round 中に force-quit、再起動後 findings.json
  が intact（旧 or 新状態のいずれか、partial なし）であることを確認

## 7. リスクと緩和策

| リスク | 緩和策 |
|---|---|
| Atomic-write ヘルパが各 JSON store 更新ごとに追加 `fsync` レイテンシを発生 | store は小ファイル書込で既に LLM hot path 外。`atomic_test.go` に microbench を追加し、median > 5ms なら dir fsync を緩める |
| `load-data` の MITL prompt が即時取り込みを期待するユーザーを煩わせる | `MITLOverrides` で per-tool opt-out 可（既存 knob）。CHANGELOG / README に明記 |
| `splitMCPName` の最長 prefix walk が大量 guardian で遅い | `len(guardians)` は人間入力 config、現実的に ≤ 10。O(n) walk で十分 |
| guard 失敗で unwrapped content を drop することが、稀な crypto/rand glitch でチャットを壊す | macOS で crypto/rand 失敗は kernel 読めない状態を意味し、アプリにより大きな問題がある。明確なエラーメッセージを surface、ユーザーリトライ |
| Local backend response cap (16 MB) で正当な巨大応答を切る | `cfg.LLM.Local.MaxResponseBytes` で設定可、デフォルト 16 MB を README に明記 |
| Object ID 幅変更で markdown report 内の既存 reference を再リンク強制 | 不要。read path は長さ非依存、旧オブジェクトは旧 ID 維持。新規生成 ID のみ 32 hex |

## 8. 明示的に out of scope

- **C5 — MITL request-ID binding.** 監査では指摘されたが、v0.1.18
  Phase 3 で buffered channel を per-request slot に置換済み、加えて
  エージェントループはシングルスレッドなので 2 MITL request が同時
  in flight にならない。追加修正なし。
- **H7 — guard wrap fallback.** 上記 L1 (fail-closed) に統合
- **H8 — `splitIdx = i + 1` budget off-by-one.** slice 算術を再読
  すると、現コードは正しい：`raw[:splitIdx]` で `splitIdx = i+1` は
  index 0..i 包含のレコード（index i は budget 入らずなので older
  尾部に回り summarise 対象）を生成。初回レビューの誤読
- **H13 — Dockerfile 直接編集.** Sandbox UX 仕様（ユーザーが自身の
  実行環境を定義）、ユーザーが trust authority
- **MCP guardian 親 crash 時の orphan.** v0.1.18 startup-sweep が
  container leak をハンドル、MCP guardian は stdio 子プロセスで親
  crash → stdin close（彼らが exit する documented signal）。v0.1.18
  で実証済、修正不要
- **Vertex AI エラーラッピング.** 再確認 — 伝播する SDK エラーは
  実運用で project ID を redact 済。変更なし
- **Loop-detector evasion.** detector は *advisory* hint であり
  safety limit ではない。max-rounds が実ループを cap。複雑化に
  見合わず
- **Frontend ESLint rule for `rehypeRaw`.** hygiene PR として価値
  あり、本ラウンド対象外（コード修正でなく guard rail）。TODO.md
  に記録
- **`MITLOverrides` キー検証.** ユーザー制御 config、ユーザーが
  trust authority。現状維持

## 9. Phasing

5 つの小コミットを順番に。各 phase は test と CHANGELOG エントリ
付。phase 間で実セッションに対して検証。Phase E 後に v0.1.20
リリース。

1. **fix(security): MCP / sandbox I/O を bound、DuckDB metadata
   query をパラメータ化 (C1, C2, C3, H4, H12).** 影響最大、C-tier
   の問題群。バックエンドのみ
2. **fix(security): Settings → Tools の MITL トグルを analysis tool で実際に効くように修正; MCP 名 parse を robust 化 (H1+H2, H3).** UI が既に広告している `IsToolMITLRequired` ゲートに analysis tool を統一、query-sql / analyze-data の hard-coded MITL バイパスを削除、挙動変更を CHANGELOG 記録。手動 ingestion run でトグル両方向の挙動を検証
3. **fix(security): JSON 書込を atomic 化 / findings ID race 修正
   (C4, H9, H10).** 新規 `internal/atomicio` パッケージ；store 全体に
   機械的 sweep
4. **feat(security): mutable-tag 警告バナー; objstore ID 拡張;
   解析 path で symlink 拒否; ToolCall args 検証 (H5, H6, H11,
   H14).** UI surface は (D) のみ、他はバックエンド
5. **fix(security): guard wrap を fail-closed に (L1).** 最小変更；
   public function 署名を変えるため最後に出荷

各 phase は `make test`（`-tags no_duckdb_arrow` 付）と integration
smoke を pass。AGENTS.md / README.md / README.ja.md / CHANGELOG.md
は behaviour 変更と同コミットで更新（プロジェクト規約）。

## 10. Verification follow-ups（実機検証中の追加修正）

v0.1.20 の実機 smoke test 中に 2 件の問題を発見し、本ラウンドの
ギャップと不可分のため同リリースに同梱した。設計 ↔ 実装の対応関係
を正直に保つため記録する。

### 10.1 Settings → Tools トグルが dispatcher の実デフォルトを反映 (commit `324f93f`)

Phase B で MITL ルーティングを `IsToolMITLRequired` に統一し
`analysisToolMITLDefault` を導入したが、frontend の
`SettingsDialog.tsx` は依然「デフォルト状態」を局所的に
`category === 'write' || category === 'execute' || source === 'mcp'`
で計算していた。この式は `analysisToolMITLDefault` 導入前から
存在し、新マップが追加されても誰も同期しなかった。具体的症状:
`load-data` (category `read`, source `analysis`) はトグル OFF で
表示され、クリックして ON / OFF してもどちらも局所デフォルトと
一致するため override が保存されず、dispatcher は新マップで
`IsToolMITLRequired("load-data") == true` を返して prompt を発火。

修正:

- `agent.ToolInfoItem` と `bindings.ToolInfo` の双方に
  `MITLDefault bool` フィールドを追加
- `Agent.ListTools` は新ヘルパ `Agent.toolMITLDefault` で値を埋める。
  これは override を見ない `IsToolMITLRequired` 同等のルール
- `SettingsDialog.tsx` は `t.mitl_default` を直接読み、
  局所計算をやめる
- 同調査で `IsToolMITLRequired` が shell tool に対して `false` を
  返す一方、dispatcher の shell tool 分岐は `tool.NeedsMITL()` を
  直接呼んでいる（別経路で同じ意図）ことも判明。
  `IsToolMITLRequired` を shell tool registry 経由に拡張し、
  dispatcher も同関数経由に統一。これで全 tool source が単一
  関数経由で MITL 解決
- `TestListTools_MITLDefaultMatchesGate` で契約を pin

### 10.2 `load-data` が `~/` を展開 (commit `f67e436`)

§4.4.4 (H14) は `config.ExpandPath` を「unchanged」と書いていたが、
`validateFilePath` 自体がそれを呼んでいなかったことを見落として
いた。実際には LLM はユーザーが `~/Desktop/foo.csv` と打てば
そのまま渡し、`filepath.Abs` は `~` を残し、`os.Lstat` が
「アクセス不能」と返し、LLM が「ファイルが見つかりません」と
返答する。修正は `validateFilePath` で `filepath.Abs` の前に
`config.ExpandPath(path)` を挟むだけの 1 行。
`TestValidateFilePath_ExpandsTilde` で regression 防止。MCP profile
path が以前から行っている挙動と一致。

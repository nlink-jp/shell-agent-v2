# サンドボックス実行 — 設計ドキュメント

> 作成日: 2026-04-28
> ステータス: レビュー用 Draft
> スコープ: 新パッケージ `internal/sandbox/`、新 `sandbox-*` ツール群、
>   config 追加、エージェントループ統合

## 1. 問題と動機

シェルツール (`list-files`、`weather` 等) は **ユーザーホスト上で
ユーザー権限** で実行される。ユーザーが事前に確認した小さな同梱
スクリプトには適切だが、LLM に任意のシェルや Python を走らせるには
不適切:

- ユーザーのファイルシステムへの副作用
- 一貫性のないランタイム — OS / インストール済みパッケージ /
  Python バージョンがユーザー毎にバラバラ
- リソース使用量を制限する手段なし (LLM が生成する暴走ループが
  ホストを食い尽くす)
- 失敗時に破棄できる clean snapshot がない

**コンテナベースのサンドボックス** で 4 点全てを解決:

- ファイルシステムアクセスをセッション毎の作業ディレクトリ
  (マウント) に限定
- 再現可能なランタイム (固定イメージ、Linux user-space 確定)
- CPU / メモリ / 時間制限はエンジンが強制
- 廃棄可能な状態 — `podman rm -f` でセッション環境をリセット

## 2. ゴール

- LLM はセッション毎のサンドボックス内でシェルコマンドと Python を
  実行できる
- 副作用 (ファイル、env 変更) はセッション内では生き残り、セッション
  間では分離、セッション削除で消失
- Podman / Docker のどちらでも動作、自動検出
- デフォルトで無効、Settings から opt-in
- 実行毎に MITL approval 必須 (他の write/execute ツールと同カテゴリ)

## 3. 非ゴール

- マルチホストオーケストレーション / Kubernetes
- GPU / ハードウェアパススルー
- 永続イメージ / commit-and-publish
- 設定可能 allow/deny ノブを超えるネットワーク egress (デフォルト off)
- リアルタイム stdout のチャット UI ストリーミング (Phase 4)

## 3a. 事前準備とセットアップ

shell-agent-v2 は macOS アプリ (Wails ビルド) として配布される。
サンドボックスはアプリが起動時に継承するシェル環境の PATH 上で
コンテナエンジンを発見できる必要がある。

### Podman (推奨)

```sh
brew install podman
brew install podman-desktop          # 任意、GUI
podman machine init                  # Linux VM 作成 (~2 GB ダウンロード)
podman machine start
podman ps                            # 動作確認 — ヘッダのみ表示、エラーなし
```

`podman machine init` が **"There is a problem finding the 'krunkit'
binary"** で失敗する場合は、Homebrew bottle が krunkit プロバイダを
要求しているが binary が PATH 上にない状態。2 つの解決策、開発時に
使ったのは A の方:

```sh
# Option A — Apple Hypervisor 使用 (Apple Silicon 推奨)
export CONTAINERS_MACHINE_PROVIDER=applehv     # ~/.zshrc に追加で永続化
podman machine reset                           # 中途半端な machine を破棄
podman machine init
podman machine start

# Option B — krunkit を明示的にインストール
brew install krunkit
```

過去の試行で残った config が krunkit を強制し続ける場合:

```sh
rm -rf ~/.config/containers ~/.local/share/containers
unset CONTAINERS_MACHINE_PROVIDER
podman machine init
```

### Docker Desktop

`brew install --cask docker` 後に Docker Desktop を起動するだけで OK。
`Engine: auto` 選択時にエージェントが自動で `docker` を拾う
(優先順は podman → docker)。

### インストール後

Sandbox 有効でアプリ起動時にログに
`sandbox: enabled (engine=…, image=…)` が出る。ツールは自動で
`sandbox-*` として **Settings → Tools** に表示される。

初回サンドボックスツールコールは image pull のため 30〜60 秒
かかる場合あり。以降は sub-second。

### イメージ切替

`Settings → Sandbox → Image` で任意の Podman/Docker イメージを指定可。
変更時:

- 既存コンテナは自動で reap (`RestartSandbox` 経由)
- 新イメージは次回サンドボックスツールコール時に lazy pull —
  ユーザーが手動で `podman pull` する必要は **ない**
- データサイエンス用途で pandas / matplotlib / numpy を即時使いたい
  場合は `jupyter/scipy-notebook` が定番 (~2 GB pull、scipy stack +
  多くの汎用パッケージ込み)

選択イメージで `pip install …` などでネットワークを必要とする場合は、
**Allow network egress** を一時 ON → install 実行 → OFF に戻す。

## 4. アーキテクチャ

```
agent loop
  │
  └─ tool: sandbox-run-shell / sandbox-run-python / sandbox-write-file
        sandbox-copy-object / sandbox-register-object / sandbox-info
       │
       ├─ MITL approval (category: execute)
       │
       └─ sandbox.Engine
            │
            ├─ ensureContainer(sessionID)
            │    │
            │    └─ 初回コール:
            │         podman/docker run -d --name shell-agent-v2-<sid>
            │              --workdir /work
            │              --volume <session>/work:/work
            │              --user $UID  --network none (default)
            │              --memory 1g  --cpus 2
            │              <image>  sleep infinity
            │
            └─ exec(sessionID, lang, code) -> ExecResult
                 │
                 └─ podman/docker exec -i <container>
                       --workdir /work
                       <interpreter> -c <code>
```

ライフサイクルフック:

- `Agent.New` → コンテナ操作なし
- セッションの初回 `run-*` コール → `ensureContainer`
- セッション削除 → `Engine.Stop(sessionID)` → コンテナ削除
- アプリ終了 → `Engine.StopAll()`、孤児コンテナは次回起動時に
  プロジェクトラベルで掃除

## 5. エンジン抽象

```go
package sandbox

type Engine interface {
    // Detect は解決された engine ("podman" or "docker") と PATH 上での
    // 利用可能性を返す。
    Detect() (string, bool)

    // EnsureContainer は sessionID 用のコンテナが起動していなければ
    // 作成・起動する。冪等。
    EnsureContainer(ctx context.Context, sessionID string) error

    // Exec は与えられたコマンドをセッションのコンテナ内で実行し、
    // 統合出力・終了コード・起動エラーを返す。
    Exec(ctx context.Context, sessionID string, args ExecArgs) (*ExecResult, error)

    // Stop はセッションのコンテナを破棄。
    Stop(ctx context.Context, sessionID string) error

    // StopAll はこのアプリが作成した全コンテナを掃除。
    StopAll(ctx context.Context) error
}

type ExecArgs struct {
    Language string   // "shell" | "python"
    Code     string
    Timeout  time.Duration
}

type ExecResult struct {
    Stdout   string
    Stderr   string
    ExitCode int
    TimedOut bool
}
```

実装は単一の `cliEngine` で、選択された CLI (`podman` または
`docker`) を `os/exec` でラップ。フラグの細部の違い以外は同一表面。

### コンテナ命名 / ラベル

```
name:   shell-agent-v2-<sessionID>
label:  app=shell-agent-v2
```

`StopAll` はラベルでフィルタするので、本アプリが作成したコンテナの
みに触れる: `podman ps -a -q --filter label=app=shell-agent-v2`。

## 6. セッション毎の作業ディレクトリ

既存アプリデータディレクトリ配下の構成:

```
~/Library/Application Support/shell-agent-v2/
  sessions/
    <sessionID>/
      chat.json
      summaries.json
      work/                       ← コンテナ内 /work にマウント
        <LLM が作成するファイル>
```

ディレクトリは初回 `EnsureContainer` で lazy 作成、既存の
`DeleteSessionDir` カスケードで削除 (work サブツリーまでカバーする
よう拡張)。

マウント: `--volume <abs-path>:/work:Z` (`Z` ラベルは Linux ホストの
SELinux 用、macOS では無害)。

LLM の `WORKDIR` は `/work`。`sandbox-run-shell` / `sandbox-run-python` の相対パスは
ここで解決される。書き込まれたファイルはコンテナ再起動を跨いで生き残り、
セッション削除で消える。

## 7. ツール表面 (LLM 向け)

全サンドボックスツールは `sandbox-` プレフィックスを共有し、モデルに
「同一セッションの `/work` ディレクトリで状態を共有する一貫した
スイート」と認識させる。計 5 本 — 実行系 2 本、データ移送系 3 本。

### `sandbox-run-shell`

```json
{
  "name": "sandbox-run-shell",
  "description": "Execute a shell command inside this session's sandbox container. Files in /work persist across calls within the session and are isolated between sessions. Side effects do not affect the host. Use for filesystem operations, package installs (pip), and orchestrating subprocesses.",
  "parameters": {
    "type": "object",
    "properties": {
      "command": {"type": "string", "description": "Shell command to execute."}
    },
    "required": ["command"]
  }
}
```

デフォルトタイムアウト: `cfg.Sandbox.TimeoutSeconds` (60秒)。

### `sandbox-run-python`

```json
{
  "name": "sandbox-run-python",
  "description": "Execute Python code inside this session's sandbox container. Working directory is /work; files there persist across calls. Each call is a fresh interpreter, but the filesystem and any installed packages persist within the session.",
  "parameters": {
    "type": "object",
    "properties": {
      "code": {"type": "string", "description": "Python source to execute."}
    },
    "required": ["code"]
  }
}
```

実装: shell が `code` を `python3 -c "$(cat)"` にパイプ、自前で
クォートエスケープ不要。

### `sandbox-write-file`

LLM → サンドボックス。モデルが既に作ったテキストコンテンツ (CSV、
JSON、ソース、Dockerfile 等) を `sandbox-run-shell` の heredoc 経由で
escape せずに `/work/<path>` に投入できる。

```json
{
  "name": "sandbox-write-file",
  "description": "Write text content to /work/<path> inside this session's sandbox. Use to seed the sandbox with data the LLM has already produced (CSVs, source files, configs) without escaping it through run-shell heredocs. Path must be relative to /work; parent directories are created if missing. Existing files are overwritten.",
  "parameters": {
    "type": "object",
    "properties": {
      "path":    {"type": "string", "description": "Relative path under /work, e.g. 'data.csv' or 'src/script.py'."},
      "content": {"type": "string", "description": "Text content to write."}
    },
    "required": ["path", "content"]
  }
}
```

実装: ホスト側マウントディレクトリ経由で直接書く (コンテナ内 hop
不要)。`..` traversal と絶対パスは reject。

### `sandbox-copy-object`

objstore → サンドボックス。中央リポジトリのオブジェクトをサンド
ボックス内にコピーし、LLM が `sandbox-run-python` (PIL, pandas 等)
でユーザーアップロード画像や過去のレポート、保存済み blob を分析
できるようにする。

```json
{
  "name": "sandbox-copy-object",
  "description": "Copy a stored object (image / blob / report) from the central object repository into /work/<path> inside this session's sandbox. Use to bring user-uploaded images or earlier reports into the sandbox for analysis. Use list-objects to find a valid object_id.",
  "parameters": {
    "type": "object",
    "properties": {
      "object_id": {"type": "string", "description": "Object ID from list-objects."},
      "path":      {"type": "string", "description": "Destination path under /work. Defaults to the object's orig_name when omitted."}
    },
    "required": ["object_id"]
  }
}
```

### `sandbox-register-object`

サンドボックス → objstore。サンドボックスで生成されたファイルを
中央リポジトリに昇格させ object ID を返す。LLM はレポート内で
`![alt](object:<id>)` として参照可能。分析 → 可視化 → レポートの
ループを手動ファイル移動なしに完結させる鍵。

```json
{
  "name": "sandbox-register-object",
  "description": "Register a file from /work (typically an output from sandbox-run-python — chart, generated CSV, etc.) into the central object repository. Returns the object ID, which can be referenced in reports as ![alt](object:ID).",
  "parameters": {
    "type": "object",
    "properties": {
      "path":      {"type": "string", "description": "Source path under /work."},
      "type":      {"type": "string", "description": "image | blob | report. Defaults to inference from MIME."},
      "name":      {"type": "string", "description": "Friendly name (orig_name) shown in the Objects panel; defaults to filename."}
    },
    "required": ["path"]
  }
}
```

実装はホスト側マウントディレクトリでファイルを読み、`type` 未指定なら
MIME から推定、`objects.Store(reader, type, mime, name, sessionID)`
を呼ぶ。

### `sandbox-info`

イントロスペクション。LLM がセッション開始時に 1 回呼んでサンドボックス
の利用可能なものを把握できる。シェルでプローブして無駄なターンを
消費するのを避ける目的。コンテナ毎に初回後はキャッシュされて軽量。

```json
{
  "name": "sandbox-info",
  "description": "Return a description of this session's sandbox: engine, image, Python version, key pre-installed packages, network policy, resource limits, and the contents of /work (path, size, mtime). Use this to discover the runtime before running code.",
  "parameters": {"type": "object", "properties": {}}
}
```

結果フォーマット (LLM 向けテキスト):

```
engine:    podman 4.9.4
image:     python:3.12-slim
python:    3.12.5
network:   off
limits:    cpus=2 memory=1g timeout=60s

packages (pip):
  pandas==2.2.2
  matplotlib==3.9.0
  numpy==1.26.4
  ...

work directory (/work):
  data.csv        12.4 KB  2026-04-28 22:45
  src/script.py    1.1 KB  2026-04-28 22:46
```

実装:

- engine + version: `podman --version` (`Detect` でキャッシュ)
- image / network / limits: `cfg.Sandbox` から取得
- python + packages: コンテナ内で
  `python3 -c "import sys; print(sys.version)"` +
  `pip list --format=freeze` を初回実行、engine 構造体に sessionID
  キーでキャッシュ
- work directory: ホスト側マウントディレクトリで walk、コンテナ
  ホップ不要、~50 件で truncate

キャッシュ無効化: `Stop(sessionID)` 時、および `sandbox-run-shell`
成功後 (`pip install` の可能性)。`sandbox-run-python` は無効化しない
(shell 経由 pip 呼び出しなしにパッケージ追加できない)。

### ファイル読み出しは専用ツール不要

`sandbox-read-file` は **意図的に追加しない** —
`sandbox-run-shell` の `cat /work/<path>` で十分にカバーでき、
既存の per-backend `MaxToolResultTokens` truncation がそのまま適用
される。専用ツール追加は重複となる。

### 結果フォーマット

実行系ツールの LLM 向け文字列は stdout、stderr (非空時)、フッタを連結:

```
<stdout>

[stderr]
<stderr>

[exit: 0]
```

`exit: <n>` は常に存在。truncation cap は per-backend
`MaxToolResultTokens` を再利用 (contextbuild / chat 既存のレンダ時
truncation)。

データ移送系ツールの結果は一行の確認、例:
`wrote 1.2 KB to /work/data.csv` あるいは
`registered as object 67ecaa…`。

## 8. 設定

`config.Config` に追加:

```go
type SandboxConfig struct {
    Enabled        bool   `json:"enabled"`               // default false
    Engine         string `json:"engine"`                // "auto" | "podman" | "docker"
    Image          string `json:"image"`                 // default "python:3.12-slim"
    Network        bool   `json:"network"`               // default false (no egress)
    CPULimit       string `json:"cpu_limit,omitempty"`   // default "2"
    MemoryLimit    string `json:"memory_limit,omitempty"`// default "1g"
    TimeoutSeconds int    `json:"timeout_seconds"`       // default 60
}
```

Settings UI: General に「Sandbox」セクション新設。

## 9. セキュリティモデル

多層防御 (粗い順):

1. **既定 off。** `Enabled=false` なら `run-shell` / `run-python` は
   ツール一覧に登場しない
2. **MITL。** 全コールが既存の承認プロンプトを通る。カテゴリは
   `execute`、ユーザーは実行前に必ずコード/コマンドを目視
   (シェルツール MITL と一貫)
3. **コンテナ分離。** ホストファイルシステムは見えない。マウントされた
   `/work` のみアクセス可
4. **既定ネットワーク off。** run コマンドに `--network=none`。
   ユーザーがネットワーク on にした場合のリスクは受容。DNS allow-list や
   proxy は配線しない
5. **リソース制限。** `--memory`、`--cpus`、コール毎のタイムアウト。
   `TimedOut=true` を LLM に通知
6. **非 root ユーザー。** `--user` でホスト UID を使用。イメージは
   非 root UID で動く必要あり (`python:3.12-slim` はマウントボリューム
   が WORKDIR なので動作する)

防御**しないもの**:

- `podman` / `docker` 自体の侵害
- コンテナ間サイドチャネル攻撃 (本サンドボックスは「ユーザーが LLM に
  実行依頼したものの隔離」用で、敵対的分離を目的としない)
- ユーザーが手動で session work dir 間でファイルをコピーした場合の
  情報リーク

## 10. 設定解決とデフォルト

```go
func (c *Config) SandboxConfig() SandboxConfig {
    s := c.Sandbox
    if s.Engine == "" { s.Engine = "auto" }
    if s.Image == "" { s.Image = "python:3.12-slim" }
    if s.TimeoutSeconds == 0 { s.TimeoutSeconds = 60 }
    if s.CPULimit == "" { s.CPULimit = "2" }
    if s.MemoryLimit == "" { s.MemoryLimit = "1g" }
    return s
}
```

`engine="auto"`: PATH 上の `podman` を優先、なければ `docker`。
両方なければ起動時に警告ログを 1 回出し、`Enabled` を実行時 false に
強制。

## 11. エージェントループ統合

- `agent.New` で `cfg.Sandbox.Enabled` かつ両 binary のいずれかが
  PATH にあれば sandbox engine を構築
- `agent.buildToolDefs` は engine が非 nil の時のみ
  `run-shell` / `run-python` を追加
- `agent.executeTool` で `run-shell` / `run-python` ケースを
  `engine.Exec` に dispatch
- `agent.LoadSession` ではコンテナを eager 起動しない。初回ツール
  使用が `EnsureContainer` をトリガ
- `agent.deleteSession` (`objstore.DeleteBySession` 後) で
  `engine.Stop(sessionID)` と `os.RemoveAll(workDir)` を呼ぶ
- `bindings.shutdown` で `engine.StopAll(ctx)`

## 12. 検証

### 単体

- `/bin/sh -c …` スタブに対する `cliEngine` (`Detect`、引数構築
  ヘルパー、ラベルフィルタパース)
- 各種 stdout/stderr 組み合わせ・終了コードでの LLM 向け結果文字列
  レンダリング

### 統合 (`podman`/`docker` 不在時はスキップ)

- `EnsureContainer` → `Exec` の自明コマンドラウンドトリップ
- 2 回の `Exec` 間のファイル永続性 (書き込み → 読み出し)
- 別セッション ID 間でのファイル不可視性
- `Stop` がコンテナを実際に削除し、後続 `Exec` が失敗すること
- タイムアウト: `Timeout: 1s` で `sleep 5` が `TimedOut: true` と
  非ゼロ exit を返す

### 手動

- dev モードでスモークテスト: sandbox 有効化、`run-python` で
  計算依頼、`run-shell` でファイル読み出し
- network=false の動作確認: コンテナ内 `curl` が失敗
- セッション削除でクリーンアップ: `podman ps` に孤児なし

## 13. 段階導入

| Phase | スコープ | デフォルト挙動 | ステータス |
|------|---------|--------------|----------|
| 1 | `internal/sandbox` パッケージ + テスト、エージェント未統合 | なし (dormant) | 完了 |
| 2 | Agent / config / Settings UI フックを `Enabled` flag 経由、初回使用時の自動イメージ pull、Settings 変更を再起動なしで反映する `RestartSandbox` | Settings から opt-in | 完了 |
| 3 | リアルタイム stdout チャットストリーミング、データサイエンス用 bundled イメージ亜種 | nice-to-have | 将来 |

## 14. Open Questions

1. **単一イメージ vs 設定可能。** 推奨: 1つの良いデフォルト
   (`python:3.12-slim` + `pip install pandas matplotlib jupyter`
   がプリベイク済みのカスタム小型イメージ) を別配布。設定で切替可能
   だが Settings に「変更しますか?」ヒントを表示
2. **起動時 pre-pull?** Sandbox 有効化時の `podman pull` で初回
   ツールコールから ~3 秒のラグが消えるが、明示的同意なしに数百 MB を
   pull することになる。推奨: 初回使用まで遅延、待機は既存の
   `tool-event` インジケータで可視化
3. **`run-shell` と `run-python` は全セッションに出現するか、有効
   セッションだけか?** 推奨: グローバルフラグ on の時は全セッション。
   per-session の work dir + コンテナはユーザーから透過
4. **クレデンシャル注入。** 一部 Python 作業は API キー (OpenAI、
   GCP) を必要とする。v1 ではスコープ外。ユーザーは prompt に
   inline で貼るか `/work/.env` に置いて source できる。host env vars
   を既定では転送しない
5. **macOS Docker Desktop 性能。** マウント I/O が Linux より遅い。
   本ユースケースでは許容、ドキュメント記載
6. **同時コール。** エージェントループはセッション毎に tool call
   シングルスレッドなので per-session call lock 不要。
   `EnsureContainer` は冪等で、コンテナ名がセッション ID キーなので
   別セッションからの並行コールも安全

## 15. 触る箇所まとめ

| ファイル | 変更内容 |
|---------|---------|
| `internal/sandbox/engine.go` | 新規: interface + cliEngine |
| `internal/sandbox/cli.go`    | 新規: podman/docker shell-out |
| `internal/sandbox/result.go` | 新規: ExecResult formatting |
| `internal/sandbox/*_test.go` | 新規: 単体 + 統合 |
| `internal/config/config.go`  | SandboxConfig + デフォルト追加 |
| `internal/agent/agent.go`    | engine 配線、dispatch、ライフサイクル |
| `bindings.go`                | Settings 表面、shutdown 時 StopAll |
| `frontend/src/App.tsx`       | Settings → General → Sandbox セクション |
| `docs/{en,ja}/sandbox-execution{,.ja}.md` | 本ドキュメント |

## 16. まとめ

新パッケージ `internal/sandbox` がユーザーの `podman` または
`docker` を使ってセッション毎のコンテナを管理する。LLM ツール 2 本
(`run-shell` / `run-python`) がコンテナ内で実行、ファイルは
`/work` にマウントされた session-scoped `work/` ディレクトリに
永続。既定 off、MITL 必須、ネットワーク既定 off、リソース制限あり。
ライフサイクルは lazy (初回ツール使用でコンテナ作成) でセッションに
紐付く (削除でコンテナと work dir 破棄)。段階導入により単体・統合
テスト緑化までパッケージは休眠。

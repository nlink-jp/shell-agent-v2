# Sandbox イメージビルド — 設計ドキュメント

> 日付: 2026-05-01
> ステータス: ドラフト（実装未着手）
> 範囲: 推奨 sandbox image (Python + CJK フォント + 解析
> スタック) と、Settings からビルド可能なビルド機構を
> 内蔵する。sandbox ツールは「イメージがビルド済み」かつ
> `Sandbox.Enabled = true` の **両方** が満たされたときに
> 初めて有効化される。

## 1. 問題

既定の sandbox image (`python:3.12-slim`) には CJK フォン
トが入っていない。matplotlib で日本語ラベルを描画すると
`□□` になる。現状のワークアラウンド: 重い image に切り替
える / `apt-get install fonts-noto-cjk` を手動実行 /
mojibake を許容。どれも発見性が低い。

関連する 2 つ目の問題: sandbox は「Enabled だが未設定」と
いう状態に静かに入りうる — 既定 image、フォントなし、解析
ライブラリも未インストール。ユーザはリッチな Python 出力を
期待してまっさらなインタプリタを得る。

推奨経路はワンクリックにしたい: **Build image** →
**Enable sandbox** → ツール登録。Dockerfile を手で編集
する必要も apt-get レシピを覚える必要もない。

## 2. ゴール / 非ゴール

### ゴール

1. CJK フォントと共通解析スタック (pandas, numpy,
   matplotlib, scipy, scikit-learn) を含む sandbox image
   を生成する Dockerfile をバイナリに内蔵する
2. Settings UI のボタンでローカルの podman/docker 上で
   このイメージをビルドできる。ビルド進捗はログ
   オーバーレイにストリームし、ユーザは進行を確認できる
   (apt-get + pip install で数分かかる)
3. sandbox ツール (8 個の `sandbox-*` ToolDef) は **設定
   image がローカル engine に存在し** かつ `Sandbox.Enabled`
   が true のときにのみ登録される。Build せず Enable した
   ユーザは明確なヒントを受け、ツールは hidden のまま
4. 既定の `cfg.Sandbox.Image` はバンドル image の正式
   タグを指す。happy path は「build → enable → 動く」
5. 上級ユーザが `Sandbox.Image` を別のタグ
   (`python:3.12-slim`、独自レジストリ image 等) に設定
   した場合は今まで通り動作する — readiness チェックは
   「我々がビルドしたか」ではなく「engine にこのタグが
   あるか」

### 非ゴール

- **起動時自動ビルドなし。** ビルドは明示的・ユーザ
  起動。予期せぬ数分待ちと黙ったネットワーク使用を回避
- **registry への push なし。** ローカルのみ
- **multi-arch matrix なし。** ビルドはローカル engine が
  サポートするアーキテクチャ (Apple Silicon なら通常
  arm64、Linux なら amd64) で動く
- **UI から内蔵 Dockerfile のライブ編集なし。** 上級
  ユーザは repo を clone して編集する。内蔵コピーが
  サポート対象
- **podman/docker のデフォルト以上のレイヤ再利用ロジック
  なし。** レイヤキャッシュは engine の標準機能でカバー
- **image GC なし。** タグはユーザが手動で削除するまで
  engine 上に残る

## 3. 詳細設計

### 3.1 内蔵 Dockerfile bundle

新パッケージ `internal/sandbox/imagebuild`:

```
internal/sandbox/imagebuild/
├── bundle.go         // go:embed all:bundle/*  + version 定数
└── bundle/
    ├── Dockerfile
    └── matplotlibrc
```

`bundle.go`:

```go
package imagebuild

import "embed"

//go:embed all:bundle
var Bundle embed.FS

// BundleVersion は bundle/ 配下の任意のファイルが、
// 既存ビルド image を無効化すべき形で変更されたら
// 必ず bump する。image タグは
// "shell-agent-v2-sandbox:<BundleVersion>" 形式なので、
// version の更新は次に Build を押した時点で再ビルドを
// 強制する。
const BundleVersion = "0.1"

// CanonicalTag は Build が生成するタグかつ
// ImageReady() が engine 上で探すタグ。
const CanonicalTag = "shell-agent-v2-sandbox:" + BundleVersion
```

`bundle/Dockerfile`:

```dockerfile
FROM python:3.12-slim

# CJK フォント — これがないと matplotlib は日本語 / 中国
# 語 / 韓国語ラベルを □□ で描画する。fonts-noto-cjk が
# フルカバー、-extra でバリエーション追加。
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        fonts-noto-cjk \
        fonts-noto-cjk-extra \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# 解析定番ライブラリ。
RUN pip install --no-cache-dir \
        pandas \
        numpy \
        matplotlib \
        scipy \
        scikit-learn

# matplotlib 設定: 日本語ラベル付きグラフが
# rcParams 明示なしでも描画されるよう、Noto Sans CJK JP
# を font fallback に入れた既定 rcParams を配置する。
COPY matplotlibrc /etc/matplotlib/matplotlibrc
ENV MATPLOTLIBRC=/etc/matplotlib/matplotlibrc

WORKDIR /work
```

`bundle/matplotlibrc`:

```
font.family: sans-serif
font.sans-serif: DejaVu Sans, Noto Sans CJK JP, Arial, Liberation Sans
axes.unicode_minus: False
```

### 3.2 `Engine.BuildImage`

`internal/sandbox/engine.go` の `Engine` インターフェース
に追加:

```go
// BuildImage は内蔵 imagebuild bundle が記述する image を
// ビルドし、`tag` でタグ付けする。engine の stdout/stderr
// は 1 行ずつ onLine にストリームする (nil 安全)。
//
// ビルドコンテキストは temp dir。bundle のファイルを
// そこに展開し、return 時にクリーンアップする。
//
// 並行呼び出しは engine 内部でシリアライズ: 2 回目は
// 1 回目が終わるまでブロックする。
BuildImage(ctx context.Context, tag string, onLine func(string)) error
```

`cliEngine.BuildImage` 実装:

```go
func (e *cliEngine) BuildImage(ctx context.Context, tag string, onLine func(string)) error {
    e.buildMu.Lock()
    defer e.buildMu.Unlock()

    bin, ok := e.Detect()
    if !ok {
        return ErrEngineNotAvailable
    }

    // 内蔵 bundle を temp dir に展開。
    workDir, err := os.MkdirTemp("", "shell-agent-v2-build-*")
    if err != nil {
        return fmt.Errorf("temp dir: %w", err)
    }
    defer os.RemoveAll(workDir)

    if err := writeBundle(workDir); err != nil {
        return fmt.Errorf("write bundle: %w", err)
    }

    // podman/docker build は -f とコンテキスト dir を受ける。
    cmd := exec.CommandContext(ctx, bin, "build", "-t", tag, workDir)
    stdout, _ := cmd.StdoutPipe()
    stderr, _ := cmd.StderrPipe()

    if err := cmd.Start(); err != nil {
        return fmt.Errorf("start: %w", err)
    }

    // stdout / stderr の両方を到着順で onLine に渡す
    // ため scanner を 2 つ走らせる。
    var wg sync.WaitGroup
    wg.Add(2)
    go streamLines(stdout, onLine, &wg)
    go streamLines(stderr, onLine, &wg)
    wg.Wait()

    if err := cmd.Wait(); err != nil {
        return fmt.Errorf("build: %w", err)
    }
    return nil
}
```

`writeBundle` は `imagebuild.Bundle` (`embed.FS`) を walk
して entry を workDir に同名でコピーする。

### 3.3 `Engine.ImageReady`

```go
// ImageReady は engine 上に `tag` が存在するかを返す。
// agent が sandbox ツールを露出するか判断するために使う。
ImageReady(ctx context.Context, tag string) (bool, error)
```

実装: `podman image exists tag` (内部の `ensureImage` で
既に使用)。exit 0 → `(true, nil)`、image missing exit
→ `(false, nil)`、それ以外の engine error → `(false, err)`。

### 3.4 Agent / sandbox 有効化のゲート

`agent.maybeStartSandbox`:

```go
if !cfg.Sandbox.Enabled {
    return
}
eng, err := sandbox.NewCLI(...)
if err != nil { ... }

// 新規: image readiness チェック。
ready, err := eng.ImageReady(ctx, cfg.Sandbox.Image)
if err != nil {
    logger.Info("sandbox: image readiness probe failed: %v — tools will stay hidden", err)
    return
}
if !ready {
    logger.Info("sandbox: image %q not built; click 'Build sandbox image' in Settings — tools will stay hidden", cfg.Sandbox.Image)
    return
}
a.sandbox = eng
// startup sweep など
```

`buildToolDefs` は既に `a.sandbox != nil` でゲートされて
いる。このゲートが暗黙に image readiness も要求すること
になる。

`SaveSettings` は `cfg.Sandbox` の差分で `RestartSandbox`
を呼ぶ (v0.1.18 で追加済み)。Build 後は config 差分なし
でも agent に readiness 再チェックさせたいので、
`Bindings.RebindSandbox()` をビルドフロー専用に追加し、
config diff を経由せず同じパスを叩く。

### 3.5 Bindings + Wails イベント

```go
// BuildSandboxImage は内蔵 image の正式タグでビルドを
// 開始する。即時 return し、進捗は Wails イベントで
// 送る:
//   - "sandbox:build:line"  payload {line string}
//   - "sandbox:build:done"  payload {tag string, error string}
// プロセス当たり同時 1 ビルド。並行呼び出しは
// ErrBuildInProgress を返す。
func (b *Bindings) BuildSandboxImage() error
```

単一ビルド invariant は `b.buildMu sync.Mutex` +
`b.buildInFlight bool` で実現。`defer` でフラグクリア。

```go
// SandboxImageStatus は Settings UI 用のスナップショット。
type SandboxImageStatus struct {
    Tag         string `json:"tag"`         // cfg.Sandbox.Image
    Ready       bool   `json:"ready"`       // engine にタグあり
    Building    bool   `json:"building"`    // ビルド進行中
    Recommended string `json:"recommended"` // imagebuild.CanonicalTag
}

func (b *Bindings) GetSandboxImageStatus() SandboxImageStatus
```

Settings ダイアログは open 時とビルドイベント受信後に
`GetSandboxImageStatus()` を読む。

### 3.6 Settings UI

既存の **Sandbox** サブセクション内 (`SettingsDialog.tsx`):

```
Sandbox (experimental)
[ ] Enable container sandbox  ← image が ready になるまで disabled
    Hint: 下の image がビルド済み AND このチェック ON の
    両方でツールが登録されます

Image: [shell-agent-v2-sandbox:0.1                    ▾]
       Status: ✓ Ready  /  ⚠ Not built — click Build below  /  ⏳ Building…
       [ Build recommended image ]   [ View build log ]

[ Engine, Network, CPU, Memory, Timeout — 既存と同じ ]
```

挙動:

- 「Build recommended image」は
  `Bindings.BuildSandboxImage()` を呼び、modal で
  `sandbox:build:line` イベントを scrollback にストリーム。
  `sandbox:build:done` で結果表示 + 閉じる
- Image 入力欄は編集可能。ユーザがカスタムタグを入力
  したら status を再チェック。engine になければ "Not
  built" 表示で Build は disabled (Build は正式タグだけ
  をターゲットにする — 任意ユーザタグはビルドしない)
- Status が "Not built" のとき "Enable" チェックは
  disabled。tooltip で理由を説明
- 補足: 「Build には数分かかります — apt-get + pip
  install。ローカル podman/docker でビルドします」

### 3.7 既定設定

`config.Default()` の `cfg.Sandbox.Image` を
`python:3.12-slim` から `imagebuild.CanonicalTag` (現状
`shell-agent-v2-sandbox:0.1`) に bump する。既存ユーザの
config は JSON ロードで Image フィールドが設定されたまま
保持される — 新規インストールのみ新既定を見る。

`python:3.12-slim` を使っているユーザは、それを pull
していれば readiness チェックを通り、sandbox ツールが
登録される。強制マイグレーションなし。Settings UI の
「Build」ボタンは正式タグだけを生成するので、内蔵フォント
を使いたいユーザは Image フィールドを正式タグに切り替える
必要がある。

### 3.8 バージョニングと再ビルドトリガ

Dockerfile か matplotlibrc を変更したときは
`imagebuild.BundleVersion` を bump する。image タグに
version が含まれるので、アプリ更新後は前のビルドが engine
に残ったまま、新しい正式タグは「Not built」になり、ユーザ
が Build を押すまでそのまま。ダイアログ表記: 「新しい
イメージが利用可能です — 再ビルドしますか？」

## 4. 影響ファイル

| ファイル | 変更 |
|---|---|
| `internal/sandbox/imagebuild/bundle.go` | 新規 — embed.FS + version + canonical tag |
| `internal/sandbox/imagebuild/bundle/Dockerfile` | 新規 |
| `internal/sandbox/imagebuild/bundle/matplotlibrc` | 新規 |
| `internal/sandbox/engine.go` | Engine インターフェースに `BuildImage`、`ImageReady` 追加 |
| `internal/sandbox/cli.go` | `BuildImage`、`ImageReady`、`buildMu` 実装 |
| `internal/agent/agent.go` | `maybeStartSandbox` を `eng.ImageReady` でゲート |
| `bindings.go` | `BuildSandboxImage`、`GetSandboxImageStatus`、ビルドフローロック |
| `internal/config/config.go` | 既定 `Sandbox.Image` → `imagebuild.CanonicalTag` |
| `frontend/src/types.ts` | `SandboxImageStatus` インターフェース |
| `frontend/src/dialogs/SettingsDialog.tsx` | Image status 行 + Build ボタン + ログ modal |
| `frontend/src/App.tsx` (or 新コンポーネント) | `sandbox:build:*` を listen するビルドログオーバーレイ |

## 5. テスト計画

### 単体
- `imagebuild.Bundle` を walk して全ファイルが空でないこと
- `cliEngine.ImageReady` が `podman image exists` の exit
  code に応じて true/false を返す (exec を mock する pipe
  経由 — 既存の sandbox テストがこのパターンを使っている)
- `cliEngine.BuildImage` が bundle を temp dir に書き出す
  こと (ファイル存在 assert) と `podman build` を正しい
  argv で起動すること (exec mock + args 検証)
- `Bindings.BuildSandboxImage` が並行呼び出しを
  `ErrBuildInProgress` で reject すること

### 統合 (PATH に podman/docker が必要)
- `TestIntegration_BuildAndUseImage`: 正式 image を build
  → そのイメージで `EnsureContainer` →
  `Exec("python", "import matplotlib; ...")`。matplotlib
  が日本語ラベルを fallback warning なしで処理することを
  確認 (warning 文字列を stderr 中で検索 — 不在で pass)
- 再実行で既存 image を再利用 (2 回目は速い)

### 手動
- image 無しで Build クリック: 進捗ストリーム、image が
  生成され status が Ready に変わり Enable がクリック可能に
- Build を素早く 2 回クリック: 2 回目が "build in
  progress"、1 回目は通常完了
- Image を存在しないタグに編集: status が Not Built に
  変わり Enable が灰、Build ボタンは正式タグだけ
  ビルドする (タグ単一を明示)
- 次リリースで `BundleVersion` bump 後の再ビルド: 新タグ
  が Not Built として表示され、旧タグは engine に残る

## 6. リスクと緩和策

| リスク | 緩和策 |
|---|---|
| 初回 build が 3-10 分。アプリが固まったとユーザが誤解 | stdout を見えるログオーバーレイにストリーム、進捗行 (apt step、pip step) を表示。Cancel ボタンで `ctx.Cancel()` |
| build 中にネットワーク切断 | build が stderr 付きで失敗、ユーザに見え再試行できる。Status は変わらない |
| 既存ユーザが custom `Sandbox.Image` を使っている場合、アップグレード時に予期せぬ既定変更 | 既定変更はフレッシュインストール限定 (config フィールドが初回 save で設定される)。既存 config は不変 |
| version bump によるタグ蓄積で disk が太る | `BundleVersion` bump 時に CHANGELOG で document。ユーザは `podman image rm shell-agent-v2-sandbox:<old>` で手動削除。自動削除はしない |
| macOS で podman machine 未起動状態で Build | engine が build process から明確なエラーを surface。既存 `Detect()` で利用可能性も報告。エラーパスに「最初に podman machine を起動してください」のヒント |
| Bundle FS は更新したが BundleVersion を bump し忘れ | 同タグで内容違いになる。緩和策: `BundleVersion` 定数の doc コメントに「bundle/* を変更したら必ず bump」と明記。CI で hash を計算して定数と一致するか assert する `//go:generate` を後で追加可能 |

## 7. フェーズ分割

順序付き 2 コミット:

1. **feat(sandbox): embed image build bundle + Engine.BuildImage / ImageReady.** 純粋な backend、UI 表面なし。テストは build-arg の形と並行ビルドロックを assert。`Sandbox.Image` 既定は `python:3.12-slim` のまま
2. **feat(ui): Settings sandbox image build flow + readiness gating.** `BuildSandboxImage` / `GetSandboxImageStatus` をダイアログに配線、build ログ modal 追加、`Sandbox.Image` 既定を正式タグに切り替え、`maybeStartSandbox` を `ImageReady` でゲートする

Phase 2 完了後 v0.1.19 リリース。

## 8. 範囲外

- 起動時の自動ビルド挙動
- ローカルビルド代わりの registry pull
- "remove image" ボタン (`podman image rm` で対処)
- Dockerfile パスの設定可能化 (内蔵がサポートレシピ。
  上級ユーザは repo 編集または `Sandbox.Image` を独自
  pre-built タグに切り替える)
- podman/docker が標準でカバーする以上の Linux /
  Windows 固有 build 調整

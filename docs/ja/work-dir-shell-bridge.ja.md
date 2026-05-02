# Shell Tool ↔ /work ブリッジ — 設計文書

> 日付: 2026-05-02
> ステータス: 次リリース向け提案
> スコープ: host 側シェルツールと sandbox コンテナで workspace
> ディレクトリを共有させ、片側で生成した artefact をもう片側や
> objstore に渡せるようにする。さらに新規組込みツール
> `register-object` を追加して、shell-only フロー (sandbox 無効)
> でも生成物を chat UI に出せるようにする。

## 1. 背景

セッション work ディレクトリ
`<DataDir>/sessions/<sessionID>/work/` は現在 sandbox 専用の概念。
コンテナが `/work` に bind-mount し、`sandbox-register-object`
ツールがそこから読み取って objstore に登録、chat UI が
`object:<ID>` として render できる。

host 側シェルツール (toolcall パッケージ経由で登録) には等価物
がない。例 `examples/generate-image.sh` は
`/tmp/shell-agent-images/` (グローバルパス) に書き出すが、書き
込みが成功しても画像は:

- Data panel の `/work` リストに現れない
- chat 内で `object:<ID>` 参照できない
- sandbox 側ツールから到達できない
- LLM には JSON ステータス文字列しか返らず、render 手段なし

v0.1.24 検証中にユーザーが指摘 — work ディレクトリは host と
コンテナで物理的に同一 (bind mount でコピーではない)。host で
シェルツールが書き込めばその瞬間に sandbox 側 `/work/<file>` と
なる — **データ移動不要**。欠けているピースは (a) シェルツールに
書き込み先を伝える手段、(b) sandbox 不要の objstore 登録経路。

## 2. Goals / Non-goals

### Goals

1. シェルツールは環境変数 `SHELL_AGENT_WORK_DIR` でセッション
   work ディレクトリの host パスを取得する
2. work ディレクトリはセッション読み込み時に作成 (sandbox
   コンテナ起動の副作用ではなく) — shell-only ユーザー (sandbox
   無効) も同じ規約を享受
3. 組込みツール `register-object` が `sandbox-register-object`
   と同等の効果 (work dir からファイルを読み objstore 登録、
   `object:<ID>` 返却) を sandbox 起動なしで提供
4. `generate-image.sh` を新フローに書き換え、「シェルツール →
   work dir → register-object → chat 表示画像」の正規例とする

### Non-goals

- **`sandbox-register-object` の deprecation はしない.** 無変更
  で動き続ける。両ツールは同じ物理ディレクトリを読むので等価
  だが、prefix が context を LLM に伝え、既存ユーザー設定を
  壊さない
- **シェルツール出力の自動 objstore 登録はしない.** stdout
  マーカープロトコルや filesystem watcher が必要で、より大きな
  設計。今は `generate-image` の後続で LLM が `register-object`
  を呼ぶ責務
- **本ラウンドで追加 env var は増やさない.** `SHELL_AGENT_WORK_DIR`
  のみ。`SHELL_AGENT_SESSION_ID` 等が後で必要になれば追加
- **セッション close 時の work dir 削除はしない** — 他の
  セッションデータと同様に永続

## 3. 詳細設計

### 3.1 Env var 注入 (`internal/toolcall`)

`Execute` がオプション `workDir` 引数を受け取る。non-empty なら
`cmd.Env = append(os.Environ(), "SHELL_AGENT_WORK_DIR="+workDir)`
を設定。empty なら挙動無変更 (env 修正なし、親環境継承)。

シグネチャ案を検討:

- (a) 第3位置引数追加 → 全テスト caller を破壊
- (b) `Options` struct に変更 → リファクタ大、caller 多数に影響
- (c) variadic options pattern → 小さく、追加的、拡張容易

**(c) を選択**:

```go
type ExecOption func(*execConfig)

func WithWorkDir(path string) ExecOption {
    return func(c *execConfig) { c.workDir = path }
}

func Execute(ctx context.Context, tool *Tool, argsJSON string, opts ...ExecOption) (string, error) {
    cfg := execConfig{}
    for _, o := range opts {
        o(&cfg)
    }
    ...
    if cfg.workDir != "" {
        cmd.Env = append(os.Environ(), "SHELL_AGENT_WORK_DIR="+cfg.workDir)
    }
    ...
}
```

既存 `Execute(ctx, tool, args)` 呼び出し点はコンパイル + 挙動
無変更。新 agent コードは `toolcall.WithWorkDir(workDir)` を渡す。

### 3.2 セッション読み込み時の work dir 作成

現状 work dir は `sandbox.cliEngine.EnsureContainer` 内で作成
され、sandbox フローに紐づく。shell-only ユーザー (sandbox 無効)
は到達しないため、`$SHELL_AGENT_WORK_DIR` が存在しない dir を
指す。

修正: agent がセッション読み込み時に sandbox 状態に関わらず dir
作成。

```go
// agent.LoadSession (またはそこから呼ばれる helper)
workDir := filepath.Join(memory.SessionDir(s.ID), "work")
if err := os.MkdirAll(workDir, 0700); err != nil {
    logger.Error("agent: workdir create: %v", err)
}
```

sandbox 側既存 `EnsureContainer` の MkdirAll は無変更 (idempotent
で害なし)。

### 3.3 新規組込みツール: `register-object`

`sandbox-register-object` をミラーするが、analysis-source
グループに所属 (セッション activeで常時公開、sandbox 依存なし)。

**ツール定義** (`analysisTools` 内):

```go
{
    Name: "register-object",
    Description: "セッション work ディレクトリ ($SHELL_AGENT_WORK_DIR — sandbox が /work として見るのと同じ物理パス) に既に存在するファイルを中央オブジェクトストアに登録し、chat が render 可能な object:<ID> 参照を返す。シェルツール (例: generate-image) で生成した artefact を chat に出すために使用 — シェルツールから $SHELL_AGENT_WORK_DIR に書き、同じ filename でこれを呼ぶ。sandbox-run-python / sandbox-run-shell で生成したものは sandbox-register-object を優先 — 物理的には同じ host ディレクトリなので両方とも同じ動作。",
    Parameters: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "path": map[string]any{
                "type": "string",
                "description": "work ディレクトリ内のパス。相対パスのみ; '..' traversal は拒否。例: 'sunset.png'",
            },
            "name": map[string]any{
                "type": "string",
                "description": "オブジェクトの人間可読名 (Data panel に表示)",
            },
            "type": map[string]any{
                "type": "string",
                "enum": []string{"image", "blob", "report"},
                "description": "オブジェクト型。省略時はファイル MIME から推論 (image/* → image, text/markdown → report, それ以外 → blob)",
            },
        },
        "required": []string{"path", "name"},
    },
}
```

**実装** (`internal/agent/tools.go`):

`toolSandboxRegisterObject` (`sandbox_tools.go`) をミラーするが
sandbox エンジンを bypass — host ファイルを直接
`memory.SessionDir(a.session.ID) + "/work" + path` から読む。

パス検証は sandbox 側既存ロジック (`safeWorkPath` / 等価) を再利用:
絶対パス拒否、`..` traversal 拒否、`os.Lstat` で symlink 拒否
(security-hardening-2 H14 パターン)。

**MITL デフォルト**:
`analysisToolMITLDefault["register-object"] = false`。
理由: ユーザーは既にチャットへのドラッグ&ドロップで objstore に
無条件にファイルを入れられる。`sandbox-register-object` も多くの
ユーザーが MITL を切って運用している。per-tool トグルで ON 化可能。

**`ListTools` での Category**: `"write"` (objstore 状態を変更)
として Settings UI で write-side ツールと group 表示。

### 3.4 generate-image.sh 書換

```sh
#!/bin/bash
# @tool: generate-image
# @description: Vertex AI Gemini でテキストプロンプトから画像生成。$SHELL_AGENT_WORK_DIR に書き、chat に画像を表示するには続けて register-object を呼ぶ。
# @param: prompt string "画像生成プロンプト"
# @param: filename string "出力ファイル名 (例: sunset.png)"
# @category: execute
# @timeout: 120
#
# REQUIRES: gem-image (https://github.com/nlink-jp/gem-image)
# REQUIRES: Vertex AI 資格情報 (gcloud auth application-default login)

INPUT=$(cat)
PROMPT=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('prompt',''))" 2>/dev/null)
FILENAME=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('filename','generated.png'))" 2>/dev/null)

if [ -z "$PROMPT" ]; then
  echo '{"error": "prompt is required"}'
  exit 1
fi

if [ -z "$SHELL_AGENT_WORK_DIR" ]; then
  echo '{"error": "SHELL_AGENT_WORK_DIR not set — このツールは shell-agent-v2 ≥ v0.1.25 が必要"}'
  exit 1
fi

export PATH="$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

OUTPUT_PATH="$SHELL_AGENT_WORK_DIR/$FILENAME"

if ! gem-image -p "$PROMPT" -o "$OUTPUT_PATH" --force 2>/dev/null; then
  echo "{\"error\": \"Image generation failed\"}"
  exit 1
fi

# 成功パス: status + filename のみ。next_step や絶対パスは含めない。
# 詳細は §6 リスク「命令形 next_step アンチパターン」「絶対パスを
# tool 出力に乗せない」参照。
python3 -c "import json; print(json.dumps({'status':'success','filename':'$FILENAME'}))"
```

follow-up 規約 (「次に register-object を path=<filename> で呼ぶ」)
は per-call 出力ではなく、ツールの `@description` (毎ラウンド LLM
に再注入される) に置く。理由は §6 参照。

### 3.5 ドキュメント

- `docs/en/object-storage.md` — sub-section「Shell tool artefacts」を
  追加し work-dir bridge を説明
- `README.md` — 「Shell script Tool Calling」配下に env var と
  `register-object` を記載
- `AGENTS.md` — gotcha エントリ

## 4. 触るファイル

| File | 変更内容 |
|---|---|
| `internal/toolcall/toolcall.go` | `ExecOption` / `WithWorkDir`; `Execute` が variadic options を受ける |
| `internal/toolcall/toolcall_test.go` | `WithWorkDir` 渡したら env var 設定、なければ未設定の test |
| `internal/agent/agent.go` | shell-tool dispatcher 分岐から `toolcall.WithWorkDir(...)` 渡し; セッション読み込み時に work dir 作成 |
| `internal/agent/tools.go` | 新規 `register-object` 定義 + `toolRegisterObject` 実装; `analysisToolMITLDefault` エントリ |
| `internal/agent/agent.go` | `register-object` を `ListTools()` (Settings UI) + dispatcher 分岐に追加 |
| `internal/bundled/tools/examples/generate-image.sh` | §3.4 通り書換 |
| `docs/en/work-dir-shell-bridge.md` / `docs/ja/work-dir-shell-bridge.ja.md` | この設計文書 |
| `docs/en/object-storage.md` (+ JA mirror) | 本文書へのポインタ sub-section |
| `CHANGELOG.md` | `[Unreleased]` Added エントリ |
| `AGENTS.md` | gotcha + ListTools 記載 |
| `README.md` / `README.ja.md` | Shell-tool セクション |

## 5. 後方互換性

| Surface | 変更前 | 変更後 | 互換 |
|---|---|---|---|
| 既存シェルツール (env var 未参照) | 無変更 | `SHELL_AGENT_WORK_DIR` 公開されるが無視 | ✅ |
| `toolcall.Execute(ctx, tool, args)` 呼出し点 | 3 引数 | 3 引数 + variadic options | ✅ 旧 caller と同一 |
| `sandbox-register-object` | 動作 | 動作 (無変更) | ✅ |
| 旧 `generate-image.sh` ユーザー | `/tmp/shell-agent-images/` 書込、chat 表示なし | 新挙動: work dir 書込、register hint | ⚠ 出力 JSON 変化 (status のみ → status + next_step)。安全と判断: (a) 元から正しく動いていない、(b) `examples/` (opt-in コピー、auto-install 対象外)。`tools/` に旧版を取り込んだユーザーはコピー保持、`examples/` から再 pull で更新 |
| `tools.go` `analysisTools` def | 11 tools | 12 tools (`+register-object`) | ✅ 追加のみ |
| `MITLOverrides` JSON keys | 無変更 | 無変更 | ✅ |

## 6. リスクと緩和策

| リスク | 緩和策 |
|---|---|
| LLM が `generate-image` 成功後 `register-object` 呼び忘れ | follow-up 規約は `generate-image` の `@description` に書き、毎ラウンド LLM に再注入される (per-call ヒントは下記アンチパターンの理由で使わない) |
| シェルツールが work dir 外のパス (`../../etc/passwd`) に書く | `register-object` 検証で絶対パス、`..` traversal、symlink leaf 拒否 (`sandbox-register-object` の `safeWorkPath` をミラー) |
| `register-object` と `sandbox-register-object` の使い分けで LLM 混乱 | 両 description で相互参照と等価性を明記。実運用では LLM はファイルを書いた側に対応する prefix を選ぶはず (シェルツール → register-object; sandbox ツール → sandbox-register-object)。物理的には同じ |
| 初回シェルツール実行時に work dir が未作成 | agent `LoadSession` で先に作成 |

### 6.1 アンチパターン: 命令形 `next_step` を tool 出力に乗せる

`generate-image.sh` の初版は出力 JSON に明示的な `next_step`
命令を含めていた:

```json
{"status":"success","filename":"sunset.png",
 "next_step":"Call register-object with path=\"sunset.png\" name=\"...\" to surface in chat"}
```

これは **LLM の planning horizon を直近のサブステップに圧縮し、
複数ステップのユーザープランを破壊しうる**。v0.1.25 検証中に観測:
ユーザーが 4 ステップのプラン (画像分析 → プロンプト検討 → 画像 3
枚生成 → レポート作成) を要求。エージェントは generate→register
のペアを 3 セット完璧に実行したが、各 `next_step` をそのラウンドの
主目的として扱い、3 つ目の register が完了して tool 出力に次の
`next_step` がなくなった時点でループ終了 — **レポート作成を行わ
なかった**。ユーザーは元目標を再指示する必要があった。

**ルール**: シェルツール出力は **状態** (何が起きた、artefact が
どこにあるか) を運ぶこと。**指示** (次に何をすべきか) は運ばない。
follow-up 規約はツールの `@description` に置く — 毎ラウンド LLM に
再注入され、override ではなく LLM のプランと対等に競合する。

### 6.2 アンチパターン: 絶対パスを tool 出力に乗せる

同初版では LLM が artefact を直接参照できるよう
`"work_dir_path": "/Users/magi/Library/.../work/sunset.png"`
(絶対 host パス) を出力に含めることも検討した。却下理由:

- 絶対パスを正規受付するエージェント側ツール (代表: `load-data`
  — まさにこの理由で MITL gate 必須にし、ユーザーが任意 host
  パスからの ingest を意識的に承認させる設計) が、curated な
  full host path リストを LLM context に持たせると魅力的なター
  ゲットとなる。プロンプトインジェクションされた LLM が load-data
  MITL prompt を立て、ユーザーが流し読みで承認した path が実は
  期待した artefact ではなく `~/.ssh/id_rsa` だった、という
  シナリオが成立する
- 絶対 host パス自体が漏えい材料 (LLM context → log →
  画面共有 → ...)

**ルール**: シェルツール出力は **相対 filename のみ** 露出する。
絶対パスを必要とするツール (`load-data`) は MITL gate を維持。

### 6.3 他の bundled ツールも更新

`write-note.sh` は元々 `/tmp/${FILENAME}` に書いていた — グローバル
filesystem 汚染、per-session スコープ無し、sandbox ツールや Data
panel から到達不能。`generate-image.sh` と同じく `$SHELL_AGENT_WORK_DIR`
に書くよう書換、出力契約も同じ (filename のみ、命令形なし)。MITL
gate は維持 — write ツールは write カテゴリのまま — だが blast radius
が host の `/tmp` への漏れではなくセッション work dir に scoped。

他の bundled ツール (`weather`, `get-location`, `list-files`,
`file-info`, `preview-file`) は stdout にデータを返すだけで artefact
書込は無いため、work dir 対応は不要。

## 7. Out of scope

- 自動登録のための stdout マーカープロトコル (例:
  `OBJSTORE-REGISTER: sunset.png image`)。大きな設計、延期
- `/work` の filesystem watcher。同上
- Data panel に `register-object` UI ボタン。Data panel は既に
  export / delete 可能、`/work` ファイルを object に昇格する UX
  は別 question (sub-section "Promote to object" 等)、延期
- `SHELL_AGENT_SESSION_ID` env var。必要時に追加
- セッション削除時の `/work` ファイル整理。既に
  `DeleteSessionDir` で対応済

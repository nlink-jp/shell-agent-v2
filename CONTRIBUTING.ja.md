# shell-agent-v2 への貢献

shell-agent-v2 は macOS ネイティブのローカルファースト chat &
agent ツールで、インタラクティブなデータ分析を提供します。
Wails v2 (Go + React) デスクトップアプリケーションとして配布。
本ドキュメントは、このツールをビルド・実行・修正・拡張したい
**新規開発者** 向けです。

英語版: [CONTRIBUTING.md](CONTRIBUTING.md) (full parity)。

## どこから読むか

- **ツールを使いたい** → [README.ja.md](README.ja.md) に
  インストール・機能・基本使用方法を記載。
- **内部構造を理解したい** →
  [docs/ja/reference/architecture.ja.md](docs/ja/reference/architecture.ja.md) で
  システム全体像を把握、そこからリンクされたサブシステム
  ドキュメントへ。
- **バグ修正・機能追加したい** → 本ファイルを最後まで読み、
  [§6 How to add X](#6-how-to-add-x) へ。

## 1. 前提

主要開発対象: macOS Apple Silicon。Intel macOS / Linux /
Windows へのクロスコンパイルは可能ですが、定常的にはテストして
いません。

- **Go** — `app/go.mod` 記載のツールチェーンバージョン
- **Node.js** — 18+ (React フロントエンドビルド用)
- **Wails CLI** — `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- **podman** (推奨) または **docker** — per-session sandbox
  ランタイム
- **python3** — 一部の統合テストでのみ必要 (不在時は clean に
  skip)

## 2. Build & Run

```sh
cd app
make build          # 本番ビルド → dist/shell-agent-v2.app
make dev            # ホットリロード dev モード (Wails)
```

**`go build` の直接実行は禁止。** Makefile は Wails を経由して
React フロントエンドをパッケージし、アセットを埋め込み、`.app`
バンドルにコード署名します。`go build` 単独実行は project root
に剥がれたバイナリを置き、起動時のコード署名を静かに破壊
します。

リリース成果物は `cp -r` ではなく `ditto` でコピーすること —
macOS は ad-hoc コード署名に拡張属性を使用し、`cp` はそれを
剥がします。

## 3. Test

```sh
cd app
make test                                                # 全パッケージ
go test -tags no_duckdb_arrow -count=1 ./internal/agent  # 1 パッケージ
```

DuckDB が依存グラフに入る場合 (大半のケース) は
`no_duckdb_arrow` ビルドタグ必須です。これなしでは Arrow
ライブラリが Wails と衝突します。Makefile が自動付与します。

外部前提が不足するテストは clean に skip:

- Sandbox テスト: `podman` または `docker` デーモン稼働必須
- MCP guardian テスト: `python3` が `PATH` 上に存在必須

テストはパッケージローカル、プロセス / ネットワーク境界では
スタブ・フェイクを使用します。

## 4. プロジェクト構造

```
shell-agent-v2/
├── app/
│   ├── bindings.go          # Wails バインディング (薄い委譲)
│   ├── main.go              # エントリポイント
│   ├── Makefile             # build / test / release レシピ
│   ├── internal/            # 全ドメインロジック
│   │   ├── agent/           # ステートマシン、実行ループ、ツール
│   │   │                    # ディスパッチ、MCP guardian、
│   │   │                    # ToolDescriptor レジストリ
│   │   ├── chat/            # チャットエンジン、メッセージ構築、
│   │   │                    # 時間コンテキスト、resolve-date
│   │   ├── llm/             # バックエンド抽象化 (local + Vertex AI)
│   │   ├── analysis/        # DuckDB エンジン (per-session,
│   │   │                    # lazy init)
│   │   ├── memory/          # 4-facility メモリモデル
│   │   ├── findings/        # 起源 provenance 付きグローバル
│   │   │                    # findings ストア
│   │   ├── toolcall/        # シェルスクリプトツールレジストリ
│   │   ├── mcp/             # mcp-guardian stdio JSON-RPC
│   │   │                    # クライアント
│   │   ├── objstore/        # 中央オブジェクトリポジトリ (image,
│   │   │                    # blob, markdown, report)
│   │   ├── sandbox/         # per-session コンテナサンドボックス
│   │   ├── contextbuild/    # LLM コンテキストビルダー
│   │   │                    # (warm/hot/summary)
│   │   ├── sessionio/       # セッション export / import
│   │   │                    # (.shellagent)
│   │   ├── bundled/         # バンドル済みシェルツールの
│   │   │                    # first-run scaffold
│   │   ├── pathfix/         # macOS app-launch PATH 正規化
│   │   ├── atomicio/        # アトミックファイル書き込みヘルパー
│   │   ├── frontendlint/    # フロントエンドビルド時 lint ヘルパー
│   │   ├── config/          # JSON 設定 (パス展開対応)
│   │   └── logger/          # 構造化ログ
│   ├── frontend/src/        # React UI
│   └── dist/                # ビルド出力 (.app, zip)
├── docs/                    # 英語 + 日本語リファレンス + 設計
│                            # ノート (en/ja 必須ミラー)
├── CHANGELOG.md             # 挙動変化、日付付き
├── README.md / README.ja.md # ユーザー向け機能 + インストール
├── CONTRIBUTING.md / .ja.md # 本ファイル
└── AGENTS.md                # LLM エージェント向け要約
```

概念モデル — ステートマシン、メモリアーキテクチャ、ツール
ディスパッチフロー — は
[docs/ja/reference/architecture.ja.md](docs/ja/reference/architecture.ja.md) から
開始してください。本ドキュメントが「どう組み合わさっているか」
の正準リファレンスで、サブシステム別の深掘りドキュメントへ
リンクします。

## 5. ドキュメントルール

リポジトリは 3 つの読者層でドキュメントを分けており、各層に
エントリポイントが 1 つあります:

| 読者 | エントリポイント | 補助ファイル |
|------|-----------------|-------------|
| ユーザー | `README.md` / `README.ja.md` | `CHANGELOG.md` |
| 新規開発者 | `CONTRIBUTING.md` / `CONTRIBUTING.ja.md` (本ファイル) | — |
| メンテナ | [`docs/en/INDEX.md`](docs/en/INDEX.md) / [`docs/ja/INDEX.ja.md`](docs/ja/INDEX.ja.md) | `docs/{en,ja}/reference/` `docs/{en,ja}/adr/` `docs/{en,ja}/history/` |

`docs/{en,ja}/` 内の構造:

- **`reference/`** — 現在の挙動、Evergreen。コード変更に合わせて
  in-place で更新。
- **`adr/`** — 連番付き設計判断記録、承認後 immutable。判断が
  変わる場合は新 ADR で supersede (誤字修正・リンクメンテは例外)。
- **`history/`** — v0.2.0 以前の audit trail、frozen。

**英語 / 日本語は必須ミラー。** 各 `docs/en/X.md` には同構造の
`docs/ja/X.ja.md` ペアが存在します。README と CONTRIBUTING も
同様の parity ルールが適用されます。

`CHANGELOG.md` は挙動変化ごとに、変更と同一 PR でエントリを
追加。

何をどこに書くか:

- **実質的な設計判断** (新サブシステム、破壊的変更、非自明な
  トレードオフ) → `docs/en/adr/` に新 ADR を作成、
  `docs/ja/adr/` にミラー。連番はリリース順、PR 起票時に次の
  番号を相談。
- **既存サブシステムの挙動変更** → 該当 `docs/{en,ja}/reference/`
  ドキュメントを in-place で更新。
- **設計ノート不要なケース** — fix や feature でも CHANGELOG
  エントリと commit メッセージで理由が十分説明できているなら
  ADR は不要。

## 6. How to add X

### 6.1 ツールを追加する

shell-agent-v2 は 5 つのツールソースを持ちます。v0.6.0
レジストリリファクタにより、そのうち 3 つ (`analysis`,
`builtin`, `sandbox`) は単一の `ToolDescriptor` 駆動
ディスパッチャに統合され、これらの追加は **1 ファイル編集** +
ハンドラ実装で済みます。

1. **ソースを選ぶ。**
   - `analysis` — per-session DuckDB エンジンに対して動作。
     例: `query-sql`, `analyze-data`, `grep-text`
   - `builtin` — DB / sandbox なしでエージェントレベルの状態に
     対して動作。例: `resolve-date`, `list-objects`,
     `get-object`
   - `sandbox` — per-session コンテナ内で実行。例:
     `sandbox-run-shell`, `sandbox-write-file`
   - `mcp` — 外部 `mcp-guardian` プロセスが公開。動的に発見、
     shell-agent-v2 側のコード変更不要
   - `shell-script` — データディレクトリ内のユーザー提供
     スクリプト、`internal/toolcall/` がパース

2. **`analysis` / `builtin` / `sandbox` の場合:**
   `app/internal/agent/tool_descriptors_*.go` の該当ビルダーに
   `ToolDescriptor` を追加。各 descriptor は Name, Description,
   Parameters (JSON Schema), Category, Source, MITLDefault,
   HideUntilDataLoaded, Handle を保持。ハンドラ関数を同所で実装。
   ディスパッチャへの配線は自動。

3. **`shell-script` の場合:** ユーザーデータディレクトリに
   ヘッダコメントブロック付きでスクリプトを配置。first-run
   scaffolder (`internal/bundled/`) にフォーマットとパース対象
   フィールドあり。

4. **`mcp` の場合:** Settings または `config.MCPProfileConfig`
   で guardian を設定。shell-agent-v2 のコード変更不要。

5. **テスト。** 外部依存のないツールは agent パッケージに
   ユニットテスト。DuckDB / sandbox / MCP に到達するツールは
   integration テスト — 同じソースの既存テストを雛形に。

6. **ドキュメント。** ユーザー観測可能 (チャット表面、Settings
   → Tools、LLM 挙動) なツールなら `README.md` + `README.ja.md`
   を更新し `CHANGELOG.md` にエントリを同一コミット / PR で追加。
   実質的な設計選択 (新ツールカテゴリ、新 MITL ゲート、新
   スキーマフィールド) は `docs/` の設計ノート対象。

v0.6.0 設計の経緯およびレジストリが enforce する構造不変条件は
[docs/ja/adr/0007-tool-registry-refactor.ja.md](docs/ja/adr/0007-tool-registry-refactor.ja.md)
に記載。

## 7. コミット・PR 規約

- **型付きコミット** — `feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`, `security:`。conventional-commits スタイル、
  カッコ内のスコープ付与推奨 (例: `feat(agent): ...`)。
- **1 コミット 1 論理変更。** 挙動変化はテストと CHANGELOG
  エントリを同居可。無関係なリファクタは別コミット。
- **what より why。** コミット本文は動機と非自明な
  トレードオフを説明する。diff が what を示す。
- **シークレット / PII をコミットしない。** GCP プロジェクト ID、
  サービスアカウントメール、個人情報はリポジトリに絶対に
  含めない。環境変数または `~/.shell-agent-v2/` のローカル
  設定を使用。
- **PR ルール。** issue へリンク、関連設計ノートへリンク、UI
  変更には手動 smoke テスト記録を含める。

組織全体ルールが上位レイヤとして適用される:
<https://github.com/nlink-jp/.github/blob/main/CONVENTIONS.md>。

## 8. リリース

リリースはメンテナが調整します。コントリビュータがリリース
コマンドを実行することはなく、PR が `main` にランディング
すれば十分です。バージョンごとのチェックリストは
`CHANGELOG.md` の過去エントリに残っています。

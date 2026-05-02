# Shell Tool 実行タイムアウト — 設計文書

> 日付: 2026-05-02
> ステータス: 次リリース向け提案
> スコープ: シェルツールスクリプトに `@timeout: N` ヘッダを追加し、
> 個別ツールがパッケージデフォルトの 30 秒キャップを上書き可能に
> する。後方互換の純粋追加変更。

## 1. 背景

`internal/toolcall/toolcall.go:17` がツール実行タイムアウトを
ハードコード:

```go
const DefaultTimeout = 30 * time.Second
```

`Execute()` は全シェルツール呼び出しを
`context.WithTimeout(ctx, DefaultTimeout)` でラップする。暴走 / ハング
したスクリプトがエージェントループを無限にブロックするのを防ぐため。
bundled tools (`weather`, `list-files`, `write-note`,
`get-location`, `file-info`, `preview-file`) には 30 秒で十分。

ユーザーから「正当に 30 秒を超えるシェルツールがある」との報告。
おそらく外部サービスをポーリングするか、重いローカルコマンドを実行する
スクリプト。キャップが完了前に `context deadline exceeded` を発火
させてしまう。

## 2. なぜ今か

ユーザーが実際にそのようなツールを書いている。issue が立つまで
キャップを隠しておく理由はない。

## 3. Goals / Non-goals

### Goals

1. スクリプトが既存の `@tool` / `@description` / `@param` /
   `@category` ヘッダと並んで、登録時に処理されるヘッダ指示で
   タイムアウトを宣言できるようにする
2. 30 秒の `DefaultTimeout` をパッケージ全体のフォールバックとして
   維持。既存スクリプト (bundled + ユーザーカスタマイズ) は無変更で
   動き続ける必要がある
3. parse 値を防御的に検証 — 数値以外や負値は黙って誤解釈せず拒否

### Non-goals

- **「全 tool 一律 N 秒」のグローバル config なし.** ユーザーが
  「config.json 直編集」を GUI アプリの UX として疑問視しており、
  Settings UI に追加するのは per-tool override が実ユースケースを
  カバーする中ではオーバーキル。後で複数の長時間 tool が累積した
  場合は Settings → Tools フィールドを追加すればよく、本変更の
  純粋追加性によりその経路は開かれている
- **override の下限 / 上限なし.** ユーザー = スクリプト作者。1 秒
  にも 1 時間にもしたければそれは作者の判断 (zero / 負値だけは
  明らかなフォーマットエラーとして拒否)
- **「無限タイムアウト」の sentinel なし.** 全スクリプトは何らかの
  bounded time で完了する必要がある (エージェントループは bounded
  call を前提)。長くしたい？ `@timeout: 7200` と書けばいい

## 4. 詳細設計

### 4.1 ヘッダ構文

```
#!/bin/bash
# @tool: heavy-poll
# @description: 遅い外部サービスをポーリング
# @param: url string "ポーリング先 endpoint"
# @category: read
# @timeout: 120
```

- キー: `@timeout:`
- 値: 正の整数、**秒**
- 値前後の空白は trim
- 正の整数として parse できないものは warn ログを出して `DefaultTimeout`
  にフォールバック。登録失敗にはしない (スクリプトはロード・利用可能、
  ヘッダだけ信頼しない)
- 小数秒 (`@timeout: 0.5`) や Go duration string (`@timeout: 90s`)
  は本ラウンドでは未対応 — JSON フィールドは `time.Duration`
  (nanoseconds) だが、ユーザー surface は明確さのため「整数秒」

### 4.2 Tool 構造体

```go
type Tool struct {
    Name        string        `json:"name"`
    Description string        `json:"description"`
    Params      []Param       `json:"params"`
    Category    Category      `json:"category"`
    ScriptPath  string        `json:"script_path"`

    // Timeout が > 0 のとき、このツール限定で package-level
    // DefaultTimeout を override する。zero (default) なら
    // DefaultTimeout を使用。`@timeout: N` スクリプトヘッダ (N は
    // 秒) で設定。
    Timeout     time.Duration `json:"timeout,omitempty"`
}
```

フィールドは `time.Duration` で、内部呼び出し点を typed に保つ
(`int * time.Second` 算術が散らばらない)。JSON は `omitempty` なので
override なしのスクリプトは registry serialisation が従来と同じ
見た目になる。

### 4.3 Execute()

```go
func Execute(ctx context.Context, tool *Tool, argsJSON string) (string, error) {
    timeout := tool.Timeout
    if timeout <= 0 {
        timeout = DefaultTimeout
    }
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    ...
}
```

1 行の条件分岐、override なしのスクリプトの挙動変更なし。

### 4.4 ヘッダ parser

`parseToolHeader` の既存 `@param` 分岐の後ろに:

```go
} else if strings.HasPrefix(line, "@timeout:") {
    raw := strings.TrimSpace(strings.TrimPrefix(line, "@timeout:"))
    if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
        tool.Timeout = time.Duration(secs) * time.Second
    } else {
        // 登録時に 1 度だけログ; スクリプトは DefaultTimeout で
        // ロードされる。
        logger.Error("toolcall: %s: ignoring invalid @timeout %q (must be a positive integer of seconds)", path, raw)
    }
}
```

`internal/logger` を使ってアプリのログ経路を 1 本化 (プロジェクト
方針: ログ destination を増やさない)。`Info` ではなく `Error` を
使う理由 — 不正ヘッダはユーザー側で修正可能な mis-config であり、
verbose ログ stream を漁らずに見える形で出すべき。

`internal/toolcall` から `internal/logger` への import を追加。
循環なし: `logger` は internal 依存ゼロ。parser のテストは hermetic
維持 — `logger.Error` は `logger.Init` 未呼出ならユニットテスト中
no-op。

### 4.5 テスト

- `TestParseToolHeader_TimeoutHonoured` — `@timeout: 90` ヘッダで
  `tool.Timeout == 90s` を期待
- `TestParseToolHeader_TimeoutMissing_DefaultsToZero` — ヘッダ行なし、
  `tool.Timeout == 0` (Execute は DefaultTimeout にフォールバック)
- `TestParseToolHeader_TimeoutInvalid_FallsBack` — `@timeout: abc`,
  `@timeout: 0`, `@timeout: -10` 全て `tool.Timeout == 0` のまま
- `TestExecute_HonoursToolTimeout` — `sleep 2` する一時スクリプトを
  書き、`tool.Timeout = 500ms` でタイムアウトエラーを期待
- `TestExecute_FallsBackToDefaultTimeout` — 同じスクリプトで
  `tool.Timeout = 0`; `DefaultTimeout` (30s) なら 2s sleep が正常完了

### 4.6 Bundled + example スクリプトのタイムアウト

同梱スクリプト全件に明示的な `@timeout` 宣言を入れ、将来のスクリプト
作者が例から option を発見できるようにする。各スクリプトの値は
実 worst-case ランタイムを反映:

| スクリプト | 種別 | `@timeout` | 根拠 |
|---|---|---|---|
| `bundled/tools/weather.sh` | bundled | `30` | JMA XML fetch; 30s で十分 |
| `bundled/tools/get-location.sh` | bundled | `30` | IP geolocation; 高速 |
| `bundled/tools/list-files.sh` | bundled | `30` | local FS walk; 高速 |
| `bundled/tools/file-info.sh` | bundled | `30` | local stat; 高速 |
| `bundled/tools/preview-file.sh` | bundled | `30` | local read + format; 高速 |
| `bundled/tools/write-note.sh` | bundled | `30` | local write; 高速 |
| `bundled/tools/examples/web-search.sh` | example | **`120`** | `gem-search` を呼び、Vertex AI Gemini grounded-search round-trip — 30s 超頻発 |
| `bundled/tools/examples/generate-image.sh` | example | **`120`** | `gem-image` を呼び、Vertex AI image-generation round-trip — 30s 超頻発 |

bundled 6 件の `30` 値は `DefaultTimeout` と同じで厳密には冗長だが、
明示的に書いておくことで option が一目で発見可能になり、「@timeout
が無いということは実は 30 秒です」という特例ルールを読み手の頭から
取り除ける。

example の 2 件は明示的に 120 秒へ引き上げ。実害のある footgun
を解消 — ユーザーが自分のデプロイにこれらを取り込むと、典型的な
agentic-search / image-gen 呼び出しで `context deadline exceeded`
を踏むため。

## 5. 触るファイル

| File | 変更内容 |
|---|---|
| `internal/toolcall/toolcall.go` | `Tool.Timeout` フィールド; `parseToolHeader` `@timeout` 分岐; `Execute` per-tool override |
| `internal/toolcall/toolcall_test.go` (新規) | parser + Execute tests |
| `internal/bundled/tools/weather.sh` + 5 つの bundled スクリプト | `@timeout: 30` を追加 (default と同値; 発見性のため明示) |
| `internal/bundled/tools/examples/web-search.sh` | `@timeout: 120` を追加 (gem-search が 30s 超頻発) |
| `internal/bundled/tools/examples/generate-image.sh` | `@timeout: 120` を追加 (gem-image も同様) |
| `CHANGELOG.md` | `[Unreleased]` Added エントリ |
| `AGENTS.md` | Gotcha: per-tool timeout override |
| `README.md` / `README.ja.md` | shell-tool セクション内 |

## 6. 後方互換性

| Surface | 変更前 | 変更後 | 互換 |
|---|---|---|---|
| `@timeout` なしのスクリプト | 30s キャップ | 30s キャップ | ✅ 同一 |
| `Execute()` の外部 caller | `(ctx, tool, args)` | シグネチャ無変更 | ✅ |
| `Tool{}` を読む外部コード | 5 フィールド | 6 フィールド (追加のみ) | ✅ |
| `parseToolHeader` 既知 directive | 4 (`@tool`, `@description`, `@param`, `@category`) | 5 (`+@timeout`) | ✅ 未知 directive は元から黙ってスキップ |
| `Tool{}` の JSON シリアライズ | `timeout` key なし | `> 0` のときのみ `timeout` key (`omitempty`) | ✅ |

on-disk format 変更なし、マイグレーション不要。

## 7. リスクと緩和策

| リスク | 緩和策 |
|---|---|
| ユーザーが `@timeout: 5min` (Go duration string) と typo し、スクリプトが黙って DefaultTimeout を使用 | 登録時に warn ログを出し、typo が `app.log` で見えるようにする |
| ユーザーが ms のつもりで `@timeout: 1` と書き、1 秒で kill される | README + bundled スクリプトコメントで「単位は秒」を明示 |
| 長時間スクリプトが LLM 呼び出しを待たせている間にエージェントループをブロック | DefaultTimeout 下でも既に発生 (25s 掛かる任意のツールがループを留める)。per-tool timeout はリードを長くするだけ、同じ仕組み |

## 8. Out of scope

- `tools.default_execution_timeout_seconds` グローバル config knob。
  必要になった時に容易に追加可
- グローバルデフォルトの Settings UI フィールド。同上、後回し
- 小数秒 / Go duration string。サブ秒精度を必要とする人が出るまで延期
- per-tool MITL prompt timeout。別の関心事

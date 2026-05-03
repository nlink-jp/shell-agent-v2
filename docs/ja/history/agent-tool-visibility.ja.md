# Agent ツール可視性 — 設計文書

> 日付: 2026-05-02
> ステータス: v0.1.21 向け提案
> スコープ: analysis tool セットの `hasData` ベース動的フィルタを
> 撤廃し、LLM が「load → query → analyse → report」の複数ステップ
> ワークフローを事前計画できるようにする。弱いローカルバックエンド
> 向けに opt-in の逃げ道を残す。

## 1. 背景

`agent.analysisTools(hasData bool)` はセッションにデータがロード
されているかで返すツールリストを変えている:

- **`hasData == false`**: `load-data`, `reset-analysis`,
  `create-report`, `list-objects`, `get-object` （5 個）
- **`hasData == true`**: 上記 + `describe-data`, `query-sql`,
  `list-tables`, `query-preview`, `suggest-analysis`,
  `quick-summary`, `promote-finding`, `analyze-data`（合計 13 個）

インラインコメント (`internal/agent/tools.go:14-16`):

> When no data is loaded, only load-data and reset-analysis are
> exposed to keep the tool count low for local LLMs.

この方針は初期 gemma2 / 初期 gemma3 時代の名残。当時はローカル
バックエンドの tool calling 信頼性が ~10 ツールを超えると急激に
劣化した。エージェントループは `buildToolDefs()` を毎ラウンド
呼ぶので、ラウンドごとの再評価で計画ギャップは隠れていた:
LLM が `load-data` を呼ぶ → 次ラウンドで新ツールが見える →
`query-sql` を呼ぶ。Tool chaining は再評価で動く。

しかし **計画可視性** は解決できていない:

1. LLM が初回応答でダウンストリームの手順を正確に列挙できない。
   「この CSV を読んで月別集計します」と返答しても、その時点で
   `query-sql` はツールリストになく、モデルは推測している
2. 誤った断り: ユーザーが「月別売上を出して」とデータ添付なしで
   尋ねると、`query-sql` も `analyze-data` も見えないので
   「実行できません」と謝ってしまう可能性。`load-data` が
   自然な最初の一歩として目の前にあるにもかかわらず
3. ツール名のハルシネーション: 弱いローカルモデルが存在しない
   `query-sql` を呼んで `unknown tool` エラーを食らい、ラウンドを
   ロスする
4. `load-data` の description はロード成功後に何が利用可能になるか
   を現状は明示していない。モデルは「analysis database」という
   字面から推測する必要がある

## 2. なぜ今か

標準ローカルモデルは **gemma-4-26b-a4b** (MoE、active 約 4B
parameters) になった。gpt-oss / gemma-4 / qwen3 世代のローカル
モデルは 30〜40 ツールを selection accuracy 劣化なしで安定処理
できる。30 ツール超は MCP guardian + sandbox + analysis tool が
1 セッションで揃った時点で既に常態化しており、v0.1.20 検証ログでも
gemma-4 が長いシーケンス（`load-data → describe-data → query-sql ×5
→ sandbox-export-sql → sandbox-run-python → create-report` 等）で
適切にツールを選び続けている。

トレードオフは反転した: 8 ツールを隠すことの selection 改善効果は
測れないレベルになった一方、計画コストは新セッションごとに発生し
続けている。

## 3. Goals / Non-goals

### Goals

1. データロード有無に関わらず、毎ラウンド全 analysis tool を LLM に
   公開
2. 弱いローカルバックエンド向けに opt-in の逃げ道を残す。旧フィルタ
   挙動を復活させる config flag、デフォルト OFF（新挙動が勝つ）
3. `load-data` description を更新し、初回応答で load-then-query
   ワークフローを推測なしで計画できるようにする
4. gemma-4-26b-a4b で (a)「この CSV を要約して」とデータなしで聞いた
   時、断る代わりに `load-data` を提案するか、(b) 30+ ツール時の
   selection accuracy に regression がないか、を確認

### Non-goals

- **新規 analysis tool の追加なし.** 同じ 11 tool、同じ shape
- **MITL gating の変更なし.** v0.1.20 Phase B で全 analysis tool が
  `IsToolMITLRequired` 経由になっている挙動は維持
- **Frontend 変更なし.** Settings → Tools リストは既に全 analysis
  tool を表示。変わるのは LLM 側の毎ラウンドの可視性
- **遡及適用なし.** 既存セッションは次のメッセージから新挙動。
  マイグレーション不要

## 4. 詳細設計

### 4.1 バックエンド変更（関数 1 つ）

`internal/agent/tools.go`:

```go
// analysisTools はデータ分析用ツール定義を返す。
//
// 全 11 ツールを毎ラウンド公開するので、LLM は load-then-analyse
// ワークフローを事前計画できる。空セッション時にデータ依存ツールが
// 呼ばれた場合は、各ツールが明確なエラーを返してモデルがリカバー
// できる。load-data でセッションを満たした後、他のツールが正常動作。
//
// 旧 hasData ベースフィルタは
// cfg.Tools.HideAnalysisToolsUntilDataLoaded で復活可能（弱い
// ローカルバックエンド向け）。デフォルト off。
func analysisTools(hasData, hideUntilDataLoaded bool) []llm.ToolDef {
    tools := []llm.ToolDef{
        loadDataDef,        // 常に公開
        resetAnalysisDef,
        createReportDef,
        listObjectsDef,
        getObjectDef,
    }
    if hideUntilDataLoaded && !hasData {
        return tools
    }
    return append(tools, dataDependentTools()...)
}
```

シグネチャに 2 つ目の bool が増える。`agent.go` の caller
（`buildToolDefs`, `ListTools`）が
`a.cfg.Tools.HideAnalysisToolsUntilDataLoaded` を読み取り渡す。

### 4.2 Config flag

`internal/config/config.go`:

```go
type ToolsConfig struct {
    ScriptDir                       string             `json:"script_dir"`
    MCPProfiles                     []MCPProfileConfig `json:"mcp_profiles"`
    DisabledTools                   []string           `json:"disabled_tools,omitempty"`
    MITLOverrides                   map[string]bool    `json:"mitl_overrides,omitempty"`
    // HideAnalysisToolsUntilDataLoaded は v0.1.21 以前の挙動
    // （データ依存 analysis tool が load-data 成功まで LLM ツール
    // リストに現れない）を復活させる。デフォルト false。
    // 30+ ツールが selection accuracy を測定的に害する弱いローカル
    // バックエンド向けの opt-in。
    // docs/en/agent-tool-visibility.md 参照。
    HideAnalysisToolsUntilDataLoaded bool              `json:"hide_analysis_tools_until_data_loaded,omitempty"`
}
```

UI 露出（Settings dialog）なし。パワーユーザー向けノブで、README +
CHANGELOG に明記。リリース後に需要が出たら Settings → General に
トグル追加。

### 4.3 `load-data` description 更新

現状:

> Load a data file (CSV, JSON, JSONL) from the HOST filesystem
> into the analysis database. Creates or replaces the table.
> Only use this for absolute host paths the user supplied, or
> files explicitly attached to the conversation. For files
> inside the sandbox /work directory ... call
> sandbox-load-into-analysis instead — load-data cannot reach
> into the container.

末尾に追加:

> Once loaded, the table is queryable via `query-sql`,
> `describe-data`, `list-tables`, `query-preview`,
> `suggest-analysis`, `quick-summary`, and `analyze-data`; use
> `promote-finding` to save insights, `create-report` to
> assemble a report.

system prompt 内のツール doc reference (`internal/chat/chat.go`)
は `sandboxGuidance` のみ条件付きで存在しており、analysis tool に
インラインブロックはない。各ツールの description が LLM 側で唯一
見える docs。

### 4.4 テスト

既存テストの更新:

- `TestAnalysisToolsFiltering` と
  `TestAnalysisToolsFilteringWithNewTools` は現状「hasData=false で
  5 個、true で 13 個」を assert。以下にリファクタ:
  - デフォルト config (`hide=false`): hasData に関わらず 13 個
  - レガシー config (`hide=true`): 5 vs 13 split
- 新規テスト
  `TestAnalysisTools_FullSetByDefault_AllowsPlanning` で、データ
  ロードなしでも `query-sql` 等が含まれることを確認
- 新規テスト
  `TestAnalysisTools_HideFlagRestoresLegacyBehaviour` で flag を
  立てた時に旧 split に戻ることを確認

`TestListTools_MITLDefaultMatchesGate` (v0.1.20 契約テスト) は
`ListTools()` の結果を iterate するだけなのでツール数に関わらず
動作継続。

## 5. 触るファイル

| File | 変更内容 |
|---|---|
| `internal/agent/tools.go` | `analysisTools` シグネチャ + 本体；inline def を named slice にリファクタ；`load-data` description 更新 |
| `internal/agent/tools_test.go` | 既存フィルタテストのリファクタ + 新規 2 件 |
| `internal/agent/agent.go` | `cfg.Tools.HideAnalysisToolsUntilDataLoaded` を `buildToolDefs` と `ListTools` に伝搬 |
| `internal/config/config.go` | `ToolsConfig` に新フィールド |
| `internal/config/config_test.go` | デフォルト値テスト |
| `bindings.go` | （変更なし — binding 層では tool list flow 不変） |
| `frontend/src/dialogs/SettingsDialog.tsx` | （変更なし — flag は config-only） |
| `docs/en/agent-tool-visibility.md` | このファイル |
| `docs/ja/agent-tool-visibility.ja.md` | 翻訳 |
| `CHANGELOG.md` | v0.1.21 エントリ |
| `AGENTS.md` | gotcha 更新 |
| `README.md` / `README.ja.md` | 新挙動 + 逃げ道を記載 |
| `TODO.md` | 繰越エントリを削除（実装したので） |

## 6. テスト計画

### Unit

- `go test ./internal/agent/ -tags no_duckdb_arrow -race` —
  リファクタ済フィルタテスト + 新規 2 件
- `go test ./internal/config/ -race` — デフォルト値テスト

### Manual smoke (gemma-4-26b-a4b)

1. **新規セッション、データなし、分析的な質問**。「今月の売上トップ
   商品を教えて」とデータ添付なしで送信。モデルは断る代わりに
   `load-data` を提案（ファイルパスを聞く）するはず
2. **新規セッション、同じメッセージで CSV 添付**。「`/Users/.../
   sales.csv` を読んでトップ商品を教えて」。モデルは load-data →
   describe-data → query-sql を 1 回の計画的応答で chain させる
   はず（複数 round trip にならず）
3. **30+ ツール時の selection accuracy**. sandbox 有効化 + MCP
   guardian 1 つ起動でツール数を数える: 30 個以上のはず。load +
   query + report のシーケンスを実行し、`unknown tool` エラーや
   明らかに違うツール選択がないことを確認
4. **レガシーモード**. `config.json` で
   `tools.hide_analysis_tools_until_data_loaded: true` 設定 → 再起動
   → (1) を再実行。モデルは断るか `load-data` のみ知っている状態に
   戻るはず

## 7. リスクと緩和策

| リスク | 緩和策 |
|---|---|
| 一部の弱いローカルモデルが広いツールリストで劣化 | README + CHANGELOG で逃げ道を目立たせ、regression を見たら 1 行 config 編集を促す |
| 既存ユーザーの `MITLOverrides` エントリが、ツールが常時可視になることで適用される機会が増える | 挙動変更なし — `IsToolMITLRequired` は既に override を参照、解決ロジックは同一 |
| `load-data` description が長くなり system prompt を膨らませる | 増分は ~50 tokens、4K〜8K 文脈 budget に対して無視可能 |
| モデルが空セッションで `query-sql` 等を呼ぶ | 各ツールが既に明示的なエラー（"no tables loaded" / "table not found"）を返す。モデルはそのエラーを見てリカバー、現行の任意ツールエラー時のフローと同じ |

## 8. Out of scope

- Settings → General に flag のトグルを追加。リリース後に需要が
  出たら追加
- Per-tool 可視性フラグ（例: "load 後でも analyze-data を隠す"）。
  既存の `DisabledTools` config がカバー
- `analysisTools` を registration パターンに書き直す。将来の整理
  として；今回の変更には現行の inline-def スタイルで十分

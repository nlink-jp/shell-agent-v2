# Tool registry リファクタリング (v0.6.0)

## 1. 動機

v0.5.0 → v0.5.1 で 6 つのバグ修正が立て続けに出た。うち 5 つ
が同じ根本原因だった: ツール名を並列リストに追加する必要が
あり、開発者が 5 (or 6) 箇所のうち**ちょうど 1 つ**を忘れる。
症状は様々 (「unknown tool」エラー、Settings → Tools への
非表示、バブルプレビュー破損) だが、原因はいつも「N 箇所追加
すべきところを N-1 箇所しか追加していない」だった。

本ドキュメントは、並列リストパターンを構造的に不可能にする
リファクタリングを規定する。各ツールは `ToolDescriptor` 値
として 1 度だけ定義され、ツール名を列挙していた既存の各面
(LLM ツール定義ビルダー、内側ディスパッチャ、外側ディスパッチャ
の case-label、MITL デフォルトマップ、MITL カテゴリスイッチ、
Settings UI カタログ、ビルダーのビルダー) はすべて *view
関数* となり、正典的な descriptor スライスから出力を導出する。

LLM やエンドユーザに見える挙動変化は無し。同じツールが同じ
順序で同じ説明とともに表示される; 同じ MITL デフォルトが
同じディスパッチで発火する; 同じ Settings トグルが同じよう
に機能する。これは純粋にコードの衛生改善であり、構造的安全
保証付き: 今後のツール追加は 1 つの descriptor リストを編集
するだけで済み、コンパイラが残りを検証する。

## 2. 現状

`docs/en/markdown-attachments.md` で 3 つの新ツール
(`analyze-text`, `grep-text`, `get-text`) を投入した。正しく
やるには **7 箇所** の編集が必要で、初回の試みでは開発者が
2 箇所を忘れた:

| # | 場所 | 何を持つか | drift リスク |
|---|------|-----------|--------------|
| 1 | `internal/agent/tools.go:30` `analysisToolMITLDefault` | per-tool MITL デフォルトの `map[string]bool` | **致命的** — 漏れたツールはセキュリティデフォルトが silently 反転 |
| 2 | `internal/agent/tools.go:53` `analysisToolMITLCategory()` switch | UI 側カテゴリ override (`sql_preview`, `analysis_plan`) | 低 — 漏れたら "write" に fall-through |
| 3 | `internal/agent/tools.go:81` `analysisTools()` ビルダー | 完全な説明 + JSON Schema パラメータ付き `llm.ToolDef[]` | **致命的** — 漏れたら LLM から完全に不可視 |
| 4 | `internal/agent/tools.go:310` `executeAnalysisTool()` switch | 内側ディスパッチャ、`toolXxx()` Go ハンドラを呼ぶ | **致命的** — 漏れたら "unknown analysis tool: %s" |
| 5 | `internal/agent/agent.go:1743` `executeTool()` 外側 case-label | 外側ディスパッチャ; `executeAnalysisTool()` 等のブランチへ routing | **致命的** — 漏れたら "unknown tool %q" (v0.5.1 のバグ) |
| 6 | `internal/agent/agent.go:980` `ListTools()` analysis セクション | Settings → Tools UI 用の手書き `ToolInfoItem` | **致命的** — 漏れたら Settings から不可視 (v0.5.1 のバグ) |
| 7 | `internal/findings/findings.go:43` `Source*` 定数 | 自動 promote findings の出所 enum | 低 — finding を発する ツールでのみ問題に |

加えて厳密には並列リストではないが同じように bit-rot する
**二次的な** drift 面が複数:

- **description の重複**: `analysisTools()` (#3) と
  `ListTools()` (#6) それぞれにツールごとの完全な散文説明
  がある。片方で編集するともう片方が silently 乖離する。
- **テストカウントアサーション**: `tools_test.go` がツール
  数を厳密に (legacy no-data で 9、legacy with-data で 17)
  アサート。新ツールごとに 3 箇所同コミットで更新が必要。

### 2.1. Sandbox ツール — 半分はすでにリファクタ済み

`internal/agent/sandbox_tools.go` は部分的な改善を示す:

- `sandboxToolDefs()` が canonical リスト (ツールごと
  `llm.ToolDef` 1 つ)。
- `ListTools()` は `sandboxToolDefs()` を **iterate** し
  名前を再列挙しない — drift フリー。
- ただし `executeSandboxTool()` は名前で独自に switch して
  おり、名前のタイポや新ツール追加で「unknown sandbox tool」
  失敗のリスクは残る。

なので sandbox 面は 6 個ではなく 2 個の並列リスト (defs +
switch) — 改善されてはいるが、まだ単一ソースではない。

### 2.2. Shell ツールと MCP — 既に正しい

`internal/toolcall/Registry` と `internal/mcp/Guardian` は
両方とも理想的パターンに従う: ツールは *1 箇所* で定義
(スクリプトヘッダーから parse、または MCP サーバから返却)
され、各面 — `executeTool()` ディスパッチ、`ListTools()` UI
— が *名前で look up* する。新ツールが自動的に出現する。

これが analysis と sandbox の目指す model。

### 2.3. 素朴な統合を阻む特殊ケースツール

4 つのツールが `analysisTools()` と `ListTools()` の
analysis セクションにリストされながら、`executeAnalysisTool()`
を経由せず `executeTool()` で直接ディスパッチされる:

- `resolve-date` (builtin、`chat.ResolveDate()` を呼ぶ)
- `list-objects` (objstore read)
- `get-object` (objstore read)
- `register-object` (objstore write + inline MITL チェック)

これらは analysis engine (`a.analysis`) に依存しないので、
engine の存在を要求する「executeAnalysisTool に委譲」パターン
に合わない。リファクタはこの routing nuance を保たねば
ならない — descriptor は「どの source カテゴリ」(UI grouping
用) と「どのハンドラ」(ディスパッチ用) の両方を表現でき、
両者が緩く結合できる必要がある。

## 3. 設計原則

- **ツールごとに single source of truth。** 各ツールはちょうど
  1 度だけ定義される。既存の各面はすべて projection になる。
- **挙動保存。** LLM 視点の変化なし、Settings 視点の変化
  なし、オンディスク形式の変化なし、テストフィクスチャ変化
  なし (カウントアサーションが動的になる以外)。同じツール
  が同じ順序で同じ MITL デフォルトで現れる。
- **加法的移行。** Phase 境界は各中間コミットでコンパイル
  通り、全テスト pass、アプリが同一に動作するように設定。
  flag day は不要。
- **既存の良パターンとの compose。** Shell ツールと MCP
  guardians は *リファクタしない* — すでに単一ソースレジ
  ストリを持つ。新 `ToolDescriptor` は外側ディスパッチャが
  参照するツールソースの **一つ**となり、既存の動的レジ
  ストリと並立する。
- **可能ならコンパイル時安全性、必要なときランタイム。**
  ハンドラクロージャは型付き; ツール名は依然として string
  key (タイポはコンパイラ検知できない) だが、タイポの結果が
  「N 個のリストにわたって half-registered」ではなく
  「descriptor map に存在しない」になる。

## 4. アーキテクチャ

### 4.1. ToolDescriptor

```go
// ToolDescriptor はツールの single source of truth —
// 同じ値が LLM ツール定義、Settings UI エントリ、MITL
// デフォルト、ディスパッチハンドラのすべてを backing する。
type ToolDescriptor struct {
    // --- Identity ---
    Name string

    // --- LLM-facing ---
    Description string
    Parameters  any // JSON Schema (`map[string]any` 等)

    // --- UI / 分類 ---
    // Category は "read" | "write" | "execute" — 汎用 MITL
    // 確認ダイアログを駆動。特殊 MITL カテゴリ (sql_preview、
    // analysis_plan) は MITLCategoryOverride 経由; Category
    // は fallback / デフォルトに残る。
    Category string

    // Source は "analysis" | "builtin" | "sandbox" | "shell"
    // | "mcp"。ToolInfoItem に surface して Settings UI が
    // 起源でエントリをグループ化できるように。ディスパッチャ
    // は使わない — それは Handle を直接使う。
    Source string

    // --- MITL ---
    MITLDefault bool

    // MITLCategoryOverride は UI が専用確認 (現在:
    // query-sql の "sql_preview"、analyze-data の
    // "analysis_plan") を描画すべきとき非空。空なら
    // Category に fall back。
    MITLCategoryOverride string

    // --- 可視性 ---
    // HideUntilDataLoaded は、analysis engine がテーブルを
    // 少なくとも 1 つロードするまで legacy モードが隠す
    // ツールに対して true。既存の config flag
    // `cfg.Tools.HideAnalysisToolsUntilDataLoaded` と命名
    // を一致させ、ポリシーと consumer が直線的に並ぶよう
    // にした。ほとんどのツールは false (常時可視)。
    HideUntilDataLoaded bool

    // --- Dispatch ---
    // Handle はツールの executor。クロージャが descriptor
    // 構築時に *Agent を capture するので、descriptor list
    // は *Agent のメソッドにできる。
    Handle func(ctx context.Context, args string) (string, ActivityEventStatus)
}
```

### 4.2. Agent 単位の descriptor リスト

完全な descriptor リストは agent 上に居る — ハンドラ
クロージャが objstore / analysis engine / session アクセス
のために `a` を capture できるように:

```go
func (a *Agent) buildToolDescriptors() []ToolDescriptor {
    return concat(
        a.builtinDescriptors(),
        a.analysisDescriptors(),
        a.sandboxDescriptors(), // a.sandbox == nil なら nil
    )
}
```

各 per-source builder がそのツールを返す; agent の
コンストラクタ (`New()`) が `buildToolDescriptors()` を 1 度
呼んで結果を `a.toolDescriptors` と O(1) lookup 用の
`map[string]ToolDescriptor` インデックスにキャッシュ。

### 4.3. View 関数は projection になる

§2 の 6 面が導出関数に折り畳まれる:

```go
// analysisTools() を置き換え — descriptor リストから
// 可視性でフィルタして LLM ツール定義を導出。
// legacyMode は cfg.Tools.HideAnalysisToolsUntilDataLoaded;
// 各 descriptor の HideUntilDataLoaded フィールドと shadow
// しないようローカル名を変えてある。
func (a *Agent) toolDefsForLLM(hasData, legacyMode bool) []llm.ToolDef {
    out := make([]llm.ToolDef, 0, len(a.toolDescriptors))
    for _, d := range a.toolDescriptors {
        if d.HideUntilDataLoaded && legacyMode && !hasData {
            continue
        }
        out = append(out, llm.ToolDef{
            Name: d.Name, Description: d.Description, Parameters: d.Parameters,
        })
    }
    return out
}

// analysisToolMITLDefault と sandbox-prefix lookup を
// 両方置き換え — どちらも descriptor 経由になる。
func (a *Agent) toolMITLDefaultFromRegistry(name string) (bool, bool) {
    d, ok := a.toolDescriptorByName(name)
    if !ok {
        return false, false
    }
    return d.MITLDefault, true
}

// analysisToolMITLCategory() switch を置き換え。
func (a *Agent) toolMITLCategory(name string) string {
    d, _ := a.toolDescriptorByName(name)
    if d.MITLCategoryOverride != "" {
        return d.MITLCategoryOverride
    }
    return d.Category
}

// executeTool() 外側 case-label + 内側 executeAnalysisTool()
// switch を置き換え。
func (a *Agent) dispatchDescriptor(ctx context.Context, tc llm.ToolCall) (string, ActivityEventStatus, bool) {
    d, ok := a.toolDescriptorByName(tc.Name)
    if !ok {
        return "", ActivityStatusError, false // descriptor ツールでない、caller が他ソースを試す
    }
    if a.IsToolMITLRequired(tc.Name) {
        if rejection := a.requestMITL(tc.Name, tc.Arguments, a.toolMITLCategory(tc.Name)); rejection != "" {
            return rejection, ActivityStatusError, true
        }
    }
    result, status := d.Handle(ctx, tc.Arguments)
    return result, status, true
}

// ListTools() の analysis + sandbox セクションを置き換え —
// descriptors を iterate。shell ツールと MCP ツールは
// 既存の動的レジストリ経由でリストされ続ける。
func (a *Agent) ListTools() []ToolInfoItem {
    items := make([]ToolInfoItem, 0, len(a.toolDescriptors))
    hasData := a.analysis != nil && a.analysis.HasData()
    legacyMode := a.cfg.Tools.HideAnalysisToolsUntilDataLoaded
    for _, d := range a.toolDescriptors {
        if d.HideUntilDataLoaded && legacyMode && !hasData {
            continue
        }
        items = append(items, ToolInfoItem{
            Name:        d.Name,
            Description: d.Description,
            Category:    d.Category,
            Source:      d.Source,
            MITLDefault: d.MITLDefault,
        })
    }
    // 次に shell + MCP + sandbox-未移行のソースを今日と同様に iterate。
    return items
}
```

### 4.4. 外側ディスパッチャ

`executeTool()` は各ツールソースを順に試す小さな router に
なる:

```go
func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) (string, ActivityEventStatus) {
    tc.Arguments = normalizeToolArgs(tc.Arguments)

    // 1. Descriptor レジストリ (analysis + builtin + sandbox)。
    if result, status, handled := a.dispatchDescriptor(ctx, tc); handled {
        return result, status
    }
    // 2. MCP guardians (既存の動的レジストリ)。
    if strings.HasPrefix(tc.Name, "mcp__") {
        return a.dispatchMCP(ctx, tc)
    }
    // 3. Shell ツールレジストリ (既存)。
    if tool, ok := a.toolRegistry.Get(tc.Name); ok {
        return a.dispatchShell(ctx, tc, tool)
    }
    return fmt.Sprintf("Error: unknown tool %q", tc.Name), ActivityStatusError
}
```

明示的な 3 ブランチ、それぞれ key 検索。ツール名を列挙する
巨大 case label は無し。

### 4.5. リファクタ後の各ツールの居場所

| ツール | Source | Handler |
|------|--------|---------|
| resolve-date | builtin | `chat.ResolveDate` wrap |
| list-objects | builtin | `a.toolListObjects` |
| get-object | builtin | `a.toolGetObject` |
| register-object | builtin | `a.toolRegisterObject` (+ descriptor 経由の inline MITL) |
| load-data | analysis | `a.toolLoadData` |
| describe-data | analysis | ... |
| query-sql | analysis | ... |
| query-preview | analysis | ... |
| suggest-analysis | analysis | ... |
| quick-summary | analysis | ... |
| list-tables | analysis | ... |
| reset-analysis | analysis | ... |
| create-report | analysis | ... |
| promote-finding | analysis | ... |
| analyze-data | analysis | ... |
| analyze-text | analysis | `a.toolAnalyzeText` |
| grep-text | analysis | `a.toolGrepText` |
| get-text | analysis | `a.toolGetText` |
| sandbox-* (8 ツール) | sandbox | 個別の `a.toolSandbox*` |

Shell ツール descriptor は `toolcall.Tool` 値を wrap して
動的に構築 (既存レジストリ、レジストリ自体の挙動変化なし)。
MCP ツール descriptor は materialize **しない** — MCP
ツールは実行時に発見され build 時に descriptor に置く静的
メタデータが無いため、ディスパッチャの prefix ブランチに
残す。

## 5. 移行 phase

各 phase は 1-2 コミット、ビルド通過、テスト pass。どの
phase も LLM-visible な変化を導入しない。

### Phase 1: `ToolDescriptor` 型 + helper map 導入 (まだ consumer 無し)

- 新ファイル `internal/agent/tool_descriptor.go` に struct
  定義と `toolDescriptorByName(name) (ToolDescriptor, bool)`
  ヘルパー。
- Agent struct に `toolDescriptors []ToolDescriptor` と
  `toolDescriptorIndex map[string]int` 追加。
- `New()` で今は空スライスに初期化。
- 挙動変化なし。コンパイル通り、全テスト pass。

### Phase 2: analysis + builtin ツールの移行

10 個の小コミットに分割。各ステップ独立コンパイル + テスト
pass、bisect が view 関数移行の特定ステップに着地できる。
各サブコミット < 100 行純変化。

- **2a** 新ファイル `internal/agent/tool_descriptors_analysis.go`
  に `a.analysisDescriptors()` を定義、14 個の analysis ツール
  を `ToolDescriptor` 値で返す。consumer 無し、データのみ。
- **2b** 新ファイル `internal/agent/tool_descriptors_builtin.go`
  に `a.builtinDescriptors()` を定義、4 つの intercepted
  ツール (resolve-date、list-objects、get-object、
  register-object) を返す。consumer 無し。
- **2c** `New()` で 2 つの builder から `a.toolDescriptors`
  を populate、O(1) lookup 用 `toolDescriptorIndex` 追加、
  ヘルパー `toolDescriptorByName()` を expose。新データの
  consumer はまだ無し — 旧経路は無変更で動く。
- **2d** `analysisTools()` を descriptor list から `[]llm.ToolDef`
  を derive する形に migrate。前後で出力 bit-identical を
  コミット時に手動 diff で確認。
- **2e** `executeAnalysisTool()` を switch ではなく
  `descriptor.Handle` 経由のディスパッチに migrate。switch
  は同コミットで削除。
- **2f** 外側 `executeTool()` の analysis case-label を
  `dispatchDescriptor()` 呼出に migrate。case label 削除。
- **2g** `analysisToolMITLDefault` map lookup を descriptor
  からの derive view に migrate。map 削除。
- **2h** `analysisToolMITLCategory()` switch を
  `descriptor.MITLCategoryOverride` から derive する形に
  migrate。switch 削除。
- **2i** `ListTools()` の analysis + builtin セクションを
  descriptor list の iteration に migrate。手書き削除。
- **2j** `tools_test.go` のカウントアサーションを hard-coded
  9 / 17 ではなく `len(a.toolDescriptors)` から derive する
  形に更新。最終 cleanup コミット; 全経路が descriptor から
  derive されている状態。

### Phase 3: Sandbox ツールの移行

Phase 2 と同じ小コミット形式で 6 個に分割:

- **3a** 新ファイル `internal/agent/tool_descriptors_sandbox.go`
  が `a.sandboxDescriptors()` を定義、8 つの sandbox ツールを
  `ToolDescriptor` 値で返す。
- **3b** `New()` で `a.sandbox != nil` のとき条件付きで
  `a.toolDescriptors` に append。
- **3c** `executeSandboxTool()` を switch ではなく
  descriptor `Handle` 経由のディスパッチに rewrite (or
  完全削除して dispatchDescriptor に sandbox 名も任せる)。
- **3d** `sandboxToolDefs()` を descriptor list から derive
  する形に migrate。
- **3e** `ListTools()` sandbox セクションを統合 descriptor
  iteration の一部に migrate。sandbox 別 loop 削除。
- **3f** 外側ディスパッチャの `sandbox-*` prefix ブランチを
  削除 (descriptor index で sandbox 名も処理されるため
  redundant)。

### Phase 4: スキップ

元プランでは shell ツールを descriptor として wrap し
`ListTools()` を単一 loop にする案だったが、レビュー結果
ドロップ: shell ツールは既に理想パターン
(`internal/toolcall/Registry` が single source of truth、
`executeTool()` は単一行 lookup、`ListTools()` はレジストリを
iterate)。descriptor wrapper を追加してもクリーンな iteration
を別のクリーンな iteration + wrapper コードに変えるだけで、
drift 防止の payoff は無い。

MCP ツールにも同じ理屈 — 実行時に guardian プロセスから
発見されるため静的 descriptor 不可。

### Phase 5: テスト + docs

- `tools_test.go` のカウントアサーションを literal `9` /
  `17` ではなく descriptor スライス参照に。
- 新規 `tool_descriptor_test.go`:
  - `TestToolDescriptors_UniqueNames` — 重複なし。
  - `TestToolDescriptors_AllHaveHandlers` — 各エントリの
    `Handle` が非 nil。
  - `TestToolDescriptors_MITLDefaultsMatchHistoricalMap` —
    新 descriptor の MITL デフォルトがリファクタ前
    `analysisToolMITLDefault` map と全名前で一致することを
    アサート。移行安全網。
  - `TestToolDescriptors_DescriptionsMatchLLMOutput` —
    `toolDefsForLLM()` 出力がリファクタ前 `analysisTools()`
    出力と bit-identical (Description テキストが単一
    ソース由来になる点を除く、それが本旨)。

- `docs/en/architecture.md` §3 (packages) と "Recent
  design notes" リストを更新。AGENTS.md ポインタに新
  ファイル追加。

### Phase 6: リリース

`v0.6.0` chore コミット、CHANGELOG エントリ、tag、push、gh
release with asset、submodule bump、check-org.sh。これまで
の全リリースと同じ 9 ステップパターン。

## 6. テスト

### 6.1. pass し続けねばならない既存テスト

- `TestAnalysisToolsFiltering` — カウントアサーションを
  descriptor スライス長 derive に。
- `TestAnalysisTools_HideFlagRestoresLegacyBehaviour` —
  同。
- `TestListTools_MITLDefaultMatchesGate` — UI が見せる
  全ツールの `MITLDefault` がディスパッチャ実挙動と一致
  することを検証。両者が同じ descriptor から derive する
  ため、リファクタ後はより強い保証に。
- `TestIsToolMITLRequired_AnalysisDefaultsMatchTable` —
  旧 map ではなく descriptor MITL デフォルト使用に refactor。

### 6.2. 新規テスト

v0.5 drift バグ class を構造的に防止する 4 つの構造的テスト。
migration 専用テストは無し (履歴的 MITL デフォルトマップ
スナップショット案は検討して却下 — 下記 4 テストで contract
surface が十分カバーされる)。

- **`TestToolDescriptors_UniqueNames`**: 2 つの descriptor
  が名前を共有しない; 偶然の重複エントリ防止。
- **`TestToolDescriptors_AllHaveHandlers`**: 各 descriptor
  の `Handle` が非 nil; 「エントリ追加したが Handler 配線
  忘れた」防止。
- **`TestDispatchDescriptor_RoutesAllNamesInLLMToolDefs`**:
  `toolDefsForLLM()` が返す全名前が
  `dispatchDescriptor()` でディスパッチ可能でなければ
  ならない。v0.5.1 の "case label 抜け" バグ class を
  構造的に捕捉。
- **`TestListTools_ContainsAllDescriptors`**: 各 descriptor
  が `ListTools()` 出力に現れる (HideUntilDataLoaded
  フィルタ除く)。v0.5.1 の "Settings タブにツール無し"
  バグが構造的に不可能に。

## 7. 互換性

- **公開 API**: Go 側公開 API 変更なし。バインディングが
  backing する `Agent` メソッド (`SendWithImages`、
  `LoadSession`、`ListTools` 等) は signature と観測挙動を
  維持。`ToolInfoItem` / `ToolDef` / `MessageData` フィールド
  形状不変。
- **オンディスク形式**: 変化なし。chat レコード、objstore
  index、セッションメモリ — すべて触らない。
- **LLM 視点**: ツール定義は同じ順序で同じ説明と
  パラメータスキーマで出力。モデルから違いは見えない。
- **Settings UI**: 同じツール一覧、同じ MITL トグル、同じ
  カテゴリラベル。純粋に rendering — React や CSS の変更
  不要。
- **Bindings.ts / wails 生成型**: 変化なし。リファクタは
  純粋に Go 内部。

## 8. リスク

- **クロージャ capture バグ。** 各 descriptor の `Handle`
  は構築時に `a *Agent` を capture。`a` が再生成 (例:
  セッション間で再 init) されると古いクロージャが死んだ
  インスタンスを指す可能性。緩和: descriptors は
  `Agent.New()` で再構築、`Agent` は in-place 再 init せず
  常に新規構築。既存のセッション復元パスでテストカバー。
- **LLM と UI の description drift**。現在は独立保守; リファクタ
  後は単一 `Description` フィールド共有。これは v0.5.0 で
  既に発生していた description 不一致 (一部は "image /
  blob / report" vs "image / blob / report / markdown") に
  対する *修正*。リファクタコミットでツールごとに正典
  テキストを 1 つ選ぶため、Settings に user-visible な
  文字列変化が数件出る可能性。CHANGELOG で明示必要。
- **クロージャ init 順序。** Descriptors は `a.toolLoadData`
  等をメソッド値として参照。メソッド値は `a` 存在後いつでも
  capture 可能、安全; ただし agent が phased init (例:
  analysis engine が初回 load まで ready でない) に分割
  された場合でも、descriptor list は先頭で構築される。
  ハンドラは `a.analysis == nil` を見て今日と同じエラーを
  返す。
- **Phase 2 は最大の変更ボリューム** (~600 行純変化) だが、
  10 個のサブコミット (2a–2j) に分割されており各 < 100 行
  純変化。サブコミットごとのレビュー負担は小さく、bisect は
  10 個の view 関数移行のいずれかにリグレッションを切り
  分けられる。

## 9. スコープ外

- **Shell ツールレジストリ再設計。** 既に single-source;
  Phase 4 はオプションの polish、挙動変化なし。
- **MCP ツール descriptor materialization。** MCP ツール
  は実行時発見; 静的 descriptors に合わない。外側ディスパッチャ
  は `mcp__` prefix ブランチを維持。
- **ツールカテゴリ語彙変更。** "read" / "write" / "execute"
  維持; MITL 特殊は "sql_preview" / "analysis_plan" 維持。
  必要なら将来リリースで統一可。
- **LLM 側ツールコールスキーマ再設計。** `llm.ToolDef`
  struct は as-is。Vertex / local backends は変化なし。
- **Settings UI 再設計。** Settings → Tools タブはレイアウト
  維持; 裏のツールリストが derive view になるだけ。
- **パフォーマンス最適化。** Descriptor lookup は index
  map 経由で `O(1)`; `ListTools()` の iteration は N ≈ 20-30
  で `O(N)`。無視できる。

## 10. 決着済の判断

設計レビューラウンドで上がった 4 つの open question を
ここで posterity 用に決着記録:

1. **Phase 4 (shell ツール descriptor view): スキップ。**
   shell ツール (`internal/toolcall/Registry`) は既に
   理想的な single-source パターン — v0.5 drift バグの
   発生面ではない。`toolcall.Tool` を `ToolDescriptor` に
   wrap してもクリーンな iteration を別のクリーンな
   iteration + wrapper コードに変えるだけで drift 防止
   payoff 無し。MCP guardians (動的で静的 descriptor 不可)
   にも同じ理屈。
2. **migration 専用テスト: ドロップ。** 元案の
   `TestToolDescriptors_MITLDefaultsMatchHistoricalMap` と
   `TestToolDescriptors_DescriptionsMatchLLMOutput` は
   1 リリース用の移行安全網だった。§6.2 の 4 つの構造的
   テスト (UniqueNames / AllHaveHandlers /
   RoutesAllNamesInLLMToolDefs / ContainsAllDescriptors) で
   contract surface はカバー済、無駄な dead weight を
   残す必要なし。
3. **`ToolDescriptor.MITLCategoryOverride`: 採用確定。**
   空文字列なら `Category` (read/write/execute → 汎用確認
   ダイアログ) に fall-back。`"sql_preview"` / `"analysis_plan"`
   のような非空値が frontend 側の専用ダイアログ (SQL preview、
   analysis-plan editor) を起動。今後の特殊 UI もディスパッ
   チャに触らずここに新文字列を足すだけ。
4. **Phase 2 コミット粒度: 10 個の小サブコミット (2a–2j)。**
   各 < 100 行純変化、それぞれ独立コンパイル + テスト pass。
   bisect-friendly、squash しない。

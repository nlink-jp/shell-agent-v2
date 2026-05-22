# ADR-0023: tool 名を `snake_case` を canonical 形に + 境界で正規化

- Status: Proposed
- Deciders: magi
- 関連: Issue #12 (発端)、ADR-0007 (tool registry refactor)

## 1. 背景

[Issue #12](https://github.com/nlink-jp/shell-agent-v2/issues/12)
は、Ollama 経由の Gemma がハイフンを含む tool 名を発行できない、と
報告している。Gemma の公式 function-calling 形式は Python の
`tool_code` ブロックである:

````
```tool_code
my_function(arg="x")
```
````

Python 識別子は `-` を許さず (`my-function(args)` は式
`my - function(args)` としてパースされる)、有効な `tool_code` を
発行する制約下のモデルは、schema や prompt で何を伝えてもハイフンを
含む識別子を生成できない。これは soft な傾向ではなく **構造的制約**
である。Google の Gemma function-calling ドキュメントは明示的に
`snake_case` または `camelCase` を推奨し、ハイフン未対応であると
明記している。

LM Studio は Gemma を OpenAI 互換 JSON tool-call shim + constrained
decoding でラップすることでこの制約を隠蔽する。Ollama は Gemma の
ネイティブテンプレートを使うため、制約が表面化する。

`executeTool()` のディスパッチャは registry に対して exact-string
lookup を行う (`app/internal/agent/agent.go:2266` →
`dispatchDescriptor` → `toolDescriptorByName`)。registry が
`list-objects` を key として持つところに Gemma が `list_objects` を
発行すると、lookup は `(zero, false)` を返し、"unknown tool" エラーに
fall through する。

### 1.1. tool source は 4 種類、コントロール度合いも 4 段階

| Source | 名前の出所 | コントロール下? |
|---|---|---|
| Descriptors (28) | `app/internal/agent/tool_descriptors_*.go` にハードコード | **完全** — 自分たちで書き、自分たちで出荷 |
| 同梱 shell スクリプト (6) | `app/internal/bundled/tools/*.sh` の `# @tool:` ヘッダ (うち 5 個が現在 kebab) | **完全** — 自分たちで書き、embed して scaffold |
| ユーザ作成 shell スクリプト | ユーザの `~/.../tools/` 下の任意ファイルの `# @tool:` ヘッダ | **不可** — ユーザ管理 |
| MCP tool registry | 上流 MCP server の tool 名、`mcp__<guardian>__<tool>` でプレフィクス | **不可** — 外部 server 管理 |

これまでのデフォルトは「命名には手を入れない / LLM に逐語的に流す /
exact match で dispatch」だった。これは上 2 行を下 2 行と同じ扱い
にしていた — 自分で所有しているのに。結果として、自分でコントロール
できるものが、サポートしたいバックエンドの制約に違反しており、しかも
それを支える仕組みが何もない、という状態になっていた。

### 1.2. 他のバックエンドも soft な同種バイアスを持つ

Ollama 経由の Qwen / Llama / Mistral は厳密に Python 構文に縛られて
いないが、`snake_case` の tool 名で大量に学習されており、grammar
enforcement off のときに同じ `-` → `_` 置換を **soft bias** として
示す。LM Studio と Vertex AI / Gemini (JSON tool-call 形式) は名前を
そのまま素通しするので今日は影響を受けないが、別のローカルバックエンドが
追加された瞬間に影響を受ける。

## 2. 決定

2 つのパートを 1 リリースでまとめて適用する。それぞれが §1.1 の
異なる行に対応する。

**Part A — 自分でコントロールできるものはソースを canonical 形に。**
自分が所有する tool 名すべてを kebab-case から `snake_case` に
リネームする:

- `tool_descriptors_{builtin,analysis,sandbox}.go` の全 28 descriptor
- `app/internal/bundled/tools/` 配下の kebab 名 同梱 shell スクリプト
  5 個 (`file-info.sh`, `get-location.sh`, `list-files.sh`,
  `preview-file.sh`, `write-note.sh`) の `# @tool:` ヘッダ行。
  スクリプトのファイル名はそのまま — `ScanDir`
  (`internal/toolcall/toolcall.go:90`) は registry の key を
  ヘッダ値で引いており、ファイル名は dispatch に使われない。
  ファイル名を rename しても dispatch には影響しないし、すでに
  ユーザの data dir に scaffold 済みの `file-info.sh` 等との
  乖離 (旧ユーザは `file-info.sh`、新ユーザは `file_info.sh`
  という状態) を作らないためにも、ファイル名は触らない方が
  整合的。

これが筋の通った fix: ソースが LLM の受け取る形そのままで書かれる。
「schema は X と言っているが Gemma は Y を発行する」という認知的不協和
が消える。

**Part B — コントロールできないものに対する境界正規化。**
canonical 関数を 1 つ追加し、registry 境界で適用する:

```go
// app/internal/agent/tool_name.go (新規ファイル)
func canonicalToolName(name string) string {
    return strings.ReplaceAll(name, "-", "_")
}
```

§3 に列挙する 5 つの境界点で適用する。これにより Part A だけでは
得られない 3 つの性質が保証される:

1. **履歴互換** — 旧名で永続化されたセッション
   (`tc.Name = "list-objects"`) が rename 後も migration ステップ
   なしに正しく dispatch される。
2. **ユーザ shell tool への許容** — `# @tool: foo-bar` と書いた
   ユーザが Gemma 上で「unknown tool」を食らわされない;
   スクリプトは内部的に `foo_bar` で dispatch される。
3. **MCP への許容** — `foo-bar` を publish する上流 MCP server も、
   server 側の変更を依頼せずに Gemma で動作する。

Part A はソースでの正しさ、Part B は境界での防御。両方必要。

## 3. 境界点 (Part B の 5 ヶ所)

| # | 場所 | File:line | 方向 | 理由 |
|---|---|---|---|---|
| 1 | `rebuildToolDescriptorIndex` | `tool_descriptor.go:125` | canonical を key に | Part A 後の belt-and-suspenders: descriptor ソースが将来 kebab に逆戻りしても index は正しく保たれる |
| 2 | `toolDescriptorByName` | `tool_descriptor.go:112` | クエリを正規化 | 生の名前を渡す caller がいても正しいエントリに当たる |
| 3 | `executeTool` | `agent.go:2257` | 入口で `tc.Name` を 1 回正規化 | 既存の `tc.Arguments = normalizeToolArgs(...)` (`agent.go:2258`) と対称。セッション履歴 replay、ユーザ shell スクリプト、MCP リターンを 1 ヶ所でカバー |
| 4 | `buildToolDefs` | `agent.go:2351` (+ descriptor, shell, MCP サブパス) | LLM に canonical 名で emit | ユーザの shell tool / MCP tool の作者が kebab を使っていても LLM には snake で送る |
| 5 | `ListTools` (UI binding) | `agent.go:1373` 付近 | UI に canonical 名で emit | Settings → Tools が LLM と同じ名前を表示 — UI が `foo-bar`、wire が `foo_bar` という乖離を防ぐ |

Part A 後、サイト #1–#2 は descriptor 用としてはほぼ冗長になる
(ソースが既に canonical)。ユーザ shell スクリプトと MCP tool 用、
そして将来 descriptor に `-` 入りが誤って追加された場合の
defense-in-depth として機能する。

### 3.1. MCP 区切り `__` は影響を受けない

MCP tool 名は `mcp__<guardian>__<tool>` の形で LLM に届く。`__`
区切りは `strings.ReplaceAll(_, "-", "_")` を素通する — 置換される
のは `-` 文字だけ。したがって `splitMCPName` (`agent_mcp.go`) の
パースロジックは変更不要; canonical 化は `<guardian>` と `<tool>`
セグメントそれぞれに対して join される前に行われる。

## 4. 変更 **しない** もの

- **ユーザ作成 shell スクリプト**。`# @tool: foo-bar` と書いた
  ユーザのスクリプトは引き続き動作する — Part B 経由で `foo_bar`
  として dispatch される。
- **すでにユーザの data dir に scaffold 済みの同梱スクリプト**。
  `Install()` (`internal/bundled/bundled.go`) は one-shot 設計で、
  既存ファイルを上書きしない。旧バージョンを入れたユーザの
  `~/.../tools/` には `# @tool: file-info` を持つ `file-info.sh` が
  残っている; Part B がこれを動かし続ける。新規 install は最初から
  canonical 名を得る。
- **上流 MCP server**。`foo-bar` を publish する server は Part B
  経由で動作。
- **ディスク上のセッション履歴**。`tc.Name = "list-objects"` を持つ
  古いレコードは Part B のサイト #3 で入口正規化され、引き続き
  正しく dispatch される。データ移行は不要。
- **MITL 記録、deferred-extraction queue、export/import JSON**。
  すべての読み出し経路は同じ dispatch / 表示サイトを通り、正規化を
  継承する。

## 5. 却下した代替案

検討して却下した各オプションについて、将来の再訪が一からやり直さない
ように理由を記録する。

### 5.1. 境界正規化のみ、rename しない

本 ADR の初版ドラフト。descriptor ソースと同梱 shell スクリプトは
kebab-case のままにし、すべて正規化レイヤで吸収する案。

却下理由: descriptor と同梱スクリプトは自分のコントロール下にある。
サポートしたいバックエンドが *発行できない* と分かっている形でソースを
書き、変換で取り繕うのは、LLM が見る形について不誠実。
`tool_descriptors_analysis.go` を見て `Name: "analyze-data"` を
目にした将来の読者は、canonical レイヤがそれを LLM に流す前に
`analyze_data` に書き換えていることを *知っていなければならない* —
これはまさに、誰かがそのファイルに触れるたびに理解コストを支払う
タイプの間接性である。ソースは wire 上で実際に流れる形で綴り、
正規化レイヤは本当に書き換えられない入力のために取っておく方が良い。

### 5.2. rename のみ、正規化レイヤなし

5.1 の鏡像: 28 descriptor と 5 同梱スクリプトを全部リネームし、
canonical レイヤを落とす案。

却下理由: それ単独だと 3 つのものが壊れる。

- **古いセッション履歴** は `tc.Name = "list-objects"` を持つ;
  `list_objects` だけを key とする registry は replay 時の dispatch
  に失敗する。one-shot 移行は構想可能だが error-prone (全セッション
  DB の DuckDB カラム書き換え + ユーザバックアップにある
  export/import JSON も)。
- **すでに scaffold 済みの同梱スクリプト** はユーザの data dir に
  kebab の `# @tool:` ヘッダのまま残る (scaffold は one-shot)。exact
  match の dispatch では突然見つからなくなる。
- **kebab 名のユーザ shell スクリプト** が無警告で壊れる。
- **kebab 名を publish している MCP server** は引き続き Gemma で
  失敗する。

正規化レイヤがあることで、rename は安全になり、書き換えられない
外部入力に対して誠実でいられる。

### 5.3. バックエンド別書き換え

active backend が Gemma/Ollama のとき、schema 送信前に tool 名を
`snake_case` に書き換え、受信時に逆マップする。回避策をローカライズ
する案。

却下理由: バックエンド固有の特別扱いを (schema-emit と
tool-call-receive の) 両方向に、必要なバックエンドごとに足すことに
なる。今日は Ollama+Gemma が騒がしいだけだが、明日は Ollama+Qwen、
明後日は別のローカルバックエンド。バックエンド別 codec を永遠に書き
続けることになる。soft bias のバックエンド (Qwen / Llama) は扱いが
不整合: codec を足さなければ `-` の schema に対して `_` を emit
し続けて確率的に失敗する。canonical-everywhere アプローチは
これらすべてを一様に扱う。

## 6. 影響とリスク

### 6.1. クロスソース衝突

現在の uniqueness テスト
(`tool_descriptor_structural_test.go` の
`TestToolDescriptors_UniqueNames`) は 2 つの *descriptor* が同じ
`Name` を持たないことしかチェックしない。shell tool や MCP tool に
対しては **チェックしない**。

本変更後:
- descriptor ソースは canonical 形なので、descriptor 内の衝突は
  今日と同じ集合のまま。
- ユーザ shell tool `foo-bar` と descriptor `foo_bar` が両方
  `foo_bar` になり衝突。
- guardian 配下に同じ名前の tool `foo_bar` がいる状態で
  `foo-bar` を publish する MCP server がいた場合、
  `mcp__<guardian>__foo_bar` namespace で衝突。

緩和策: `TestToolDescriptors_UniqueNames` を descriptor ソース内
の canonical uniqueness に拡張。shell tool 登録時に canonical 名が
既存 descriptor と衝突したら runtime warning (非致命的) を出し、
descriptor 側を優先する。MCP tool は `mcp__<guardian>__` で
namespace されているのでクロス guardian 衝突は不可能;
guardian 内衝突は病理的なケース — warning を出して後勝ち / drop する。

### 6.2. UI / docs / コメント — 一回限りの search-replace

rename 後、tool 名を文字列リテラルで言及するすべての箇所を更新する
必要がある。調査済みの範囲:

- 28 descriptor ソースの `Name:` リテラル
- 5 同梱スクリプトの `# @tool:` ヘッダ (ファイル名は変更しない)
- `app/frontend/src/dialogs/MITLDialog.tsx:70–73` — MITL UI 用の
  特別 argument フィールド向け小さな dispatch-by-name テーブル
  (`'sandbox-run-shell': 'command'` 等)
- tool 名を言及するユーザ向け TS コピー:
  `FindingsDisclosure.tsx:171`、`ChatInput.tsx:155`、
  `SettingsDialog.tsx:816, 887`
- Go / TS 内の tool 名を逐語的に書いたコメント (機械置換)
- `app/internal/...` 配下の tool 名を文字列で参照しているテスト

機械的な作業だが diff の幅は広い。canonical レイヤがあるおかげで
PR 途中で個別の rename が壊さない: どこかで stale な
`list-objects` が残っていても Part B が `list_objects` に正規化し、
dispatch は成功し続ける。

### 6.3. ドキュメント

README.md、README.ja.md、AGENTS.md は例の中で tool 名を逐語的に
言及している (「`analyze-data` ツールは…」)。`analyze_data` 形式に
更新する。

### 6.4. セッションと scaffold dir の後方互換

§4 参照。古いセッション履歴と既に scaffold 済みの同梱スクリプトは
正規化レイヤ経由で動作し続ける。データ移行も scaffold 書き換えも
不要。

## 7. 実装計画

Part A と Part B を 1 PR で同梱。両者は 1 つの論理変更であり、
別々に出荷すると不誠実なソース状態 (A 欠落) か、古いセッション /
scaffold スクリプトの破壊 (B 欠落) のどちらかが残る。

rename 込みで ~400 LoC 程度。

1. **まず Part B の足場** — `app/internal/agent/tool_name.go` で
   `canonicalToolName` を定義、`tool_descriptor.go` (×2) と
   `agent.go` (×3) の 5 つの境界に適用。このコミット時点で既存
   テストは全 PASS する: canonical 化は既に canonical な入力に
   対しては no-op、既存の kebab 入力に対しては無害な書き換え。
2. **Part A descriptor rename** — `tool_descriptors_{builtin,analysis,sandbox}.go`
   の 28 個の `Name:` リテラル。同ファイル内コメントで旧名に
   言及している箇所も更新。
3. **Part A 同梱スクリプトのヘッダ rename** — 5 同梱スクリプトの
   `# @tool:` ヘッダを snake_case に更新。ファイル名は据え置き:
   dispatch はヘッダ値を key にしており (`ScanDir` /
   `internal/toolcall/toolcall.go:90`)、ユーザの data dir に
   scaffold 済みのコピーもどのみち触らない。`bundled_test.go`
   (ファイル名を assert している) も変更不要。
4. **Frontend MITL dispatch テーブル** —
   `app/frontend/src/dialogs/MITLDialog.tsx:70–73` のキーを
   canonical 形に更新。
5. **機械的な掃除** — tool 名を逐語的に書いたコメント、ユーザ向け
   TS コピー、tool 名を文字列で参照しているテストファイル。
   ファイルごとに 1 回の search-replace pass。
6. **テスト**:
   - `TestToolDescriptors_UniqueNames` を canonical uniqueness に
     強化。
   - 新規 `tool_name_test.go` — `canonicalToolName` の
     table-driven テスト + `list-objects` → `list_objects` の
     dispatch ラウンドトリップを行うリグレッションテスト
     (セッション履歴互換の検証)。
7. **ドキュメント** — README.md、README.ja.md、AGENTS.md の例を
   canonical 名に更新。AGENTS.md には短い注を追加:
   「tool 名は `snake_case`; registry は境界で `-` を `_` に
   正規化するので、ハイフンを使ったユーザ shell スクリプトや
   MCP 名も引き続き動作する」。
8. **CHANGELOG.md** — 次バージョンの新セクション、枠は
   `fix(agent): accept snake_case tool calls from Gemma/Ollama (issue #12)`
   で、組み込み / 同梱 tool も rename した旨を併記。

各コミットで `go test ./internal/... -tags no_duckdb_arrow` が PASS
すること。

## 8. スコープ外

- **次回起動時にユーザの scaffold 済み同梱スクリプトを書き換える
  こと**。scaffold は設計上 one-shot であり、「古い install を直す」
  ステップは導入しない; Part B の正規化がカバーする。
- **ローカルバックエンドでの constrained-decoding サポート。** Gemma を
  全く別レイヤで fix する案。別の論点。
- **MCP guardian 名のリネーム** (AGENTS.md で kebab-case に制約
  されている)。guardian 名は `mcp__<guardian>__<tool>` の中にあり、
  それ自体は LLM が emit する function name ではなく、ルーティング
  要素にすぎない。
- **CamelCase の許容。** 将来バックエンドが `listObjects` を produce
  した場合の正規化は別判断。本 ADR は `-` ↔ `_` の等価性だけに
  コミットする。

# Markdown 添付 (v0.5)

## 1. 動機

agent の分析対象は現状 `load-data` が取り込める CSV / JSON
/ JSONL に限定される。表形式に乗らないテキスト系コンテンツ
には first-class な home が存在せず、ユーザは inline ペースト
(context budget で truncate)、サンドボックスに手動配置、
あるいは意味構造を潰す CSV への前処理を強いられる。

このリリースでは **Markdown 添付** を first-class な
object type として導入し、これに直接動作する 3 ツール
(`analyze-text` / `grep-text` / `get-text`) を追加する。
ねらいは、ドキュメント (仕様書、監査レポート、長文 prose)
を表形式パイプラインに無理矢理通さずに agent が探索できる
ようにすること。

PDF / DOCX / その他バイナリ形式は v0.5 では明示的に scope
外。外部 converter contract を別途設計したいため、v0.5 は
純粋な Markdown 対応のみを出す。

### 1.1. ユースケース

1. **単一ドキュメント Q&A** — ユーザが Markdown を添付 →
   質問 → agent が `get-text` で読む / `analyze-text` で
   要約。
2. **テーブル化できない JSON をテキストとして扱う** — 部分
   対応: ユーザが `.md` ファイル内の code fence に JSON を
   包む。JSON 直接の text 扱いは将来リリースで。
3. **Markdown Q&A** — 直接添付、ユースケース 1 と同じ流れ。
4. **複数ドキュメント Q&A** — セッション内に複数添付が共存。
   agent は `list-objects` で列挙、新ツールで個別に読む。

### 1.2. ボーナス: 既存 Report が入力データ化

`create-report` (agent のレポート生成ツール) は既に出力を
objstore に `TypeReport` として保管 — 中身は Markdown。
新ツール群は `TypeReport` オブジェクトも `TypeMarkdown` と
**並んで**受け付けるので、過去に生成したレポート (同セッション
内 / `.shellagent` export/import 経由で別セッション) を
ツール 1 発で再分析できるようになる。新メカニズム不要。

## 2. 設計原則

- **Markdown のみ。** v0.5 では自動変換なし。PDF は後続
  リリースに pending。in-process subprocess 起動の
  security / resource / UX 複雑性を回避。
- **Determinism (case X)。** 添付は system prompt に注入
  **しない**。LLM は既存の `list-objects` で添付を発見し、
  新ツールで読む。理由: Memory / Findings が育つにつれ
  eager injection は余剰 context を非決定的に縮小し、同じ
  添付が同セッション内の異なる時点 / 異なるセッションで
  違う window で分析される。再現性は小型添付の 1-turn
  ergonomic より重要。
- **ID 統一。** 全 storage object — image, blob, report,
  添付 markdown — は既存の 32-hex (旧 12-hex) `object:<id>`
  規約で参照。filename-as-handle は採用せず。LLM は images /
  reports で既にこの言語を話せており、テキスト添付もそのまま
  乗る。
- **objstore 最小拡張。** 新 `ObjectType` 定数 1 個
  (`TypeMarkdown`)、新 optional `ObjectMeta` フィールド 2 個
  (`Lines`, `Tokens`)。それ以外 (storage 層、session
  ライフサイクル、export/import、ID rewrite) は無改変で再利用。

## 3. 不採用案

### 3.1. system prompt への eager 注入 (case Y)

system prompt に添付 content をトークン予算まで含める;
大きい添付はメタ情報のみ。

**不採用** — Memory と Findings がセッションを通じて蓄積し、
余剰 context を縮小する。同じ添付がセッション序盤では inline
収まり後半は溢れる、結果として同じ分析プロンプトが時間と共に
違う context window を生む。再現性は小型添付の 1-turn
ergonomic より優先。

### 3.2. attach 時に自動 line-table load (case Z)

添付すると即座に DuckDB の `(line_number, text)` テーブルに
materialize する。

**不採用** — 2 つの異なる保管 home (content の objstore /
分析投影の DuckDB) を 1 つのユーザジェスチャに混ぜている。
本設計の議論を通じて毎回同じ違和感に収束した: ユーザは
「これを attach する」の mental model を 1 つだけ欲しがり、
「attach AND DB に投影」を欲しがらない。LLM が後で行に
対する SQL を必要としたら、sandbox 経由 (`awk '{print NR","$0}'
| load-data`) で実現できる。

### 3.3. filename ベースのツールハンドル

ツールが `analyze-text("audit.md", ...)` を取る (vs
`analyze-text("object:abc123", ...)`)。

**不採用** — チャットメッセージ、レポート、画像参照すべて
`object:<id>` を正規参照形式として使っている。filename
ハンドルは並列の taxonomy を生み、LLM が 2 系統を行き来
することになる。collision / encoding / session 横断曖昧性
など、既存規約には無い failure mode を導入する。

### 3.4. in-process PDF / DOCX converter

Go ライブラリで PDF parser を同梱 / pandoc などを shell out。

**v0.5 では不採用** — subprocess execution contract、
security review、resource 上限、error-handling UX、converter
selection が必要。それぞれ非自明な設計判断。本リリースに
くっつけるより専用リリースに defer。

### 3.5. `load-text-data` ツール (明示的 DB 投影)

Markdown 添付を DuckDB line-table に投影する新ツール。

**v0.5 では不採用** — (a) `analyze-text` と `grep-text` で
要約と検索は native にカバー、(b) 行への SQL が本当に
必要な場面では agent + sandbox の組合せが shell 1 行 +
既存 `load-data` で処理可能、(c) 明確に要求するユースケース
が無い状態でツール面を増やすと v0.5 scope が膨らむ。

## 4. ストレージモデル

### 4.1. ObjectMeta 拡張

```go
type ObjectMeta struct {
    ID        string     // 不変: 32-hex (legacy: 12-hex)
    Type      ObjectType // 不変 + 新 TypeMarkdown
    MimeType  string     // 不変
    OrigName  string     // 不変 (表示ラベルのみ)
    CreatedAt time.Time  // 不変
    SessionID string     // 不変
    Size      int64      // 不変
    Lines     int        `json:"lines,omitempty"`  // 新: text/markdown 系のみ
    Tokens    int        `json:"tokens,omitempty"` // 新: memory.EstimateTokens キャッシュ
}

const (
    TypeImage    ObjectType = "image"     // 不変
    TypeBlob     ObjectType = "blob"      // 不変
    TypeReport   ObjectType = "report"    // 不変
    TypeMarkdown ObjectType = "markdown"  // 新
)
```

### 4.2. Attach パイプライン

1. ユーザが添付ボタン / drag-drop / paste で `.md` / `.txt`
   ファイルを選択。frontend が FileReader で正しい MIME
   (`text/markdown` / `text/plain`) の data URL として読込。
2. `bindings.SaveImage` をより正確な
   `bindings.SaveAttachment` にリネーム (or shadow) し、
   任意のサポート添付 MIME を受ける。image 限定 flow は
   既存 frontend 経路の後方互換性のため残す。
3. `objstore.SaveDataURL` の type 推論を拡張:
   ```
   image/*           → TypeImage     (不変)
   text/markdown     → TypeMarkdown  (新)
   text/plain        → TypeMarkdown  (新; markdown として扱う)
   application/json  → 現状 TypeBlob、変更なし
                       (defer; v0.5.x or v0.6)
   その他            → TypeBlob      (不変)
   ```
4. `objstore.SaveDataURL` は既存通り `Store()` に委譲。
5. 完成した `ObjectMeta` を既存 `Store()` パス経由で
   objstore に保存。`OrigName` はユーザのファイル名を
   表示用に保持 (LLM-facing handle は ID のまま)。`Lines` /
   `Tokens` は `Store()` 自身が算出する (§4.4 参照) ため、
   attach 側コードでロジックを重複させない。

### 4.3. サイズ上限

attach 時点で 50 MB hard limit (ファイル content を読む前
に拒否) — markdown ドキュメントが 50 MB に達することは
めったになく、それを超えると data-URL ラウンドトリップと
EstimateTokens スキャンが顕著に高コストになる。それより
大きい content は sandbox + register-object 経由
(ユーザが手動で `/work` にコピー後) で取り込む。

### 4.4. `Store()` 内での `Lines` / `Tokens` auto-fill

監査で表面化した非対称リスク: `create-report`
(`agent/tools.go:596`) は出力を `objstore.Store(reader,
TypeReport, "text/markdown", ...)` で既に `TypeReport` として
書き出している。新 `TypeMarkdown` パスでだけ `Lines` /
`Tokens` を計算すると、レポートは同じ種類の content を持つ
にもかかわらず `list-objects` でこれらカラム無しで表示
されることになる。LLM は metadata の非対称を見て一貫しない
振る舞いをし、レポートに対する `get-text` フローも行数
アンカーを失う — このフィールドを導入する動機 (「LLM に
sandbox で `wc -l` を呼ばせない」) からのリグレッション。

修正: `Store()` 自身が `mimeType` を見て `text/` で始まる
場合に `Lines` と `Tokens` を auto-compute する。ロジック:

```go
// Store() 内、io.Reader が byte buffer に完全に読み込まれた後
// (これは write path のため既に行われている):
if strings.HasPrefix(mimeType, "text/") && len(content) > 0 {
    meta.Lines = bytes.Count(content, []byte{'\n'}) + 1
    meta.Tokens = memory.EstimateTokens(string(content))
}
```

**新規書き込み**への効果:
- 新 `TypeMarkdown` 添付 → `Lines` / `Tokens` 付与 ✓
- 新 `TypeReport` (`create-report` 経由) → `Lines` /
  `Tokens` 付与 ✓ (`toolCreateReport` への変更不要 —
  MIME ベースの dispatch が自動で処理する)

**レガシー object** (本リリース前に保存された pre-v0.5 の
`TypeReport` 行) に対しては、`objstore.Load()` を **lazy
backfill** で拡張する:

```go
// Load() 内、既存 index.json 読込のあとで:
for _, meta := range s.index {
    if meta.Lines > 0 { continue }           // 既に populated
    if !isTextMIME(meta.MimeType) { continue } // image/blob は skip
    content, err := os.ReadFile(s.DataPath(meta.ID))
    if err != nil { continue }                // data file 欠損は許容
    meta.Lines  = bytes.Count(content, []byte{'\n'}) + 1
    meta.Tokens = memory.EstimateTokens(string(content))
    s.dirty = true
}
if s.dirty { _ = s.save() }  // 埋めた metadata を永続化
```

挙動:
- v0.5 初回起動: レガシー `TypeReport` (および `text/*`
  MIME を持つ仮想的な `TypeBlob`) の `Lines` / `Tokens` が
  `objstore.Load()` で埋まる。更新済み index は保存される
  ので、このコストはちょうど 1 回だけ発生。
- 2 回目以降の起動: `meta.Lines > 0` で loop body が
  short-circuit するので backfill は実質ゼロコスト。
- 読込失敗 (out-of-band で data file 削除など) は許容 —
  該当エントリは `Lines = 0` のまま、アプリ全体は動作継続。
- 新規 v0.5 インストールでは backfill 経路に到達しない
  (新規書込がすべて `Store()` 側 auto-fill を通るため)。

これでアップグレード時に self-healing になる: migration UI
不要、ユーザ操作不要、恒久的な非対称も無し。レガシーレポート
を含むセッションを 1 度でも触った後は、`list-objects` 出力は
レガシー object と新規 object で一様になる。

### 4.5. セッションライフサイクルの再利用

- **Export (`.shellagent` bundle, v0.4.0)**: `SessionID ==
  当該セッション` の全 object を含める。`TypeMarkdown` は
  自動的に乗る。
- **Import**: ID 再生成は既存 `sessionio/rewriter.go` の
  regex `\b(object:)?([a-f0-9]{32}|[a-f0-9]{12})\b` で
  sweep して `chat.json` / `summaries.json` の参照を書換。
  新コード不要。
- **Delete (v0.4.2)**: `a.objects.DeleteBySession` は type
  に関わらず session タグ付き object をすべて削除。
- **Rename (v0.4.5)**: rename は `a.session.Title` のみ
  変更。添付は無影響。
- **Private session (v0.3.0)**: privacy フラグは Memory
  promotion を制御する。添付は構造上 per-session なので
  cross-session leakage path は無い。

## 5. ツール仕様

3 ツールとも `object` 引数を取り、値は `object:<id>` (推奨)
あるいは生の `<id>`。対象 object の `Type` は
`TypeMarkdown` または `TypeReport`。他 type は明示エラーで
返し、LLM が別アプローチを選べるようにする (image は
vision、CSV は `query-sql` 等)。

### 5.1. `analyze-text`

```
analyze-text(object: string,
             perspective: string,
             lines?: string)  // 例: "1000-5000"
```

(全体 or 範囲限定の) ドキュメントに対して sliding-window
要約 + finding 抽出を実行。既存
`internal/analysis/summarizer.go` を再利用し、上流に
**テキスト chunker** を挟む (§6 参照)。Findings は
セッションの findings store に
`source = SourceAnalyzeText` (新定数) で自動 promote、
Findings panel が origin でフィルタ可能に。

返却: `analyze-data` 同様の markdown レポート (running
summary + grouped findings)、10,000 文字で truncate
(既存 v0.2.0 挙動と同じ)。

### 5.2. `grep-text`

```
grep-text(object: string,
          pattern: string,        // RE2 regex
          lines?: string,         // 検索範囲限定
          max_matches?: int = 200,
          context_lines?: int = 2)
```

`<line_number>: <line content>` でマッチ返却 + 指定行数の
`-A` / `-B` コンテキスト。生マッチ数が `max_matches` を
超えたら、絞り込みを促す構造化エラー
(`Too many matches (3,847 > 200). Narrow the pattern, or
restrict via the lines argument.`)。

`pattern` は RE2 (Go の regexp)。PCRE 機能は無し。

### 5.3. `get-text`

```
get-text(object: string,
         lines: string)  // 必須; 例: "542-560"
```

指定行範囲を verbatim 返却、各行先頭に行番号を付ける
(`542: ...\n543: ...\n`) — 引用の曖昧性を排除。1 呼出
1,000 行で hard cap (それ以上の範囲は `analyze-text`
または複数 `get-text` を推奨するエラー)。

### 5.4. Sandbox 連携

v0.5 監査で確認: 既存 `sandbox-copy-object` ツール
(`sandbox_tools.go:60`) は任意 object type を objstore から
`/work` にコピーする。description だけ更新して markdown
言及を追加するが、新ツールは不要。逆方向 (`/work` →
objstore) は既存 `sandbox-register-object` / `register-object`
が処理、`type` enum に `"markdown"` を追加して LLM が
`/work` にあるファイルを生成した際に正しくタグ付けできる
ようにする。

## 6. `analyze-text` 用 chunker

`internal/analysis/summarizer.go` のサマライザは `rows
[]string` を取り、設定された window サイズで歩く。markdown
向けには chunker が `[]string` を返し、各要素が
ドキュメントの 1 chunk。

### 6.1. チャンキング戦略

1. **目標 chunk サイズ**: ~2,000 tokens (`memory.EstimateTokens`
   で推定)、10% overlap。MVP では tool 引数で expose せず、
   呼出側の内部 struct で構成可能。
2. **行原子性**: chunk 境界が行の途中に入らない。
3. **見出し aware (markdown 特有)**: chunk 境界が markdown
   見出し (`#`, `##`, `###`) の ~10% 以内に落ちる場合、
   見出しに snap して各 chunk が section break で始まる
   ようにする。best-effort — 見出し無しの degenerate
   入力は純粋な行原子性に fall back。
4. **`max_line_width` バックストップ**: 単一行が 10,000
   chars を超える場合 (まれ; code fence 内の minified
   JSON、base64 blob 等) は width 上限で強制改行、1 行が
   1 chunk の token 予算を一発で超えないようにする。
5. **総チャンク数上限**: 1 回の分析で 1,000 chunks。
   超過時は LLM に `lines: ...` での scope 絞りを促す
   エラー。

### 6.2. ウィンドウ間引継ぎ

既存 `summarizer.Analyze` は window 間で running summary
文字列 + 累積 findings リストを保持済。そのロジックに変更
なし。chunker が SQL 行文字列の代わりにテキスト chunk を
渡すだけ。

## 7. LLM 向け表面

### 7.1. 発見

既存 `list-objects` ツールの出力拡張:

```
- ID: a1b2c3d4e5f6 | Type: markdown | Name: audit.md
    | Size: 1234567 bytes | Lines: 45231 | Tokens: 312000
    | Created: 2026-05-11 10:30:00
```

`Lines` / `Tokens` カラムは `markdown` / `report` type に
対して出力、`image` / `blob` では省略。`list-objects` の
description も markdown 添付について言及するよう更新。

### 7.2. attach 直後 anchor

既存の `Image (object ID: xxx):` 規約 (生成元:
`llm/backend.go:imageIDPrefix`) と同パターンで、新規 attach
した markdown を含むユーザターンの chat message preamble
に 1 行追加:

```
Document (object ID: a1b2c3d4e5f6, name: audit.md, 312k tokens):
```

この行は添付を含む user-message turn に出る。`list-objects`
を呼ぶラウンドトリップなしに最近の attach を LLM が
in-context で把握できる。

### 7.3. system prompt 変更

3 つの新ツールには tool definition 側で description を
付ける。「attached documents」セクションは prompt 本体に
**注入しない** (case X — system prompt は session の成長に
対して invariant)。

既存 object 処理ガイダンス内に 2 段落のブロックを追加し、
2 種類の markdown 系 type の provenance 区別とどのツールが
適用できるかを LLM に伝える:

> Markdown content lives in the object store as two distinct
> types with different provenance:
>
> - **TypeReport** — markdown you (the agent) generated
>   previously via the `create-report` tool. These are your
>   own prior conclusions.
> - **TypeMarkdown** — markdown the user attached. These are
>   user-supplied source material.
>
> The three text tools (`analyze-text`, `grep-text`,
> `get-text`) operate on both types interchangeably; each
> takes an object ID. Use `list-objects` to enumerate, then:
> `analyze-text` for sliding-window summarisation of long
> content, `grep-text` for regex search, `get-text` for
> verbatim reading of a specific line range. Use
> `sandbox-copy-object` to expose either type to the sandbox
> `/work` directory when shell tools are needed.

provenance の区別を意図的に明示するのは、LLM が citation や
信頼度を適切に較正できるようにするため — 自身の以前の
レポートから引用する findings は、ユーザ提供のソース素材
から出てきた同じ findings に比べて、incremental information
が少ない。

### 7.4. Tool filtering

3 つの新ツールは **常時可視** (data-load による dynamic
gating なし)。`analyze-data` / `query-sql` 等 post-v0.1.20
挙動と一致。LLM は `list-objects` の出力から適用可否を
判断。

## 8. Frontend / UI

### 8.1. 添付ボタン拡張

`frontend/src/chatpane/ChatInput.tsx` の MIME フィルタを
`image/*` から `text/markdown` と `text/plain` も含むよう
拡張。drag-drop と paste handler も同じフィルタ拡大を
受ける。

### 8.2. Data disclosure panel と preview dispatch

`frontend/src/DataDisclosure.tsx` は既に type 別レンダリング
済。`TypeMarkdown` は 📝 アイコンカードに割り当てる
(`TypeReport` の 📄 と区別し、ユーザが「添付された物」と
「agent が生成した物」を一目で見分けられるように)。

`App.tsx` の preview dispatch (line 786 付近) を拡張し、
`TypeMarkdown` を **`TypeReport` と同じ branch に流す** —
両方とも `GetObjectText` で本文を取得、最初の markdown
見出しからタイトルを導出、`ReportViewer` を開く:

```tsx
} else if (obj.type === 'report' || obj.type === 'markdown') {
    const text = await window.go.main.Bindings.GetObjectText(obj.id)
    const title = (text.split('\n')[0] || '').replace(/^#\s*/, '')
                  || obj.orig_name || obj.id
    setExpandedReport({title, content: text})
}
```

これは意図的にユーザ添付 Markdown と agent 生成レポートを
**ビューア視点で交換可能**として扱う設計 — どちらも objstore
内の Markdown 文字列であり、既存 ReportViewer は両者にとって
正しいコンポーネント。delete / select-all / export フロー
も同じ dispatch 拡張で自動的に新 type を拾う。

### 8.3. サイズ警告

50 MB を超える markdown を attach しようとした場合、
data-URL ラウンドトリップ前に明示エラーを表示 ("File too
large; copy to sandbox /work and use register-object
instead")。

## 9. テスト計画

### 9.1. Unit (`internal/objstore/`)

- `TestObjectMeta_Migration_LegacyIndexLoadsWithoutLinesTokens`
  — 旧 index.json (Lines/Tokens なし) を load し、backfill
  発火**前**にフィールドが 0 にデフォルトすることを確認。
- `TestObjstoreLoad_BackfillsLegacyTextObjectMetadata` —
  手で index.json を書いて `Lines=0`/`Tokens=0` の
  `TypeReport` エントリと、既知の markdown を含む実 data
  file を作る; `Load()` を呼ぶ; (a) in-memory meta に
  `Lines = 期待値` と `Tokens = memory.EstimateTokens(content)`
  が入る、(b) disk 上の index.json も埋まった値で更新済み
  (= dirty index が再保存されている) をアサート。`TypeReport`
  と仮想的なレガシー `text/plain` `TypeBlob` の両方を
  cover し、MIME ベース predicate を検証。
- `TestObjstoreLoad_DoesNotBackfillImagesOrBinaries` —
  `Lines=0` の image と非テキスト blob を追加; Load 経由で
  そのまま放置されることをアサート。
- `TestObjstoreLoad_ToleratesMissingDataFile` — data file
  が削除された ID を index エントリが参照; Load は error
  なくスキップ、エントリは `Lines=0` のまま。
- `TestSaveDataURL_Markdown_PopulatesLinesAndTokens` — 既知
  の markdown body を渡し、Lines = 期待値、Tokens =
  `memory.EstimateTokens(body)` をアサート。
- `TestStore_TypeReportGetsLinesAndTokens` — `Store(reader,
  TypeReport, "text/markdown", ...)` を呼んで
  (`toolCreateReport` を模倣)、Lines/Tokens が populated
  されることをアサート。
- `TestSaveDataURL_TextPlain_TreatedAsMarkdown` —
  `text/plain` MIME が `TypeMarkdown` で保存される。
- `TestSaveDataURL_OversizedRefused` — 50 MB 超を拒否。

### 9.2. Unit (`internal/analysis/`)

- `TestTextChunker_TokenBudget` — chunks が target 以下に
  収まる。
- `TestTextChunker_LineAtomic` — chunk が行の途中で分断
  しない。
- `TestTextChunker_HeadingAware` — 許容範囲内で境界が
  見出しに snap する。
- `TestTextChunker_LongLineBackstop` — 病的な単行入力が
  `max_line_width` で強制改行される。
- `TestTextChunker_RespectsTotalChunkCap` — > 1,000 chunks
  → ヒント付きエラー。

### 9.3. Integration (`internal/agent/`)

- `TestAnalyzeText_Roundtrip` — 小さい markdown を attach、
  analyze-text 実行、結果に running summary + findings が
  `SourceAnalyzeText` タグ付きで載ることを確認。
- `TestGrepText_HitsAndContext` — 既知パターン検索;
  マッチ format + コンテキスト行を確認。
- `TestGrepText_TooManyMatches` — max_matches 超過; 絞り込み
  推奨のエラー文言を確認。
- `TestGetText_RangeRead` — 特定範囲読み出し; 行番号
  prefix 確認。
- `TestGetText_RangeTooLarge` — > 1,000 行要求; エラー確認。
- `TestTextTools_RejectWrongType` — `TypeImage` object に
  analyze-text 呼出; type-mismatch エラー確認。

### 9.4. ライフサイクル

- `TestSession_ExportImport_TextAttachment_IDsRewritten`
  — markdown 添付付きセッションを export、新規 data dir
  に import、新セッションの `chat.json` 内参照が再生成
  された ID を指すこと確認。

### 9.5. Frontend

- 型チェックのみ (`npm run build`)。マニュアルスモーク:
  markdown drag-drop、markdown text paste (添付化)、
  Data panel カードクリック → ReportViewer 開く。

## 10. 互換性

- **公開 API**: 純粋に追加 — `ObjectType` 定数 1 個、
  optional `ObjectMeta` フィールド 2 個、Findings source
  定数 1 個、agent ツール 3 個 (新)、既存ツール 3 個
  (`list-objects`、`sandbox-copy-object`、`register-object`
  enum) の不変かつ拡張的変更。
- **オンディスクフォーマット**: 後方互換。旧 `index.json`
  エントリは `Lines = 0` / `Tokens = 0` で load、migration
  ステップ無し。
- **セッション bundle (`.shellagent`)**: 既存 v0.4.0
  フォーマットは type 付き object を既に含むので
  `TypeMarkdown` は無改変で乗る。Import の ID-rewrite
  regex は関連参照を既にカバー。
- **v0.5 未満からの `.shellagent` bundle**: 問題なく load。
  単に markdown 添付を持たないだけ。

## 11. スコープ外 (v0.5)

- **PDF / DOCX / その他バイナリ形式** — defer。外部
  converter contract 設計が必要 (subprocess execution、
  security、timeout、resource 上限、error UX、converter
  selection)。専用設計パスに値する。
- **JSON-as-text** — 部分対応のみ (ユーザが `.md` 内 code
  fence に包む)。専用の `text/json` → `TypeMarkdown`
  マッピング (pre-store pretty-print 付き) は field
  evidence 後の自明な follow-on。
- **`load-text-data` (line-table 投影)** — agent +
  sandbox の組合せが rare ケースを既にカバー。ユース
  ケースが顕在化するまで専用ツールを追加しない。
- **添付ごとの auto-summary** — attach 時に LLM 呼出で
  1 行 description 生成。1 attach あたり追加 LLM ラウンド
  トリップのコストで marginal な gain。ユーザは
  `analyze-text` で必要時に summarise すれば良い。
- **ストリーミング attach (> 50 MB のファイルピッカー)**
  — 現状の data-URL ラウンドトリップは cap 未満ならすべて
  動作、それ以上は sandbox 経由。native path 用
  `bindings.SaveFile(path)` は v0.6 候補。
- **Settings UI からの chunker 設定** — chunker のデフォルト
  (2k tokens, 10% overlap, 10k char max line, 1k chunk
  cap) はコードで固定。field 経験が tuning を要求するなら
  surface 検討。

## 12. 設計中に決着した open question

| ID | 問い | 決着 |
|----|------|------|
| D1 | `analyze-text` の Findings source | 新 `SourceAnalyzeText` 定数 — `SourceAnalyzeData` と区別し、Findings panel が origin でフィルタ可能。 |
| D5 | text tools の dynamic filtering | 常時可視。post-v0.1.20 の `analyze-data` / `query-sql` 挙動と一致。 |
| D7 | system prompt 更新 | 既存 object 処理ガイダンス内に 1 文追加。新 section は作らない。 |

## 13. リスク

- **ローカル LLM の tool-call 信頼性**: 弱いローカル
  バックエンドは数ターン前の添付に対し `list-objects` を
  一貫して呼ばないかもしれない。緩和策: attach 直後 anchor
  行が user turn 内で即時 in-context 認識を提供、過去 turn
  の添付は LLM が enumerate するかどうかに依存。フィールド
  テスト必要。
- **EstimateTokens vs バックエンド tokenizer の drift**:
  キャッシュ `Tokens` 値はローカル CJK-aware estimator
  ベースで、Vertex の実 tokenizer から 10-30% 乖離する
  可能性。LLM の読む/skip 判断には十分許容、context budget
  enforcement には使わない。
- **`text/plain` を markdown と過大主張**: markdown 構造を
  持たない `.txt` ファイルは markdown renderer 経由でも
  問題なく描画 (semantic loss なし) だが、LLM が応答内で
  「markdown」と説明する可能性。コスメティック — 機能的
  failure なし。

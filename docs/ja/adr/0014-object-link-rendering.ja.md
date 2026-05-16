# オブジェクトリンクレンダリング — `[name](object:ID)` によるテキスト/markdown プレビュー — 設計ノート

**ステータス:** v0.9.0 で実装済み (2026-05-16)。
**ターゲット:** v0.9.0 (v0.8.0 からの minor bump — ユーザーから見える
新しいレンダリング挙動、Wails バインディング 1 個追加、破壊的変更
なし、3 箇所の parallel-list drift を 1 箇所に統合)。
**報告者:** ユーザー — 「レポートの中から LLM がオブジェクトストア
内のテキストやmarkdownをポイントすると、レポート内の埋め込み
オブジェクトがうまく動作しなくなる」。インライン画像
(`![alt](object:ID)`) は動作するが、非画像参照
(`[name](object:ID)` が `TypeMarkdown` / `TypeReport` を指す場合)
は `<a href="object:ID">` の死んだリンクとしてレンダリングされ、
クリックしても無反応で、エクスポートリゾルバとも相互作用が壊れる。

本ノートは既存の `img` オーバーライドと対称な **`a` コンポーネント
オーバーライド**、新規フロントエンドコンポーネント `ObjectLink`、
型判別用の小さな `GetObjectMeta` バックエンドバインディング、
`resolveObjectRefsForExport` の type-aware 修正、そして対応する
ツール記述子・システムプロンプト編集 (新たにサポートする
`[name](object:ID)` 参照形式を LLM に教える) を仕様化する。

---

## 1. 問題

shell-agent-v2 は 4 つのオブジェクト型を保管している
(`internal/objstore/objstore.go:30`):

| 型               | 出自                                                | 自然なインライン形式 |
|------------------|----------------------------------------------------|---------------------|
| `TypeImage`      | ユーザー添付 / `generate-image` / `register-object` | `![alt](object:ID)` |
| `TypeMarkdown`   | ユーザー添付の `.md` / `.txt` (v0.5.0、ADR-0006)    | `[name](object:ID)` |
| `TypeReport`     | `create-report` 出力                                | `[name](object:ID)` |
| `TypeBlob`       | その他のバイナリ                                    | `[name](object:ID)` (ダウンロード) |

チャットレンダラ (`MessageItem.tsx`、`dialogs/ReportViewer.tsx`、
`App.tsx` の cmd-popup `ReactMarkdown`) は `object:` URL に対して
`img` コンポーネントのみオーバーライドしている:

```tsx
// MessageItem.tsx:79
img: ({src, alt}) => {
    if (src?.startsWith('object:')) {
        const id = src.slice(7)
        return <ObjectImage id={id} alt={alt || ''} onClick={onLightbox} />
    }
    return <img src={src} alt={alt || ''} className="message-image" onClick={...} />
},
```

`urlTransform` は 3 箇所すべてで `object:` を ReactMarkdown の
サニタイザを通過させているが、**`a` コンポーネントオーバーライドは
存在しない**。結果:

1. **死んだリンク。** LLM が `TypeMarkdown` ソースに対して
   `[my-doc.md](object:abc123…)` を出力。ReactMarkdown は
   `<a href="object:abc123…">my-doc.md</a>` を emit。ブラウザは
   `object:` プロトコルを理解しないのでクリックは no-op (Wails /
   WebKit ビルドによっては「開けません」ダイアログ)。ユーザーは
   アンダーラインの効かないリンクを見ることになる。

2. **画像 in リンクの誤レンダー。** 一部の LLM (特に
   `list-objects` を読んだ後) は
   `[![](object:imgID)](object:reportID)` を emit — リンクの中に
   画像。画像は既存の `ObjectImage` オーバーライドで描画されるが、
   外側 `<a>` は死んでいるので画像クリックは無反応 (もしくは
   壊れたブラウザナビゲーション)。

3. **非画像に対する `![alt](object:ID)` の誤入力。** LLM が構文を
   混同して `TypeMarkdown` の ID に `![my-doc.md](object:abc123…)`
   を書く。`ObjectImage` が `Bindings.GetImageDataURL(id)` を呼び、
   `objects.LoadAsDataURL` が `data:text/markdown;base64,…` を返す
   が `<img>` は markdown をレンダーできず、壊れた画像アイコン
   表示。レポート中の他の画像埋め込みは正常だが、これだけ
   「壊れている」状態でユーザーはレポート全体を不安定と感じる。

4. **エクスポートリゾルバの型盲目。** `Bindings.
   resolveObjectRefsForExport` (`bindings.go:1595`) は `(object:`
   を区別なくスキャンし、全マッチに対して `LoadAsDataURL` を呼ぶ。
   `[name](object:ID)` リンクで `ID` が `TypeMarkdown` /
   `TypeReport` の場合、リンクの `href` が
   `data:text/markdown;base64,…` に書き換えられる — `object:` 死に
   リンクよりさらに悪く、外部 markdown リーダーは `data:` URL を
   インラインプレビュー表示しない。エクスポートされた `.md` には
   テキスト参照ごとに数キロバイトのインライン `href` blob が並び、
   外部で開いてもリンクは機能しない。

5. **Drift 面積。** 3 つの React 箇所がそれぞれ `urlTransform` と
   `img` オーバーライドの並行コピーを持つ。上記バグは 3 箇所
   すべてで噛むし、今後 `a` / `code` / `details` などの新しい
   コンポーネントを追加するときも 3 ファイルに同じ変更を入れる
   必要がある。これは v0.6.0 がツール記述子側 (ADR-0007) で
   レジストリリファクタにより解消した parallel-list drift と同じ
   パターン。

ケイパビリティギャップはレンダリング側だけにある。「オブジェクトを
ID で参照する」配管は既にエンドツーエンドで存在: オブジェクト ID は
レコード内に永続化された安定 32-hex 文字列、ドキュメント添付チップ
の `Bindings.GetObjectText` が markdown 本文をクリック時に取得
(`MessageItem.tsx:42 openDocumentAttachment`)、ReportViewer は
任意のソースから `{title, content}` ペアを受け取れる。これと同じ
フローをインラインリンクに繋ぐ。

---

## 2. ゴール

1. **対称的なコンポーネントオーバーライド。** `[name](object:ID)`
   が参照先オブジェクト型に応じてクリック可能なプレビューチップに
   レンダーされる。`![alt](object:ID)` が参照先オブジェクトの MIME
   に応じて画像にレンダーされるのと対称。両形式とも一級市民とする。
2. **markdown defaults の単一ソース化。** `urlTransform` 関数と
   object-aware `img` / `a` コンポーネントオーバーライドを **1 つの
   モジュール** (`frontend/src/markdown/objectMarkdown.tsx`) に集約。
   現行 3 箇所はそこから import する。新しいコンポーネントを 1 箇所
   追加すれば全体に伝播する。
3. **型ミスマッチの優雅な fallback。** ID が非画像の
   `![alt](object:ID)` は `ObjectLink` チップに退化 (壊れた画像
   アイコンにしない)、ID が画像の `[name](object:ID)` はインライン
   `<ObjectImage>` でレンダー (死にリンクにしない)。LLM の意図を
   オブジェクトの実型と突き合わせて正規化する。
4. **型認識エクスポート。** `resolveObjectRefsForExport` は
   `TypeImage` に対してのみ `data:` URL をインライン化する。他の型
   は `object:ID` の href をそのまま残す (アプリ内で再オープン
   可能)、または後続フェーズで `<details>` ブロックに展開。エクス
   ポート済み `.md` に数キロバイトの `data:text/markdown` blob を
   入れない。
5. **ツール / プロンプトの明確化。** `create-report` 記述子と
   エージェントシステムプロンプトが 2 つの参照形式を明示的に並列
   記載する。どちらのソースを読む LLM も 1 回で両規約を学べる。

非ゴール:

- **Transclusion / `{{embed:ID}}` ディレクティブ。** v0.9.0 では
  実装しない。§5.1 参照。ゴール 1–3 の click-to-preview UX が
  ほとんどのケース (provenance + jump-to-source) をカバー; インライン
  展開が本当に必要なら別 ADR。
- **ネストレポートのバックスタック。** Report A の `ReportViewer`
  内から Report B を開くと現在表示が単純に置換される (§3.1.4)。
  スタック / breadcrumb ナビは将来の課題。
- **マルチファイルバンドルエクスポート。** 単一の自己完結
  `.md` がエクスポート形式; 非画像の `data:` URL 挙動を直すだけ。
  フォルダ内の `.md` 兄弟ファイル形式は対象外。
- **クロスセッションオブジェクト可視性の厳格化。** `Bindings.
  GetObjectText` が既に強制する可視性モデルをそのまま再利用; 本 ADR
  はアクティブセッションが見られるオブジェクトの範囲を変更しない。

---

## 3. 設計

### 3.1 フロントエンド

#### 3.1.1 新モジュール: `frontend/src/markdown/objectMarkdown.tsx`

`object:` 認識 ReactMarkdown 配線の単一ソース。エクスポート:

```ts
// 許可プロトコル + object: passthrough。現在の 3 つの urlTransform
// コピーのドロップイン置換。
export function urlTransform(url: string): string

// ファクトリ: ReactMarkdown の `components` マップを object: 認識
// img / a オーバーライド付きで返す。呼び出し側が所有するコール
// バック (lightbox / report-viewer) を渡す。
export function objectComponents(opts: {
    onLightbox: (src: string) => void;
    onExpandReport: (r: {title: string; content: string}) => void;
}): Components
```

内部的に `objectComponents` は以下を返す:

```ts
{
    img: ({src, alt}) => /* 3.1.2 参照 */,
    a:   ({href, children}) => /* 3.1.3 参照 */,
}
```

両オーバーライドは共有 `useObjectMeta(id)` フック (こちらも
エクスポート、`ObjectLink` も使用) に委譲。フックは ID ごとに
`GetObjectMeta(id)` を 1 回フェッチし、`ObjectImage.tsx:4` の既存
`objectCache` (データ URL 用) と同じパターンのインメモリ
キャッシュで重複排除。Meta キャッシュとデータ URL キャッシュは
ID をキーとする兄弟マップ; セッション切替時に呼ばれる
`clearObjectCache` で両方クリア。

#### 3.1.2 `img` オーバーライドの挙動

`src` が `object:` で始まる `img` — ID を抽出し meta フェッチ:

| オブジェクト型               | レンダー                                                   |
|------------------------------|-----------------------------------------------------------|
| `TypeImage`                  | 既存の `<ObjectImage>` (データ URL → `<img>` + lightbox)  |
| `TypeMarkdown` / `TypeReport`| `<ObjectLink>` チップ (LLM が誤入力 — 退化)              |
| `TypeBlob`                   | `<ObjectLink>` チップ (ダウンロード)                      |
| meta フェッチ失敗            | 「Object {id} not found」バッジ — 既存のエラー span      |

`TypeImage` ハッピーパスは現行と同一; ミスマッチ fallback だけが新規。

#### 3.1.3 `a` オーバーライドの挙動

`href` が `object:` で始まる `a` — ID を抽出し meta フェッチ:

| オブジェクト型 | レンダー                                                                                              |
|----------------|-------------------------------------------------------------------------------------------------------|
| `TypeImage`    | `<ObjectImage>` インライン (LLM の誤入力意図を尊重; `![…](object:…)` と同じ lightbox affordance)   |
| `TypeMarkdown` | `📝` アイコン付き `<ObjectLink>` チップ — クリック → `openDocumentAttachment` 相当 → `ReportViewer` |
| `TypeReport`   | `📄` アイコン付き `<ObjectLink>` チップ — クリック → レポート格納コンテンツで `ReportViewer`         |
| `TypeBlob`     | `📎` アイコン付き `<ObjectLink>` チップ — クリック → 既存の `Bindings.ExportObject(id)` (保存ダイアログ) |
| meta 不在      | muted 状態の `<ObjectLink>` チップ、title「missing object」、クリック no-op                          |

`object:` 以外の `href` は通常の
`<a target="_blank" rel="noreferrer">` にフォールバック。

#### 3.1.4 新コンポーネント: `frontend/src/ObjectLink.tsx`

`ObjectImage` の兄弟。視覚的チップとクリックハンドラディスパッチ
を担う。Props:

```ts
interface Props {
    id: string;
    children: React.ReactNode;       // LLM が書いたリンクテキスト
    onExpandReport: (r: {title: string; content: string}) => void;
    onLightbox: (src: string) => void;
}
```

内部で `useObjectMeta(id)` を呼ぶ:
- **ロード中**: muted チップ「Loading…」 (`ObjectImage` skeleton と
  同じ)。
- **エラー / 不在**: muted チップ、title「Object {id} not found」、
  クリック no-op。
- **解決済み**: アイコン + `children` (LLM 提供ラベル、なければ
  `meta.OrigName`、それも無ければ `meta.ID[:8]` にフォールバック)。
  クリックでディスパッチ:
  - markdown / report: `Bindings.GetObjectText(id)` を呼び、最初の
    見出しを title にパース (`MessageItem.openDocumentAttachment:72`
    の正規表現を再利用)、`onExpandReport({title, content})` を発火。
  - image: `Bindings.GetImageDataURL(id)` を呼び `onLightbox(dataURL)`
    を発火。
  - blob: `Bindings.ExportObject(id)` を呼ぶ (既存の保存ダイアログ
    フロー)。

ネストケース: `ReportViewer` の中から `ObjectLink` をクリックすると
`App` が既に接続している**同じ** `onExpandReport` を呼ぶ。既存の
`ReportViewer` は `App.tsx` ルートツリーにマウントされ、現在の
`expandedReport` state を表示するので、ネストクリックは表示中の
レポートを単純に置換する。これが意図された v0.9.0 挙動;
バックスタックはユーザー要望が出たら別 UX ADR で。

#### 3.1.5 現行 3 つの `ReactMarkdown` 箇所のマイグレーション

各箇所のインライン `urlTransform` + `components` prop を import に
置換:

```diff
- import ReactMarkdown, {defaultUrlTransform} from 'react-markdown'
- function urlTransform(url) { … }
- <ReactMarkdown urlTransform={urlTransform} components={{ img: … }}>
+ import ReactMarkdown from 'react-markdown'
+ import {urlTransform, objectComponents} from './markdown/objectMarkdown'
+ const components = useMemo(
+   () => objectComponents({onLightbox, onExpandReport}),
+   [onLightbox, onExpandReport],
+ )
+ <ReactMarkdown urlTransform={urlTransform} components={components}>
```

変更対象: `MessageItem.tsx` (チャットバブル)、`ReportViewer.tsx`
(フルスクリーン overlay)、`App.tsx` (cmd-popup)。この変更後は、
将来 `code` / `details` オーバーライドを追加するときに
`objectMarkdown.tsx` を 1 箇所編集するだけで済む。

### 3.2 バックエンド

#### 3.2.1 新バインディング: `Bindings.GetObjectMeta(id) ObjectInfo`

`bindings.go` には既に `ObjectInfo` (`bindings.go:1327`) と
リスト系の `ListObjects` / `GetSessionObjects` がある。フロント
エンドにはポイントルックアップが無く、1 ID を引くために
`ListObjects` を O(N) で走査することになる。自然な兄弟を追加:

```go
// GetObjectMeta は単一オブジェクトのメタデータを返す。ID が不明
// ならエラー。フロントエンド ObjectLink コンポーネントが object:ID
// 参照をどうレンダーするか (インライン画像 / ドキュメントチップ /
// ダウンロードチップ) を決めるために使う。
func (b *Bindings) GetObjectMeta(id string) (ObjectInfo, error) {
    m, ok := b.objects.Get(id)
    if !ok {
        return ObjectInfo{}, fmt.Errorf("object %s not found", id)
    }
    return ObjectInfo{ /* ListObjects と同じフィールドマッピング */ }, nil
}
```

新しいエラーパスなし; `b.objects.Get` は既存で正準ルックアップ。
テスト: ハッピーパスのユニットテスト (`bindings_test.go` は既に
`GetObjectText` を同様にカバー) と not-found ケース。

#### 3.2.2 型認識 `resolveObjectRefsForExport`

現行実装 (`bindings.go:1595`) は `(object:ID)` マッチごとに
`LoadAsDataURL` を呼ぶ。以下に置換:

```go
func (b *Bindings) resolveObjectRefsForExport(content string) string {
    if b.objects == nil || !strings.Contains(content, "object:") {
        return content
    }
    // 前方に走査; TypeImage のオブジェクトに対するマッチだけを
    // 書き換える (自己完結 .md がインライン化できる唯一の型)。
    result := content
    pos := 0
    for {
        idx := strings.Index(result[pos:], "(object:")
        if idx < 0 {
            break
        }
        absIdx := pos + idx
        end := strings.Index(result[absIdx:], ")")
        if end < 0 {
            break
        }
        id := result[absIdx+8 : absIdx+end]
        meta, ok := b.objects.Get(id)
        if !ok {
            // 不明 ID → マークしてスキップ (無限ループ回避)。
            result = result[:absIdx] + "(missing-object:" + result[absIdx+8:]
            pos = absIdx + len("(missing-object:") + len(id) + 1
            continue
        }
        if meta.Type != objstore.TypeImage {
            // 非画像: object: href をそのまま残す。エクスポート
            // されたファイルはアプリに再インポートしたとき再オープン
            // 可能; 外部リーダーで未レンダーリンクとして見えるのは
            // 内部 URL が見えないのと変わらない。
            pos = absIdx + end + 1
            continue
        }
        du, err := b.objects.LoadAsDataURL(id)
        if err != nil || du == "" {
            pos = absIdx + end + 1
            continue
        }
        result = result[:absIdx+1] + du + result[absIdx+end:]
        pos = absIdx + 1 + len(du) + 1
    }
    return result
}
```

主要変更: `b.objects.Get(id)` の型チェックで非画像なら
`LoadAsDataURL` 呼び出しを短絡。前方に進む `pos` カーソルは
従来の「ゼロから再スタート」ループの置換 — 非画像はスライドを
変えないので、無限ループを避けるため進める必要がある。

テスト (`bindings_test.go`):
- 画像のみコンテンツ: v0.8.0 と同じ出力 (`(object:imageID)` →
  `(data:image/...)` 書き換えの regression guard)。
- 混在コンテンツ: 画像参照は書き換え、`[name](object:markdownID)`
  href はそのまま、`[name](object:reportID)` href もそのまま。
- 不明 ID: `missing-object:` に書き換え (現行通り)、連続する
  複数の不明 ID でカーソルが正しく進む (無限ループなし)。
- Blob 参照: href はそのまま。

### 3.3 ツール記述子更新

`internal/agent/tool_descriptors_analysis.go:88` (create-report
Description) の現行テキストに 1 文を追加:

> "Reference images with `![alt](object:ID)`. **Reference other
> documents (markdown / reports) with `[title](object:ID)` — they
> render as clickable preview chips that open the linked content
> in the report viewer.**"

他の記述子は編集不要 — 3 つのテキストツール (`analyze-text` /
`grep-text` / `get-text`) は既に `object:<id>` を ID パラメータと
してドキュメント化済み (`tool_descriptors_analysis.go:119,148,185`)、
そのパラメータ形式は本 ADR が導入するチャットレンダリング markdown
形式とは無関係。

### 3.4 システムプロンプト更新

`internal/agent/agent.go:2242-2246` の「To reference objects」
ブロックを拡張。現行テキスト:

```
To reference objects from the session:
1. For images attached in the current message: read the anchor immediately preceding each image
2. For other objects (older images, reports, files): use the list-objects tool to discover available objects, then get-object to retrieve them
3. In reports, reference images with: ![description](object:ID)
Never fabricate image URLs or object IDs.
```

項目 3 を 2 項目に置換:

```
3. In reports, reference images with: ![description](object:ID)
4. In reports, reference other documents (markdown / reports) with: [title](object:ID) — the renderer turns this into a clickable preview chip that opens the linked content
```

加えて、agent.go:2240 のプロンプトは既に
`Image (object ID: ...):` が**入力**アンカーで LLM 自身の出力では
emit してはいけないと明記している。そのパラグラフを 1 文拡張して
ドキュメント版もカバー:

```
The same rule applies to "Document (object ID: ..., name: ..., Nk tokens):" lines that prefix user-attached markdown / text — that anchor is also INPUT-only. To cite a document in your reply or in a report, use the markdown form [title](object:ID); do not paste the anchor line verbatim.
```

理由: 項目 1–2 は発見と取得をカバー; 項目 3–4 が正しくレンダー
される 2 つのインライン参照構文をカバーするようになる。拡張した
アンカールールの文は、画像向けに長年存在した規約と同じ形で
ドキュメント向けの入力 vs 出力の曖昧さを閉じる。「捏造禁止」
ガードレールは末尾でそのまま。

他箇所のプロンプト編集なし。agent.go:2248-2253 の「TypeReport vs
TypeMarkdown」ブロックは既に両型を説明しており、参照構文を
繰り返す必要はない。

### 3.5 Document anchor 規約の明文化

**既存の暗黙ルールを成文化するもので挙動変更ではない。**
`Document (object ID: …):` アンカー (v0.5.0、ADR-0006) は
`PrependDocumentAnchors` (`internal/llm/backend.go:152-182`) が
生成し、`internal/contextbuild/render.go:85` で 1 つのガード下に
適用される:

```go
if r.Role == "user" && len(r.DocumentIDs) > 0 && opts.ObjectLookup != nil {
    content = llm.PrependDocumentAnchors(content, r.DocumentIDs, opts.ObjectLookup)
}
```

つまりアンカーは**ユーザー添付ドキュメント** (`Record.DocumentIDs`
が populate される — ユーザーが `.md` / `.txt` をチャットに
ドロップしたとき) のみに対して発火する。`toolCreateReport` で
生成されたレポートは後続ターンでドキュメントアンカーを **得ない**:

- レポート全文は既にレコードに `Role: "report"` 行としてインライン
  化されている (`tools.go:374-382 pendingReport` → agent ループが
  `AddToolResult` の後に append)。LLM はコンテンツを直接見られる;
  「代わりに立てる」必要がない。
- アンカーは LLM がメッセージテキスト内で見られないコンテンツの
  **代理**である (ユーザー添付はインライン化されない — LLM は
  `analyze-text` / `grep-text` / `get-text` でオンデマンドに読む)。
  レポートはこのカテゴリに含まれない。

したがってユーザー添付 MD と LLM 生成レポートの非対称性は**意図
されており正しい**; 歴史的な混乱 (「自分が作ったレポートはなぜ
アンカーで出てこない?」) は、本 ADR が追加する
`[title](object:ID)` レンダリングの不在から来ていた。新しい
レンダーパスが整えば、新レポートの中から過去レポートを参照する
正準形式は `[title](object:ID)` で、捏造アンカー行ではない。

承認し (同じ PR の `reference/architecture.md` に新セクション
「Object reference conventions」として記載する) ルール:

1. **`Image (object ID: ID):`** — ユーザー添付画像の入力専用
   アンカー。LLM は決して emit してはいけない。画像を引用するには
   `![alt](object:ID)` を使う。
2. **`Document (object ID: ID, name: …, N tokens):`** — ユーザー
   添付 markdown / テキストの入力専用アンカー。LLM は決して emit
   してはいけない。ドキュメントを引用するには `[title](object:ID)`
   を使う。
3. **`![alt](object:ID)`** — インライン画像用 LLM 出力。画像と
   してレンダーされる (もし ID が非画像に解決される場合は
   ドキュメント / blob チップとして退化 — §3.1.2 fallback)。
4. **`[title](object:ID)`** — インラインドキュメント / レポート /
   blob 参照用 LLM 出力。クリック可能なチップとしてレンダー
   (§3.1.3)。チャット返信内でもレポート内でも許容; チップ挙動は
   両コンテキストで同一。
5. **レポートに遡って anchor を付与しない。** どのコードパスも
   `Role: "report"` レコードに `DocumentIDs` を追加しないし、
   今後も追加すべきでない — 代理アンカー機構は LLM が見られない
   コンテンツのためのもので、レポートは常にインライン可視。

将来、現在のコンテキストウィンドウから (要約により) 圧縮で抜けた
過去レポートに新レポートを基づかせたいユースケースが出た場合、
正しい機構は **ドキュメント添付プロモーション**: `object:ID` を
取り、次のユーザーメッセージの `DocumentIDs` に追加する将来ツール
を作り、既存レンダーパス経由でアンカーが自然に発火するように
する。これは別 ADR (長セッションでのコンテキストウィンドウ圧迫と
紐づく見込み); v0.9.0 では扱わない。

### 3.6 ツール記述子クロスリンク監査

3 つのテキストツール記述子は `object` パラメータに「`object:<id>`
or bare ID」と既に書いてある
(`tool_descriptors_analysis.go:119,148,185`)。ここは変更不要 —
**入力**パラメータ構文は本 ADR が導入する**出力** markdown 形式と
無関係。

監査項目 1 つ: `list-objects` 記述子はツール結果に ID を返す。LLM
は今後、後続レポートで `[name](object:ID)` 参照に変換しうる。
これは意図された新挙動 (provenance UX の改善)。「memory feedback:
tool result internal IDs」の注意は **チャット返信** コンテキストで
は引き続き有効 (ユーザーが頼んでない ID リンクを volunteer しない);
**レポート内**参照には適用されない、ソース引用が目的そのもの。
記述子編集は不要 — 区別は §3.4 のシステムプロンプト項目 3–4 で
表現される。

### 3.7 挙動マトリクス (レンダー出力)

本 ADR 後の最終状態。**W** = LLM が書く; **R** = ユーザーが見る
レンダリング結果。

| W                          | オブジェクト型 | R (v0.8.0 — 現行)                            | R (v0.9.0 — 本 ADR 後)                       |
|----------------------------|---------------|----------------------------------------------|----------------------------------------------|
| `![alt](object:ID)`        | image         | インライン画像 + lightbox クリック ✓        | インライン画像 + lightbox クリック (現行通り) |
| `![alt](object:ID)`        | markdown/report | 壊れた画像アイコン ✗                       | ドキュメントチップ → ReportViewer で開く     |
| `![alt](object:ID)`        | blob          | 壊れた画像アイコン ✗                         | ダウンロードチップ                            |
| `![alt](object:ID)`        | missing       | 壊れた画像アイコン ✗                         | 「Object not found」バッジ (既存)             |
| `[title](object:ID)`       | image         | 死んだリンク ✗                               | インライン画像 (LLM 誤入力意図を尊重)         |
| `[title](object:ID)`       | markdown/report | 死んだリンク ✗                             | ドキュメントチップ → ReportViewer で開く ✓    |
| `[title](object:ID)`       | blob          | 死んだリンク ✗                               | ダウンロードチップ → 保存ダイアログ ✓         |
| `[title](object:ID)`       | missing       | 死んだリンク ✗                               | muted チップ「missing object」、クリック no-op |
| `[![](object:imgID)](object:reportID)` | 混在 | 画像は描画、外側リンク死ぬ ⚠️           | 画像描画、外側リンク → ReportViewer ✓         |

エクスポート (`SaveReport` / `ExportObject`):

| 格納レポート内 W            | オブジェクト型 | エクスポート `.md` (v0.8.0)                       | エクスポート `.md` (v0.9.0)                            |
|-----------------------------|---------------|--------------------------------------------------|------------------------------------------------------|
| `![alt](object:ID)`         | image         | `![alt](data:image/...;base64,…)` ✓              | 現行通り ✓                                            |
| `[title](object:ID)`        | markdown      | `[title](data:text/markdown;base64,…)` ✗ (壊れ) | `[title](object:ID)` — そのまま残す                 |
| `[title](object:ID)`        | report        | `[title](data:text/markdown;base64,…)` ✗         | `[title](object:ID)` — そのまま残す                 |
| `[title](object:ID)`        | blob          | `[title](data:application/octet-stream;base64,…)` ✗ | `[title](object:ID)` — そのまま残す               |
| `[title](object:ID)` × N    | 不明複数      | 1 つでも不明だと後続マッチが無限ループ ⚠️       | 前方走査カーソル、ループ無し ✓                       |

---

## 4. エッジケース

1. **LLM がオブジェクト型と合わない インライン形式を書く。**
   §3.1.2 / §3.1.3 のレンダリング fallback でカバー。レンダラが
   意図をオブジェクト型に正規化 — 壊れた画像アイコンも死にリンクも
   出さず、LLM の特別訓練も不要。

2. **Meta フェッチ往復レイテンシ。** 各 `ObjectLink` /
   `ObjectImage` が `GetObjectMeta` (クリックで `GetObjectText`
   等) をトリガする。Wails IPC はプロセス内; 1 呼び出しは最大でも
   数百マイクロ秒。`useObjectMeta` キャッシュ (§3.1.1) はセッション
   内で重複排除するので、同じオブジェクトを 50 回参照する長レポート
   でも 1 回しか課金されない。セッション切替で
   `clearObjectCache` が既に走る (`App.tsx` セッション変更 effect);
   meta キャッシュ向けの兄弟 clear を追加。

3. **レポート内ネストレポート。** §3.1.4: `ReportViewer` 内の
   リンククリックは、外側チャットペインが配線したのと同じ
   `onExpandReport` を呼ぶ; `App.tsx` の `expandedReport` state が
   置換され、`ReportViewer` が新しい `{title, content}` で再描画。
   バックスタックなし。ユーザーが深い連鎖で迷ったらチャットから
   オリジナルを再オープン。`reference/architecture.md` 内
   「Report navigation」セクションに記載。

4. **サイクル。** Report A が Report B にリンク、B が A にリンク。
   置換のみのナビゲーション (自動再帰なし、クリックはユーザー
   アクション) ではサイクルはランタイム的に問題にならない。エクス
   ポートリゾルバは非画像参照を follow しない (§3.2.2) ので、ここでも
   再帰なし。

5. **レポートがまだ参照している間にオブジェクトが削除される。**
   チップは muted「missing object」状態で描画 (§3.1.4 エラー分岐)。
   レポート markdown はディスク上で不変; 参照が解決しないだけ。
   `ObjectImage` が現在欠落画像を扱うのと整合。

6. **クロスセッション参照。** `Bindings.GetObjectText` /
   `GetImageDataURL` / `GetObjectMeta` はすべて `b.objects.Get(id)`
   を使う — グローバルルックアップ。`b.objects` はシングルトン
   オブジェクトリポジトリでセッション per ではない。v0.8.0 の
   可視性ルール (一度格納されたオブジェクトは存在) がそのまま; 本
   ADR は狭めも広げもしない。セッション間オブジェクト参照は今日
   既に可能; 新挙動も対称。

7. **プライベートセッション相互作用。** プライベートセッション
   (v0.3.0 privacy controls) は既に自分で作っていないオブジェクトを
   list-objects ツール結果から除外するが、下層 `Bindings.Get*`
   呼び出しはそのフィルタを強制しない。本 ADR は `GetObjectText` /
   `GetImageDataURL` の現行挙動を変えない; あるセッションのレポート
   が別セッションで export/import 経由で開かれ、インポート側に
   ない ID を埋め込んでいたらチップは「missing object」を表示 —
   既存の fallback 挙動。

8. **`object:` ID のツール結果 emission。** memory feedback「tool
   result internal IDs」は、LLM が冗長リンクを書くようプロンプト
   する ID をツール結果が露出しないよう注意していた。そのガイダンス
   は引き続き有効; 本 ADR はツール結果形状を変更しない。
   `create-report` は既に意図的にレポート自身の ID を結果から省く
   (`tools.go:384-389`) — レポートは即座にユーザーに表示される
   ので正しい判断。新しい `[title](object:ID)` 形式はレポート
   コンテンツの中から**他**のオブジェクトを参照するためのもので、
   チャット返信で LLM がレポートを self-link するためではない。

9. **`[name](object:ID)` を含む markdown テーブルセル。** テーブル
   外と同じ挙動 — ReactMarkdown は `<table>` 内のアンカー要素にも
   同じ `components.a` オーバーライドを適用。remark-gfm の標準
   パイプラインで検証済み。チップレイアウトは `display:
   inline-flex` なのでセルは正常に流れる。

10. **`[name](object:ID)` を含むコードフェンス。** コードフェンスは
    markdown リンクパースを抑制; リテラルテキストがそのまま現れる。
    特別処理不要 — これが望ましい挙動 (ユーザーは構文を*見せたい*、
    invoke したいわけではない)。

11. **LLM が誤って出力に `Document (object ID: …):` アンカーを
    echo する。** §3.4 のシステムプロンプト編集が明示的に禁止する
    — 古くから存在する `Image (object ID: …):` アンカー echo 禁止
    (agent.go:2240) と対称。仮に LLM が emit しても (モデル drift か
    古いコンテキスト) リテラルテキストとしてレンダー — 読めるが
    見た目が悪いだけ、機能的破綻なし。ランタイムガードは置かず、
    画像アンカーと同じくプロンプト規律に依存。

12. **古いレポート向けの `DocumentIDs` 遡及 backfill。** 対象外
    (§3.5 ルール 5)。`Role: "report"` レコードに `DocumentIDs` を
    追加するコードパスは作らない。古いレポートは引き続き
    `role="report"` 行としてレコードにインライン化されたまま; 今後
    LLM が引用するときは*新*レポート内の `[title](object:ID)`
    参照で。

---

## 5. 却下した代替案

### 5.1 Transclusion ディレクティブ `{{embed:object:ID}}`

検討: レポートレンダラ (または `toolCreateReport` 前処理) が
インライン展開する新構文。却下理由:

- click-to-preview UX (ゴール 1–3) が共通ケース (provenance +
  jump-to-source) をカバーする; ユーザーが欲しいのは大量インライン
  展開ではない。実レポートは 5 ソース引用; 5 つの長文を
  インライン展開するとレポート自体の narrative が埋もれる。
- 再帰 / サイクル / サイズ爆発のハンドリングが非自明な複雑さを
  追加する (`tools.go` に深さ制限、サイクル検出、コンテンツ
  バジェット上限が必要)。実証されていないニーズには時期尚早。
- 新構文は後方互換性の負債 — 標準 markdown で訓練された LLM は
  自然には emit しないので、システムプロンプトで教える必要があり
  (永続的にプロンプトトークンを食う)。
- 後にリテラルインライン展開が必要なら、正しい形は**レポート
  前処理**: `toolCreateReport` でオプトインディレクティブをパース
  して置換する。これは実ユースケースドライバとともに専用 ADR で
  扱うべきで、ここではない。

### 5.2 「非画像オブジェクトは常にインライン」スマートレンダリング

検討: ユーザーが markdown オブジェクトに対する `[name](object:ID)`
を見たとき、デフォルトでコンテンツをインライン展開。却下: インライン
展開は全レポートを 1MB のコンテンツに変える; click-to-preview チップ
はレポートの視覚構造を保ちつつソースから 1 クリック。5.1 と同じ
トレードオフ。

### 5.3 リダイレクトで `object:` href をクライアント側解決

検討: `object:` href ナビゲーションをキャッチして Wails バインディング
にルートする service worker / interceptor を登録。却下: Wails の
webview embed は macOS でクリーンなプロトコルハンドラフックを公開
しておらず、実装は dev (vite) と build (packaged) ターゲットで分岐
する。コンポーネントオーバーライドアプローチは React-native な
解; プロトコル配管不要で dev と build で同一に動作。

### 5.4 エクスポートで `object:` href を残す代わりにコンテンツインライン

§3.2.2 向けに検討: `.md` エクスポート時に `[name](object:reportID)`
を `<details><summary>name</summary>…content…</details>` ブロックに
置換。v0.9.0 で却下:

- プレゼンテーション (折りたたみブロック) とリンク (URL) を混ぜる。
  外部リーダーの中には `<details>` をレンダーしないものもある。
- 再帰展開 (レポート → レポート) は 5.1 と同じサイクル / 深さ
  ハンドリングが必要。
- `object:` href はクリーンな再インポート挙動を持つ: エクスポート
  された `.md` を shell-agent に再インポート (ADR-0001 の
  `.shellagent` バンドル経由)、または将来「import markdown」
  パス経由で、リンクは再び解決する。
- 需要が出たら将来 ADR でリッチなエクスポートモードを追加可能。
  defer。

### 5.5 `img` と `a` を単一コンポーネントで扱う

検討: `ObjectImage` と `ObjectLink` を 1 つの `ObjectReference` に
畳み込み、型でインラインレンダリングを決定。却下: 2 コンポーネント
は意味のある異なるレンダー出力 (`<img>` vs チップ)、prop 形状
(`alt` vs `children`)、クリックハンドラ (`onLightbox` vs
`onExpandReport` + `onLightbox`) を持つ。1 つのフック
(`useObjectMeta`) 共有で十分; コンポーネント共有は内部 switch が
両コードパスを曖昧にする。

---

## 6. テスト / 不変条件

### 6.1 フロントエンド (`vitest` 導入なら; 無ければコンポーネントレベルスモーク)

- `ObjectLink` が型ごとに期待アイコンをレンダー (`GetObjectMeta`
  を 4 つの `ObjectType` でモック)。
- `TypeMarkdown` クリックで `GetObjectText` から取得したコンテンツ
  と共に `onExpandReport` 呼び出し; 最初の行 `# ` 除去が title。
- `TypeImage` クリックで `GetImageDataURL` から取得したデータ URL
  と共に `onLightbox` 呼び出し。
- `TypeBlob` クリックで `Bindings.ExportObject(id)` 呼び出し。
- オブジェクト不在: チップ muted、クリック no-op (コールバック発火
  なし)。
- `objectComponents` factory: `urlTransform` が `object:` を許可;
  `defaultUrlTransform` は例えば `javascript:` をストリップ
  (ReactMarkdown から継承するサニタイゼーション挙動の regression
  guard)。

### 6.2 バックエンド (`bindings_test.go`)

- `GetObjectMeta` ハッピーパス: 既知 ID が `Type` / `Lines` /
  `Tokens` を正しく populate した `ObjectInfo` を返す。
- `GetObjectMeta` not found: `(ObjectInfo{}, error)` を返す。
- `resolveObjectRefsForExport`:
  - 画像のみレポート → 全 `(object:imgID)` が
    `(data:image/...;base64,…)` に書き換え (v0.8.0 挙動の
    regression)。
  - 混在レポート → 画像参照は書き換え、markdown / report / blob
    href は `(object:ID)` のまま。
  - 連続 2 つの不明 ID → 両方 `(missing-object:ID)`; 無限ループなし、
    前方カーソルが各々を越えて進む。
  - `object:` 部分文字列なし → 同一出力 (early-return ガード)。

### 6.3 構造的

- `objectMarkdown.tsx` が `react-markdown` から
  `defaultUrlTransform` を import する唯一のファイル
  (lint / grep ベース不変条件テスト)。4 つ目の箇所がオーバーライドを
  複製したらマージ前にテストが catch。
- `bindings.go` のエクスポートリゾルバ内で `LoadAsDataURL` の呼び
  出しがちょうど 1 つ (型認識 switch)。grep テスト不変条件。

---

## 7. 互換性

破壊的変更なし。

- **既存格納レポート** (objstore 内の `TypeReport` コンテンツ):
  ディスク上で不変。`![alt](object:imgID)` 参照は同一レンダー;
  `[name](object:ID)` 参照 (もしあれば) は死にリンクの代わりに
  プレビューチップにレンダー — 純粋な改善。
- **古いバージョンからエクスポートされた `.md` ファイル** で
  `[name](data:text/markdown;base64,…)` (v0.8.0 の壊れた出力) を
  含むもの: 本変更の影響なし (古い export を再処理しない)。今後の
  export は綺麗。
- **ツール表面**: `create-report` description が 1 文増える; 既存
  スキーマ・params・結果形状は不変。
- **システムプロンプト**: `[title](object:ID)` についての箇条書き
  1 行追加; 既存指示は不変。
- **`Bindings.GetImageDataURL` / `GetObjectText` / `ExportObject`**:
  シグネチャと semantics は不変。既存 `ObjectImage` /
  `openDocumentAttachment` パスと新規 `ObjectLink` パス両方が使用。
- **`Bindings.GetObjectMeta`**: 新規追加。Wails は次ビルドで
  `Bindings.d.ts` typings を再生成。

マイグレーションスクリプトなし。スキーマ変更なし。バンドル形式
変更なし (`.shellagent` は objstore を変更なしで運ぶ)。

---

## 8. フェージング

単一 PR。各部分は密結合 (レンダリング変更は `GetObjectMeta`
バインディングを必要とし、プロンプト / 記述子編集はレンダリング
変更なしでは無意味)。PR 内コミット:

1. `feat(objstore): GetObjectMeta binding + tests`
2. `refactor(frontend): centralise object-aware markdown defaults
   in objectMarkdown.tsx; migrate MessageItem / ReportViewer /
   App.tsx cmd-popup to import`
3. `feat(frontend): ObjectLink component + a-component override`
4. `fix(export): type-aware resolveObjectRefsForExport (image
   only) + forward-walking cursor`
5. `feat(agent): create-report descriptor mentions
   [title](object:ID); system prompt gains item 4 and extended
   anchor-rule sentence (Document anchor input-only rule をカバー)`
6. `docs: ADR-0014 + CHANGELOG entry + INDEX.md / INDEX.ja.md
   update + reference/architecture.md "Object reference
   conventions" section codifying §3.5 rules 1–5`
7. `test: bindings_test for GetObjectMeta + resolveObjectRefs
   matrix; frontend smoke for ObjectLink; grep-invariant test
   that objectMarkdown.tsx is the only defaultUrlTransform
   importer`

`docs-mirror-check.sh` が EN/JA mirror を従来通り強制。

---

## 9. 範囲外 (必要なら後続 ADR で)

- Transclusion ディレクティブ `{{embed:object:ID}}` (§5.1)。
- 参照ドキュメントごとに別 `.md` を持つマルチファイルエクスポート
  バンドル (§5.4)。
- ネスト連鎖向け `ReportViewer` 内バックスタックナビゲーション
  (§4.3)。
- プライベートセッションの型別可視性厳格化 (§4.7)。
- `<details>` / `<sub>` 等のレンダラサポート — object-link レンダ
  リングとは別; 現行「raw HTML 不可」ルールは継続。

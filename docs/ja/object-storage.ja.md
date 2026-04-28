# セントラルオブジェクトストレージ — 設計ドキュメント

> 作成日: 2026-04-26
> ステータス: Draft
> 関連: [エージェントデータフロー](agent-data-flow.ja.md) セクション 5

## 1. 目的

セントラルオブジェクトストレージ (objstore) は、エージェントセッション中に
生成または消費されるすべてのバイナリ・構造化成果物の単一リポジトリを提供する。
オブジェクトは 12 文字の 16 進 ID でシステム全体を通じて参照される —
セッションレコード、LLM コンテキスト、Markdown コンテンツ、フロントエンド表示。

### 1.1 オブジェクトとは

以下の条件を満たす個別の成果物:
- ディスク上にバイナリまたはテキスト表現を持つ
- セッションレコード間で参照される必要がある
- ユーザが表示、ダウンロード、埋め込み可能

例: ユーザアップロード画像、ツール生成チャート、分析結果エクスポート、
レポート文書、シェルツール出力ファイル。

### 1.2 一元化の理由

v1 では画像が data URL としてセッションレコードに直接保存されていた:
- セッション JSON の肥大化 (画像 1 枚 = 100KB+ の base64)
- 同一画像の複数参照時に重複保存
- LLM が識別子で画像を参照する手段がない
- レポート画像がインライン表示不可 (安定した参照がない)

v2 ではレコードに ID のみ保存。バイナリデータは objstore に置く。

## 2. オブジェクトモデル

### 2.1 ObjectMeta

```go
type ObjectMeta struct {
    ID        string     `json:"id"`
    Type      ObjectType `json:"type"`       // image, blob, report
    MimeType  string     `json:"mime_type"`
    Filename  string     `json:"filename"`
    CreatedAt time.Time  `json:"created_at"`
    SessionID string     `json:"session_id,omitempty"`
    Size      int64      `json:"size"`
}
```

### 2.2 オブジェクトタイプ

| タイプ | 定数 | 生成元 | 例 |
|--------|------|--------|-----|
| Image | `TypeImage` | ユーザアップロード、ツール出力 | PNG, JPG, WebP |
| Blob | `TypeBlob` | シェルツール成果物 | CSV, JSON, テキスト |
| Report | `TypeReport` | create-report ツール | Markdown 文書 |

### 2.3 ID 生成

- 12 文字の 16 進 (6 ランダムバイト, `crypto/rand`)
- 衝突リスク: 約 281 兆分の 1
- プレフィックスなし — タイプはメタデータに保存

## 3. ストレージレイアウト

```
~/Library/Application Support/shell-agent-v2/
├── objects/
│   ├── index.json            # ObjectMeta 配列 (権威ソース)
│   └── data/                 # バイナリファイル
│       ├── a1b2c3d4e5f6.png  # ID + MIME 由来の拡張子
│       ├── f6e5d4c3b2a1.md
│       └── ...
├── sessions/
│   └── {session-id}/
│       ├── chat.json          # レコードは ID でオブジェクト参照
│       └── analysis.duckdb
├── pinned.json
├── findings.json
└── config.json
```

### 3.1 インデックスファイル

`index.json` は ObjectMeta の JSON 配列。メタデータの権威ソース。

- 全変更操作 (Save, Delete) 後に書き込み
- 起動時にロード
- index.json が欠損/破損の場合、ファイルシステムからリビルド
- `sync.RWMutex` によるスレッドセーフ

## 4. スコープ: グローバル vs セッション

### 4.1 設計判断: セッションアフィニティ付きグローバル

オブジェクトは **単一グローバルリポジトリ** に保存 (セッションごとのディレクトリではない)。

理由:
- セッション間オブジェクト共有 (Findings が他セッションの画像を参照可能)
- シンプルな実装 (アプリ全体で 1 つの Store インスタンス)
- セッション間移動時のオブジェクト移行不要

### 4.2 セッションアフィニティ

`ObjectMeta.SessionID` でオブジェクトの生成元セッションを追跡。

用途:
- **セッション削除時のクリーンアップ**: そのセッションが作成したオブジェクトを削除
- **セッション別一覧**: LLM ツールで関連オブジェクトのみ表示
- **将来**: セッション別ストレージクォータ

v1 では SessionID が定義されているが未使用だった。v2 では**全 Save で SessionID を設定する**。

## 5. ライフサイクル

### 5.1 作成

```
ソース                  → 保存メソッド        → 保存タイプ
────────────────────────────────────────────────────
ユーザ画像アップロード   → SaveDataURL()      → TypeImage
シェルツール成果物       → Save()             → TypeBlob
create-report 出力      → Save()             → TypeReport
query-sql 結果エクスポート → Save()           → TypeBlob
```

### 5.2 参照

| 場所 | 形式 | 例 |
|------|------|-----|
| セッションレコード | `ObjectIDs []string` | `["a1b2c3d4e5f6"]` |
| LLM コンテキスト | テキストマーカー | `[Image ID: a1b2c3d4e5f6]` |
| Markdown | URL スキーム | `![説明](object:a1b2c3d4e5f6)` |
| ツール結果 | 成果物マーカー | `[Artifacts: a1b2c3d4e5f6]` |

### 5.3 解決

```
Markdown 内の object:ID
  → フロントエンド ReactMarkdown img コンポーネント
    → src が "object:" で始まる?
      → GetObjectDataURL(id) バインディング呼び出し
        → objstore.LoadAsDataURL(id)
          → ディスクからバイナリ読み込み
          → base64 エンコード
          → data:mime;base64,... を返す
```

### 5.4 削除

明示的な削除:
- セッション削除 → 該当 SessionID の全オブジェクト削除
- 手動クリーンアップ (将来の管理 UI)

v2 スコープでは自動 GC なし。

## 6. LLM 統合

### 6.1 画像の LLM への送信

v1 の実証済みパターンに従う:

```
LLM メッセージ構築:
  ObjectIDs を持つ各レコードについて:
    最新の画像レコードの場合:
      → objstore から data URL をフルロード
      → マルチモーダルコンテンツとして送信 (OpenAI Vision 形式)
      → ラベル付与: "[Image ID: abc123, attached at 15:04:05]"
    
    最新でない画像レコードの場合:
      → テキスト参照のみ送信
      → "[Past image ID: abc123 — use get-object tool to view again]"
```

### 6.2 LLM ツール

#### list-objects

セッション内の全オブジェクト一覧。ID、タイプ、ファイル名、作成時刻を返す。
`type_filter` パラメータでフィルタ可能 (image/blob/report/all)。

#### get-object

ID でオブジェクト取得。
- 画像: `__IMAGE_RECALL_BLOB__{id}__` マーカー (メッセージビルダーが解決)
- テキスト: コンテンツ文字列 (30KB まで)
- バイナリ: メタデータ説明のみ

### 6.3 レポート内のオブジェクト参照

1. LLM が `list-objects` でセッション画像を発見
2. LLM がレポートに `![説明](object:abc123)` 構文で記述
3. `create-report` がレポートを objstore に保存 (TypeReport)
4. フロントエンドが `object:` URL をバインディング経由で解決

### 6.4 画像リコールマーカー

LLM が `get-object` で画像を取得した場合:
- ツールは `__IMAGE_RECALL_BLOB__{id}__` を返す (テキストマーカー)
- メッセージビルダーがマーカーを検出
- 次の LLM 呼び出しで実際の data URL に展開
- data URL がツール結果レコードを汚染するのを防止

## 7. フロントエンド統合

### 7.1 Wails バインディング

```go
SaveImage(dataURL string) (string, error)          // data URL → ID
GetObjectDataURL(id string) (string, error)        // ID → data URL
SaveObjectToFile(id string) error                  // ID → ファイル保存ダイアログ
```

### 7.2 ReactMarkdown オブジェクト解決

```tsx
<ReactMarkdown components={{
    img: ({src, alt}) => {
      if (src?.startsWith('object:')) {
        const id = src.slice(7)
        return <ObjectImage id={id} alt={alt} />
      }
      return <img src={src} alt={alt} />
    }
}} />
```

### 7.3 遅延読み込み

フロントエンドはインメモリキャッシュを維持:
- 初回アクセス: `GetObjectDataURL(id)` 呼び出し → 結果キャッシュ
- 以降: キャッシュから返却
- キャッシュ無効化: セッション切替時

### 7.4 Objects サイドバーパネル (v0.1.3+)

中央リポジトリを直接閲覧できる専用パネル。

**リスティング**: `bindings.ListObjects()` が全オブジェクトのメタデータ
（`ID / Type / MimeType / OrigName / CreatedAt / SessionID / Size`）を
新しい順で返す。各 row:

- **image** — 既存 `ObjectImage` でサムネ。クリックで lightbox
- **report** — クリック可能なドキュメントアイコン (📄)。クリックで
  `bindings.GetObjectText(id)` で markdown を取得、既存の全画面レポート
  ビューアで表示
- **blob** — 汎用アイコン、プレビューなし

**参照スキャン付きバルク削除**: Findings / Pinned と同じ `BulkActions`
ツールバー再利用。2 クリック確認パターン:

```
1回目クリック → bindings.ObjectReferences(selectedIds) を実行
                各 id を参照中のセッション数を集計
                （Record.ObjectIDs マッチ OR Report 内の markdown 画像参照
                "object:<id>" 部分文字列）
                confirming ステートのボタンラベル:
                  "Confirm delete N"           参照なし
                  "K/N still in use — confirm" 参照あり
2回目クリック → bindings.DeleteObjects(ids) で実削除
```

単体 × 削除も同ロジック: 参照0なら即削除、>0 なら × が `!` に変化、
6秒の確認ウィンドウ。

**エクスポート**: 各 row の `⤓` → `bindings.ExportObject(id)` → save dialog。
`TypeReport` は `resolveObjectRefsForExport` を通って image refs が
`data:` URL に展開されるので保存 `.md` は self-contained。image / blob は
そのまま書き出し。デフォルトファイル名は `OrigName`、無ければ `<id><ext>`
（`MimeType` から拡張子を導出: `png/jpg/gif/webp/md/json/txt`）。

**カスケード挙動**: ユーザー駆動削除は他セッションを scan / 更新しない。
dangling refs が発生し得るが、`ObjectImage` が `object not found` 時に
`🖼 alt` プレースホルダにフォールバックするのでクラッシュはしない
（broken refs はグレースフルに見えなくなる）。
`objstore.DeleteBySession` の既存 cascade は変更なし。

## 8. v1 → v2 差分

| 項目 | v1 | v2 |
|------|-----|-----|
| SessionID | 定義済みだが未使用 | 全 Save で設定 |
| レコードフィールド | `Images []ImageEntry` | `ObjectIDs []string` |
| 参照構文 | `image:ID`, `blob:ID` | `object:ID` (統一) |
| GC | なし | セッション別削除 |
| レポート保存 | objstore 外 | objstore に TypeReport で保存 |

## 9. 実装チェックリスト

### Phase 1: objstore コア更新
- [ ] ObjectType 定数追加 (TypeImage, TypeBlob, TypeReport)
- [ ] ObjectMeta に SessionID 追加、全 Save で設定
- [ ] ListBySession(sessionID) メソッド
- [ ] DeleteBySession(sessionID) メソッド
- [ ] インデックスリビルドのタイプ保持

### Phase 2: レコード移行
- [ ] Record の `ImageURLs []string` → `ObjectIDs []string`
- [ ] AddUserMessage をオブジェクト ID 受け入れに変更
- [ ] SendWithImages: objstore に先に保存、ID を渡す
- [ ] BuildMessages: 最新画像を objstore から解決

### Phase 3: LLM ツール
- [ ] list-objects ツール実装
- [ ] get-object ツール (画像リコールマーカー対応)
- [ ] create-report を objstore に保存

### Phase 4: フロントエンド
- [ ] ReactMarkdown object: URL 解決コンポーネント
- [ ] 画像キャッシュと遅延読み込み
- [ ] レポート保存時の object: URL → インライン base64 解決

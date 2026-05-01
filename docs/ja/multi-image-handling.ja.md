# マルチ画像入力の取り扱い — 設計ドキュメント

> 日付: 2026-05-01
> ステータス: v0.1.16 でリリース済み (6c3bf8f)
> 範囲: agent / LLM backend が複数添付画像を含む user
> メッセージをパッキングする際、各画像と永続 object ID
> の対応を確実に保つための仕様

## 1. 問題

ユーザがチャットメッセージに N 枚の画像を添付して送る。
現状 agent は単一の multimodal メッセージにまとめている:

```
[text("質問"),
 text("=== BEGIN IMAGE 1 (id: a) ==="), image_a, text("=== END IMAGE 1 ==="),
 text("=== BEGIN IMAGE 2 (id: b) ==="), image_b, text("=== END IMAGE 2 ==="),
 …]
```

`local` backend (LM Studio + llama.cpp + Gemma 3
multimodal) で症状: 3 画像のとき image 1 と image 3 の
記述がレポート上で常に入れ替わる — section 1 は image-1
の正しい object ID を持ちながら image-3 の内容で記述、
section 3 はその逆。section 2 は正常。同一プロンプトを
Vertex AI (gemini-2.5-flash) に投げると正常動作する。

切り分け済み:
- フロントエンド `FileReader` の race を `Promise.all` で
  修正済み（添付順を保持）
- ObjectIDs を `chat.go` / `contextbuild/builder.go` 経由
  で伝搬し、LLM に渡る `Message` で `ImageURLs[i]` ↔
  `ObjectIDs[i]` の対応がとれている
- 各画像に `BEGIN/END` text anchor を追加 — Vertex は
  正しい image↔ID 対応を出すようになった。local は依然
  swap する

結論: これは **llama.cpp の mmproj 経路における
multi-image inference バグ** であり、当方コードの
ordering バグではない。Gemma 3 自体は multi-image-per-
turn で訓練・評価されている（technical report で最大 ~8
枚評価）。Vertex / vLLM / HF Transformers は正常。
llama.cpp の mmproj cache が同一 prompt 内の画像で slot
を再利用し、`<start_of_image>` マーカーと埋め込みテンソル
の positional binding がずれる（issue #12344 系、2025
後半に断続的に修正、LM Studio ビルド差あり）。

`BEGIN/END` ラッパーはむしろ悪化要因: ID ラベルと続く
画像の間にトークンを挟み、バグ slot 再割当の窓を広げる。

## 2. ゴール / 非ゴール

### ゴール

1. `local` backend で 1 つの user record に画像 ≥2 枚の
   とき、各画像を **別の user turn** で送る。これにより
   llama.cpp の per-prompt mmproj slot バグが順序を
   入れ替えられない。各画像 turn は対応する object ID を
   Google 推奨形式の short prefix として保持する
2. 冗長な `=== BEGIN/END IMAGE N ===` ラッパーを削除し、
   画像直前に 1 行の prefix のみにする — 両 backend に
   恩恵があり、llama.cpp のトークナイズと戦わなくて済む
3. Vertex の挙動は意味的に同一: 1 つの user turn に
   text-prefix と image part を attach 順で interleave。
   Vertex 側の差分は prefix 簡素化のみ
4. 永続化される `Record.ImageURLs` / `Record.ObjectIDs`
   は parallel array のまま — 分割は backend の request
   ビルド時のみ発生し、session record 形式は不変。過去
   メッセージの UI 表示に影響なし

### 非ゴール

- **フロントエンド変更なし。** `pendingImages` の順序、
  `Bindings.SendWithImages` の引数形は今のまま
- **ツール追加や system prompt 全面書き換えなし。**
  `BEGIN/END` 段落の差し替えのみ
- **backend 切替フラグの導入なし。** local 側の分割は
  常時動作。トグルなし
- **9 枚以上は対象外。** Gemma 3 の multi-image 訓練は
  ~8 枚まで。人為的に上限は設けないが、それ以上の正常性
  も保証しない

## 3. 詳細設計

### 3.1 backend 別 image-packing ルール

分割は各 backend の request builder
(`internal/llm/local.go`, `internal/llm/vertex.go`) 内で
行う。データ形が違うため:

| Backend | 1 メッセージに N 画像のとき | 出力 |
|---|---|---|
| `local` (OpenAI Vision) | 常に | N+1 個の `requestMessage`: N 個の user turn が `[text(prefix), image_url]` を保持、最後に元の `Content` テキストだけの user turn |
| `vertex_ai` | 常に | 1 個の `Content` ブロック: `[text(Content), text(prefix), image, text(prefix), image, …]`（現行パターン、prefix のみ簡素化） |

local で N=1 のときは現行と同じ単一 user turn と等価
（ただし「画像 turn + 質問 turn」の 2 turn 構成、コスト
は無視できる）。

### 3.2 prefix 形式

`=== BEGIN IMAGE N (object ID: x) ===` … `=== END IMAGE
N (object ID: x) ===` を 1 行に置換:

```
Image (object ID: <id>):
```

理由: Google ドキュメントが Gemma multimodal で推奨する
パターンと一致。ID と画像の間にトークンを挟まない。
text encoder が overweight しない程度に短い。

### 3.3 local backend — 複数 user turn に分割

`local.buildRequest` の擬似コード:

```go
case (multimodal user message, len(ImageURLs) >= 1):
    for i, imgURL := range m.ImageURLs {
        req.Messages = append(req.Messages, requestMessage{
            Role: "user",
            Content: []contentPart{
                {Type: "text", Text: fmt.Sprintf("Image (object ID: %s):", m.ObjectIDs[i])},
                {Type: "image_url", ImageURL: &imageURL{URL: imgURL}},
            },
        })
    }
    if m.Content != "" {
        req.Messages = append(req.Messages, requestMessage{
            Role:    "user",
            Content: m.Content,
        })
    }
```

エッジケース:
- `len(ObjectIDs) < len(ImageURLs)`: 不足分は positional
  `Image %d:` にフォールバック（旧レコード対応）
- `m.Content == ""`: 末尾の質問 turn をスキップ
  （画像のみのメッセージ）
- N=1: 画像 turn + 質問 turn の 2 turn 構成。空 content
  の余分な turn が 1 つ増えるが、gemma は問題なく処理。
  決定論性とコードのシンプルさを優先

### 3.4 Vertex backend — 単一 turn、prefix 簡素化

`vertex.convertMessages` の擬似コード:

```go
case (multimodal user message):
    parts := []*genai.Part{genai.NewPartFromText(m.Content)}
    for i, dataURL := range m.ImageURLs {
        if i < len(m.ObjectIDs) {
            parts = append(parts, genai.NewPartFromText(
                fmt.Sprintf("Image (object ID: %s):", m.ObjectIDs[i])))
        }
        if p := dataURLToGenaiPart(dataURL); p != nil {
            parts = append(parts, p)
        }
    }
    contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: parts})
```

Vertex は llama.cpp の slot バグを抱えていないので 1
`Content` にまとめて OK。prefix のみ簡素化する。

### 3.5 system prompt 更新

`agent.go` の `defaultSystemPrompt` の `BEGIN/END` 段落を
以下に置換:

> ユーザが画像を共有すると、会話中の各画像の直前に
> `Image (object ID: xxxxxxxxxxxx):` 形式の短い text 行が
> 配置される。画像直前の ID がその画像の永続 object ID。
> 各画像の記述はその ID 行の直後にある内容のみに基づき
> 行うこと。レポートで画像を参照する際は
> `![alt](object:ID)` でその ID を厳密に使う。現在添付の
> 画像識別に `list-objects` を呼ぶな。

### 3.6 影響ファイル

| ファイル | 変更 |
|---|---|
| `internal/llm/local.go` | multi-image を per-image user turn に分割。新 prefix |
| `internal/llm/vertex.go` | BEGIN/END 削除、1 行 prefix |
| `internal/agent/agent.go` | system prompt 段落書き換え |
| `internal/llm/backend_test.go` | `TestLocalBuildRequest_ImageAnchors` を分割形式で書き直し。multi-image ケースで N+1 メッセージを assert |

`chat.go`, `contextbuild/builder.go`, frontend, bindings,
session record 形式は変更なし。

## 4. テスト計画

### 単体
- `TestLocalBuildRequest_SingleImage`: 1 画像 → 2
  `requestMessage`（画像 + 質問）
- `TestLocalBuildRequest_MultipleImages`: 3 画像 → 4
  `requestMessage`（画像 × 3 + 質問）。各画像 turn は
  `[text(id 付き prefix), image_url]` 厳密一致。質問
  turn は元の `m.Content` のみ
- `TestLocalBuildRequest_NoObjectIDs`: ImageURLs はあるが
  ObjectIDs が空の旧レコード → positional `Image N:` に
  フォールバックしつつ per-image turn 分割は行う
- `TestVertexConvertMessages_MultiImage`: 単一 Content
  ブロック、parts 順序が `[text(content), prefix1,
  image1, prefix2, image2, …]`。BEGIN/END マーカーなし

### 手動
- Local Gemma セッションで 3 枚の異なる画像を添付し、
  各画像を記述させる。1↔3 入れ替わりが解消したことを
  確認。5 枚でも繰り返す
- Vertex セッションで 3 枚の画像、挙動が変わらない
  （引き続き正常）ことを確認

## 5. リスクとロールアウト

| リスク | 緩和策 |
|---|---|
| N user turn 分割が cross-image タスク（「画像 2 と画像 3 を比較」）で混乱 | 最終質問 turn が画像を object ID で明示参照する（prefix 行がモデルに規約を学習させる）。Gemma 3 は multi-turn user content で訓練済み、連続 user turn は exotic ではない |
| local リクエストのトークン数増（1 画像あたり prefix + role envelope で ~30 tokens） | 画像自体の 256 SigLIP tokens に比べ無視できる |
| 旧セッション再ロード時、ImageURLs と並んで ObjectIDs が未設定のレコード | "ObjectIDs なし" 分岐で positional `Image N:` prefix を使い動作させる |
| 一部の llama.cpp ビルドが連続 `user` メッセージを受け付けない可能性 | 現行 LM Studio ビルドで検証する。OpenAI-compat シムは仕様上受理する。受理しないビルドが見つかったら現行（単一 turn 詰め）にフォールバック — 回避対象のバグ自体ビルド依存 |

単一コミット: `feat(llm): per-image user turns for local
backend, simpler image-ID prefix`。frontend / session
storage と疎結合なので段階分けなし。デプロイ初回セッション
で動かなかったら revert。

## 6. 範囲外

- multi-image fix が入っていない古い llama.cpp ビルドでの
  cross-image 推論品質 — そういう環境では Vertex 推奨
- ローカルモデルが multimodal でないケースの検出と警告
  （LLM 呼び出し時にモデルがテキストのみ ack を返すこと
  で既に表面化している）
- 画像のダウンサンプル / 解像度上限（別 TODO、正確性より
  トークンコストの問題）

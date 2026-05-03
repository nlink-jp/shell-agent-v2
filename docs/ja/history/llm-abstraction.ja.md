# LLM 抽象化層 — 設計ドキュメント

> 作成日: 2026-04-26 (初版), 2026-05-02 (v0.1.19 反映)
> ステータス: 実装済み — 下記 Phase 1〜4 はすべてマージ済み。
> Phase 2 のツールコール round-trip は v0.1.19 で精査
> （並列 FunctionResponse 集約、Gemini Thought Part フィルタ、
> 永続レコード `ToolCalls` による round-trip 再現）。
> 関連: [エージェントデータフロー](agent-data-flow.ja.md),
> [オブジェクトストレージ](object-storage.ja.md),
> [Tool-call round-trip](tool-call-roundtrip.ja.md).

## 1. 問題

現在の `llm.Backend` インターフェースはバックエンド固有の関心事が漏れている:

1. **ロールマッピングが間違った層にある** — `BuildMessages` (chat.go) が
   gemma-4/LM Studio ワークアラウンドとして `tool` → `user` にマッピング。
   Vertex AI (Gemini) は `tool` ロールをネイティブ処理するため、この変換は
   Vertex AI を壊す。

2. **ツール定義が変換されない** — `ToolDef` は OpenAI JSON Schema 形式。
   Vertex AI は `genai.FunctionDeclaration` を必要とする。

3. **マルチモーダルが抽象化されていない** — `Message.ImageURLs` は data URL。
   Local バックエンドは OpenAI Vision コンテンツパーツで送信。Vertex AI は
   `genai.NewPartFromBytes()` で生バイナリが必要。

4. **ツール結果フォーマットが抽象化されていない** — Local (gemma-4) は
   `role="user"` が必要。Vertex AI (Gemini) はネイティブの
   `FunctionResponse` パーツを使用。

根本原因: 抽象化が生の `Message` 構造体を素通しさせているため、各バックエンドが
アプリケーションレベルのロールセマンティクスを理解する必要がある。

## 2. 設計方針

### 2.1 メッセージモデル

`Message` はアプリケーションレベルの型として定義。明示的なロール定数を使用:

```go
type Role string

const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleTool      Role = "tool"
    RoleReport    Role = "report"
    RoleSummary   Role = "summary"
    RoleSystem    Role = "system"
)
```

### 2.2 バックエンドの責務

各バックエンドが以下を自己完結で処理:

| 関心事 | Local (LM Studio) | Vertex AI (Gemini) |
|--------|-------------------|-------------------|
| システムプロンプト | 先頭メッセージ `role="system"` | `GenerateContentConfig.SystemInstruction` |
| ユーザメッセージ | `role="user"` | `genai.RoleUser` |
| アシスタント | `role="assistant"`（ツール呼び出しがあれば `tool_calls` 配列付き） | `genai.RoleModel`（ツール呼び出しがあれば `FunctionCall` Parts 付き） |
| ツール結果 | `role="tool"` + `tool_call_id`（OpenAI 公式仕様。`cmd/tooltest-local` で gemma-4 にも適用可能と検証） | `genai.Part{FunctionResponse{Name, Response}}` — **同一 assistant turn の並列呼び出しは 1 つの user Content にまとめる必要あり**（さもなくば Vertex 400） |
| レポート | `role="assistant"` | `genai.RoleModel` |
| サマリー | `role="system"` | `SystemInstruction`（追加） |
| 画像 | OpenAI Vision content 配列；2 枚以上は 1 user turn に 1 枚（mmproj スロット再利用バグ対策） | 1 つの Content に text + 画像 Parts を packed |
| ツール定義 | OpenAI `tools` パラメータ | `genai.Tool{FunctionDeclarations}`（Parameters は `ParametersJsonSchema` で生 JSON Schema を直接） |
| レスポンスのツール呼び出し | `response.choices[0].message.tool_calls` | `response.Candidates[0].Content.Parts[].FunctionCall`（`Thought == true` の Part は content 連結前にスキップ — §6 参照） |
| ストリーミング | `ChatStream` 利用可、ただし `agentLoop` からは未使用（ツールチェイン中は最終ラウンドが事前に分からない） | 同左 |
| Round-trip 永続化 | `assistant.tool_calls` を `memory.Record.ToolCalls` から再構築 | `genai.Part{FunctionCall}` を同フィールドから再構築 — [tool-call-roundtrip.ja.md](./tool-call-roundtrip.ja.md) |

### 2.3 変換レイヤ

各バックエンドが内部で `convertMessages()` を実装する:

```go
// Local バックエンド
func (l *Local) convertMessages(messages []Message) []requestMessage {
    for _, m := range messages {
        switch m.Role {
        case RoleTool:    role = "user"      // gemma-4 ワークアラウンド
        case RoleReport:  role = "assistant"
        case RoleSummary: role = "system"
        default:          role = string(m.Role)
        }
        // ImageURLs → OpenAI Vision content parts に変換
    }
}

// Vertex バックエンド
func (v *Vertex) convertMessages(messages []Message) []*genai.Content {
    for _, m := range messages {
        switch m.Role {
        case RoleSystem:  → SystemInstruction (contents から除外)
        case RoleAssistant, RoleReport: → genai.RoleModel
        case RoleTool:    → genai.Part{FunctionResponse{Name, Response}}
        case RoleSummary: → SystemInstruction (追加)
        default:          → genai.RoleUser
        }
        // ImageURLs → genai.NewPartFromBytes() に変換
    }
}
```

### 2.4 ツール定義変換

```go
// Local: 既に OpenAI 形式、pass through
func (l *Local) convertTools(tools []ToolDef) []requestTool { ... }

// Vertex: genai 形式に変換
func (v *Vertex) convertTools(tools []ToolDef) []*genai.Tool {
    var decls []*genai.FunctionDeclaration
    for _, t := range tools {
        decls = append(decls, &genai.FunctionDeclaration{
            Name:                 t.Name,
            Description:          t.Description,
            ParametersJsonSchema: t.Parameters,
        })
    }
    return []*genai.Tool{{FunctionDeclarations: decls}}
}
```

### 2.5 ツールコールレスポンス解析

```go
// Local: response.choices[0].message.tool_calls から解析 (実装済み)

// Vertex: response.Candidates[0].Content.Parts から抽出
func extractToolCalls(resp *genai.GenerateContentResponse) []ToolCall {
    for _, part := range resp.Candidates[0].Content.Parts {
        if part.FunctionCall != nil {
            calls = append(calls, ToolCall{
                Name:      part.FunctionCall.Name,
                Arguments: marshalArgs(part.FunctionCall.Args),
            })
        }
    }
}
```

### 2.6 BuildMessages の変更

`BuildMessages` (chat.go) は **ロールマッピングを一切しない**。
アプリケーションレベルのロールで `[]Message` を構築し、そのまま渡す:

```go
func (e *Engine) BuildMessages(...) []Message {
    // システムプロンプト
    messages = append(messages, Message{Role: RoleSystem, Content: ...})

    // セッションレコード — ロールはそのまま保持
    for _, r := range session.Records {
        messages = append(messages, Message{
            Role:      Role(r.Role),  // user|assistant|tool|report|summary
            Content:   content,
            ImageURLs: r.ImageURLs,
            ToolName:  r.ToolName,
        })
    }
    return messages
}
```

各バックエンドがアプリケーションレベルメッセージを API 固有形式に変換する。

## 3. マルチモーダル対応

### 3.1 Local バックエンド

data URL → OpenAI Vision content array。実装済み。

### 3.2 Vertex AI バックエンド

gem-cli の実装パターンを再利用:
- `genai.NewPartFromBytes(data, mime)` でバイナリ → Part 変換
- テキストと画像を同一 Content の Parts 配列に混在

## 4. 再利用コード

| ソース | コード | 用途 |
|--------|------|------|
| gem-cli | `internal/input/input.go:fileToInlineData()` | 画像 → genai.Part |
| gem-cli | `internal/input/input.go:detectMIME()` | MIME 判定 |
| data-agent | `internal/llm/vertexai.go` | genai クライアント設定 |
| shell-agent v1 | `internal/client/client.go` | OpenAI ストリーミングパーサー |

## 5. 実装チェックリスト

### Phase 1: chat.go からロールマッピング除去 (完了, v0.1.13)
- [x] Role 定数を llm パッケージに定義
- [x] BuildMessages はロールを変換せずそのまま渡す
- [x] tool 結果は Local バックエンドの `convertMessages()` で処理
- [x] report→assistant, summary→system も Local 側で処理

### Phase 2: Vertex AI ツール呼び出し (完了; v0.1.19 で再強化)
- [x] `convertTools()` → `genai.FunctionDeclaration` 変換
- [x] `Part.FunctionCall` からツール呼び出しパース
- [x] ツール結果 → `FunctionResponse` パーツ変換
- [x] **v0.1.18**: assistant 永続レコードに `ToolCalls` を保存し、
      次ラウンドで Vertex 用 FunctionCall Part / OpenAI 用
      `tool_calls` をプロトコル正準形で再構築
- [x] **v0.1.19**: 並列 FunctionResponse Parts を 1 つの user
      Content にまとめる（さもなくば「N calls / 1 response each」
      で Gemini が 400 を返す）
- [x] **v0.1.19**: `ThinkingConfig{IncludeThoughts: false}` を
      明示設定し、`parseResponse` / `extractText` で
      `Part.Thought == true` のパートをスキップ（chain-of-thought
      が assistant content に漏れない）
- [x] **v0.1.18**: 経験的検証ハーネス
      `app/cmd/tooltest-vertex` / `app/cmd/tooltest-local`
      （proper / hack / loop モード）でドキュメント準拠仕様が
      両バックエンドで動作することを確認

### Phase 3: Vertex AI マルチモーダル (完了, v0.1.16)
- [x] data URL → `genai.NewPartFromBytes()` 変換
- [x] テキスト+画像混在 Content 構築
- [x] **v0.1.16**: 各画像直前に `Image (object ID: x):` の
      アンカー行を入れ、レポート内で参照すべき object ID を
      モデルが正しく対応付けられるようにした（Local 側の
      llama.cpp mmproj スロット再利用バグへの workaround も兼ねる）

### Phase 4: テスト (完了)
- [x] Vertex AI 統合テスト
- [x] バックエンド切替テスト (同一会話、異なるバックエンド)
- [x] マルチモーダルテスト — v0.1.17 で Gemma 3 multimodal の
      上限である N=8 まで手動確認

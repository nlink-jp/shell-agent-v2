# LLM 抽象化層 — 設計ドキュメント

> 作成日: 2026-04-26
> ステータス: Draft
> 関連: [エージェントデータフロー](agent-data-flow.ja.md), [オブジェクトストレージ](object-storage.ja.md)

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
| システムプロンプト | role="system" | `SystemInstruction` |
| ユーザメッセージ | role="user" | `genai.RoleUser` |
| アシスタント | role="assistant" | `genai.RoleModel` |
| ツール結果 | **role="user"** (gemma ワークアラウンド) | `genai.Part{FunctionResponse}` |
| レポート | role="assistant" | `genai.RoleModel` |
| サマリー | role="system" | `SystemInstruction` (追加) |
| 画像 | OpenAI Vision コンテンツ配列 | `genai.NewPartFromBytes()` |
| ツール定義 | OpenAI `tools` パラメータ | `genai.Tool{FunctionDeclarations}` |

### 2.3 BuildMessages の変更

BuildMessages は **ロールマッピングをしない。** アプリケーションロールをそのまま渡す。
各バックエンドが自分の `convertMessages()` で API 固有形式に変換する。

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

### Phase 1: chat.go からロールマッピング除去
- [ ] Role 定数を llm パッケージに定義
- [ ] BuildMessages はロールを変換せずそのまま渡す
- [ ] tool→user マッピングを Local バックエンドの `convertMessages()` に移動
- [ ] report→assistant, summary→system も同様

### Phase 2: Vertex AI ツール呼び出し
- [ ] `convertTools()` → `genai.FunctionDeclaration` 変換
- [ ] `Part.FunctionCall` からツール呼び出しパース
- [ ] ツール結果 → `FunctionResponse` パーツ変換
- [ ] Vertex AI 統合テスト

### Phase 3: Vertex AI マルチモーダル
- [ ] data URL → `genai.NewPartFromBytes()` 変換
- [ ] テキスト+画像混在 Content 構築
- [ ] gem-cli の `detectMIME()` パターン再利用

### Phase 4: テスト
- [ ] Vertex AI 統合テスト (ADC + プロジェクト必要)
- [ ] バックエンド切替テスト (同一会話、異なるバックエンド)
- [ ] マルチモーダルテスト (両バックエンド)

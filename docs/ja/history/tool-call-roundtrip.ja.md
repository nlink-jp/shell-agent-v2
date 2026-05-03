# ツール呼び出しラウンドトリップ — 設計ドキュメント

> 日付: 2026-05-02
> ステータス: リリース済み — `memory.Record.ToolCalls` の
> ラウンドトリップ永続化と、並列 FunctionResponse の Content
> 集約をいずれも v0.1.19 でマージ。並列集約は最後の必須ピース
> で、モデルが並列ツール呼び出しを実用するや否や Gemini が
> 「N calls / 1 response each」を HTTP 400 で拒否していた。
> 経験的検証ハーネスは `app/cmd/tooltest-vertex` と
> `app/cmd/tooltest-local` に同梱。
> 範囲: assistant 側のツール呼び出し構造をエンドツーエンドで
> 永続化・再生して、Vertex（および OpenAI 互換のローカル
> バックエンド）が自分の出した function call を認識できる
> ようにする。「Vertex が成功したツールを 3 回連発する」
> ループに加えて、並列呼び出し時の HTTP 400 退行も解消。
>
> 引用元（執筆前に検証済み）:
>   - [Gemini Function Calling docs](https://ai.google.dev/gemini-api/docs/function-calling)
>   - [Vertex AI Function Calling docs](https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/function-calling)
>   - [OpenAI Cookbook — how_to_call_functions_with_chat_models](https://cookbook.openai.com/examples/how_to_call_functions_with_chat_models)
>   - [LM Studio OpenAI-compat tools](https://lmstudio.ai/docs/developer/openai-compat/tools)
>   - [googleapis/python-genai #813 — duplicate function call hallucination](https://github.com/googleapis/python-genai/issues/813)
>   - [Portkey error library 10067 — `tool_calls` must be followed by tool messages](https://portkey.ai/error-library/tool-call-response-error-10067)

## 1. 問題

実セッション、2026-05-02、sandbox-run-python（素数判定）:

```
Round 0: assistant content="承知…" + sandbox-run-python  → success "[2,3,…,97]"
Round 1: assistant content=""    + sandbox-run-python（同コード） → 同じ結果
Round 2: assistant content=""    + sandbox-run-python（同コード） → 同じ結果
Round 3: assistant content="…素数は以下…" (最終応答)
```

LLM (Vertex / gemini-2.5-flash) が成功したツール呼び出しを
3 回連発し、4 ラウンド目でやっと summarize した。
v0.1.16 の loop detection (Feature 1) は `status=error`
で gating されているので、全 success のこのケースには
発火しない。

なぜ Vertex が同じ呼び出しを繰り返すか追跡: 当方コードは
ユーザ側に
`genai.NewPartFromFunctionResponse(toolName, {result: "…"})`
を送る一方、assistant 側は前ターンを **plain text**
(`genai.NewContentFromText("[Calling: sandbox-run-python]", RoleModel)`)
で送っている。対応する `FunctionCall` part は送っていない。

Vertex プロトコル視点:

```
[user: text "素数を出して"]
[model: text "[Calling: sandbox-run-python]"]   ← FunctionCall part なし
[user: FunctionResponse(sandbox-run-python, …)] ← orphan
[model: ???]
```

これは Google が文書化したラウンドトリップに違反している。
公式 Gemini ドキュメント
([function-calling](https://ai.google.dev/gemini-api/docs/function-calling)
の "Step 4 — Create user friendly response") は次を示す:

```python
contents.append(response.candidates[0].content)  # FunctionCall を含む
contents.append(types.Content(role="user", parts=[function_response_part]))
```

`functionCall` part を含むモデルの content をそのまま履歴
に append してから `functionResponse` を append する。
`"[Calling: foo]"` のような text 代用品を合成するのは
誤りと文書化されている。Vertex AI のミラー文書および
Google の Python/Go/Node/Java/REST サンプルでも同一の
ラウンドトリップを示している。

観測された事象 — orphan な response の後で Gemini が同一
`functionCall` を再発行 — は、文書化された failure
mode に一致する。同一クラスの hallucination の公開報告
が複数存在する:
[python-genai #813](https://github.com/googleapis/python-genai/issues/813)、
[Multimodal Live API duplicate-call bug](https://discuss.ai.google.dev/t/bug-multimodal-live-api-v1beta-triggers-identical-tool-calls-twice-in-rapid-succession/135700)。

**ローカル OpenAI 互換バックエンド (LM Studio + gemma)
は意図的に範囲外。** 当方コードは既に gemma 向けの
ワークアラウンドを動かしている: `local.go:241` で
`RoleTool → role="user"` にマップ、コメントは
*"gemma-4 stays in tool-calling mode with role=\"user\""*。
ローカル経路は OpenAI の `tool_calls` /
`tool_call_id` ラウンドトリップを **使っていない**。
ツール結果は plain user メッセージとして戻し、gemma は
その形から tool-calling を継続するよう訓練されている
ようだ。重複呼び出しの症状は **Vertex 側のみ** で観測
されている。

ローカル経路を canonical OpenAI tool_calls
(`role: "assistant", tool_calls: [...]` + 後続の
`role: "tool", tool_call_id: ...`) に切り替えるのは別
施策で固有のリスクがある — 非 gemma OpenAI 互換モデル
には効くが gemma の現行動作を退行させる可能性。
(a) ローカルでも重複呼び出しバグを再現するか、
(b) 現行 shim がボトルネックになる非 gemma OpenAI 互換
モデルをサポートするまで、本設計の範囲外。

## 2. ゴール / 非ゴール

### ゴール

1. LLM が function call を発行したとき `resp.ToolCalls`
   を assistant `Record` に永続化し、セッション再ロード
   と再生で assistant→tool の対応を維持する
2. ToolCalls をビルドパイプライン (`chat.BuildMessages*`、
   `contextbuild.Build`) を通して `llm.Message` に伝搬
   する
3. assistant tool call を **Vertex のみ**
   `genai.NewPartFromFunctionCall(name, args)` で emit。
   ローカルバックエンドの既存 gemma 向け shim
   (`RoleTool → role="user"` の plain text) はそのまま
4. 後方互換: 修正前のセッション record (ToolCalls
   フィールド無し) も動作する — 空ケースは現行の
   text-only emit に gracefully degrade

### 非ゴール

- **ローカルバックエンドの tool_calls 移行なし**。§1
  参照。現行の `RoleTool → role="user"` shim はそのまま。
  `local.go` は新フィールド `ToolCalls` を読まない (tool
  call のみの assistant ターンも現行通り fall through)
- **既存セッションの遡及修復なし**。修正前に保存された
  セッションは再ロード時にバグが残る。許容: ループ
  問題は「過去のレコードを agent loop が継続使用する」
  特殊ケース。一般的でない
- **MCP 側の tool-call 構造変更なし**。MCP ガーディアン
  ツール呼び出しは既に Tool result content で round-trip
  しており、assistant 側の bookkeeping は LLM の責任
  (本修正後は native ツールと同じコードパス)
- **ストリーミング側の改修なし**。streaming は現状
  無効化済み (`canStream := false`)。streaming partial
  delta における tool-call 構造は本範囲外
- **`[Calling:]` プレースホルダ削除なし**。LLM が
  narrative を出さなかった場合の chat content として
  `"[Calling: foo]"` text は引き続き emit する。chat UI
  はこれを使用。LLM 表現として送信するのを ToolCalls 充填
  時に止めるだけ (LLM には適切な FunctionCall part を
  代わりに送る)

## 3. 詳細設計

### 3.1 永続化レコード形式

`memory.Record` に追加:

```go
type Record struct {
    // … 既存フィールド …

    // ToolCalls はこのターンで assistant が発行した
    // function call を保持する。Role == "assistant" かつ
    // 応答に tool calls が含まれていたときのみ populate。
    // 後続の agent loop 実行で verbatim 再生されるので、
    // LLM (Vertex FunctionCall / OpenAI tool_calls) が
    // 整合した assistant→tool ペアを見ることができる
    ToolCalls []ToolCallRecord `json:"tool_calls,omitempty"`
}

type ToolCallRecord struct {
    ID        string `json:"id"`
    Name      string `json:"name"`
    Arguments string `json:"arguments"` // LLM が emit した raw JSON 文字列
}
```

`AddAssistantMessage` は現行シグネチャ維持。新規
`AddAssistantMessageWithToolCalls(content string, calls
[]ToolCallRecord)` を追加し、tool-call 経路で使用。

旧セッション JSON ファイルにこのフィールドはない。
`omitempty` で fresh record にも tool calls なしの
場合は出力しない。

### 3.2 LLM メッセージキャリア

`llm.Message`:

```go
type Message struct {
    Role      Role
    Content   string
    ImageURLs []string
    ObjectIDs []string
    ToolName  string
    ToolCalls []ToolCall  // 新規 — Role == RoleAssistant のとき populate
}
```

`llm.ToolCall` は既存の構造体 (agent-loop dispatch で
既に使用)。フィールドは `memory.ToolCallRecord` と 1:1。

### 3.3 ビルドパイプライン

`chat.BuildMessages` / `chat.BuildMessagesWithBudget` と
`contextbuild.Build` の両方で `r.ToolCalls` を生成する
`llm.Message` にコピー。両ビルド経路の形式変更なし。
フィールドを通すだけ。

### 3.4 Vertex emit

`vertex.buildContents`:

```go
case RoleAssistant, RoleReport:
    if len(m.ToolCalls) > 0 {
        // FunctionCall(s) — 次のメッセージの user 側
        // FunctionResponse とペアにする。オプションの
        // narrative text は最初の Part として配置
        var parts []*genai.Part
        if m.Content != "" && !strings.HasPrefix(m.Content, "[Calling:") {
            parts = append(parts, genai.NewPartFromText(m.Content))
        }
        for _, tc := range m.ToolCalls {
            args := map[string]any{}
            _ = json.Unmarshal([]byte(tc.Arguments), &args)
            parts = append(parts, genai.NewPartFromFunctionCall(tc.Name, args))
        }
        contents = append(contents, &genai.Content{
            Role:  genai.RoleModel,
            Parts: parts,
        })
    } else {
        contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleModel))
    }
```

`[Calling: …]` プレースホルダ text のスキップ重要:
assistant が narrative を持たず tool call のみのとき、
プレースホルダが FunctionCall を duplicate してしまい、
モデルをさらに混乱させる。

**Gemini 3 注意点**: Gemini docs に記述あり
*"Gemini 3 now always returns a unique id with every
functionCall. Include this exact id in your
functionResponse so the model can accurately map your
result back to the original request."*。
`genai.ToolCall` は `ID` を保持しており、現行の
`agent.session.AddToolResult(tc.ID, …)` でルーティング、
`memory.Record.ToolCallID` で round-trip する。本修正後、
その id は (Go SDK のデフォルト挙動で) `FunctionResponse`
経由で戻り、新たに assistant 側に emit する `FunctionCall`
と正しくペアリングされる。

### 3.5 Local (OpenAI 互換) emit — 変更なし

`local.go` の `buildRequest` は **本変更で修正しない**。
現行の `RoleTool → role="user"` マッピングと
`[Calling: foo]` プレースホルダ text もそのまま。
ビルドパイプラインは新しく `ToolCalls` を `llm.Message`
に乗せるが、`local.go` は当面そのフィールドを無視する。

ここでローカル変更しない理由:
- 修正対象の重複呼び出し症状は Vertex でのみ観測
- gemma-4 (ユーザのローカルモデル) には現行構造に依存
  したワークアラウンドが既に存在 — canonical OpenAI
  tool_calls に切り替えると退行する可能性
- ローカルを canonical OpenAI tool_calls プロトコルへ
  切り替えるのは独立した設計案件 — 本設計の範囲外

### 3.6 Agent loop 配線

`internal/agent/agent.go` agentLoop の LLM 呼び出し後:

```go
if len(resp.ToolCalls) > 0 {
    // … 既存の toolNames 構築 …
    a.session.AddAssistantMessageWithToolCalls(resp.Content, resp.ToolCalls)
} else {
    a.session.AddAssistantMessage(resp.Content)
}
```

モデルが function call のみ emit したとき `resp.Content`
は空。それでも記録する (chat UI が render 時に
`"[Calling: …]"` を代入できるよう)。代入は chat UI 表示
にのみ留め、LLM 向けメッセージ変換時に抑制する。

### 3.7 Chat UI

フロントエンド変更なし。tool-call only assistant ターンに
対するプレースホルダ text は引き続き chat に表示される。
本修正は次ターンに LLM へ送信する内容のみ変更する。

## 4. 影響ファイル

| ファイル | 変更 |
|---|---|
| `internal/memory/memory.go` | `Record.ToolCalls`、`ToolCallRecord`、新規 `AddAssistantMessageWithToolCalls` |
| `internal/llm/backend.go` | `Message.ToolCalls` |
| `internal/llm/vertex.go` | assistant role で FunctionCall part を emit。ToolCalls 設定時に `[Calling:]` プレースホルダをスキップ |
| `internal/llm/local.go` | **変更なし**（§3.5 参照） |
| `internal/chat/chat.go` | BuildMessages / BuildMessagesWithBudget で ToolCalls を伝搬 |
| `internal/contextbuild/builder.go` | Build で ToolCalls を伝搬 |
| `internal/agent/agent.go` | `resp.ToolCalls` 非空時に `AddAssistantMessageWithToolCalls` を呼ぶ |
| `internal/llm/backend_test.go` | 新テスト: `TestLocalBuildRequest_AssistantToolCallsRoundTrip` |
| `internal/llm/vertex_test.go` | 新テスト: `TestVertex_BuildContents_AssistantToolCalls` |
| `internal/agent/agent_test.go` | 新テスト: mock backend で 3 ラウンドの合成ループ、assistant tool calls がセッションに残り再発行されないことを assert |

## 5. テスト計画

### 単体
- **`TestVertex_BuildContents_AssistantToolCalls`**:
  ToolCalls を持つ assistant に対し生成される
  `[]*genai.Content` の Parts が
  `[FunctionCall{Name, Args}]` (Content が
  非プレースホルダ時は先行 Text part もあり) であること
- **`TestVertex_SkipsCallingPlaceholder`**: Content が
  `[Calling:` で始まるとき Text part は emit しない
- **`TestLocalBuildRequest_PreservesGemmaShim`**: Local
  バックエンドの `RoleTool` メッセージが従来通り
  `role: "user"` (`role: "tool"` ではない) を出力する
  ことを確認、`Message.ToolCalls` フィールド追加で
  gemma shim を退行させていないことを assert

### 手動
1. **修正前再現**: v0.1.18 ビルドをロード、Vertex に
   「素数を出して」と依頼。N=2-3 回の重複
   `sandbox-run-python` 呼び出しを観察
2. **修正後再現**: 同プロンプトを修正後ビルドで実行。
   1 回のツール呼び出し + text-only summarize ラウンドの
   構成を期待
3. **既存セッション互換**: 修正前に Vertex + ツール呼び
   出しを使ったセッションを開く。会話を継続。古いターン
   は record に ToolCalls 無いので引き続きモデルを混乱
   させる可能性があるが、新ターンは正しく動作する

## 6. リスクと緩和策

| リスク | 緩和策 |
|---|---|
| narrative text と ToolCalls が両方ある場合に assistant Content が duplicate される (Vertex が text Part と FunctionCall Part を emit、モデルが re-narrate する可能性) | モデルは前ターンに自分が出したものを見るだけで、それを反映させるのは正しい。当方が修正するバグは「プレースホルダ text のみ送って FunctionCall が無かった」ケース |
| `[Calling:]` プレースホルダフィルタが、たまたまその文字列で始まる user メッセージを誤適用 | フィルタは `Role == RoleAssistant` でスコープ。user content には影響しない |
| ToolCallRecord.Arguments が無効 JSON (jsonfix で破損、gemma が hallucinate) | Vertex 経路: `json.Unmarshal` 失敗で `args = {}` 残るが FunctionCall は emit する — 何もしないより良い。Local 経路: 現行の tool dispatch でも今行われているように verbatim pass-through |
| 既存ディスク上のセッションが空 ToolCalls で再ロード、Vertex を引き続き混乱させる | 範囲外。修正前のセッションは新ターンには非推奨。リスクは「ユーザが長期セッションを再開して、ツール呼び出しの再生をトリガする follow-up を尋ねる」ケースに限定 — 一般的でない |
| `llm.Message.ToolCalls` 追加によるローカルバックエンド退行 | ローカルの `buildRequest` は `ToolCalls` を引き続き無視する。gemma shim (RoleTool → role=user) が動作することを単体テストで assert |

## 7. フェーズ分割

単一コミット:
**fix(llm): persist and replay assistant tool calls so
Vertex/OpenAI see well-formed function call→response
pairs.**

memory、llm、chat、contextbuild、agent に触れるが密結合
のためフェーズ分割の意味なし。

手動検証後 v0.1.19 リリース。

## 8. 範囲外

- 同一 args dedupe / loop detection 拡張。Gemini docs と
  観測報告
  ([python-genai #813](https://github.com/googleapis/python-genai/issues/813)、
  [Multimodal Live API bug](https://discuss.ai.google.dev/t/bug-multimodal-live-api-v1beta-triggers-identical-tool-calls-twice-in-rapid-succession/135700))
  で orphan-response 履歴が root cause として識別され
  ている。プロトコル修正でその root cause が消える。
  残留する duplicate-call ケースの有無は実証次第 —
  手動テスト (§5) で確認する。残るようであれば、
  別シグナルとして dedupe 拡張を後追加できる
- MCP tool-call 構造変更。MCP ガーディアン結果は同じ
  `RoleTool` パスを通る。assistant→tool ペアリング
  修正で MCP ツールも同じく恩恵を受ける。MCP 固有作業
  なし
- ストリーミング partial-delta tool-call 組み立て
- 旧セッションのマイグレーションユーティリティ。
  修正前に保存された record は ToolCalls 無しなので、
  そのセッションを再開すると orphan response を引き続き
  生成。既存セッションの fresh ターンは恩恵を受ける
  (修正後に追記された分は新フィールドが書き込まれる)
- LM Studio + gemma の応答側パーサ制限
  (`[TOOL_REQUEST]…[END_TOOL_REQUEST]` 形式)。当方は
  送る側のみ修正

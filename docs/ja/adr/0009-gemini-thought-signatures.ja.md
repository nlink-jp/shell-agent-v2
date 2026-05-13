# Gemini 3 thought signature の tool-use ターン間保持 — 設計ノート

**ステータス:** ドラフト (2026-05-13); 承認待ち
**対象バージョン:** v0.6.2 (v0.6.1 上のポイントリリース)
**報告:** ユーザー — Vertex AI バックエンドを Gemini 3 系
モデルに移行後の 400 エラー。

本ノートは、Gemini 3 がレスポンスの各 Part に付与する不透明な
「thought signature」トークンを Vertex AI バックエンドが
キャプチャ・再生する方式を定める。これなしでは Gemini 3 系
モデルに対する複数ステップの tool-use ターンが 2 回目の LLM
ラウンドで HTTP 400 INVALID_ARGUMENT で失敗する。

---

## 1. 問題

ユーザーが Vertex バックエンドを Gemini 3 系モデルへ切り替え
たところ、通常の Q&A は動作したが、tool-loop の途中で次の
エラーが発生:

```
Error: LLM: vertex AI: Error 400, Message: Unable to submit request
because function call `default_api:weather` in the 6. content block
is missing a `thought_signature`. Learn more:
https://docs.cloud.google.com/vertex-ai/generative-ai/docs/thought-signatures,
Status: INVALID_ARGUMENT, Details: []
```

### 時系列 (`app.log` 2026-05-13 20:19 より)

1. ユーザーが並列 weather 検索を誘発する質問を送信。
2. Vertex が 1 ターンで 5 個の `weather` function call を含む
   レスポンスを返却。`parseResponse` (vertex.go:309-327) は
   `FunctionCall.Name` と `FunctionCall.Args` を `ToolCall` に
   取り込むが、Part の残り (Gemini 3 が各 Part に付与する不透明
   な継続トークン `Part.ThoughtSignature` を含む) を破棄する。
3. エージェントは 5 件全てを dispatch し、結果を集約、
   `buildContents` 経由でフィードバック。
4. `buildContents` (vertex.go:191-209) は `model` ロールの
   Content を 5 個の `genai.NewPartFromFunctionCall` Part で
   再構築。`NewPartFromFunctionCall` は `ThoughtSignature` を
   セットしない — Name と Args しか配線しない (genai v1.54
   `types.go:1452-1460`)。再構築されたターンはシグネチャが
   ゼロ値であること以外、元と構造的に同一。
5. Vertex は次の `GenerateContent` リクエストを拒否 — モデルの
   reasoning continuity が破壊された。content block 6 (エージェ
   ントが再生した assistant の tool ターン) の function call に
   シグネチャが付いていない。

### なぜ Gemini 3 がこれを強制するか

Vertex AI thought-signatures ドキュメントによれば、Gemini 3 は
推論状態を各 Part に付与した不透明トークンとして返す。
クライアントがそのターンを会話履歴として再生する場合 (例:
tool-use ラウンド間)、シグネチャは Part と一緒に伝播する必要
がある。シグネチャなしで function call を返送するのは
プロトコル違反 — モデルは改ざんまたは捏造ターンとして扱い、
400 を返す。

この要件は **Gemini 3 で新規**。Gemini 2.5
(shell-agent-v2 の従来デフォルト) はシグネチャを付与しないので、
既存の parse-and-rebuild ラウンドトリップで動作した。バグは
ユーザーが Vertex モデル設定を移行するまで休眠する。

---

## 2. 目標

- **Gemini 3 に対する複数ステップ tool-use が動作する。**
  並列・逐次の tool 呼び出しが報告された 400 なしでエージェント
  ループをラウンドトリップする。
- **Gemini 2.5 以前で regression なし。** 旧モデルはシグネチャを
  emit しない; キャプチャされたスライスは空のまま、新コードパス
  は no-op になる。
- **ローカルバックエンドで regression なし。** シグネチャ
  フィールドは Vertex 専用、OpenAI 互換のローカルクライアントは
  決して触らない。
- **セッション export / import がシグネチャを保持する。**
  Gemini 3 会話中に export されたセッションは、reasoning
  continuity を失わずに re-import できる。

非目標:

- (不透明な) シグネチャバイト列を UI に表示する。
- シグネチャの内容をデコード・検査する。Vertex ドキュメントは
  シグネチャが不透明な継続トークンであることを明記しており、
  bytes-in / bytes-out で扱う。
- シグネチャペイロードサイズの最適化。Gemini 3 のシグネチャは
  Part 1 つにつき数十バイト規模。1 ターン 10 Part × 100 ターン
  でも < 100 KB、メッセージ本文・添付に比べて無視できる。

---

## 3. レスポンス内のシグネチャ配置

Vertex AI thought-signatures ドキュメントによれば、
`Part.ThoughtSignature` (`genai/types.go:1397`) は単一レスポンス
内で **3 種類の Part shape** にセットされうる:

| Part 種別 | 識別フィールド | 備考 |
|----------|--------------|------|
| Thought | `Thought == true` | 内部推論。`vertex.go:303` で現在ユーザー向けコンテンツから除外。 |
| Text | `Text != ""`, FunctionCall なし | アシスタントの散文応答。`parseResponse` がこれらを `m.Content` に連結。 |
| Function call | `FunctionCall != nil` | ツール呼び出し。`parseResponse` がこれらを `m.ToolCalls` に取り込み。 |

`ThinkingConfig.IncludeThoughts = false` (`vertex.go:47-49`) を
設定しており、これは *大半の* thought パートを抑制する — が、
thought 風の Part が依然到着する可能性あり (既存コードコメントは
Gemini 2.5 Flash がそのフラグ off でも "THOUGHT" プレフィックス
付きテキストを返した事例を記録)。Gemini 3 は thought テキストが
露出しない場合でも opaque-signature を持つ Part を返す可能性が
ある。

報告されたエラーは特に function-call シグネチャ欠落だが、
ドキュメント上の契約は「全 Part のシグネチャを返送せよ」。
function-call ケースだけ設計すると、テキスト・thought シグネチャ
ケースで同種のバグが潜伏する。

---

## 4. 設計

### 4.1 データモデル — 採用案 (Option A)

既存構造体に最小限のフィールドを追加:

```go
// llm.ToolCall
type ToolCall struct {
    ID               string
    Name             string
    Arguments        string
    ThoughtSignature []byte `json:"thought_signature,omitempty"`
}

// llm.Message
type Message struct {
    // ... 既存フィールド ...

    // ThoughtSignatures は Gemini 3+ の不透明な per-part 継続
    // トークンをキャプチャするフィールド群。キャリアした
    // Part 種別ごとにグループ化される。各スライスは元 Part
    // の到着順を保持。Vertex バックエンド専用、他バックエンドは
    // 無視。
    ThoughtSigsThought [][]byte `json:"thought_sigs_thought,omitempty"`
    ThoughtSigText     []byte   `json:"thought_sig_text,omitempty"`
}
```

3 つのバケット:

1. **Function-call シグネチャ** — `ToolCall` ごとに 1 つ、位置で
   ペアリング。最も一般的、本 ADR を発火させたケース。
2. **Thought-part シグネチャ** — `Message` 上のスライス。単一
   assistant ターンに複数の thought part が含まれうるため。
   thought *テキスト* は既存プライバシーフィルタリングに準拠して
   破棄するが、シグネチャはキャプチャする。
3. **Text-part シグネチャ** — 単一値。既存パーサが全 text part
   を `m.Content` に連結するため、最後に観測した text-part
   シグネチャのみ保持。(1 ターン内の複数 text part は稀;
   問題が観測されたら `[][]byte` スライスに昇格させる。)

### 4.2 キャプチャ (parseResponse, vertex.go:279)

既存通り Part を順に走査。各 Part について:

- `part.Thought == true` → `ThoughtSignature` が非空ならローカル
  `thoughtSigs [][]byte` に追加。続けて `continue` (既存のフィル
  タ挙動は維持)。
- `part.Text != ""` → text を `textParts` に追加 (既存);
  `ThoughtSignature` が非空なら `textSig` を上書き。
- `part.FunctionCall != nil` → `ToolCall` を既存通り構築;
  `tc.ThoughtSignature = part.ThoughtSignature` をセット。

`thoughtSigs` と `textSig` を返却 `Response` に添付。Response
オブジェクトはセッション状態に書き込まれる assistant `Message`
になるので、シリアライズ経由でフィールドが永続化される。

### 4.3 再生 (buildContents, vertex.go:164)

`RoleAssistant` の model Content を emit する際:

```go
case RoleAssistant, RoleReport:
    if len(m.ToolCalls) > 0 {
        var parts []*genai.Part

        // 1. キャプチャした thought シグネチャを thought part
        //    として再生。Vertex ドキュメントによれば空テキストは
        //    許容; load-bearing はシグネチャ本体。
        for _, sig := range m.ThoughtSigsThought {
            p := &genai.Part{Thought: true, ThoughtSignature: sig}
            parts = append(parts, p)
        }

        // 2. text content をシグネチャ付き (あれば) で再生。
        if m.Content != "" {
            p := genai.NewPartFromText(m.Content)
            if len(m.ThoughtSigText) > 0 {
                p.ThoughtSignature = m.ThoughtSigText
            }
            parts = append(parts, p)
        }

        // 3. 各 function call をシグネチャ付きで再生。
        for _, tc := range m.ToolCalls {
            var args map[string]any
            if tc.Arguments != "" {
                _ = json.Unmarshal([]byte(tc.Arguments), &args)
            }
            if args == nil { args = map[string]any{} }
            p := genai.NewPartFromFunctionCall(tc.Name, args)
            if len(tc.ThoughtSignature) > 0 {
                p.ThoughtSignature = tc.ThoughtSignature
            }
            parts = append(parts, p)
        }

        contents = append(contents, &genai.Content{
            Role:  genai.RoleModel,
            Parts: parts,
        })
    }
```

順序: thoughts → text → function calls。典型的な Gemini 3 Part
順序に一致; 稀な並び替えは保持されない (§6 リスク参照)。

### 4.4 永続化

新フィールドは `Message` に乗り、`memory.Record` 経由で
セッションストレージおよび `.shellagent` export bundle に
シリアライズされる。Go の encoding/json は `[]byte` を base64
で emit する — 自動、カスタム marshaller 不要。`omitempty` で
旧セッションと非 Gemini 3 会話は clean に (JSON にゼロバイト
追加)。

Inbound 側 (Load / Import): 欠落フィールドは nil にデフォルト。
旧セッションを Gemini 3 で再開すると空シグネチャ状態で再開する
— 正しい挙動 (エージェントが再プロンプトを発し、Gemini 3 は
そのターン以降から新規シグネチャ emit を開始)。

### 4.5 ChatStream

ストリーミング経路 (`ChatStream`, vertex.go:65) は現状テキストの
みを返す; tool call を表面化しない。エージェントループは tool が
関与する場合 `Chat` (非ストリーミング) を使うので、シグネチャ配線
は専らこちらに属する。ChatStream は本 ADR の影響を受けない。

---

## 5. テスト

### 5.1 ユニットテスト

`vertex_test.go` (または新規 `vertex_thought_signatures_test.go`)
に追加:

- **`TestParseResponse_CapturesFunctionCallSignature`** —
  `ThoughtSignature: []byte{0x01, 0x02, 0x03}` を持つ function-
  call part を 1 つ含む `genai.GenerateContentResponse` を合成;
  返却 `ToolCall.ThoughtSignature` が入力と等しいことを assert。
- **`TestParseResponse_CapturesTextSignature`** — シグネチャ付き
  text-only part; `result.ThoughtSigText` を assert。
- **`TestParseResponse_CapturesThoughtSignatures`** — 異なる
  シグネチャを持つ 2 つの thought part (`Thought: true`);
  `result.ThoughtSigsThought` の長さ 2 と順序を assert。
- **`TestBuildContents_ReplaysFunctionCallSignature`** —
  シグネチャ付き tool call を 1 つ持つ `Message` を入力;
  結果 `[]*genai.Content` に function-call Part の
  `ThoughtSignature` が一致する model Content があることを
  assert。
- **`TestBuildContents_ReplaysThoughtSignatures`** —
  `ThoughtSigsThought = [[a,b],[c,d]]` の message を入力;
  シグネチャ一致と `Thought: true` を持つ 2 つの thought part が
  順に emit されることを assert。
- **`TestBuildContents_EmptySignaturesNoop`** — 全シグネチャ
  フィールド空の入力; emit された Content に thought part が
  なく、function-call part の `ThoughtSignature` がゼロ値である
  ことを assert。Gemini 2.5 互換の regression テスト。

### 5.2 統合

ネットワークなしで完全な Vertex tool-use loop をモックするのは
非現実的。手動 smoke が必要 (§8 ロールアウト)。

### 5.3 セッションラウンドトリップ

- **`TestMessageJSONRoundTrip_PreservesSignatures`** in `llm/`
  (or `memory/`): 3 シグネチャバケット全てを持つ `Message` を
  JSON にマーシャル、アンマーシャル、比較。base64 エンコードが
  対称であることを assert。

---

## 6. リスクと未解決事項

- **再 emit 時の Part 順序。** Vertex ドキュメントは、再生時に
  *厳密な* 元 Part 順序がモデルに要求されると明示していない。
  我々の実装は canonical な thoughts → text → function-calls
  順で emit する。Gemini 3 が text と function-call の相対位置
  (例: 2 つの function call の間に text が出現) に敏感である
  ことが判明したら、この設計はその順序を失い、応答が微妙になる
  可能性。緩和策: 観測されたら Option B (parts-array スナップ
  ショット, §7 参照) にエスカレート。
- **`ChatStream` の乖離。** ストリーミングはシグネチャを
  キャプチャしない (genai SDK のストリーミング iterator
  サーフェスは per-part でシグネチャを clean に露出しない)。
  当面 ChatStream は tool 不使用の会話応答にのみ使われるため
  シグネチャは無関係。tool use を ChatStream に拡張する場合は
  再検討。
- **旧セッションとの互換性。** 本 ADR 以前に記録されたセッション
  にはシグネチャがない。そのようなセッションをロードして Gemini
  3 に対して継続すると、次の tool-use ラウンドで失敗する —
  履歴 assistant ターンにシグネチャがないため。**緩和策**:
  ドキュメント化された制約 — 旧セッションを Gemini 3 へ移行する
  際は会話を再開する。自動 truncation は侵襲的すぎるとして却下。
- **シグネチャ永続化のプライバシー含意。** シグネチャは不透明
  blob だが推論状態をエンコードする。セッション JSON /
  `.shellagent` export に永続化することは、export を読む人が
  それらを見ることを意味する。プライベートセッション (ADR-0003
  準拠) はユーザーデータディレクトリ内に保持; シグネチャ内容
  について追加保証は行わない。
- **コンテキストウィンドウコスト増加?** リクエストごとに再生
  されるシグネチャはプロンプトトークンを消費する (モデルは
  エンコードされたシグネチャを input 予算に課金)。Google ドキュ
  メントからのベスト推定は per-signature 数トークン; 典型的な
  会話深度ではコンテキスト予算の < 1% のコスト。最適化しない。

---

## 7. 却下した代替案

### 7.1 Option B — フル Parts 配列スナップショットを保存

3 つの per-field シグネチャバケットの代わりに、順序付き
`AssistantParts []AssistantPart` を保存。各 part は
`{Kind, Text, ToolCall, Signature}` を保持。parseResponse は
配列をそのまま構築; buildContents は iterate して再構築する。

v0.6.2 では却下:

- 世に出回っているすべてのシリアライズ済み `Message` に影響
  するより大きなデータモデル変更。
- §6 で言及された順序リスクは、報告された Gemini 3 レスポンス
  では観測されていない; canonical な thoughts → text → calls
  順は十分と見られる。
- 後で容易に移行可能 — Option A の 3 フィールドは Option B の
  配列から derive 可能 — ので選択は可逆。

### 7.2 function-call シグネチャのみ修正

`ToolCall.ThoughtSignature` を追加し、text / thought シグネチャ
は無視。これは報告されたエラー文言に対する *最小* 修正。

却下:

- 報告されたエラーは初回遭遇; 次の Gemini 3 会話で text- または
  thought-シグネチャ失敗が出れば別の追加修正が必要になる。
- 3 ロケーション全てをカバーするデータモデル拡張は trivial —
  1 フィールドより 3 フィールド。
- 結局使わないシグネチャをキャプチャするパフォーマンスコストは
  ゼロ。

### 7.3 Gemini 2.5 に留まる

Vertex モデルのデフォルトを Gemini 2.5 に pin し、Gemini 3 を
「tool use 非対応」と文書化する。

却下。ユーザーは Gemini 3 の能力を活用するため明示的に移行
した; アップグレード拒否はその目的に反する。Gemini 2.5 は
end-of-life に近づきつつある (`feedback_gemini3_migration` は
Gemini 2 → 3 移行期限を 2026-10-16 以降と記録)。

### 7.4 リトライ時に tool call を strip

"thought_signature" を含む 400 が出たら、assistant ターンの
tool call を削除して retry。粗野 — モデルの prior tool plan が
失われ、無限ループの可能性。ハックとして却下。

---

## 8. 互換性 & ロールアウト

- LLM 観測可能: なし。シグネチャはサーバー側不透明状態;
  ユーザー可視チャットコンテンツは変化なし。
- API: `ToolCall` と `Message` に新規 optional フィールド追加。
  `[]byte` 型は `omitempty` 下で base64 エンコード JSON 文字列に
  シリアライズされる。既存シリアライズ済みセッションおよび
  export は clean にパースされる (フィールドは nil にデフォル
  ト)。
- Bindings / frontend: 変更なし。
- ローカルバックエンド: 変更なし; シグネチャフィールドは無視。
- セッション export / import: シグネチャがバンドルと一緒に
  伝播。Vertex アクセスを持つユーザーであればクロスマシン
  import が動作する。

### ロールアウト

ADR-0008 (mcp-abort) と同じパターン、bisect しやすい
コミットに分割:

1. `feat(llm): preserve Gemini 3 thought signatures on ToolCall and Message`
   — backend.go フィールド追加 + JSON ラウンドトリップの
   ユニットテスト。
2. `feat(llm/vertex): capture and replay Gemini 3 thought signatures`
   — vertex.go の parseResponse キャプチャ、buildContents 再生、
   および §5.1 の vertex_test.go アサーション。
3. `docs: describe Gemini 3 thought-signature preservation`
   — CHANGELOG v0.6.2 entry + AGENTS / README pointer + INDEX
   ADR row。
4. `chore: release v0.6.2`。

タグ前に maintainer 側の手動 smoke が必要: Vertex モデル
設定を Gemini 3 系 model ID に切り替え、複数ツールを呼ぶ質問
(元々失敗した `weather` フロー) をプロンプト、エージェント
ループが 400 なしで完走することを確認する。

# セッション復元時のツールイベントバブル復活

## ステータス

設計書 — 承認後に実装。

## 課題

チャット応答の中でツール呼び出しが発生すると、ライブ UI では各
呼び出しが `tool-event` バブル（ツール名 + ステータス running →
success / error）として表示される。しかしセッションを切り替えて
過去会話を復元すると、それらのバブルは消える:

```go
// bindings.go LoadSession
case "tool":
    continue
```

つまり復元したセッションは、ユーザ／アシスタントの平文だけが
並ぶスレッドに見え、活動の文脈は完全に失われる。グラフ生成や
SQL 実行、サンドボックスでのオブジェクト登録を含むセッションでは
履歴の意味が大きく欠落する — どのツールが何をどの順で実行し、
エラーがあったかどうかをユーザは追えなくなる。

復元に必要な情報の **ほとんどは既に session に入っている**。tool
turn の `memory.Record` は次を保持する:

- `Role: "tool"`
- `ToolName` — 実行されたツール名
- `ToolCallID` — Vertex/OpenAI のツールコール対応 ID（復元では
  使わない）
- `Content` — ツール結果テキスト
- `Timestamp`

不足しているのは **ステータス**（success / error）。ライブ実行時は
`executeTool` の戻り値 `ActivityEventStatus` で分類しているが、
これは活動イベントには流すものの **永続化されていない**。

## ゴール

- 復元したセッションでも、ライブと同じ `tool-event` バブルが
  同じ順序で、正しい success / error の見た目で表示される
- 新規セッションは正確に再現できるよう、必要な情報を永続化する
- 既存（ディスク上の）セッションは、ステータス情報がなくても
  破綻なく復元される — デフォルトは `success`（多数派）
- `Cancel` で現れる「Cancelled」結果も今と同じく無事に表示される
  — 三つ目のステータスは導入しない

## 非ゴール

- 復元時に *running* 状態を再生しない（実際には進行していない
  のに「実行中」が固まったように見えるのは誤解を招く）。復元時は
  必ず `success` か `error` で終端
- 復元バブルにツール **引数** を表示しない（ライブのバブルも
  ツール名のみなので揃える）
- ツール呼び出し付き assistant turn の `progressTool`「思考中」
  バナーの再現はしない（一時的なものとして設計されている）
- ツール **結果** をバブルに添付しない（ライブでは LLM だけが見る、
  ユーザに見せるのはバブルだけ。復元も同じ）

## 設計

### 1. ツール結果のステータスを永続化する

`memory.Record` に `Status` フィールドを追加:

```go
// memory/memory.go
type Record struct {
    // … 既存フィールド …
    Status string `json:"status,omitempty"` // Role == "tool" の時のみ
}
```

許容値: `"success"`, `"error"`。空文字列は本変更前の旧レコード扱いで、
読み出し側で `success` に解釈する。

`AddToolResult` のシグネチャ更新:

```go
// memory/memory.go
func (s *Session) AddToolResult(callID, toolName, result, status string)
```

agent ループで既に持っている値を渡す:

```go
// agent/agent.go
result, status := a.executeTool(ctx, tc)
a.session.AddToolResult(tc.ID, tc.Name, result, string(status))
```

ここの `status` は `tool_end` 活動イベントに使っている
`ActivityEventStatus` と同じ値 — 同一ソースなので食い違いの
リスクなし。

### 2. ステータスをフロントエンドへ流す

`MessageData` は Wails 境界を越えるバブルの形。optional な status
を追加:

```go
// bindings.go
type MessageData struct {
    Role      string `json:"role"`
    Content   string `json:"content"`
    Timestamp string `json:"timestamp"`
    Status    string `json:"status,omitempty"`
}
```

TS 側も同じ:

```ts
// frontend/src/types.ts
export interface MessageData {
    role: string;
    content: string;
    timestamp: string;
    status?: 'success' | 'error';
}
```

### 3. LoadSession で tool レコードをバブルに変換

`continue` を構築済み `tool-event` 行に置き換え:

```go
// bindings.go LoadSession
case "tool":
    status := r.Status
    if status == "" {
        // 本変更前のレコード。エラーを暗黙化させたくないので
        // success にデフォルトする
        status = "success"
    }
    msgs = append(msgs, MessageData{
        Role:      "tool-event",
        Content:   r.ToolName,
        Status:    status,
        Timestamp: r.Timestamp.Format("15:04:05"),
    })
```

`ToolName` が空のレコード（旧デバッグデータ等）は空バブルを描画
しないよう `continue` でスキップ。

### 4. フロントエンドのマッピング

`App.tsx` は既に `LoadSession` の `MessageData[]` から `messages`
（`ChatMessage[]`）を組み立てている。そのマップで `status` を通す
だけ。`MessageItem` は既に `ChatMessage.status` で `tool-event`
を描画しているので、コンポーネント側の変更は不要。

## 後方互換性

- `Record.Status` は `omitempty` — 既存の chat.json は `Status==""`
  で読まれ、`success` 扱いになる
- `AddToolResult` のシグネチャ変更は呼び出し元が agent ループに
  限定されている。`Record` を直接組み立てるテストは 1 フィールド
  追加で対応（後述）
- ライブ動作は変わらず — `tool_end` 活動イベントを駆動している
  status と同じものが永続レコードにも乗るだけ

## ファイル単位の変更

| ファイル | 変更 |
|---------|------|
| `app/internal/memory/memory.go` | `Status` フィールド追加、`AddToolResult` シグネチャ変更 |
| `app/internal/memory/memory_test.go` ほか呼び出し箇所 | テストの初期化を 1 引数追加 |
| `app/internal/agent/agent.go` | `executeTool` の status を `AddToolResult` に渡す |
| `app/bindings.go` | `MessageData` に `Status` 追加、`case "tool"` をバブル構築に置き換え |
| `app/frontend/src/types.ts` | `MessageData` に optional `status` 追加 |
| `app/frontend/src/App.tsx` | `LoadSession` 後の `MessageData[]` → `ChatMessage[]` マップで `status` を通す |

新規ファイル・ダイアログは無し。

## テスト戦略

### バックエンド (Go)

- **`memory_test.go`**:
  - `AddToolResult(_, _, _, "success")` → レコードが `Status="success"`
    で `MarshalJSON` / `UnmarshalJSON` を round-trip
  - 旧 chat.json（status フィールド無し）を読み込み、`Status == ""`
    で正しくデコード
- **`bindings_test.go`**（or LoadSession の既存テスト拡張）:
  - `Role:"tool", Status:"success"` のレコードが復元時に
    `Role:"tool-event"`, `Content:<toolName>`, `Status:"success"`
    に展開
  - `Status:""` → 復元時に `Status:"success"`
  - `Status:"error"` → そのまま伝搬

### 手動

1. 成功ツールと意図的に失敗するツール（例: 存在しない pip パッケージの
   インストール）を含む turn を実行
2. アプリを再読み込み、そのセッションへ切替
3. 両バブルが同じ順で再表示され、success / error の見た目が正しい
   こと（失敗側は赤）を確認
4. **本変更以前** に作ったセッションをロード。ツールバブルが
   success スタイル（旧データのデフォルト）で出ること

### 検証コマンド

```sh
cd app
go test -tags no_duckdb_arrow ./internal/memory/... ./...
make build
```

E2E: ビルド済み .app を起動 → ツール呼び出しを含むセッションを
ロード → チャットスレッドに `tool-event` バブルが見えること。

## 明示的に非対象

- ツール **引数** の永続化（より情報量の多い復元バブル）
- 復元時の `running` 状態再生
- ユーザにツール結果テキストを見せる（ライブも復元も対象外）
- Cancel 状態の専用扱い — 「Cancelled」content 文字列はそのまま
  通る、ステータスは `success` のまま（ツールが失敗したのではなく、
  ユーザが中断したものなので）

## リスク

- **テストの掃き出し**: `AddToolResult` 呼び出し箇所の引数追加。
  コンパイラが拾うので機能リスクは無い。PR diff が少し大きくなる
  程度
- **旧データ**: `Status==""` は `success` にデフォルトする。
  過去に失敗したツールは緑表示になる。受け入れる — 失敗を error
  にデフォルトする方が成功（多数派）の誤表示を増やすため
- **スキーマ膨張**: `memory.Record` にフィールドが 1 つ増える。
  既に Vertex 用（`ToolCalls`）、レガシー用（`SummaryRange`）、
  画像用フィールドが共存しているので、`omitempty` の文字列 1 つ
  追加は既存ポリシーと整合

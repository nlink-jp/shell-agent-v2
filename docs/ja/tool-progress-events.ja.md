# `tool_progress` Activity イベント — 設計ノート

**ステータス:** 設計ドラフト (2026-05-07); 承認待ち。
**対象バージョン:** v0.4.1 (v0.4.0 の次)
**Issue:** [#5](https://github.com/nlink-jp/shell-agent-v2/issues/5) — analyze-data の進捗バブルが "実行中" のまま固まる

本ノートは agent の activity event プロトコルに対する小規模な
ターゲット変更を仕様化する。長時間実行ツールが進捗ティック
ごとに新しい "実行中" バブルを発出するのではなく、**1 つの
チャットバブルを in-place で更新できる** ようにする。

直接の動機は `analyze-data`: sliding-window summarizer が
window ごとに `tool_start` を 1 つ出すが、それら sub バブルに
対応する `tool_end` が決して出ないため、各 window が "実行中"
の pill を恒久的にチャットペインに残してしまう (#5)。

---

## 1. 目的

- **#5 の修正** — `analyze-data` の sub 進捗バブルがツール
  完了後に固まらないようにする。
- **視覚ノイズ削減** — 複数 window の analyze-data は 1 つの
  バブルが in-place 更新される (N 個積み上がらない)。
- **再利用可能な仕組み** — 将来の長時間ツール (大規模
  `query-sql`、複数ファイルのサンドボックスジョブ等) が
  bespoke な UI コードなしに同じプロトコルを使える。
- **ワイヤレベルの後方互換** — 新イベント型は追加のみ; 旧
  frontend は無視するだけ、旧 backend は何も emit しない。

---

## 2. ワイヤフォーマット

### 2.1 新イベント型

```
ActivityEvent.Type = "tool_progress"
```

使用フィールド:

| フィールド | 必須 | 意味 |
|------------|------|------|
| `Type` | yes | リテラル `"tool_progress"` |
| `ToolCallID` | yes | **親** ツール呼出の ID — 親の `tool_start` / `tool_end` と同じ値。frontend がバブルを特定するために使用。 |
| `Detail` | yes | バブルの新しい表示テキスト。前の `Detail` を in-place で置換。 |
| `Status` | n/a | 常に空。Status 遷移は引き続き `tool_end` 経由。 |

### 2.2 ライフサイクル

3 window 走る `analyze-data` 呼出の例:

```
tool_start    detail="analyze-data"                tool_call_id=tc-1
tool_progress detail="analyze-data — window 1/3"   tool_call_id=tc-1
tool_progress detail="analyze-data — window 2/3"   tool_call_id=tc-1
tool_progress detail="analyze-data — window 3/3"   tool_call_id=tc-1
tool_end      detail="analyze-data"  status=success tool_call_id=tc-1
```

バブルのライフサイクル: `running ("analyze-data")` → 現在の
window を示すよう 3 回更新される → `success ("analyze-data —
window 3/3")`。最終テキストは最後の `tool_progress` が設定した
もの; `tool_end` はテキストを親名にロールバックしない。

(完了後にバブルが単に `"analyze-data"` と表示される方を好む
場合、agent は `tool_end` の前に `Detail: "analyze-data"` の
`tool_progress` を 1 回 emit すればよい。本範囲外; 視覚的
デフォルトは「最後に報告されたものを表示」。)

---

## 3. Backend 変更

### 3.1 `internal/agent/agent.go`

- struct コメントに `ActivityEvent.Type == "tool_progress"` を
  記述。
- `emitActivity` のコード変更なし — 全イベント型を opaque に
  forward する。

### 3.2 `internal/agent/tools.go` — `toolAnalyzeData`

window ごとの `tool_start` callback (line 827-831) を、親の
`tool_call_id` を持つ `tool_progress` に置き換え:

```go
// 親 ID を捕捉。これにより progress イベントが新規バブルを
// spawn せず、既存の "analyze-data" バブルをターゲットにする。
parentToolCallID := /* … §3.3 参照 … */

result, err := summarizer.Analyze(ctx, args.Prompt, rows, func(idx, total int) {
    a.emitActivity(ActivityEvent{
        Type:       "tool_progress",
        Detail:     fmt.Sprintf("analyze-data — window %d/%d", idx+1, total),
        ToolCallID: parentToolCallID,
    })
})
```

### 3.3 親 `ToolCallID` の伝播

現在、親 `tool_start` は `internal/agent/agent.go:1498` で
`agentLoop` の中から発火される。そこでは `tc.ID` がスコープ
内にある。ツール関数自体 (`toolAnalyzeData`) は現状 call ID
を受け取らない。

選択肢:

- **A.** すべてのツール関数に `tc.ID` を新規引数として渡す。
  もっともクリーンな API だが、`tools.go` の全関数 (~30 個)
  のシグニチャを触る。
- **B.** ツール呼出前に agent struct にアクティブな tool call
  ID を格納し、呼出後に clear。ツール関数は
  `a.activeToolCallID()` accessor 経由で読む。`agentLoop` +
  小さな accessor へのローカル変更; ツールシグニチャは安定。

**推奨: B**。ここでの目的は #5 を修正し、再利用可能な進捗
メカニズムを導入することであって、30 個のツールシグニチャを
リファクタすることではない。「アクティブな tool call」という
概念は実在する (Idle/Busy 保証により agent ごとに同時実行は
1 ツールのみ) ので、struct に格納するのは正直な設計。将来
tool-call コンテキストを first-class にする必要が出てきたら、
accessor は適切な context-passed value へ進化させる清潔な
seam として残る。

実装スケッチ:

```go
// agent.go (Agent struct)
activeToolCallID string  // agentLoop が設定、tool 関数が読む

// agentLoop, ツールディスパッチ呼出直前:
a.mu.Lock()
a.activeToolCallID = tc.ID
a.mu.Unlock()
defer func() { a.mu.Lock(); a.activeToolCallID = ""; a.mu.Unlock() }()
```

備考: `a.mu` は state machine を guard する既存ロック。極小
field-write のために保持しても問題なし。

---

## 4. Frontend 変更

### 4.1 `frontend/src/App.tsx` — activity event handler

3 つ目の `else if` 分岐を追加 (`tool_end` と `tool_start` の間;
順序は問わない):

```typescript
} else if (data.type === 'tool_progress') {
    // マッチする running tool-event を in-place 更新。
    // tool_call_id でマッチさせて、複数並行ツール (将来:
    // 現状ではない) が cross-contaminate しないようにする。
    if (!data.tool_call_id) return
    setProgressTool(data.detail || '')
    setMessages(prev => {
        let idx = -1
        for (let i = prev.length - 1; i >= 0; i--) {
            const m = prev[i]
            if (m.role === 'tool-event' && m.status === 'running' && m.toolCallId === data.tool_call_id) {
                idx = i
                break
            }
        }
        if (idx === -1) return prev
        const next = prev.slice()
        next[idx] = {...next[idx], content: data.detail || ''}
        return next
    })
}
```

### 4.2 防御的注意点

- イベントに `tool_call_id` が無い場合 (legacy backend)、
  分岐は no-op。古い bundle に対するリグレッションなし。
- ID にマッチする running バブルが無い場合 (バブルが既に
  success/error に遷移した後にイベントが届いた場合)、分岐は
  no-op。設計上 race-safe。
- footer の `progressTool` インジケータもバブルと同時に更新
  され、status-bar テキストも最新 window を反映する。

---

## 5. 変更しないもの

- `tool_start` / `tool_end` の semantics は無変更。
- `chat.json` records への `tool_call_id` 永続化 — `tool_progress`
  は純粋に transient; 何も記録されない。
- 新イベントを使わない他ツールはこれまでと完全に同じ動作。

---

## 6. 検証

### Unit
- 新しい agent unit テストは厳密には不要 (本変更はフィールド
  代入 + イベント emit)。`agent_test.go` で
  `activeToolCallID` がツール呼出中に設定され、後で clear
  されることを assert する小規模テストを追加することは可能だが、
  これは bookkeeping であり、テスト追加コストが利得を上回る。

### 手動 smoke
1. 複数 window の `analyze-data` をトリガーするだけの行数を
   ロード (sliding-window summarizer は 1 window に収まらない
   程度のテーブルサイズが必要)。
2. `analyze-data` を呼ぶプロンプトを送信。
3. **修正前:** N 個の "analyze-data (window k/N)" バブルが全部
   "実行中" のまま永遠に残る。
4. **修正後:** 1 つの "analyze-data" バブルが進捗に応じて
   テキストを "analyze-data — window k/N" に更新し、最後の
   window のテキストを表示したまま success 状態で終了。
5. 他のツール (`load-data`, `query-sql` 等) は通常の start/end
   バブルライフサイクルが無変更で表示される。

---

## 7. 却下した代替案

- **各 sub バブルに paired `tool_end` を emit** (issue triage の
  Option A)。backend 変更は小規模だが、複数 window 実行後も
  チャットペインに N 個の独立したバブルが残る — 結局ノイジー、
  結局無駄。他の長時間ツールに一般化しない。
- **footer status-bar のみ更新、チャットバブル更新なし。**
  本変更が部分的に対象としている in-bubble の可視性 (ユーザー
  は window カウントを transient な footer だけでなくチャット
  履歴で見たい) を失う。
- **進捗用に新しい永続化 record タイプ。** やりすぎ; 進捗は
  定義上 transient。チャット履歴は最終的なツール結果を持つ
  べきで、進捗トレースを持つべきではない。

---

## 8. 範囲外

- ツール間の並行性 (複数ツール同時実行) — agent は今日時点で
  単一ツール同時; 将来変わったら `activeToolCallID` はスカラ
  ではなくスタックになるが、ワイヤフォーマット (イベントごとの
  `tool_call_id`) は既にこれに対応している。
- セッション再ロード時の進捗 replay 用永続化 — 範囲外; 永続化
  された tool-event バブルは最終ステータスのみ持つ、これが
  適切な詳細レベル。
- 同じプロトコルの非ツール agent 活動 (memory 抽出進捗、
  サマライゼーション進捗等) への一般化 — 後で同じイベント型
  の下で追加可能。

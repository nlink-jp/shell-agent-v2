# エージェントループのレジリエンス — 設計ドキュメント

> 日付: 2026-05-01
> ステータス: ドラフト（実装未着手）
> 範囲: agent loop / LLM 呼びだしの seam を共有する 2 つの
> 隣接した観測 / 自己修正機能

## 1. 問題

TODO ファイルに既に記載されている、実 Vertex セッションで
観測したが未対応のパターン 2 つ:

1. **同じエラーで詰まるループ。** Vertex (gemini-2.5-flash)
   がエスケープされていないマルチライン文字列リテラルを含む
   Python を生成、`SyntaxError: unterminated string literal`
   に遭遇、その後同じ broken パターンで `sandbox-run-python`
   を 6 ラウンド連投、`max rounds (10) reached` でセッション
   終了。各リトライは些細な変更だけで、モデルは本当の原因に
   気付かなかった。

2. **サイレントな 429 backoff。** 同セッションで会話途中に
   `RESOURCE_EXHAUSTED` が 2 回発生。リトライ層は正しく処理
   （backoff 4.8s と 5.7s、両方 attempt 2 で成功）したが、
   UI には何も出ず、ユーザは「いつもより遅い Thinking…」
   としか分からなかった。リトライ進行中なのか hang なのか
   判断する手段がない。

両方とも agent-loop / LLM-call seam の観測ギャップ。core
control flow は変えず、明確に定義された 1 箇所にフィード
バックフックを追加するだけ。

## 2. ゴール / 非ゴール

### ゴール

1. LLM が同じツールを 3 ラウンド連続で `status=error` で
   呼んだら、次の LLM メッセージリストに**transient な
   修正ヒント**を注入してモデルに「詰まっている」ことを
   気付かせる
2. リトライ層が backoff に入った（transient failure 検出、
   次の attempt 前の待機）とき、ユーザに見える短い
   ステータスバッジを表示し、遅いラウンドが backoff
   なのか hang なのか判別可能にする
3. どちらも persisted state は変更しない。両方とも既存
   フロー上に乗せる観測フック

### 非ゴール

- **新リトライポリシーなし。** 既存の指数バックオフ + 3
  attempt キャップそのまま
- **新 max-round 処理なし。** ハードコードされた 10
  ラウンドキャップは別 TODO、本書範囲外
- **ロールバック / state 変更なし。** 修正ヒントは 1 度
  きりのアドバイザリ、次のコールはモデルが駆動
- **新 UI パネルなし。** backoff シグナル用に footer-status
  に小バッジ追加のみ

## 3. Feature 1 — ループ検知と修正ヒント

### 3.1 検知

`Agent` に小さなリングバッファを追加:

```go
type toolCallTrace struct {
    Name   string
    Status ActivityEventStatus
}

// recentToolCalls は直近 RecentToolWindow (3) 個を保持、
// "同じツール、すべて error" 連続を検出する
recentToolCalls []toolCallTrace
```

各コールの status 計算後に `toolCallTrace{tc.Name, status}`
を push、`RecentToolWindow = 3` で cap。古いエントリは
押し出される。

ループは以下の条件で検出:
- `len(recentToolCalls) == RecentToolWindow` かつ
- 全エントリで `Name` が同一 かつ
- 全エントリで `Status == ActivityStatusError`

### 3.2 ヒント注入

ループ条件が次のラウンドの先頭で hit したら（次の LLM
コールを組み立てる前）、`buildMessagesV2` が生成した
`messages` slice に transient な system message を prepend。
**ヒントは `session.Records` には追加しない** — 1 度きり
の nudge であり、メモリではない。

ヒント本文（英語、LLM はマルチリンガルなので UI ロケール
無関係に通る）:

> System note: you have called `<tool>` three times in a row
> and each call returned an error. Stop retrying with minor
> variations. Try a substantively different approach — for
> example, write the input to a `/work` file first and
> inspect it with `sandbox-run-shell` before re-running, or
> abandon this branch and ask the user for clarification.

ツールファミリ別バリアントは後で追加可能。v1 は generic
ヒントのみ。

ヒントは連続エラー stretch ごとに**最大 1 回**発火。
発火後はバッファをリセット、よって直前 2 つと同一の 3 回目
コールでも 4 回目では再発火しない — 次のトリガには新しい
stretch が必要。

### 3.3 テレメトリ

ヒント注入時に INFO で 1 行ログ:

```
[INFO] agentLoop: loop-detection: <tool> hit error 3× in a row, injected corrective hint
```

`app.log` で postmortem できるし、最終的に回復しても
モデルが misbehave していたことの手がかりになる。

### 3.4 エッジケース

| ケース | 挙動 |
|---|---|
| MITL 拒否は今エラー扱い | OK — 3 回連続拒否自体がヒント発火に値する強いシグナル |
| 異なるツールが交互 (A, B, A) | ヒントなし — リングは全 3 つで `Name` 一致を要求 |
| ループ途中でユーザが abort | エージェントループ終了時にバッファリセット |
| 4 ラウンド目で成功 | 古いエントリが押し出される時点でバッファクリア |

## 4. Feature 2 — backoff ステータスの UI 露出

### 4.1 バックエンドフック

`RetryPolicy` にコールバック追加:

```go
type RetryPolicy struct {
    PerRequestTimeout time.Duration
    MaxAttempts       int
    Backoff           backoff.Backoff
    // OnBackoff は backoff 期間ごとに 1 回呼ばれる — つまり
    // attempt N が retryable error で失敗した後、`wait`
    // sleep の前。"rate-limited, retrying" 状態を UI に
    // 出すために使う。任意、nil で OK
    OnBackoff func(attempt int, wait time.Duration, err error)
}
```

`retry.go` の `do` ループ内、`retryable` を判定し `wait`
を計算した後で `r.policy.OnBackoff` を（設定されていれば）
呼ぶ。

### 4.2 エージェントへの配線

`agent.setBackend` が `DefaultRetryPolicy(...)` でポリシーを
組み、加えて以下を配線:

```go
policy.OnBackoff = func(attempt int, wait time.Duration, err error) {
    a.emitActivity(ActivityEvent{
        Type:   "retry_backoff",
        Detail: fmt.Sprintf("attempt %d: %s (waiting %s)", attempt, classifyErr(err), wait.Round(100*time.Millisecond)),
    })
}
```

`classifyErr` は短いユーザフレンドリラベルを生成:
- `Error 429` → `"rate limit"`
- `503` / `unavailable` → `"server unavailable"`
- `context deadline exceeded` → `"timeout"`
- それ以外 → `"transient error"`

### 4.3 フロント処理

`App.tsx` の既存の `agent:activity` リスナーに `'retry_backoff'`
ケースを追加。発火時:

1. Transient な `retryStatus` state（詳細文字列）にセット
2. footer の `input-status-bar` 内、backend バッジと
   message-counts span の間に小バッジとして表示
3. agent が `tool_end` を報告（コールが最終的に返って
   きた、成功/失敗問わず）したか、新しい `tool_start` が
   到着したらバッジクリア

CSS: 1 ルール `.retry-badge`、subtle な warning トーン
（`var(--text-error)` テキストに `var(--bg-msg-error)` 背景、
muted）。既存トークン再利用、新色なし。

### 4.4 エッジケース

| ケース | 挙動 |
|---|---|
| 1 Chat 内で複数リトライ | `OnBackoff` が attempt ごとに発火、UI ラベルが各回更新 |
| リトライ成功 | 成功した Chat が return、次の `tool_end` でバッジクリア |
| リトライ枯渇 | 最終 Chat がエラー return、agent loop の通常エラー経路、次の `tool_start` または session abort でバッジクリア |
| 複数セッション / ユーザ | activity events は今と同様アクティブセッション scope、変更なし |

## 5. テスト計画

### Feature 1

- 単体テスト: agent に合成のツール実行ループ（3 回同じ
  name、すべて error）を流し、次の message-build に
  ヒントが先頭 system entry として渡ることを assert。
  ループが続いてもヒントは 1 度きり発火することを assert
- 手動: sandbox session で Python `SyntaxError` を 3 回
  誘発、`app.log` に loop-detection 行が出ること、LLM
  の次の返答がヒントに言及することを確認（Vertex は
  通常そうする、gemma は微妙）

### Feature 2

- 単体テスト: 偽 backend が初回 `Error 429`、次回成功で
  `WithRetry` を駆動。`OnBackoff` が `attempt=1` で
  ちょうど 1 回呼ばれ、エラーを `"rate limit"` に分類、
  非空の wait duration を露出することを assert
- 手動: 実 Vertex 429（または一時停止テストサーバで
  シミュレート）、失敗から ~0.5 秒以内にバッジ表示、
  コール完了で消えることを確認

## 6. リスクと緩和策

| リスク | 緩和策 |
|---|---|
| ヒント注入で LLM が「ループに入っているようです…」と毎返答開始 | 発火条件のみ注入、system note として（user message ではなく）、1 回発火後クリア。Vertex は短く acknowledge して進む傾向 |
| `OnBackoff` コールバックが `tool_end` と race する goroutine で発火 | backoff は同期的な `Chat()` 内で起こる、`tool_end` は `Chat()` return 後発火、race なし |
| 速いリトライで footer バッジが flicker | バッジはマウント維持、ラベルだけ更新、CSS transition flicker なし |

## 7. 範囲外

- max-rounds キャップの configurable 化（別 TODO、
  loop detection と並んで TODO.md に記載）
- ツールファミリ別ヒントバリアント（次回イテレーション、
  v1 は generic メッセージ）
- リトライ層の pause / resume コントロール
- UI に「時間ごとのエラー」永続パネル

## 8. フェーズ分割

順序付き 2 コミット:

1. **Feature 2（小さい / 観測専用）。**
   `OnBackoff` callback + activity event + footer バッジ。
   agent-control 変更なし、低リスク、診断に有用
2. **Feature 1（LLM 向け）。**
   リングバッファ + ヒント注入 + テレメトリログ。
   LLM に積極的に話しかけるため高リスクだが、ヒントは
   1 度きり、`RecentToolWindow=3` でゲート

各フェーズ後に実セッションで手動検証してから次へ。

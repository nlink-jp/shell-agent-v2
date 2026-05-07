# セッション削除 — UX & 並行性ハードニング

**ステータス:** 設計ドラフト (2026-05-07); 承認待ち。
**対象バージョン:** v0.4.2 (v0.4.1 の次)
**Issue:** [#6](https://github.com/nlink-jp/shell-agent-v2/issues/6) — セッション削除: 確認なし、進行中フィードバックなし、state machine 未統合

本ノートはセッション削除に対する 3 部構成の修正を仕様化する。
削除パスはサイドバー上で唯一の破壊的操作でありながら、確認を
求めず、進捗フィードバックを与えず、agent state machine
(Send / Load / Export / Import を既に serialise しているもの)
で gate されていない。これらどれ 1 つでも単独で defect だが、
合わせると現実のデータ破損を許してしまう (アクティブセッション
を Send 中に削除 → chat.json が部分的に蘇る)。

---

## 1. 目的

- **誤削除なし** — X アイコンへの不意の click が、セッションの
  chat、メモリ、findings、サンドボックスファイル、DuckDB を
  破壊しないようにする。
- **作業の可視化** — 数秒かかる削除 (大規模 objstore、大規模
  `work/` ツリー) は何かが起きていることを表示し、click を
  失ったように見えないようにする。
- **削除中の並行操作なし** — Export / Import / Send / Load を
  保護する同じ busy-gate を Delete にも適用。アクティブ
  セッション削除後、agent はクリーンな post-delete 状態
  (削除済みセッションへの dangling pointer がない、後続 Save
  で復活させない) を保つ。

---

## 2. 現状の並行性ストーリー (バグ)

`bindings.DeleteSession` は入口で `IsBusy()` をチェックする
だけで、削除中に agent state を Busy に **遷移させていない**。
そのため削除中:

- `agent.State == StateIdle`
- `IsBusy()` は false を返す
- 他のエントリポイント (`Send`, `LoadSession`, `NewSession`,
  `ExportSession`, `ImportSession`) は block されない
- frontend は何も起きていることを知らない

具体的な失敗モード:

| # | シーケンス | 結果 |
|---|-----------|------|
| F1 | A を削除 → 削除完了前に user が LoadSession(A) を click | `chat.json` のレース: read が成功 (部分的) するか mid-RemoveAll で失敗するか |
| F2 | アクティブセッション A を削除 → user がチャット入力 → Send | `Send` は `IsBusy` gate を通過、`agentLoop` 実行、`a.session.AddUserMessage` が指したままの `*Session` を変更、末尾の `a.session.Save()` が `sessions/A/chat.json` に書き込み。`Save()` 内の `os.MkdirAll` がディレクトリを再作成。正味効果: セッション A が部分的に populate された dir として "蘇る"、user の意図と切り離され、サイドバーリストとも切り離される。 |
| F3 | A を削除 → ExportSession(A) | per-session ファイルへの同様のレース、export bundle が不完全か mid-stream で失敗する可能性 |
| F4 | 同じセッションの 2 つの同時削除 | 両方が objstore.DeleteBySession + sandbox.Stop + RemoveAll を並行呼出 — 通常無害だが sandbox container 撤去で spurious error が返ることがある。 |

修正は v0.4.0 の Export / Import 作業と同じ形: 操作のライフ
タイムにわたり agent state を Busy に保つ。すべての失敗モード
が消える、なぜなら全競合エントリポイントが `state != Idle`
チェックで早期失敗するから。

---

## 3. 設計

### 3.1 Backend: `agent.DeleteSession`

orchestration を所有する `*Agent` の新メソッド:

```go
// DeleteSession はセッションの per-session ファイル、その
// objstore オブジェクト、その sandbox コンテナを削除する。
// 並行 Send / Load / Export / Import が ErrBusy を返すよう
// 操作中ずっと agent state-machine スロットを保持する。
// sessionID がアクティブセッションを指す場合、agent の
// per-session ポインタ (session, sessionMemory, findings,
// analysis Engine) は dir 削除前に clear/close される; bindings
// 層がこの後に別セッションへの切替 (または新規作成) を担当する。
func (a *Agent) DeleteSession(ctx context.Context, sessionID string) error {
    a.postTasksWg.Wait()
    a.mu.Lock()
    if a.state != StateIdle { a.mu.Unlock(); return ErrBusy }
    a.state = StateBusy
    a.mu.Unlock()
    defer func() { a.mu.Lock(); a.state = StateIdle; a.mu.Unlock() }()

    isActive := a.session != nil && a.session.ID == sessionID
    if isActive {
        if a.analysis != nil { _ = a.analysis.Close() }
        a.session = nil
        a.sessionMemory = nil
        a.findings = nil
    }
    if a.objects != nil { _ = a.objects.DeleteBySession(sessionID) }
    if a.sandbox != nil { _ = a.SandboxStop(ctx, sessionID) }
    return memory.DeleteSessionDir(sessionID)
}
```

備考:
- state guard は Send / Load / Export / Import を既に serialise
  している同じ `Agent.mu`。新規ロックは導入しない。
- ディレクトリ削除前に analysis Engine を Close することで
  DuckDB ファイルハンドルが解放され、含むディレクトリへの
  RemoveAll が開いた `*sql.DB` と争わない。
- `a.session`/`a.sessionMemory`/`a.findings` を nil クリア
  することで F2 (Send 後の Save が dir を蘇らせる) を防ぐ:
  万一 Send が漏れたとしても (state が Busy のため不可能だが)、
  nil session で安全に失敗する。
- `memory.DeleteSessionDir` の error のみ caller に surface
  される; 他 (objstore index save、sandbox stop) は best-effort、
  現在の bindings 層の挙動と同じ。

### 3.2 Bindings 層

`bindings.DeleteSession` は thin pass-through に:

```go
func (b *Bindings) DeleteSession(sessionID string) error {
    return b.agent.DeleteSession(b.ctx, sessionID)
}
```

既存の `IsBusy` early-exit はなくなる — agent メソッド自身の
state-machine gate が単一の真実源、v0.4.0 後の ExportSession /
ImportSession 形と一致。

### 3.3 Frontend: 2 クリック確認 + Deleting 状態

`Sidebar.tsx` に per-row state 2 つ:

```typescript
// Per-row: X が現在 "確認?" 状態のセッション。同時に 1 行の
// み confirm 状態になれる。
const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null)
// Per-row: 削除リクエストが in-flight のセッション。
const [deletingSession, setDeletingSession] = useState<string | null>(null)
```

X 上の click フロー:

1. **Idle** → user が X を click → state は `confirmingDelete = id`
   に。X アイコンが ✓? に変わり、`aria-label="Confirm delete"`。
2. 3 秒タイマー起動; 期限切れで `confirmingDelete = null`
   (X に戻る)。行外の click も clear。
3. **Confirming** → user が再度 ✓? を click →
   `deletingSession = id`、`confirmingDelete = null`。行が
   grey になる; X / ✎ ボタンは disabled; タイトルが
   "Deleting…" に置換 (or 並記)。
4. ハンドラが `Bindings.DeleteSession(id)` を await。resolve
   後、parent (`App.tsx`) がセッションリストを refresh、
   (アクティブセッションが削除された場合) 自動切替 — `App.tsx`
   の既存 `handleDeleteSession` が両分岐を既にカバー済。
5. **Deleting** → ハンドラ完了 → `deletingSession = null`
   (`sessions` が refresh されたので行はリストから消えている)。

`Bindings.DeleteSession` が reject した場合 (例: agent が
post-tasks 実行中で `ErrBusy`)、行の `Deleting…` 状態は clear
され、エラー toast / inline message が surface される。残り
セッションリストは defensive に再 fetch される。

### 3.4 視覚的扱い

- **Confirm 状態** — X グリフを ✓ + 小さな疑問符 superscript
  または `?` suffix で置換; 同幅で行が reflow しない。Tooltip
  は "Click again to confirm; or click elsewhere to cancel"
  に変更。
- **Deleting 状態** — 行のテキスト色を `--text-dim` に;
  小さな inline spinner (CSS `@keyframes spin` を `↻` のような
  単一 Unicode char に) を date の代わりにタイトルに前置。
  行内の全ボタンは `disabled` + `pointer-events: none`。

### 3.5 グローバル認識用の activity event

オプション: 削除の周りに `agent:activity` イベント (type
`tool_start` + `tool_end`、Detail = `delete-session
<short-id>`) を emit する。これにより既存 footer "progress
tool" インジケータが削除も無料で表示できる。

**判断: 今回はスキップ**。Per-row spinner で十分かつ
よりローカル。削除用にグローバルイベントを追加すると "tool"
用語がぼやけ、チャットペインバブルとして表示しないために
frontend フィルタリングが必要になる。

---

## 4. エッジケース

| ケース | 挙動 |
|--------|------|
| 確認タイマー切れ (3 秒) で 2 クリック目なし | 行が X に戻る、アクションなし |
| Confirm 中に user がサイドバーの別場所を click | Confirm clear (document への click-outside listener) |
| A を confirm 後、B の confirm を開始 | A の confirm clear、B が confirm 中になる (single-row 不変条件) |
| Confirm + rename click | Rename 優先、confirm clear |
| 唯一のセッションを削除 | 今日と同じ: ハンドラが削除後に新 "New Session" 自動作成 (App.tsx 既存ロジック) |
| アクティブセッション削除 | 今日と同じ: ハンドラが残りの最初のセッションへ自動切替 (App.tsx 既存ロジック)。LoadSession 実行前に DeleteSession 内部で `agent.session` が nil クリアされるので安全になった。 |
| 連続 2 click (ダブル click race) | 1 つ目で confirm、2 つ目で confirmation; 意図的として扱う。シングルクリック・シングルクリックの ergonomics と引き換えの許容リスク。 |
| セッション B の ExportSession 中にセッション A を削除 | 両方が `Agent.mu` の state gate を通る。後着が ErrBusy。Frontend がエラーを surface し deleting 状態を clear。 |
| 削除中アプリクラッシュ | 部分削除の可能性 (objstore index 更新済だが dir 未削除等)。今日と同じ; 本変更で対応せず。atomicio writes により index/dir はバイト単位で torn にはならない。 |

---

## 5. 実装フェーズ

単一フェーズ — 変更は小さく、3 部 (Q3 / Q4 / Q5、プロジェクト
タスク) は密結合。

1. **Backend** (Q3): `agent.DeleteSession`、`bindings.DeleteSession`
   pass-through。Build green。
2. **テスト** (Q4): agent テスト 3 つ — RejectsWhenBusy、
   Inactive、Active (session pointer をクリア)。
3. **Frontend** (Q5): per-row state、click ハンドラ、視覚
   状態。Frontend build + 手動 smoke。

---

## 6. 検証

### Unit
- `TestAgent_DeleteSession_RejectsWhenBusy` — state Busy →
  ErrBusy。
- `TestAgent_DeleteSession_Inactive` — 別セッションがアクティブ、
  干渉なし; 削除セッションの dir 消失、アクティブセッションは
  intact。
- `TestAgent_DeleteSession_Active` — 削除対象がアクティブ
  セッション; その後 `a.session`、`a.sessionMemory`、
  `a.findings` が nil; dir 消失; `a.analysis.Close()` 呼出済。

### 手動 smoke
1. 非アクティブセッションの X を click → 行が ~3s ✓? を
   表示 → 再度 click → 行が grey 化、"Deleting…" 表示、その後
   行が消える。
2. X click、3s 何もせず待つ → 行が X に戻る。
3. セッション A の X を click、サイドバーの別場所を click
   → 行が X に戻る。
4. アクティブセッションの ✓? を click → 行 grey 化 →
   削除後、サイドバーが別セッションへ自動切替 (残りなければ
   自動作成)。
5. Send (長時間 LLM 呼出) を開始 → X click → confirm →
   binding が ErrBusy で reject → 行の deleting 状態 clear、
   エラーメッセージ surface。
6. (レース試行) セッション A の ✓? を click、削除実行中に
   即 LoadSession(B) を試行 → LoadSession が busy-gate から
   ErrBusy を返す; UI は無視 (同じ Busy 状態でボタン disable
   済のはず)。

---

## 7. 却下した代替案

- **モーダル "Are you sure?" ダイアログ** — 動作するが、
  2-click confirm よりフローを中断し、Findings / Global
  Memory / Session Memory の既存 in-row 2-click bulk-delete
  UX と非整合。
- **Trash / soft-delete + N 日 retention** — シングルユーザー
  ローカルアプリには過剰。誰も求めていない restore UI、
  retention policy、cleanup job を追加することになる。
- **削除中サイドバー全体をロック** (例: オーバーレイ spinner)
  — per-row 操作に対するグローバルレベルフィードバック。
  Per-row state でより正確に処理できる。
- **delete に per-session export lock を追加** (v0.4.0 で
  ドロップしたロック) — 冗長; agent state machine が既に
  全エントリポイントを serialise している。

---

## 8. 範囲外

- **複数セッション一括削除** (複数選択して全削除)。今日の
  UX は 1 件ずつ; 同じ per-row state パターンで後で追加可能。
- **同一セッション内の delete undo** — ストレージコストに
  見合わない; リカバリ可能性が欲しい場合は user が
  export/backup workflow (v0.4.0 の `.shellagent` bundle)
  に責任を持つ。
- **Confirm timeout の設定可能化** — 3 秒は妥当なデフォルト;
  もっと長い/短い窓が欲しい user は声を上げてほしい。

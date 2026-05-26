# ADR-0025: ツール呼び出しターンのアシスタント説明テキストをセッション復元時に復元する

- Status: Accepted
- Deciders: magi
- Related: ADR-0024 (セッション復元), `docs/ja/history/agent-data-flow.ja.md §3.2`, `docs/ja/history/tool-event-restore.ja.md`

## 1. 背景

報告された不整合: ターンがツール呼び出しを行う際、アシスタントの「これから
何をするか」説明テキストは**リアルタイム**ビューではチャットバブルとして
表示されるが、**セッション復元後は欠落**する。ライブと復元ビューが乖離する。

根本原因は、コード/ドキュメントの3箇所が同期していないこと:

- **ライブ (agent loop)** — `agentLoop`（`app/internal/agent/agent.go`
  ~2079-2093）はツール呼び出しターンを
  `AddAssistantMessageWithToolCalls(resp.Content, …)` で永続化し、同じ
  `resp.Content` を実チャットバブルとして emit する:
  `a.emitActivity(ActivityEvent{Type: "assistant_text", Detail: resp.Content})`。
  コメントは「説明をライブ会話のアシスタントバブルとして表示する。**ディスク
  から再読込した際に同じ内容が表示されるのと一致する**」と述べる。
- **フロントエンド** — `assistant_text` アクティビティハンドラ
  （`App.tsx` ~288）は `{role:'assistant', content}` を push し、コメントは
  「ディスクからの再読込は session.Records 経由で既に同じ内容を表示する —
  これはライブ UX をそれに合わせるだけ」と述べる。
- **復元 (LoadSession)** — `bindings.go` は実際には逆を行っていた:
  `case "assistant": … if len(r.ToolCalls) > 0 { continue }` でテキストを
  破棄。`agent-data-flow.ja.md §3.2` がこれを文書化していた:
  *「`ToolCalls` を持つターンはチャットバブルとして復元しない — その narrative
  は一時的な progressTool バナーだった」*。

つまりライブ emit と両コメントは復元一致を主張する一方、復元パスと §3.2
ドキュメントは古い「スキップ」挙動のまま。「一時的な progressTool バナー」
という根拠は**陳腐化**している: narrative は ephemeral バナーから実体の
ある・永続化される・ライブ表示されるバブルへ昇格したが、LoadSession（と
ドキュメント）が追随していなかった。

元のスキップは「thought-style preamble（例 `シンクタイム: 3秒`）を引き戻す」
懸念もあった。これはもう当てはまらない: `agentLoop` は `resp.Content` を
永続化・ライブ emit の**前に**クリーニングする
（`chat.CleanResponse` → `stripGemmaToolCallTags` → `StripCurrentGuardTags`
→ `TrimSpace`）ので、永続化 Content = ライブ表示テキスト。

## 2. 決定

`LoadSession` の `case "assistant"` で、`Content` が非空ならば**ターンが
`ToolCalls` を持つか否かに関わらず**復元する。引き続きスキップするもの:

- レガシー pre-r3 プレースホルダ `Content == "[Calling: …]"`（ユーザに
  表示されたことがない）、
- 空 `Content`（説明なしの純ツール呼び出しターン — ライブでも何も表示
  されておらず、tool-event バブルがそのターンを担う）。

すなわち `if len(r.ToolCalls) > 0 { continue }` を削除する。ツール呼び出し
自体は `tool` レコード → tool-event バブルで別途表示され続ける
（tool-event-restore.md）。レコードは時系列順に走査されるため順序は保たれる。

復元 Content は**再クリーニングしない** — 既存の最終返信復元パス（同様に
永続化 Content をそのまま emit する）と揃える。

## 3. 影響

- ツール呼び出しターンでライブと復元ビューが一致（報告バグ修正）。
- 順序保持: 説明バブルはレコード位置（同ターンの tool-event バブルの前）に
  現れる。
- `agent-data-flow.ja.md §3.2` を更新: `assistant` 行はツール呼び出しターンを
  スキップとは言わず、ツール呼び出しターンは（非空・クリーン済の）説明
  テキストをバブルとして復元し、ephemeral な progressTool「思考中」バナーの
  みが再生されない、と記す。

### エッジケース: レガシー pre-clean セッション

クリーニングパイプライン導入前に書かれたセッションは、ライブで表示された
ことのない未クリーンの preamble を `Content` に持つツール呼び出しレコードが
ありうる。そのまま復元すると軽微なノイズが出うる。許容する理由: (a) 既存の
最終返信復元パス（レガシー Content を再クリーニングせずそのまま emit）と
対称、(b) 読込時の再クリーニングは別個の広い変更。ここで特別扱いする価値は
ない。

## 4. 却下した代替案

- **逆方向 — ライブの `assistant_text` emit をやめ、旧「復元時スキップ」に
  ライブを合わせる。** 却下: ライブ説明バブルは意図的に追加された（「これから
  何をするか・なぜか」というテキストは有用で、以前は「捨てられていた」）。
  削除は UX 退行。バグは復元がライブに一致しないことで、望ましいのはライブの
  挙動。
- **復元時に Content を再クリーニング (CleanResponse on read)。** スコープ
  クリープとして却下: 長年の最終返信復元パスも変えてしまう。レガシーノイズが
  報告されたら別個の hardening として追跡。
- **現状維持。** 却下: 報告された不整合そのもの。

## 5. 実装

- `app/bindings.go` — `LoadSession` `case "assistant"`: `len(r.ToolCalls) > 0`
  スキップを削除; `[Calling:` と空 Content スキップは維持。（本 ADR 承認待ち、
  未コミット）
- `app/bindings_test.go` — `TestLoadSession_RestoresToolEventBubbles`:
  非空 Content のツール呼び出しアシスタントレコードが `assistant` バブルとして
  （その tool-event バブルの前に）復元される; 空 Content の純ツール呼び出し
  レコードは引き続きスキップ。（完了; テスト通過）
- `docs/en|ja/history/agent-data-flow §3.2` — `assistant` 役割行を修正後の
  復元ルールに更新。

検証: `go test -tags no_duckdb_arrow ./...` green; 手動 — 説明付きツール
呼び出しターンを実行→セッション再読込→説明バブルがライブと同順で再出現する
ことを確認。

## 6. スコープ外

- 復元時の永続化 Content 再クリーニング（§3 エッジケース参照）。
- ephemeral progressTool「思考中」バナーの復元時再生（意図的に未永続化、
  不変）。
- 復元バブルへのツール引数/結果の表示（tool-event-restore.md non-goals、
  不変）。

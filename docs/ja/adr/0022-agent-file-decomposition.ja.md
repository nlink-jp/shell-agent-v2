# ADR-0022: agent.go の最小限分解

- Status: Implemented in v0.14.3 (2026-05-21)
- Deciders: magi
- 関連: Issue #10 (発端)

## 1. 背景

[Issue #10](https://github.com/nlink-jp/shell-agent-v2/issues/10) は
`app/internal/agent/agent.go` が大きい (3,697 LOC、~100 関数、#11 後の
44 → 33 フィールド) と報告し、8 つの責務別ファイルへの分割を提案した。
議論を経て 2 つの事実が浮かび上がった:

1. **Issue は metric 駆動であり、pain 駆動ではない。** トリガーは
   「ファイルが大きい」だけ。誰かが迷子になった / ナビゲーションに
   過剰な時間がかかった / ファイルサイズが原因でミスをした、という
   具体ケースの記録はない。コミット履歴、PR レビュー、post-mortem の
   いずれにも認知負荷起因の失敗は記録されていない。

2. **Go ではまとまった巨大ファイルは普通。** `net/http/server.go` は
   ~3,900 LOC で広く読みやすいとされる — すべてのメソッドが同じ
   struct のレシーバ、同じロック規律、定義された役割を持つ。
   `agent.go` も同じ形状: 100+ メソッドが単一の `*Agent` レシーバ、
   単一の mutex、1 つの cohesive なコンポーネント内の明確な役割。

metric 駆動の苦情への素直な反応は metric 駆動の修正 (8 ファイル、
11 ファイル、何でも閾値以下にすればいい) だが、具体的な pain が無い
時にこれをやるのは **間違った答え** だ — レビュー/移行コストは払うが
測定可能な何も払い戻されず、cross-file の摩擦を増やすだけになる。

## 2. 決定

`agent.go` から **2 ファイルのみ** を抽出する。両方の独立性基準を
満たすものを選ぶ:

1. **トピックの直交性** — コードが Agent の send / loop / lifecycle /
   FSM コアと噛み合わない
2. **読者がスキップできる** — Agent コアを読む人がこのコードを
   スクロールで通り過ぎたり頭に入れたりする必要がない

2 ファイル:

| 新ファイル | LOC | 内容 |
|---|---:|---|
| `agent_mcp.go` | ~250 | MCP guardian 管理 (startGuardians, spawnGuardian, validateBinaryPath, validateProfilePath, MCPStatuses, stopGuardians, RestartGuardians, restartGuardian, splitMCPName) + `MCPStatus` 型 |
| `agent_extract.go` | ~600 | Memory 抽出アルゴリズム (extractMemories, parseExtractionLine, looksLikeTurnToken, matchFactToUserTurn, detectUserLanguageHint, hasSignificantCJK, extractCJKNgrams, extractKeywords, parseTurnToken, stripGemmaToolCallTags) |

抽出後、`agent.go` は 3,697 LOC から **~2,850 LOC** に。スタイルガイド
によってはまだ大きいが、抜けるのは読者が最もスキップしたい部分:

- **MCP** は独自の検証 / spawn / ルーティングロジックを持つ自己完結
  サブシステム。Send / Loop / Memory に集中する読者は読む理由ゼロ。
- **Extraction** は単一のアルゴリズム的フロー (extractMemories +
  parsing + CJK helpers)。agentLoop や postResponseTasks を読むのに
  UTF-8 / Jaccard / プロンプト整形コードを 600 行スクロールすべきでない。

## 3. 抽出を **しない** ものとその根拠

検討して却下した各クラスタについて、将来の再訪が一からやり直さない
ように理由を記録する:

- **Handler インフラ** (~400 LOC)。パターン多めだが `emit*` /
  `notify*` メソッドは短く、ロジックよりデータに近い読み心地。bindings
  ブリッジを使用メソッドと別ファイルに置くと、便利より不便な場面の
  方が多い。将来 handler 登録がもっと複雑になれば再訪。

- **Send パイプライン** (~400 LOC)。`agentLoop` と密結合 (dispatch
  されたメッセージはそこでターンになる)。Send をループと並んで読むのが
  common case で、分割するとほぼ payback なしにナビゲーションだけ増える。

- **Agent loop / executeTool / buildToolDefs** (~700 LOC)。これは
  コア。抽出すると `agent.go` residual が「もはや Agent ファイルでは
  ない」状態に — 「Agent からループを引いたもの」になるのは変なメンタル
  モデル。struct が定義されている場所にループを残す方が良い。

- **Post-response orchestration** (~350 LOC)。`postResponseTasks` は
  `agentLoop` の defer から発火; 並んで読むのが自然な流れ。特に
  auto-dispatch 経路は両方を 1 つのウィンドウで見たい。

- **Memory / Findings CRUD アクセサ** (~300 LOC)。小さく浅い
  メソッド群。`agent.go` から抽出すると `bindings.go` → Agent の
  ナビゲーションが長くなり、明瞭度の利得がない。

- **Profile / Backend 切替** (~450 LOC)。Send / Loop / Lifecycle 経路と
  状態を共有。コロケーションすることで `/model` 問題をデバッグする
  読者がタブを切り替えずに周辺 Send コードを見られる。

- **Tool 検査 API** (~300 LOC)。bindings が使う浅い読み出しメソッド。
  Memory CRUD と同じ論点。

- **Lifecycle (maybeStartSandbox, SetBaseContext/Objects/Analysis,
  LoadSession, sessionWorkDir)** (~250 LOC)。LoadSession は中央的 —
  profile、sessionMemory、findings、guardians に触る。struct + New と
  コロケーションすることで読者に wiring の全体像を渡せる。

## 4. 将来の再訪

本 ADR は「**今後二度と分解しない**」とは言っていない。
「**metric だけでなく、記録された pain によって駆動される分解**」と
コミットする。将来のコントリビュータが以下を報告したら:

- 具体的なナビゲーション pain (「X を 20 分探した、ファイル別なら
  助かった」)
- 具体的なコントリビューション pain (「新しいメソッドをどこに置くか
  分からなかった」)
- 具体的なレビュー pain (「3000 行ファイル全体に渡る差分でレビュー
  不能だった」)

そのときフォローアップ ADR で §3 の却下クラスタの 1 つ以上を抽出
すれば良い。将来の抽出は **具体的 pain** を指せ、ファイルサイズだけを
指してはいけない。

## 5. 実装

2 コミット + リリース:

1. `refactor(agent): extract MCP guardian management to agent_mcp.go`
2. `refactor(agent): extract memory extraction to agent_extract.go`
3. `chore: release v0.14.3` — patch bump (内部再構成、API 変更なし、
   挙動変更なし)

各抽出は pure file move: 全関数が同じ `*Agent` レシーバ、同じ
パッケージ、同じロック規律で着地。挙動変更なし。各コミットで
`go test ./... -tags no_duckdb_arrow` が PASS すること。

## 6. Issue #10 のクローズ

本 ADR の存在 + v0.14.3 リリースで Issue #10 をクローズする。Issue に
本 ADR を解決として引用し、却下クラスタが §3 にカタログ化されている
旨をコメントする — 将来の再評価が一からではなく上に積み上げられる。

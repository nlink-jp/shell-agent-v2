# フロントエンド分割 — 設計ドキュメント

> 日付: 2026-04-30
> ステータス: v0.1.15 でリリース済み。本ドキュメントの分割
> レイアウトは現状と一致する。後続の UI 開発（BlobPreview /
> MITL 調整等）も同じコンポーネントツリー上で展開しており、
> モノリスには戻していない。
> 範囲: `app/frontend/src/App.tsx` のみ。CSS、ビルド、Wails
> バインディングは触らない

## 1. 問題

v0.1.14 時点で `App.tsx` は 1457 行に達し、まだ膨張中。1 ファイル
に以下が同居している:

- `window.go.main.Bindings` のグローバル宣言（〜50 行）
- 10 個以上の TypeScript インタフェース（Settings, Finding,
  ObjectInfo …）
- 3 つの手書きサブコンポーネント（`MessageItem`,
  `BulkActions`, `BackendBudgetEditor`）
- `App` コンポーネント本体: useState 30 個以上、effect 約 10 個、
  全イベントハンドラ、サイドバーツリー全体、チャットペイン、
  オーバーレイ 4 種（MITL / Settings / lightbox / report viewer）、
  cmd ポップアップ。

症状:

- どんな変更でも毎回同じファイルを触ることになる
- 検索置換が危険（`status` / `active` / `settings` といった識別子
  が無関係な箇所に重複出現）
- 新規貢献者（および各種ツール）が小さな変更のために 1k 行以上
  を読む必要がある
- `wails dev` のホットリロード遅延がファイルサイズに比例して
  増大

## 2. ゴール / 非ゴール

### ゴール

- `App.tsx` を **～450 行**の調整層に削減（トップレベル state
  所有 + サブコンポーネントのレンダ）
- 主要 UI 面（Settings ダイアログ、サイドバー、各種オーバーレ
  イ）をそれぞれ別ファイルに分離
- ランタイム挙動はバイト単位で同一: 同じ DOM、同じ CSS クラス、
  同じイベント順序、同じ Wails バインディング

### 非ゴール

- **state 管理ライブラリ導入なし。** Zustand / Redux / Context
  リファクタは見送り。state は `App` 内の `useState` /
  `useRef` のままで、props で下流に渡す。目的はファイル境界を
  きれいにすることであり、データフロー再設計ではない
- **CSS の再編成なし。** クラス名は変えないので、移動先 JSX が
  参照する `App.css` のルールはそのまま動作する
- **新しい抽象なし。** 既存にないヘルパフック（`useMITL` /
  `useSidebarPrefs`）は範囲外。ファイル分割後にもし follow-up
  機能が楽になりそうなら検討する

## 3. ターゲットファイル構成

```
app/frontend/src/
├── App.tsx                     ~450  (1457 から削減)
├── App.css                     変更なし
├── themes.css                  変更なし
├── ChatInput.tsx               変更なし
├── ObjectImage.tsx             変更なし
├── DataDisclosure.tsx          変更なし
│
├── types.ts                    ~120   新規 — Settings, Finding, ObjectInfo,
│                                       MessageData, ChatMessage, SessionInfo,
│                                       LLMStatus 等
├── bindings.ts                 ~70    新規 — `window.go.main.Bindings`
│                                       グローバル宣言とメソッド
│                                       シグネチャの集合
│
├── components/
│   ├── MessageItem.tsx         ~110   App.tsx から移動
│   ├── BulkActions.tsx         ~60    App.tsx から移動
│   └── BackendBudgetEditor.tsx ~30    App.tsx から移動
│
├── sidebar/
│   ├── Sidebar.tsx             ~190   アコーディオン + Sessions パネル +
│   │                                  Memory パネル + bottom-nav。
│   │                                  現状の 756–944 行範囲がほぼそのまま
│   └── (v1 ではこれ以上分割しない。再利用が必要になるまで
│        Sidebar.tsx 内にインライン保持)
│
├── dialogs/
│   ├── SettingsDialog.tsx      ~280   v0.1.10 の Settings オーバーレイ
│   │                                  (1167–1415)。タブ: General /
│   │                                  Tools / MCP。
│   │                                  props: `{settings, onChange, onClose,
│   │                                  tools, mcpStatus}`
│   ├── MITLDialog.tsx          ~130   1047–1166。Approve /
│   │                                  Reject / Reject-with-feedback
│   ├── Lightbox.tsx            ~30    1416–1422 + 既存スタイル
│   └── ReportViewer.tsx        ~50    1423–1456（拡大レポート全画面
│                                      ビューア）
```

## 4. フェーズ分割

各フェーズは self-contained なコミット、shippable な状態を維持、
DOM 差分なし。

### Phase 1 — 型抽出（低リスク）

`App.tsx` 内のすべての `interface` / `type` を `types.ts` に
移動。`window.go.main.Bindings` の `declare global` は
`bindings.ts` に分離（バインディング層がドメイン型を汚染しない
ため）。

`App.tsx` は両方から import。JSX や挙動の変更ゼロ — 純粋な
カット & ペースト + import パス追加。

`App.tsx` から〜150 行削減。

### Phase 2 — サブコンポーネント抽出（低リスク）

`MessageItem` / `BulkActions` / `BackendBudgetEditor` を
`components/` に移動。それぞれ default export 化。props は
すでに明示的なので、変わるのは import パスのみ。

さらに〜200 行削減。

### Phase 3 — Settings ダイアログ（中リスク）

最大の連続塊。`dialogs/SettingsDialog.tsx` に抽出、props:

- `settings: Settings`
- `tools: ToolInfo[]`
- `mcpStatus: MCPStatus[]`
- `onUpdate(patch: Partial<Settings>): void` — 既存の
  `updateSetting` コールバック
- `onClose(): void`

ダイアログ内部の `settingsTab` / `mcpExpanded` 状態はローカル
化。tools / MCP restart / theme save バインディング呼び出しは
`App` に残す（既存の `onUpdate` パッチパターン経由で）。

〜280 行削減。

### Phase 4 — オーバーレイダイアログ（低リスク）

`MITLDialog` / `Lightbox` / `ReportViewer` を `dialogs/` に
抽出。それぞれ小さな固定 props 集合。

〜210 行削減。

### Phase 5 — Sidebar（中リスク）

Sidebar は `App` の state（sidebarPanel 選択、sessions リスト、
findings、pinned memory 等）と密結合。`sidebar/Sidebar.tsx` に
抽出、これらすべてを props として受け取る + アクションコール
バック（`onSelectSession`, `onNewSession`, `onDeleteFinding` 等）。

最も props-drilling が多くなるフェーズ。props リストが
扱いにくくなったら Phase 6（範囲外）で context 導入を検討、
それまでは props のまま。

〜190 行削減。

### Phase 6（MVP 後）— オプション改善

Phase 1〜5 後にまだ問題があれば:

- 大きな per-overlay JSX をさらに細かいファイルに分解
  （`SettingsTabGeneral.tsx`, `SettingsTabTools.tsx`,
  `SettingsTabMCP.tsx`）
- GitHub #4 の幅 / 折りたたみ永続化パターンを `useSidebarPrefs()`
  フックに切り出し
- props-drilling が悪化したら `BindingsContext` を導入

すべて仮定的、明示的な follow-up 苦情があった時のみ着手。
先回りはしない。

## 5. テスト戦略

現状フロントエンドの単体テストはなく、検証は dev モード手動。
各フェーズで:

1. `cd app && make build` で TypeScript エラーゼロ
2. ビルド済みアプリ起動、同じチェックリスト走査:
   - メッセージ送信（Vertex + Local 両方）
   - `/model` 切替、backend バッジ更新確認
   - Settings 開く、theme + sandbox image + LLM timeout 変更、
     保存、再起動後永続化確認
   - MITL プロンプトトリガ（`query-sql`）、Approve / Reject
     両経路
   - finding の session リンククリック、セッション切替確認
   - サイドバーリサイズ、折りたたみ、再起動 → Issue #4 永続化
     継続確認
3. DOM の CSS クラス名差分（視覚回帰チェック）— フェーズ間で
   差が出ないこと

各フェーズ後に毎回実行、最後にまとめてではなく。

## 6. リスクと緩和策

| リスク | フェーズ | 緩和策 |
|---|---|---|
| Sidebar の props-drilling 爆発 | 5 | v1 では受け入れる、props が ~12 を超えたら context 導入検討 |
| `types.ts` と component ファイル間の循環インポート | 1〜5 | `types.ts` は型のみ export、component から import しない |
| Wails dev サーバの hot-reload エッジケース | 全 | 各フェーズを `make dev` でもテスト（`make build` だけでなく） |
| 微妙な再レンダパフォーマンス変化 | 2, 5 | `MessageItem` は `memo` 化、保持。他は plain |

## 7. 範囲外

- コンポーネントテスト追加（別案件、現状ゼロのまま、悪化なし）
- `react-markdown` プラグイン依存の置換
- Wails JS バインディングファイルの自動生成（`wails build` が
  処理、触らない）

## 8. ロールバック

各フェーズは単一コミット。フェーズ N の検証失敗時はそのコミット
を `git revert`、前フェーズまでは出荷可能なまま。

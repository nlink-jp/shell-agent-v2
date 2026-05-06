# セッション Import / Export — 設計ノート

**ステータス:** 設計確定 (Open question は 2026-05-07 解消済); 実装可能。
**対象バージョン:** v0.4.0 (v0.3.0 の次)

本ドキュメントは、セッション全体 — 会話履歴、セッションメモリ、
findings、サマリ、サンドボックスファイル、セッションスコープの
DuckDB データベース — を 1 つの可搬な bundle にまとめて、同一
または別マシンで import 可能にし、その際にプライバシーフラグも
保持する **完全 export/import 機能** を仕様化する。

---

## 1. 目的

- **可搬性**: マシン間 (および、DuckDB のクロスプラットフォーム
  バイナリ形式のおかげで OS / アーキテクチャ間) でセッションを
  手作業のファイル配線なしで移動できる。
- **完全性**: per-session の成果物 — chat 履歴、per-session
  memory、findings、サマリキャッシュ、サンドボックスの `work/`
  ディレクトリ、analysis DuckDB — のすべてを含めることで、
  import 後のセッションが元と機能的に同一になる。
- **プライバシー保持**: `Private` フラグを bundle に埋め込み、
  移動後も import 先で勝手に Global Memory promotion が始まら
  ないようにする。
- **ワンステップ UX**: サイドバーから 1 クリックで export、
  1 クリックで import。キーボード派のためにスラッシュコマンド
  (`/export`, `/import`) も提供する。

---

## 2. ユースケース

- **バックアップ**: 高リスク操作 (DB drop、サンドボックス
  `rm -rf`) の前に進行中の調査スナップショットを保存。
- **マシン間移行**: ノート PC からワークステーションへ (または
  逆方向に) 調査を移動。
- **再現可能なハンドオフ**: デバッグセッションを共同作業者へ
  渡し、相手が import して同一状態 (chat + 解析テーブル +
  サンドボックスファイル) から再開できる。
- **プライバシー対応アーカイブ**: プライベートセッションを
  `.shellagent` bundle としてアーカイブ。次回ロード時に
  Global Memory に再露出されない。

---

## 3. Bundle フォーマット

### 3.1 ファイル拡張子

`.shellagent`

カスタム拡張子を選択した理由は 2 つ:

- Finder / ファイルダイアログで任意の `.zip` ファイルと区別
  でき、bundle がうっかりダブルクリックで Downloads に展開
  される事故を防ぐ (bundle は手動で unzip するものではない);
- 将来のイテレーションで OS 側のファイルアソシエーションを
  実現可能 (v0.4.0 では out-of-scope)。

bundle の中身は標準的な ZIP アーカイブで、任意の zip ツールで
中身を確認できる。アプリは拡張子をソフトな約束事として扱う。

### 3.2 内部構造

```
manifest.json           # 必須 — import 時に最初に検証
chat.json               # 必須
session_memory.json     # 必須 (空配列でも可)
findings.json           # 必須 (空配列でも可)
summaries.json          # 任意 — サマリキャッシュがあれば
analysis.duckdb         # 任意 — DuckDB を一度でも開いていれば
work/                   # 任意 — サンドボックスツールを使った場合
    <ユーザーファイル, 再帰的>
objects/                # 任意 — セッションが objstore object を所有する場合
    index.json          # ObjectMeta 配列 (sessionID は除去済)
    data/<id>           # blob 本体、object 1 つにつき 1 ファイル (32-hex ID; 12-hex legacy 互換)
```

zip 内のパスはすべてフォワードスラッシュ。シンボリックリンクは
辿らないし bundle にも書かない (セキュリティ)。展開時、正規化
パスが target ディレクトリの外に逃げるエントリは拒否する
(zip-slip 対策)。

### 3.3 manifest.json スキーマ

```json
{
    "schema_version": 1,
    "exported_at": "2026-05-07T12:34:56Z",
    "exported_by_app_version": "0.4.0",
    "session": {
        "original_id": "abc123",
        "title": "Investigate slow queries",
        "private": false
    }
}
```

フィールド意味:

| フィールド | 意味 |
|------------|------|
| `schema_version` | Bundle スキーマバージョン。v0.4.0 は `1` を書く。Import は他の値を拒否。 |
| `exported_at` | RFC3339 UTC タイムスタンプ。情報のみ。 |
| `exported_by_app_version` | bundle を生成した shell-agent-v2 のバージョン。情報のみ。互換性チェックには使わない (schema_version を使う)。 |
| `session.original_id` | Export 時のセッション ID。トレーサビリティのため保持するが、import 後の ID には使わない。 |
| `session.title` | Export 時のタイトル。Import セッションのタイトル基底値 (衝突時は suffix が付く)。 |
| `session.private` | プライバシーフラグ。Import 後のセッションにそのまま引き継ぐ。 |

Import 時の検証ルール:

1. Bundle が ZIP として parse できる。
2. `manifest.json` がアーカイブのルートに存在し、JSON として
   parse できる。
3. `schema_version == 1` であること。それ以外 → 「unsupported
   bundle schema version: N」で拒否。
4. `chat.json`, `session_memory.json`, `findings.json` が存在
   し JSON として parse できる。
5. 任意ファイル (`summaries.json`, `analysis.duckdb`, `work/*`,
   `objects/`) は欠けていてもよい。
6. zip エントリの正規化パスが target ディレクトリの外に出る
   もの → 「bundle contains unsafe path」で拒否。
7. `objects/` がある場合、`objects/index.json` から参照される
   全エントリに対応する `objects/data/<id>` が必須、逆も
   同じ (`objects/data/<id>` ファイルにも index エントリが
   必要)。不一致 → 「bundle objects index/blobs out of sync」
   で拒否。

---

## 4. Export 動作

### 4.1 共通フロー (任意のセッション)

1. **per-session export ロック** を取得
   (`internal/sessionio` 内の sessionID をキーとした
   sync.Mutex)。
2. `sessions/<id>/` に存在する成果物を確定。
3. このセッションが所有する objstore object を
   `objstore.Store.ListBySession(id)` で列挙。返り値は
   point-in-time スナップショット; 呼出中は live index が
   RLock されるので並行 `Store()` で iteration 中に変異
   しない。
4. Manifest JSON を組み立てる。
5. ZIP アーカイブを destination パスへストリーミング書込。
   per-session 成果物に加え、Step 3 が空でなければ
   `objects/index.json` (sanitised metadata; bundle 自体が
   session スコープなので `sessionID` は剥がす) と各 object
   の `objects/data/<id>` (`objstore.Load(id)` で読み出し)
   を含める。
6. ロック解放。
7. 監査ログ:
   `session exported: id=<id> private=<bool> bytes=<N> objects=<K> dest=<path>`。

### 4.2 アクティブセッションの追加ステップ

「アクティブセッション」とは現在 agent にロードされている
セッション (DuckDB 接続が開いている可能性があり、バックグラ
ウンドタスクがメモリストアの参照を保持している可能性がある)。

state machine との統合:

1. **`agent.State == Idle` を待機。** CLAUDE.md にある通り
   agent は Idle/Busy state machine を持つ。Export はライタで
   あり、in-flight なツール実行と競合する。リクエスト時点で
   Busy なら binding は `ErrAgentBusy` を返し、UI は
   「リクエスト処理中のため export できません — agent が
   idle になるまで待ってください」を表示する (UI スレッドを
   無言で block しない)。
2. **agent の session-write ロックを取得** (各 turn 後の
   `chat.json` 永続化で `Save()` が取るのと同じロック)。
   これにより export と書き込みを行うバックグラウンドタスク
   (タイトル生成、メモリ圧縮、pinned-memory 抽出) が serialise
   される。
3. **インメモリ状態をディスクへ flush**: `session.Save()`,
   `sessionMemory.Save()`, `findings.Save()`,
   `summaryCache.Save()`。
4. **`analysis.Engine` を Close** (このセッションで開いて
   いれば)。Engine は背後の `*sql.DB` を閉じる `Close()` を
   公開する。閉じることで WAL が flush され、ファイルハンドル
   が解放され、後続のバイナリコピーが整合性のあるファイルを
   見られる。
5. **コピー**: per-session 成果物 (chat.json, session_memory.json,
   findings.json, summaries.json, analysis.duckdb, work/) と
   bundle の `objects/` ディレクトリ (§4.1 Step 5) を ZIP
   ストリームへ。
6. **Engine の reopen は遅延** — Engine は次回利用時に lazy
   初期化される (既存パターン)。明示的な eager reopen は
   不要。次の解析ツール呼出で自然に再オープンされる。
7. session-write ロックと per-session export ロックを
   **解放**。

### 4.3 レースコンディション一覧

| # | シナリオ | 対策 |
|---|----------|------|
| R1 | Active session の export 中にユーザーがメッセージ送信 | Agent は export の間 Idle を保持 (state-write ロック)。送信は input 層でロック解放まで queue される。 |
| R2 | Export のコピー中にバックグラウンド抽出 (memory / pinned / title) が走る | バックグラウンドタスクは永続化前に同じ session-write ロックを取得する。Export がそれを保持しているので、バックグラウンドライタは block される。 |
| R3 | サンドボックスコンテナが `work/` へ書き込み中 | サンドボックス実行は agent.Busy の一部なので R1 のゲートで既にカバーされる。Export はサンドボックス実行がない時のみ進む。 |
| R4 | セッション B が active な状態でセッション A を export | Per-session ロックは独立。A は休眠中で Engine も開いておらず、バックグラウンドタスクの参照もない。普通のコピーで安全。 |
| R5 | 同じセッションを 2 つ同時に export | Per-session export ロックが serialise する。2 つ目は待つ。 |
| R6 | 以前の使用で DuckDB の lazy 接続が残っている | §4.2 の Step 4 で閉じる。次のユーザー操作による解析呼出で lazy に再オープン。 |
| R7 | 書込中にディスク満杯 | ZIP 書込がエラーを返す → 一時ファイルを削除 → 元セッションは無傷。エラーを UI に通知。 |
| R8 | Export 中にアプリがクラッシュ | Destination ファイルは temp + rename で書く (atomicio パターン)。partial temp ファイルが残る可能性はあり、ユーザーが手動削除可能。元セッションは影響なし。 |
| R9 | Export のコピー中にツールが `objstore.Store()` で新 object を作成 | Active session: agent.Idle ゲートでツール実行が止まる。Non-active session: そのセッションを対象とする agent 活動はない。`ListBySession` のスナップショット仕様により iteration 中の変異は発生しえない。 |
| R10 | 並行する `objstore.Delete()` (例: 別セッションの `DeleteBySession`) | objstore は単一 mutex で保護されており、§4.1 Step 3 のスナップショットは整合性が保たれる。*このセッション* の object 削除はユーザーがセッションを明示的に削除した場合のみ発生し、per-session export ロックでガードされる。 |

### 4.4 非アクティブセッションの export

State machine の心配なし。セッションは休眠中: Engine も開いて
いないし、バックグラウンドタスクも参照を保持していないし、
agent state machine と協調する必要もない。Export は単純な
ファイルコピー (§4.1 の per-session export ロック内で実行し、
あり得ない R5 にも備える)。

### 4.5 Export ファイル名

Save dialog が提示するデフォルト名:
`<safe-title>-<YYYYMMDD-HHMMSS>.shellagent`

`<safe-title>` はセッションタイトルから `/\:*?"<>|` および
ASCII 制御コードを `_` に置換し、64 文字に切り詰めたもの。
タイトルが空なら `session-<short-id>` にフォールバック。

ユーザーは save dialog でこの提案名を上書き可能。

---

## 5. Import 動作

1. ユーザーがネイティブ open dialog で `.shellagent` ファイル
   を選択 (フィルター: `*.shellagent`)。
2. **検証** (§3.3 に従い zip + manifest + schema_version +
   必須ファイル + zip-slip チェック)。失敗時はその具体的
   エラーを UI に通知して中断。
3. **新セッション ID を生成** (UUID)。元 ID は manifest にのみ
   保持し、再利用しない — これにより、同じ ID を持つ既存
   セッションとの衝突を回避。
4. **タイトル衝突解決**: 既存の `ListSessions()` 内に同じ
   タイトルのセッションがあれば ` (imported)` を付ける。
   `<title> (imported)` も既にあれば ` (imported 2)`、`3`、…
   と続ける。
5. `sessions/<newID>/` ディレクトリを **作成**。
6. Bundle のエントリ (`objects/` を除く) を新ディレクトリへ
   **展開**。`work/` サブツリーは as-is で展開。
7. **Object 登録** (`objects/` が bundle にあれば): 各
   `objects/index.json` エントリに対し新 object ID を生成し、
   blob を `objstore.Store(blob, type, mime, origName,
   newSessionID, newObjectID)` で登録、`oldID → newID` map を
   蓄積。Object ID と参照は **常に新規生成** (preserve-or-collide
   パスは持たない); §5.3 を参照。
8. **chat.json の書き換え**: `id` フィールドを `<newID>` に;
   各 `Record.ObjectIDs[]` を map で remap; 各
   `Record.Content` に対し正規表現
   `\b(?:object:)?([a-f0-9]{12}|[a-f0-9]{32})\b` で sweep し、
   マッチした旧 ID を新 ID に置換。(12-hex は ID 幅変更前の
   legacy bundle 互換のため。) 他のフィールド — title, private —
   はそのまま保持。
9. **summaries.json の書き換え** (存在すれば): 各
   `SummaryEntry.Summary` テキストに同じ正規表現 sweep を適用。
   Summarizer LLM が source records の markdown 画像参照を
   要約に paraphrase で残しうるため、import 後のダングリング
   参照を防ぐためにこの sweep が必須。(`session_memory.json`
   と `findings.json` は sweep しない — 書込パスが ref を
   埋め込まないため; §5.3 参照。)
10. 次の `ListSessions()` 呼出で session registry に **再登録**
    される (registry はディスクから読むので、無効化すべき
    インメモリキャッシュはない)。
11. **監査ログ**:
    `session imported: original_id=<orig> new_id=<new> private=<bool> bytes=<N> objects=<K>`。
12. **自動切替**: binding は新セッション ID を返し、フロント
    エンドは即座に `LoadSession(newID)` を呼ぶ。

### 5.1 エッジケース

| ケース | 動作 |
|--------|------|
| Bundle が valid な zip でない | 拒否: 「not a valid .shellagent bundle」 |
| `manifest.json` が欠けているか parse 不能 | 拒否: 「missing or corrupt manifest.json」 |
| `schema_version` != 1 | 拒否: 「unsupported bundle schema version: N」 |
| 必須ファイルが欠けている | 拒否: 「bundle missing required file: X」 |
| Zip-slip パス | 拒否: 「bundle contains unsafe path: X」 |
| 新セッションディレクトリが既に存在 (UUID 衝突 — 実質的にあり得ない) | 新 UUID で 1 回 retry; ダメなら fail。 |
| 別 OS / アーキテクチャの DuckDB ファイル | 受容。DuckDB v0.x+ のバイナリ形式は darwin/linux/x86/arm 間でポータブル。検証は最初の解析呼出に遅延。 |
| 展開中にディスク満杯 | partial な新セッションディレクトリを掃除し、エラーを通知。 |
| Object 登録の途中で `objstore.Store()` が失敗 | ロールバック: 新 sessionID で既に登録した object を `Delete()`、partial セッションディレクトリを削除、エラーを通知。 |
| Bundle の `objects/index.json` が存在しない blob を参照 (または逆) | 検証時に拒否 (§3.3 ルール 7)。 |
| Open dialog をキャンセル | No-op。 |

### 5.2 Import 時のプライバシー姿勢

Manifest の `Private` フラグは import 後セッションの
`chat.json` に逐語保持される。これにより:

- 別マシンから import した private セッションは private の
  まま — このマシン上でも fact が Global Memory へ promote
  されることはない。
- 別マシンから import した非 private セッションは非 private
  のまま — このマシン上の以後の turn で生成された fact は
  通常ルールに従い Global Memory へ **promote される**。
  Import を firewall したいユーザーは、会話を再開する前に
  UI で privacy をトグルすればよい (これは以後の turn にのみ
  影響する; 過去 chat の内容は Global Memory から retroactive
  に scrubbed されない — 過去内容は bundle の中にあって、
  このマシンの Global Memory にはまだ無いため)。

### 5.3 Object ID 戦略

Import される object には常に新 ID を発行する。理由は 2 つ:

- **衝突は確率事象でなく決定論的に発生する**。ユーザーが
  セッションを export し、元セッションを削除せずに同じ
  bundle を同マシンで re-import すると、bundle 内のすべての
  object ID が **グローバル** objstore (セッション横断で
  共有) の既存エントリと衝突する。新 ID 生成にすればこの
  経路が自明に安全になり、同じ bundle を複数回 import する
  ことも自然に成り立つ。
- **objstore は各 object に厳密に 1 つの `sessionID` を
  タグ付ける** — `DeleteBySession` のクリーンアップに使われる。
  元 ID を保持して index エントリを新 sessionID に向け直す
  と、元セッションから所有権を黙って奪うことになり、import
  したセッションを削除すると元の object が orphan になる。
  新 ID にすればこの衝突を回避できる。

代償は import 時の参照書き換え。永続化監査によれば、書き換え
範囲は **2 ファイル × 3 箇所** に限定される:

| ファイル | フィールド | 理由 |
|----------|-----------|------|
| `chat.json` | `Record.ObjectIDs[]` | 構造化配列、直接 remap。 |
| `chat.json` | `Record.Content` | user/assistant/tool メッセージ本文に `![alt](object:ID)` markdown が含まれうる。 |
| `summaries.json` | `SummaryEntry.Summary` | Summarizer LLM が source records の `object:ID` ref を要約に paraphrase しうる。 |

`session_memory.json` / `findings.json` /
`global_memory.json` は **明示的に sweep しない**:

- `session_memory.json`: fact は `sanitizeMemoryText()` で
  制御文字除去・空白圧縮を経るし、抽出 prompt が markdown
  画像参照を fact に持ち込むことを禁止している。
- `findings.json`: system prompt が markdown 画像記法は
  chat content でのみ使うよう LLM に指示している; promote
  -finding ツールは finding テキストを逐語格納するが、LLM
  が `object:` ref をそこに埋めない。
- `global_memory.json`: export 対象外; promotion 経路は fact
  サニタイズを通り markdown を落とす。

将来これらストアのいずれかに object ref 埋込が導入された
場合、本セクションと書き換え器をセットで更新する必要がある。

---

## 6. UI

### 6.1 Sidebar — セッションリストのホバー行

サイドバーの各セッション行は既に hover 時のみ表示される
アクションアイコン (rename, delete) を持つ。3 つ目を追加:

- **Export** アイコン (📤 もしくは ⬇) — クリックで `/export`
  と同じフローで該当セッションを export (提案デフォルト名付き
  save dialog)。

セッションが Busy 状態のときアイコンは隠す (export は
`ErrAgentBusy` で拒否されるため); tooltip で理由を示す。

### 6.2 Sidebar — bottom-nav

既存の `+ New Private Chat` ボタンの下に追加:

- **Import Chat** ボタン (📥 アイコン) — `*.shellagent` で
  フィルタリングしたネイティブ open dialog を開く。Import
  成功時に新セッションを自動ロード。

### 6.3 スラッシュコマンド

- `/export` — **現在の** セッションを export。セッション未
  ロード時はステータスメッセージのみで no-op。Agent が Busy
  なら `ErrAgentBusy` を通知。
- `/import` — Open dialog を開く (bottom-nav ボタンと同じ)。

### 6.4 ファイルダイアログ

両方の dialog で Wails ランタイムの save/open dialog API を
使う:

- Export: `SaveFileDialog`、`Filters: [{DisplayName: "shell-agent-v2 session", Pattern: "*.shellagent"}]`、`DefaultFilename` は §4.5 から。
- Import: `OpenFileDialog`、同フィルター。

---

## 7. 監査ログ

新規 INFO レベル log 行 2 種 (どちらも privacy-safe — fact 内容
やメッセージテキストを含まない):

- `session exported: id=<id> private=<bool> bytes=<N> objects=<K> dest=<path>`
- `session imported: original_id=<orig> new_id=<new> private=<bool> bytes=<N> objects=<K>`

`<path>` はユーザー自身が選択した destination パスなので、
これをログに残しても新たな disclosure を導入しない。両行とも
v0.3.0 で導入した log-level filter に従う。

---

## 8. 実装フェーズ

A → B → C の順に進める。A は UI なしで完全にテスト可能。

### Phase A — Bundle フォーマット + I/O

- 新パッケージ `internal/sessionio/`:
  - `Manifest` 構造体 + `MarshalManifest` / `UnmarshalManifest`
  - `ExportSession(srcDir, destPath, sessionMeta, objects []ObjectExport) error`
  - `ImportSession(srcPath, destBaseDir, objstore ObjstoreWriter) (newID string, manifest Manifest, err error)`
  - 参照書き換え器: `Record.ObjectIDs[]` / `Record.Content` /
    `SummaryEntry.Summary` に対する regex sweep + 構造化
    remap ヘルパー
  - Zip-slip 対策
  - Atomic dest 書込 (temp + rename)
  - `ObjstoreWriter` は小さなインタフェース (`Store(blob,
    type, mime, origName, sessionID, objectID)`)。本パッケージ
    を live objstore から疎結合に保ち、単体テストを容易に
    する。
- テスト:
  - Roundtrip: fixture セッションディレクトリ + fixture
    objects を作成 → export → import → 元と展開後を diff、
    chat.json / summaries.json 内の全 object ref が新 (登録
    された) ID を指し、fake objstore で resolve できることを
    確認。
  - 不正 bundle の拒否 (各検証ルール、objects index/blob 不
    一致を含む)。
  - Zip-slip の拒否。
  - タイトル衝突時の suffix 動作。
  - 参照書き換え器の単体テスト: legacy 12-hex ID、32-hex ID、
    `object:` prefix の有無、word boundary 上の ID、マッチ
    すべきでない例 (より長い文字列内の random hex 等)。

### Phase B — State-machine 統合

- `internal/sessionio` 内に sessionID をキーとする per-session
  export ロック。
- `agent` パッケージへフック: `ExportActiveSession` メソッド
  が session-write ロックを取得 → flush → Engine close →
  各所有 object に対し `objstore.ListBySession()` + `Load()` →
  bundle へ copy → release。
- `ImportSession` agent メソッド: `sessionio` 経由で展開し
  (live objstore へ object 登録 + 参照書換も sessionio が
  実施)、自動切替のため `LoadSession(newID)` を呼ぶ。
- テスト:
  - Export とツール呼出を同時実行 → agent が `ErrAgentBusy`
    を通知。
  - 別セッションが active な状態で非 active セッションを
    export → active を妨げず成功。
  - DuckDB Engine の close + lazy reopen を、export 後の解析
    呼出で検証。
  - 同じ bundle を多重 import → 両セッションが異なる新 ID と
    異なる新 object ID を持ち、objstore 衝突なし。

### Phase C — Bindings + フロントエンド

- `bindings.ExportSession(id, dest) error`。
- `bindings.ImportSession(src) (string, error)` で新 ID を返す。
- Wails save/open dialog。
- Sidebar export アイコン (hover 行)。
- Sidebar `Import Chat` ボタン (bottom-nav)。
- `/export` および `/import` スラッシュコマンド。
- Import 後のフロントエンド自動ロード。
- §10 の手動 smoke。

---

## 9. 本リリースで扱わないこと

- Bundle の **暗号化** (keyring が必要; 後送り)。
- **一括 export / import** (1 つの bundle に複数セッション)。
- **クロスバージョンのスキーマ移行** (v1 のみ; 将来バージョン
  は migrator がない限り v1 bundle を拒否する)。
- **クラウド同期** (リモートバックエンド統合なし)。
- **選択的 export** (例: 「DuckDB を除く全部」)。
- Export 同士の **diff / merge**。
- Import 前の **bundle プレビュー** (即 import する)。

---

## 10. 手動 smoke チェックリスト

**Phase A/B (CLI 単独):**
1. 非アクティブセッションの roundtrip → import 成功、内容
   一致。
2. アクティブセッションの roundtrip → DuckDB が close され、
   次の解析呼出で再オープンされる; データ破損なし。
3. Manifest version を改ざん → 想定エラーで失敗。
4. Zip-slip bundle を作成 → import が拒否。
5. 添付画像と `create-report` 生成レポートを含むセッションの
   roundtrip → import 成功; imported 側の `Record.ObjectIDs[]`
   と `Record.Content` / `SummaryEntry.Summary` 内の
   `![alt](object:ID)` markdown が **新** object ID を指し、
   blob が resolve できる。
6. `objects/index.json` が存在しない blob を参照する bundle
   → import が想定エラーで拒否。
7. 多重 import: 同 bundle を 2 回 import → 両方成功し、新
   sessionID も新 objectID も別々; `objstore.Store` 衝突なし。

**Phase C (UI):**
1. 非アクティブセッションの sidebar Export アイコン →
   提案ファイル名付き save dialog → 選択パスにファイルが
   出力される。
2. Agent が Busy 状態で sidebar Export アイコン →
   tooltip「agent busy」表示; クリックは no-op もしくは
   エラー通知。
3. `/export` コマンド → 現在セッションに対し Export アイコン
   と同じ動作。
4. `Import Chat` ボタン → `*.shellagent` でフィルターした
   open dialog → import 後、imported セッションへ自動切替、
   サイドバーリスト更新。
5. `/import` コマンド → Import ボタンと同じ動作。
6. タイトル衝突: 既存タイトルと一致する bundle を import →
   imported タイトルに ` (imported)` suffix が付く。
7. プライバシー round-trip: private セッションを export →
   元を削除 → import → 🔒 表示、Pin ボタン非表示、監査ログ
   に `private=true`。
8. 非 private の round-trip: export → import → 🔒 なし、
   通常メモリルーティングが復活。
9. (可能なら) クロスマシン sanity: 1 マシンで export、別
   マシンで import → 正常に開ける、DuckDB クエリも動く、
   埋め込み画像が描画される。
10. `app.log` に export / import 時の新 INFO 行 2 つが
    現れ、`objects=K` が実際の object 数と一致。
11. UI 経由の画像 roundtrip: 画像添付付きメッセージを送信
    → export → 元セッション削除 → import → imported セッション
    で画像が描画され、開くと同一バイト列。

---

## 2026-05-07 解消の決定事項

| # | 質問 | 決定 |
|---|------|------|
| 1 | Bundle ファイル拡張子 | `.shellagent` (カスタム; ソフトな約束事) |
| 2 | Active session export 中の DuckDB | Close → copy → reopen (lazy)。State machine: Idle を待機、session-write ロック保持、reopen は次回利用に遅延。 |
| 3 | Import 時のタイトル衝突 | 自動 suffix ` (imported)`、` (imported 2)`、… |
| 4 | Export ファイル名 | 自動命名 `<title>-<YYYYMMDD-HHMMSS>.shellagent` + save dialog (ユーザーが上書き可能) |
| 5 | `work/` のフィルタリング | 全部含める (サイズ上限なし、`.DS_Store` フィルタなし) |
| 6 | Import 後の挙動 | Imported セッションへ自動切替 |
| 7 | Schema version 不一致 | 拒否 (v1 で移行ロジックを持たない) |
| 8 | Import 時の Object ID 戦略 | 常に新 ID を生成; `Record.ObjectIDs[]` (構造化)、`Record.Content` (regex sweep)、`SummaryEntry.Summary` (regex sweep) を書き換え。Bundle に `objects/index.json` + `objects/data/<id>` を含む。`session_memory.json` / `findings.json` は sweep 不要 — 書込パスが ref を埋めない。 |

# System Rules — リファレンス

> Status: **v0.7.0+** の挙動。Evergreen — コード進化に合わせて
> in-place 更新。設計根拠: [ADR-0012](../adr/0012-system-rules.ja.md)。
> Audience: エンドユーザー + コントリビューター。

System Rules はユーザーがオーサリングする単一の Markdown
ファイル。その内容を毎ターンの LLM system prompt の冒頭近くに
注入する。全セッションでエージェントに守って欲しい **恒久的な
指示** に使う — Claude Code 等の
[`AGENTS.md`](https://agents.md) / `CLAUDE.md` に相当する
shell-agent-v2 版。

System Rules は 5 つ目のメモリ施設では **無い**
([memory-model.ja.md](memory-model.ja.md) 参照)。4 つのメモリ
施設 (Records / Session Memory / Findings / Global Memory) は
*ランタイム中に学習* された状態を保持する。System Rules は
ユーザーが事前にオーサリングする *宣言的な設定* を保持する。

---

## 1. 用途

System Rules に向いている内容の例:

- `特に指示がない限り日本語で応答すること。`
- `コードを示すときは差分の隣に短い根拠を添えること。`
- `結果が約 20 行を超える場合は長文インライン回答ではなくレポート作成をデフォルトとすること。`
- `「rm -rf」系の操作を提案するときは必ず明示的な確認を求めること。`

不向き (Global Memory を使う):

- 「ユーザーは tab より space を好む」 — それは学習された
  preference。チャットから Pin to Global Memory で promote する。
- 「ユーザー名は Alice」 — 同様、Global Memory。

切り分け: **System Rules はエージェントの「どう振る舞うか」、
Global Memory は「ユーザーは誰か」を扱う。**

### すぐに使えるサンプル

リポジトリには
[`examples/system_rules/`](../../../examples/system_rules/) に
コピー&ペースト用テンプレートを同梱している。それぞれ特定の
繰り返し発生するエージェントバイアスや作業上の問題に焦点を当てた
もの。1 つ選んで `Settings → System Rules` を開き、貼り付け、
保存、新しいチャットを開始する。カタログと貢献ガイドラインは
[examples README](../../../examples/system_rules/README.md)
を参照。

---

## 2. 保存先

```
~/Library/Application Support/shell-agent-v2/system_rules.md
```

ファイルは平 UTF-8 Markdown — frontmatter 無し、スキーマ無し。
空ルールで Save するとディスクに空ファイルが書かれ、エージェントは
それを「ルール無し」として扱う。ファイル欠落も空ファイルと同等に
扱う (エラーは表面化しない)。

書込は atomic (tmp + rename + 親ディレクトリ fsync、
`internal/atomicio` 経由)。Save 途中で crash しても前の内容が
残る。

---

## 3. 編集

### Settings UI

**Settings → System Rules** で Markdown textarea を開く。フッタ
にライブの `chars · ~tokens` カウンタと、現役バックエンドの
`MaxContextTokens` に対する色分け助言が出る:

| context 予算に占める比率 | インジケータ |
|---|---|
| `< 5%` | 緑 (ok) |
| `5% – 20%` | 黄 (warn) |
| `≥ 20%` | 赤 (high) |

ボタン:

- **Save** — ファイル書込 + エージェントに伝播。textarea が
  未変更のときは disabled。
- **Reload from disk** — ファイル再読込。外部エディタで編集した
  あと使う。

### 外部エディタ

ファイルを直接開く:

```bash
$EDITOR ~/Library/Application\ Support/shell-agent-v2/system_rules.md
```

Settings → System Rules で **Reload from disk** をクリックすると
外部編集が textarea に反映される。エージェントは毎ターン disk
から fresh に読むので、Reload を押さなくても次のチャット
メッセージから新内容が反映される。

---

## 4. 注入方式

`chat.Engine.BuildSystemPrompt` は system prompt を以下のように
組み立てる:

```
<base prompt: エージェントのロール、ツール使用プロトコル、…>

The user has defined the following standing instructions. Treat
them as high-priority rules that override the default agent
behaviour unless they conflict with safety or security guidelines.

<system_rules>
<あなたの内容>
</system_rules>

Current date and time is …
Yesterday: …
[Location: …]

[sandbox guidance — sandbox 有効時]

Important facts you remember about the user:
<Global Memory>

Notes about the current session:
<Session Memory>

Analysis findings in this session:
<Findings>
```

空 / 欠落ルール → `<system_rules>` ブロック自体を完全省略 (ヘッダ
も marker も出さない、v0.6.6 と バイト一致)。前置文もルールが
空でない時のみ出す。

位置の根拠 (ADR-0012 §3.3):

- **base prompt の後**: base prompt がエージェントのコアロールと
  プロトコルを設定する。System Rules はその上に乗せるユーザー
  定義の調整。
- **temporal context / sandbox guidance / 3 メモリチャネルの前**:
  信頼済みユーザー指示を、派生ランタイムデータより前に置く。
  `feedback_prompt_injection_position` (防御指示は冒頭) を
  「*全ての* 信頼済み指示面を *全ての* データ面の前に」へ一般化
  したもの。

---

## 5. 信頼モデル

System Rules の内容は **全幅信頼** で読まれる。`<system_rules>`
エンベロープは clarity のため (LLM と人間デバッガが境界を
認識できるように)、injection 防御ではない。trust の方向はユーザー
データ guard と逆: System Rules はユーザー *から* の指示で、
ユーザー *についての* データではない。

**信頼できない内容を貼り付けないこと**。ファイルを編集できる者は
エージェント挙動に全権限を持つ。

---

## 6. トークン予算

System Rules は Global Memory / Session Memory / Findings と
全く同じく毎ターン context 予算を消費する。長いルールは会話履歴
とツール結果に使える予算を食う。

Settings UI はパーセント助言を出すのみで、silent truncation は
**しない**。ユーザーが書いた指示を黙って切り詰めるのは驚きだから
だ。50,000 tokens 書けば agent はそれを尊重し、会話履歴側で
コストを払う。

助言が黄/赤になったら、優先策:

1. **削る** — 大抵のルールは短くできる。エージェントに網羅的な
   散文は不要。
2. **Global Memory に昇格** — ルールが実は学習された preference
   なら Global Memory に pin し、System Rules から消す。

---

## 7. セッション途中でルールを変更した場合

新規/変更のルール Save は hot — 次ターンから新ルールが効く。
ただし現セッションの *過去* ターンはそのまま残る。LLM は会話履歴
で自分の過去応答を見るので、system prompt が変わっても以前の
パターンを模倣しがち (in-context conditioning)。

特に **クリア** した場合に目立つ:

> 「応答末尾に「にゅ」を付ける」というルールを設定して数ターン
> 会話したあとクリアした。次の assistant ターンでもまだ末尾に
> 「にゅ」が付く — ルールが残っているのではなく、モデルが
> 自分自身の過去出力を会話履歴で見て模倣しているため。

クリア時、Settings UI はこの旨の助言を表示する。変更が本当に
効いたか最も確実に検証する方法: **新規セッションを開く**。
fresh セッションには模倣すべき過去 assistant ターンが無いので、
現ルールのまま挙動する。

ルール *変更* 時 (クリアではなく) も同じ効果が小さく出る — 新旧
ルールが矛盾する場合、モデルが新パターンに落ち着くまで数ターン
の drift がありうる。

## 8. 並行性 & ホットリロード

Settings での Save は hot — 次のエージェントターンが自動的に
新内容を拾う。ターン進行中の Save は、進行中ターンの snapshot を
壊さない (そのターンの prompt は既に組み立て済み)。次のユーザー
メッセージで `a.mu` 配下の `agent.SystemRules()` 経由で Store から
fresh に読み直す。

`BuildSystemPrompt` は `systemRules` を関数引数で受け取る (共有
engine field ではない)、ので Settings goroutine の Store 書込と
turn goroutine の Store 読込の間に data race は無い。ADR-0012
§3.3 参照。

---

## 9. 関連リンク

- [ADR-0012](../adr/0012-system-rules.ja.md) — 設計根拠と却下
  代替案。
- [memory-model.ja.md](memory-model.ja.md) — 4 メモリ施設と
  System Rules が memory ではなく configuration である理由。
- [`internal/sysrules/`](../../../app/internal/sysrules/) — Store
  実装。
- [INDEX.ja.md](../INDEX.ja.md) — ドキュメントカタログ全体。

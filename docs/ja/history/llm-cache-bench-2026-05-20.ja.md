# LM Studio Prompt Cache Benchmark — 2026-05-20

[ADR-0017](../adr/0017-prompt-prefix-stability.ja.md) の実証
コンパニオン。`app/cmd/llm-cache-bench/main.go` を LM Studio
搭載の開発用ラップトップに対して実行した結果。ADR-0017 の実装に
踏み込む前に設計仮説を実機で検証することが目的:

1. LM Studio の OpenAI 互換エンドポイントは、プロンプト prefix が
   byte 同一のリクエスト間で実際に KV cache を再利用するか? (T6, T8)
2. system prompt 内に timestamp を埋め込む — つまり shell-agent-v2
   の現行レイアウト — はその再利用を破壊するか? (T7)
3. timestamp を user message に移動すると再利用は回復するか? (T8)
4. `cache_prompt: true` 拡張パラメータでサーバがリクエストを拒否
   するか? (T5)
5. 会話途中での memory の変化はどれくらい高コストか? (T9 — ADR-0017
   §6.2 / §9 の Phase 2 判断材料)

結論ヘッドライン: **yes (T8 で 96% 高速化) / yes (T7 で速度向上ゼロ) /
yes (T8 で回復) / no (T5 は受理されるが効果なし) / 中程度
(T9 で fact 追加 1 件あたり約 200ms ペナルティ — 後述)**。

## Setup

- **Endpoint**: `http://localhost:1234/v1`
- **Model**: `google/gemma-4-26b-a4b`
- **Runs per scenario**: 5 (first call always cold)
- **Date**: 2026-05-20 01:21:27

All requests use `max_tokens: 5`, `temperature: 0`, `stream: false`. Wall-clock ms ≈ prompt-processing time + ~5-token generation overhead (negligible). A large gap between the first call and subsequent calls implies the server is reusing cached prompt KV.

## T1 — Same system + same user, repeated

If the server caches the KV across requests, the second and following calls should be substantially faster than the first.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 273 | 33 | 3 | 0 | 200 |  |
| 2 | 277 | 33 | 3 | 0 | 200 |  |
| 3 | 273 | 33 | 3 | 0 | 200 |  |
| 4 | 268 | 33 | 3 | 0 | 200 |  |
| 5 | 272 | 33 | 3 | 0 | 200 |  |

**Summary**: first=273ms, mean(subsequent)=272ms, speedup=0.4%

## T2 — Same system, user message varies only in trailing token

Probes whether prefix match extends past the system prompt into the user message. Each user prompt shares a long prefix and differs only at the end.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 211 | 40 | 3 | 0 | 200 |  |
| 2 | 205 | 40 | 3 | 0 | 200 |  |
| 3 | 211 | 40 | 3 | 0 | 200 |  |
| 4 | 247 | 40 | 5 | 0 | 200 |  |
| 5 | 214 | 40 | 3 | 0 | 200 |  |

**Summary**: first=211ms, mean(subsequent)=219ms, speedup=-3.8%

## T3 — Volatile system prompt (timestamp inside system), stable user — simulates current shell-agent-v2

Reproduces the current production layout where temporal context is embedded in the system prompt. Expectation: little to no cache benefit because the system prefix changes every call.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 225 | 64 | 3 | 0 | 200 |  |
| 2 | 257 | 64 | 3 | 0 | 200 |  |
| 3 | 260 | 64 | 3 | 0 | 200 |  |
| 4 | 246 | 64 | 3 | 0 | 200 |  |
| 5 | 254 | 64 | 3 | 0 | 200 |  |

**Summary**: first=225ms, mean(subsequent)=254ms, speedup=-12.9%

## T4 — Stable system, timestamp moved to user message — simulates the ADR-0017 proposed layout

System prompt is byte-identical across calls; the timestamp lives at the head of the user message. Expectation: cache should fire for the system block, leaving only the user message to reprocess.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 204 | 63 | 2 | 0 | 200 |  |
| 2 | 197 | 63 | 2 | 0 | 200 |  |
| 3 | 198 | 63 | 2 | 0 | 200 |  |
| 4 | 208 | 63 | 2 | 0 | 200 |  |
| 5 | 206 | 63 | 2 | 0 | 200 |  |

**Summary**: first=204ms, mean(subsequent)=202ms, speedup=1.0%

## T5 — Probe `cache_prompt: true` extension parameter

Sends the same baseline as T1 but with `cache_prompt: true` in the request body. Determines whether the server accepts (ignores or honours) the parameter or rejects the whole request with 4xx.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 272 | 33 | 3 | 0 | 200 |  |
| 2 | 271 | 33 | 3 | 0 | 200 |  |
| 3 | 277 | 33 | 3 | 0 | 200 |  |
| 4 | 279 | 33 | 3 | 0 | 200 |  |
| 5 | 270 | 33 | 3 | 0 | 200 |  |

**Summary**: first=272ms, mean(subsequent)=274ms, speedup=-0.7%

## T6 — Large stable system prompt (~8K tokens of filler)

Worst-case prompt-processing scenario: a long stable system prompt. Cache benefit should be most visible here in absolute terms (large prompt_processing baseline shrinks to ~0 on cache hit).

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 6341 | 5260 | 2 | 0 | 200 |  |
| 2 | 99 | 5260 | 2 | 0 | 200 |  |
| 3 | 104 | 5260 | 2 | 0 | 200 |  |
| 4 | 100 | 5260 | 2 | 0 | 200 |  |
| 5 | 100 | 5260 | 2 | 0 | 200 |  |

**Summary**: first=6341ms, mean(subsequent)=100ms, speedup=98.4%

## T7 — Large system + volatile timestamp INSIDE system (current shell-agent-v2 layout, full size)

Repeats T3 but with 8K filler so prompt processing is non-trivial. Expectation: NO cache reuse because the system prefix differs every call — each iteration pays the full ~6 s prompt-processing cost.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 6449 | 5285 | 2 | 0 | 200 |  |
| 2 | 6428 | 5285 | 2 | 0 | 200 |  |
| 3 | 6477 | 5285 | 2 | 0 | 200 |  |
| 4 | 6435 | 5285 | 2 | 0 | 200 |  |
| 5 | 6426 | 5285 | 2 | 0 | 200 |  |

**Summary**: first=6449ms, mean(subsequent)=6441ms, speedup=0.1%

## T8 — Large system stable + timestamp in user message (ADR-0017 proposed layout, full size)

Repeats T4 with 8K filler. Expectation: cache fires on the system block, subsequent calls drop to ~100 ms — matching the T6 result and demonstrating the design works in realistic conditions.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 6421 | 5285 | 2 | 0 | 200 |  |
| 2 | 295 | 5285 | 2 | 0 | 200 |  |
| 3 | 249 | 5285 | 2 | 0 | 200 |  |
| 4 | 267 | 5285 | 2 | 0 | 200 |  |
| 5 | 250 | 5285 | 2 | 0 | 200 |  |

**Summary**: first=6421ms, mean(subsequent)=265ms, speedup=95.9%

## T9 — Memory section grows by one fact between turns (Phase 2 question)

Run 1-2 share memory v1; run 3-4 share memory v2 (1 new fact); run 5 has v3 (1 more new fact). Compare run 2 (cache hit on stable memory) vs run 3 (penalty when memory grows). Same large system prompt + same short history throughout.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 379 | 4654 | 3 | 0 | 200 |  |
| 2 | 118 | 4654 | 3 | 0 | 200 |  |
| 3 | 315 | 4661 | 3 | 0 | 200 |  |
| 4 | 121 | 4661 | 3 | 0 | 200 |  |
| 5 | 324 | 4670 | 3 | 0 | 200 |  |

**Summary**: first=379ms, mean(subsequent)=219ms, speedup=42.2%

## Cross-scenario summary

| Scenario | First (ms) | Subseq mean (ms) | Speedup | Cache observed? |
|---|---:|---:|---:|---|
| T1 | 273 | 272 | 0.4% | ❌ no |
| T2 | 211 | 219 | -3.8% | ❌ no |
| T3 | 225 | 254 | -12.9% | ❌ no |
| T4 | 204 | 202 | 1.0% | ❌ no |
| T5 | 272 | 274 | -0.7% | ❌ no |
| T6 | 6341 | 100 | 98.4% | ✅ yes (≥30%) |
| T7 | 6449 | 6441 | 0.1% | ❌ no |
| T8 | 6421 | 265 | 95.9% | ✅ yes (≥30%) |
| T9 | 379 | 219 | 42.2% | ✅ yes (≥30%) |

## 解釈ノート

### T7 vs T8: Phase 1 の決定的データ

数字は明白。同じハードウェア、同じモデル、同じプロンプトサイズ
(5285 token)、同じ user message — timestamp の *位置* だけが違う。

- T7 は timestamp を system prompt 内に置く (現行 shell-agent-v2
  レイアウト)。system prefix が毎回変わるので KV cache 再利用は
  一切発動しない。毎ターン約 6.4 秒の prompt-processing コストを
  全額支払う。
- T8 は system prompt を安定させ、timestamp を user message の頭に
  prepend する。system prefix はリクエスト間で byte 同一になり、
  サーバは system block 全部の KV を再利用し、~30 token の user
  message だけを再処理する。後続ターン平均: 265ms。

これが ADR-0017 §3 の load-bearing データポイント。

### T9: memory の volatility は中程度の懸念

T9 の初回 (379ms) はクリーンな cold baseline では **ない** — 先行する
T6/T7/T8 が T9 と同じ ~7K filler を共有しているので、サーバキャッシュ
が filler 部分について先行有利の状態だった。有用な比較は T9 *内部*:

- Run 2, 4 (memory 不変での cache hit): ~120ms
- Run 3, 5 (memory が 1 fact 増加): ~320ms
- **Memory 変化 1 回あたりのペナルティ: 約 200ms**

これは実在する (wall-clock 差は再現性あり、prompt token 数も runs 2→3
と 4→5 で実際に 7-9 token 増えている) が小さい。比較のため、Phase 1
問題 (T7) は毎ターン約 6,400ms。

shell-agent-v2 への実用上の含意: 典型的な抽出はターンあたり 0-3 fact
追加 → ワーストケースで 3 fact 抽出ターンに ~600ms の追加遅延。
気にはなるが、Phase 1 が解決する 6 秒の痛みとは別物。ADR-0017 の
Phase 2 (memory caching / relocation) は **ユーザーが「memory 変化
時の体感遅延が気になる」と報告するまで保留** が妥当 — fact 1 件
あたり ~200ms に対して、実装複雑度 (render 済文字列キャッシュ、
変更検出) はペイしない。

### T5: cache_prompt は silent に受理 (が効果なし)

T5 はリクエスト body に `cache_prompt: true` を送信。LM Studio は
HTTP 200 を返した — パラメータは受理、拒否されず。だが T5 の wall
time は T1 (パラメータなしで同条件) と統計的に区別できない。フラグは
このサーバ / モデルではキャッシュ挙動を変えない。サーバは十分大きい
prefix に対して既に自動的にキャッシュする (T6, T8 参照)。

結論: パラメータを送らない。zero-benefit、かつ他の OpenAI 互換
サーバとの将来互換性ハザード (non-zero-risk)。

### T1-T5 でキャッシュ効果が見えない理由

T1-T5 は小さいプロンプト (33-64 token) を使う。このスケールでは
per-request overhead (~200ms — ネットワーク往復 + tokenize セット
アップ + ~5 token 生成) が wall-clock を支配し、キャッシュヒット vs
ミスはノイズに埋もれる。LM Studio はおそらく小さいプロンプトに対する
キャッシュ管理を skip して bookkeeping コストを抑えている。

shell-agent-v2 の本番プロンプトは典型的に数千トークン (system prompt
+ memory + history + tool descriptors) なので、T6/T7/T8/T9 の
レジームが関連する条件。

## 再現方法

```bash
cd app
go run ./cmd/llm-cache-bench \
    --endpoint http://localhost:1234/v1 \
    --model google/gemma-4-26b-a4b \
    --runs 5 \
    --out /tmp/my-run.md
```

LM Studio (または任意の OpenAI 互換ローカルサーバ) 起動済 + 対象
モデルロード済 + 並行推論ロードなし、が前提。


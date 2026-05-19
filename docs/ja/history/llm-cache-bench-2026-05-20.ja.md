# LM Studio Prompt Cache Benchmark — 2026-05-20

[ADR-0017](../adr/0017-prompt-prefix-stability.ja.md) と
[ADR-0018](../adr/0018-guard-nonce-stability.ja.md) の実証
コンパニオン。`app/cmd/llm-cache-bench/main.go` を LM Studio
搭載の開発用ラップトップに対して実行した結果。実装前に設計仮説を
実機で検証することが目的:

1. **T1-T9** (ADR-0017): LM Studio の OpenAI 互換エンドポイントは、
   prompt prefix が byte 同一の時に実際に KV cache を再利用するか?
   何が破壊するか (system 内 timestamp → T7)、何が救うか (user 内
   timestamp → T8)?
2. **T10a-c** (ADR-0018): production の実 wrap
   (`<user_data_XXX>...</user_data_XXX>` で各 user record を包む)
   を伴う条件で、`nlk/guard` の per-turn nonce 回転が ADR-0017 の
   勝ちを silent に潰しているのではないか? T10 ファミリーで定量化
   + 提案する scan-and-rotate 案も検証。

結論ヘッドライン:

- T8 (ADR-0017 レイアウト) → 安定 prefix で 96% 高速化。
- T10a (現行 production の per-turn nonce) → **0% 高速化** —
  ADR-0017 の勝ちが production セッションで silent に失われていた
  ことを確認。
- T10b (per-session-stable nonce、ADR-0018 通常ケース) → **93%
  高速化** — 勝ちを取り戻す。
- T10c (rotate-on-detection、ADR-0018 漏洩検知ケース) → rotate
  イベントで 1 ターンだけ高コスト、その後また cache warm。

## Setup

- **Endpoint**: `http://localhost:1234/v1`
- **Model**: `google/gemma-4-26b-a4b`
- **Runs per scenario**: 5 (first call always cold)
- **Date**: 2026-05-20 08:36:52

All requests use `max_tokens: 5`, `temperature: 0`, `stream: false`. Wall-clock ms ≈ prompt-processing time + ~5-token generation overhead (negligible). A large gap between the first call and subsequent calls implies the server is reusing cached prompt KV.

## T1 — Same system + same user, repeated

If the server caches the KV across requests, the second and following calls should be substantially faster than the first.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 277 | 33 | 3 | 0 | 200 |  |
| 2 | 276 | 33 | 3 | 0 | 200 |  |
| 3 | 274 | 33 | 3 | 0 | 200 |  |
| 4 | 269 | 33 | 3 | 0 | 200 |  |
| 5 | 276 | 33 | 3 | 0 | 200 |  |

**Summary**: first=277ms, mean(subsequent)=273ms, speedup=1.4%

## T2 — Same system, user message varies only in trailing token

Probes whether prefix match extends past the system prompt into the user message. Each user prompt shares a long prefix and differs only at the end.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 205 | 40 | 3 | 0 | 200 |  |
| 2 | 210 | 40 | 3 | 0 | 200 |  |
| 3 | 203 | 40 | 3 | 0 | 200 |  |
| 4 | 245 | 40 | 5 | 0 | 200 |  |
| 5 | 215 | 40 | 3 | 0 | 200 |  |

**Summary**: first=205ms, mean(subsequent)=218ms, speedup=-6.3%

## T3 — Volatile system prompt (timestamp inside system), stable user — simulates current shell-agent-v2

Reproduces the current production layout where temporal context is embedded in the system prompt. Expectation: little to no cache benefit because the system prefix changes every call.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 226 | 64 | 3 | 0 | 200 |  |
| 2 | 251 | 64 | 3 | 0 | 200 |  |
| 3 | 254 | 64 | 3 | 0 | 200 |  |
| 4 | 247 | 64 | 3 | 0 | 200 |  |
| 5 | 254 | 64 | 3 | 0 | 200 |  |

**Summary**: first=226ms, mean(subsequent)=251ms, speedup=-11.1%

## T4 — Stable system, timestamp moved to user message — simulates the ADR-0017 proposed layout

System prompt is byte-identical across calls; the timestamp lives at the head of the user message. Expectation: cache should fire for the system block, leaving only the user message to reprocess.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 199 | 63 | 2 | 0 | 200 |  |
| 2 | 197 | 63 | 2 | 0 | 200 |  |
| 3 | 200 | 63 | 2 | 0 | 200 |  |
| 4 | 199 | 63 | 2 | 0 | 200 |  |
| 5 | 199 | 63 | 2 | 0 | 200 |  |

**Summary**: first=199ms, mean(subsequent)=198ms, speedup=0.5%

## T5 — Probe `cache_prompt: true` extension parameter

Sends the same baseline as T1 but with `cache_prompt: true` in the request body. Determines whether the server accepts (ignores or honours) the parameter or rejects the whole request with 4xx.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 272 | 33 | 3 | 0 | 200 |  |
| 2 | 272 | 33 | 3 | 0 | 200 |  |
| 3 | 277 | 33 | 3 | 0 | 200 |  |
| 4 | 276 | 33 | 3 | 0 | 200 |  |
| 5 | 262 | 33 | 3 | 0 | 200 |  |

**Summary**: first=272ms, mean(subsequent)=271ms, speedup=0.4%

## T6 — Large stable system prompt (~8K tokens of filler)

Worst-case prompt-processing scenario: a long stable system prompt. Cache benefit should be most visible here in absolute terms (large prompt_processing baseline shrinks to ~0 on cache hit).

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 6232 | 5260 | 2 | 0 | 200 |  |
| 2 | 119 | 5260 | 2 | 0 | 200 |  |
| 3 | 102 | 5260 | 2 | 0 | 200 |  |
| 4 | 98 | 5260 | 2 | 0 | 200 |  |
| 5 | 99 | 5260 | 2 | 0 | 200 |  |

**Summary**: first=6232ms, mean(subsequent)=104ms, speedup=98.3%

## T7 — Large system + volatile timestamp INSIDE system (current shell-agent-v2 layout, full size)

Repeats T3 but with 8K filler so prompt processing is non-trivial. Expectation: NO cache reuse because the system prefix differs every call — each iteration pays the full ~6 s prompt-processing cost.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 6591 | 5285 | 2 | 0 | 200 |  |
| 2 | 6627 | 5285 | 2 | 0 | 200 |  |
| 3 | 6593 | 5285 | 2 | 0 | 200 |  |
| 4 | 6420 | 5285 | 2 | 0 | 200 |  |
| 5 | 6314 | 5285 | 2 | 0 | 200 |  |

**Summary**: first=6591ms, mean(subsequent)=6488ms, speedup=1.6%

## T8 — Large system stable + timestamp in user message (ADR-0017 proposed layout, full size)

Repeats T4 with 8K filler. Expectation: cache fires on the system block, subsequent calls drop to ~100 ms — matching the T6 result and demonstrating the design works in realistic conditions.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 6273 | 5285 | 2 | 0 | 200 |  |
| 2 | 290 | 5285 | 2 | 0 | 200 |  |
| 3 | 244 | 5285 | 2 | 0 | 200 |  |
| 4 | 245 | 5285 | 2 | 0 | 200 |  |
| 5 | 259 | 5285 | 2 | 0 | 200 |  |

**Summary**: first=6273ms, mean(subsequent)=259ms, speedup=95.9%

## T9 — Memory section grows by one fact between turns (Phase 2 question)

Run 1-2 share memory v1; run 3-4 share memory v2 (1 new fact); run 5 has v3 (1 more new fact). Compare run 2 (cache hit on stable memory) vs run 3 (penalty when memory grows). Same large system prompt + same short history throughout.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 363 | 4654 | 3 | 0 | 200 |  |
| 2 | 139 | 4654 | 3 | 0 | 200 |  |
| 3 | 327 | 4661 | 3 | 0 | 200 |  |
| 4 | 115 | 4661 | 3 | 0 | 200 |  |
| 5 | 322 | 4670 | 3 | 0 | 200 |  |

**Summary**: first=363ms, mean(subsequent)=225ms, speedup=38.0%

## T10a — Per-turn guard nonce rotation (v0.13.0 production behaviour)

Simulates the current production wrapping: every turn gets a fresh nonce, so every wrapped user record in the conversation history has different bytes between turns. Expectation: NO cache reuse — every turn pays the full prompt-processing cost because the byte prefix diverges immediately after the system block.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 1644 | 4348 | 3 | 0 | 200 |  |
| 2 | 1503 | 4308 | 3 | 0 | 200 |  |
| 3 | 1507 | 4308 | 3 | 0 | 200 |  |
| 4 | 1586 | 4324 | 3 | 0 | 200 |  |
| 5 | 1585 | 4324 | 3 | 0 | 200 |  |

**Summary**: first=1644ms, mean(subsequent)=1545ms, speedup=6.0%

## T10b — Per-session stable guard nonce (ADR-0018 normal case)

Same setup as T10a but the nonce is held constant for all runs — modelling the proposed scan-and-rotate normal case where no leak is detected and the nonce stays put. Expectation: cache fires for the full conversation history starting from run 2.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 1519 | 4316 | 3 | 0 | 200 |  |
| 2 | 109 | 4316 | 3 | 0 | 200 |  |
| 3 | 105 | 4316 | 3 | 0 | 200 |  |
| 4 | 106 | 4316 | 3 | 0 | 200 |  |
| 5 | 104 | 4316 | 3 | 0 | 200 |  |

**Summary**: first=1519ms, mean(subsequent)=106ms, speedup=93.0%

## T10c — Rotate-on-detection mid-conversation (ADR-0018 leak case)

Runs 1-2 share nonce A; runs 3-5 share nonce B (simulating a detected nonce leak that triggered a rotate). Expectation: run 3 invalidates the cached history (byte divergence at first user record); runs 4-5 hit cache for the new history. The bench answers: how expensive is one rotation event? — i.e. the worst-case turn under the proposed design.

| Run | wall ms | prompt tok | out tok | cached tok | http | error |
|---:|---:|---:|---:|---:|---:|---|
| 1 | 1614 | 4332 | 3 | 0 | 200 |  |
| 2 | 112 | 4332 | 3 | 0 | 200 |  |
| 3 | 1511 | 4316 | 3 | 0 | 200 |  |
| 4 | 113 | 4316 | 3 | 0 | 200 |  |
| 5 | 111 | 4316 | 3 | 0 | 200 |  |

**Summary**: first=1614ms, mean(subsequent)=461ms, speedup=71.4%

## Cross-scenario summary

| Scenario | First (ms) | Subseq mean (ms) | Speedup | Cache observed? |
|---|---:|---:|---:|---|
| T1 | 277 | 273 | 1.4% | ❌ no |
| T2 | 205 | 218 | -6.3% | ❌ no |
| T3 | 226 | 251 | -11.1% | ❌ no |
| T4 | 199 | 198 | 0.5% | ❌ no |
| T5 | 272 | 271 | 0.4% | ❌ no |
| T6 | 6232 | 104 | 98.3% | ✅ yes (≥30%) |
| T7 | 6591 | 6488 | 1.6% | ❌ no |
| T8 | 6273 | 259 | 95.9% | ✅ yes (≥30%) |
| T9 | 363 | 225 | 38.0% | ✅ yes (≥30%) |
| T10a | 1644 | 1545 | 6.0% | ❌ no |
| T10b | 1519 | 106 | 93.0% | ✅ yes (≥30%) |
| T10c | 1614 | 461 | 71.4% | ✅ yes (≥30%) |


## 解釈ノート

### T7 vs T8 (ADR-0017 Phase 1 の証拠)

同じハード、同じモデル、同じプロンプトサイズ (5285 token)、同じ
user message — timestamp の *位置* だけが違う。

- T7 (system prompt 内に timestamp) — system prefix が毎回変わり、
  KV cache 再利用は不発、~6.4 s/ターン。
- T8 (timestamp を user message へ移動) — system 安定、cache は
  system block 全体を再利用。初回以降 ~265 ms。

ADR-0017 §3 の load-bearing データポイント。

### T10a / T10b / T10c (ADR-0018 の証拠)

これが v0.13.0 production テストで ADR-0017 の勝ちが見えなかった
理由:

- **T10a** は v0.13.0 の実挙動を simulate: wrap nonce が
  `BuildSystemPrompt` 内 `e.guardTag = guard.NewTag()` で毎ターン
  回転する。会話履歴内の各 wrapped user record が毎ターン違う nonce
  で render され、byte prefix は system block の直後で diverge。
  **結果: runs 2-5 で 0% 高速化。** system block は cache されるが、
  会話履歴は cache されず、履歴が prompt サイズの大部分 (4308 token)
  を占めるので効果ゼロ。
- **T10b** は ADR-0018 通常ケースを simulate: 1 つの安定 nonce を
  セッション全体で使う。**結果: 93% 高速化** (1519 → 106 ms)。
  per-turn rotation が silent に捨てていた勝ちを取り戻す。
- **T10c** は rotation-on-detection を simulate: runs 1-2 が nonce
  A、runs 3-5 が nonce B (turn 3 で漏洩検知してローテートしたと
  仮定)。run 3 は期待通り cache miss (~1500 ms) だが、runs 4-5 は
  即座に cache warm に復帰 (~110 ms)。**検知イベントあたり高コスト
  1 ターン** が scan-and-rotate 設計のワーストケース。

### T9 (memory volatility — ADR-0017 Phase 2 deferred 判断材料)

T9 の初回 (363 ms) はクリーンな cold baseline では **ない** —
先行する T6/T7/T8 が T9 と同じ ~7K filler を共有しているので、
サーバキャッシュが filler 部分で先行有利。有用な比較は T9 *内部*:

- Runs 2, 4 (memory 不変での cache hit): ~120 ms
- Runs 3, 5 (memory が 1 fact 増加): ~325 ms
- **Memory 変化 1 回あたりのペナルティ: 約 200 ms**

実在するが小さい。この規模では ADR-0017 Phase 2 (memory
caching / relocation) は **defer** — ユーザーが残余レイテンシを
報告したら再評価。

### T5: cache_prompt は silent に受理 (が効果なし)

T5 はリクエスト body に `cache_prompt: true` を送信。LM Studio
は HTTP 200 を返した — パラメータは受理、拒否されず。だが T5 の
wall time は T1 (パラメータなしで同条件) と統計的に区別できない。
フラグはこのサーバ / モデルではキャッシュ挙動を変えない。サーバ
は十分大きい prefix に対して既に自動的にキャッシュする。

### T1-T5 でキャッシュ効果が見えない理由

T1-T5 は小さいプロンプト (33-64 token) を使う。このスケールでは
per-request overhead (~200ms — ネットワーク往復 + tokenize セット
アップ + ~5 token 生成) が wall-clock を支配し、cache hit vs miss
はノイズに埋もれる。LM Studio はおそらく小さいプロンプトに対する
キャッシュ管理を skip して bookkeeping コストを抑えている。

shell-agent-v2 の本番プロンプトは典型的に数千トークン (system
prompt + memory + history + tool descriptors) なので、T6/T7/T8/T10
のレジームが関連する条件。

## 再現方法

```bash
cd app
go run ./cmd/llm-cache-bench \
    --endpoint http://localhost:1234/v1 \
    --model google/gemma-4-26b-a4b \
    --runs 5 \
    --out /tmp/my-run.md
```

LM Studio (または任意の OpenAI 互換ローカルサーバ) 起動済 +
対象モデルロード済 + 並行推論ロードなし、が前提。

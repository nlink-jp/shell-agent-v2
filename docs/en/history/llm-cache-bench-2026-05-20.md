# LM Studio Prompt Cache Benchmark — 2026-05-20

Empirical companion to [ADR-0017](../adr/0017-prompt-prefix-stability.md)
and [ADR-0018](../adr/0018-guard-nonce-stability.md). Produced by
`app/cmd/llm-cache-bench/main.go` against a developer laptop
running LM Studio. The point of the experiments was to validate
the design hypotheses before committing implementation:

1. **T1-T9** (ADR-0017): Does LM Studio's OpenAI-compatible
   endpoint actually reuse KV cache across requests when the
   prompt prefix is byte-identical? What kills it (timestamp in
   system → T7), what saves it (timestamp in user → T8)?
2. **T10a-c** (ADR-0018): With realistic production wrapping
   (`<user_data_XXX>...</user_data_XXX>` around each user record),
   the per-turn nonce rotation in `nlk/guard` was suspected of
   defeating ADR-0017's win. T10 family quantifies it and tests
   the proposed scan-and-rotate alternative.

Headline answers:

- T8 (ADR-0017 layout) → 96 % speedup on stable prefix.
- T10a (current production behaviour with per-turn nonce) → **0 %
  speedup** — confirms ADR-0017's win is silently lost on every
  real-world session because of the wrap nonce rotation.
- T10b (per-session-stable nonce, the ADR-0018 normal case) → **93 %
  speedup** — recovers the win.
- T10c (rotate-on-detection, the ADR-0018 leak case) → one
  expensive turn at the rotation event, then back to cache-warm.

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


## Interpretation notes

### T7 vs T8 (ADR-0017 Phase 1 evidence)

Same hardware, same model, same prompt size (5285 tokens), same
user message — only the *position* of the timestamp differs.

- T7 (timestamp inside system prompt) — system prefix changes
  every call, KV-cache reuse never fires, ~6.4 s/turn.
- T8 (timestamp moved into user message) — system stable, cache
  reuses the entire system block. ~265 ms after the first call.

Load-bearing data point for ADR-0017 §3.

### T10a / T10b / T10c (ADR-0018 evidence)

This is the story behind why ADR-0017's win didn't show up in
v0.13.0 production tests:

- **T10a** simulates v0.13.0's actual behaviour: the wrap nonce
  rotates per turn (via `e.guardTag = guard.NewTag()` inside
  `BuildSystemPrompt`). Every wrapped user record in the
  conversation history renders with a different nonce in each
  turn's request, so the byte prefix diverges right after the
  system block. **Result: 0 % speedup across runs 2-5.** The
  system block is cached but the conversation history isn't, and
  the history dominates the prompt size at 4308 tokens.
- **T10b** simulates the proposed ADR-0018 normal case: one
  stable nonce for the whole session. **Result: 93 % speedup**
  (1519 → 106 ms). Recovers the win the per-turn rotation was
  silently throwing away.
- **T10c** simulates rotation-on-detection: runs 1-2 share
  nonce A, runs 3-5 share nonce B (as if a leak was detected at
  turn 3). Run 3 sees a cache miss as expected (~1500 ms), but
  runs 4-5 immediately return to cache-warm (~110 ms). One
  expensive turn per detection event is the worst-case cost of
  the scan-and-rotate design.

### T9 (memory volatility — informs deferred Phase 2 of ADR-0017)

T9's first call (363 ms) is NOT a clean cold baseline — the
preceding T6/T7/T8 scenarios share the same ~7K filler, so the
server's cache had a head-start on the filler portion. The useful
comparison is *within* T9:

- Runs 2, 4 (cache hit on stable memory): ~120 ms
- Runs 3, 5 (memory grew by one fact): ~325 ms
- **Penalty per memory mutation: ~200 ms**

Real, reproducible, but small. ADR-0017 Phase 2 (memory caching /
relocation) is **deferred** at this magnitude — re-evaluate if
users surface the residual latency.

### T5: cache_prompt is silently accepted (and useless here)

T5 sent `cache_prompt: true` in the request body. LM Studio
returned HTTP 200 — the parameter was accepted, not rejected.
But T5's wall times are statistically indistinguishable from T1
(same scenario without the parameter), so the flag did not
change caching behaviour on this server / model. The server
already caches automatically for sufficiently large prefixes.

### Why T1-T5 show no cache effect

T1-T5 use small prompts (33-64 tokens). At that scale the
per-request overhead (~200 ms — network round-trip + tokeniser
setup + ~5-token generation) dominates the wall-clock; cache hit
or miss is in the noise. LM Studio likely skips cache management
for tiny prompts to avoid amortising bookkeeping costs.

shell-agent-v2's production prompts are typically thousands of
tokens, so T6/T7/T8/T10 are the relevant regimes.

## Reproducing

```bash
cd app
go run ./cmd/llm-cache-bench \
    --endpoint http://localhost:1234/v1 \
    --model google/gemma-4-26b-a4b \
    --runs 5 \
    --out /tmp/my-run.md
```

Requires LM Studio (or any OpenAI-compatible local server) up,
target model loaded, no concurrent inference load.

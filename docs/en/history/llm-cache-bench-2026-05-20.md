# LM Studio Prompt Cache Benchmark — 2026-05-20

Empirical companion to [ADR-0017](../adr/0017-prompt-prefix-stability.md).
Produced by `app/cmd/llm-cache-bench/main.go` against a developer
laptop running LM Studio. The point of the experiment was to
validate the design hypotheses before committing to ADR-0017's
implementation:

1. Does LM Studio's OpenAI-compatible endpoint actually reuse KV
   cache across requests when the prompt prefix is byte-identical?
   (T6, T8)
2. Does embedding a timestamp inside the system prompt — the
   current shell-agent-v2 layout — defeat that reuse? (T7)
3. Does moving the timestamp into the user message recover the
   reuse? (T8)
4. Does the `cache_prompt: true` extension parameter cause the
   server to reject the request? (T5)
5. How expensive is mid-conversation memory mutation? (T9 — informs
   the Phase 2 decision in ADR-0017 §6.2 / §9.)

Headline answers: **yes (96 % speedup on T8), yes (T7 sees zero
speedup), yes (T8 recovers it), no (T5 is accepted as a no-op),
and modestly (~200 ms penalty per added fact in T9 — see
discussion at the bottom).**

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

## Interpretation notes

### T7 vs T8: the Phase 1 result

The numbers are decisive. Same hardware, same model, same prompt
size (5285 tokens), same user message — only the *position* of the
timestamp differs.

- T7 puts the timestamp inside the system prompt (current
  shell-agent-v2 layout). The system prefix changes every call,
  so KV cache reuse never fires. Every turn pays the full
  ~6.4 s prompt-processing cost.
- T8 keeps the system prompt stable and prepends the timestamp
  to the user message. The system prefix is byte-identical
  across calls; KV cache reuses for the entire system block and
  the server only re-processes the ~30-token user message.
  Average subsequent turn: 265 ms.

This is the load-bearing data point for ADR-0017 §3.

### T9: memory volatility is a modest concern

T9's first call (379 ms) is NOT a clean cold baseline — the
preceding T6/T7/T8 scenarios share the same ~7K filler with T9, so
the server's cache had a head-start on the filler portion. The
useful comparison is *within* T9:

- Runs 2, 4 (cache hit on stable memory): ~120 ms
- Runs 3, 5 (memory grew by one fact): ~320 ms
- **Penalty per memory mutation: ~200 ms**

This is real (the wall-clock difference is reproducible and the
prompt-token count actually grew by 7-9 tokens between runs 2→3
and 4→5), but small. For comparison, the Phase-1 problem (T7) was
~6 400 ms per turn.

Practical implication for shell-agent-v2: typical extraction adds
0-3 facts per turn. Worst case is ~600 ms additional latency on a
turn that extracted 3 facts. That's noticeable but not the
6-second pain Phase 1 addresses. ADR-0017's Phase 2 (memory
caching / relocation) should be **deferred until users report the
residual latency is bothersome** — the implementation complexity
(rendered-string caching, change detection) doesn't pay for itself
at ~200 ms per fact.

### T5: cache_prompt is silently accepted (and useless here)

T5 sent `cache_prompt: true` in the request body. LM Studio
returned HTTP 200 — the parameter was accepted, not rejected.
But T5's wall times are statistically indistinguishable from T1
(same scenario without the parameter), so the flag did not change
caching behaviour on this server / model. The server already
caches automatically for sufficiently large prefixes (see T6, T8).

Conclusion: skip the parameter. It's zero-benefit and a
non-zero-risk future compat hazard with other OpenAI-compatible
servers.

### Why T1-T5 show no cache effect

T1-T5 use small prompts (33-64 tokens). At that scale the
per-request overhead (~200 ms — network round-trip + tokenisation
setup + ~5-token generation) dominates the wall-clock; cache hit
or miss is in the noise. LM Studio likely skips cache management
for tiny prompts to avoid amortising bookkeeping costs.

shell-agent-v2's production prompts are typically thousands of
tokens (system prompt + memory + history + tool descriptors), so
the T6/T7/T8/T9 regime is the relevant one.

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


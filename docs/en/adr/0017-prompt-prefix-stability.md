# Prompt prefix stability for KV cache reuse — Design Note

**Status:** Proposed (2026-05-20).
**Target:** v0.13.0 (minor bump — internal message assembly
change; no schema migrations, no Wails binding signature changes).
**Reported by:** User — "when running shell-agent-v2 on local
LM Studio, the PROMPT PROCESSING time dominates each turn. Token
generation only takes a few seconds. The hand-feeling is that
every turn re-tokenises the entire prompt from scratch."

This note specifies a **prompt-prefix-stable message assembly**:
hold the system prompt and the conversation history byte-identical
across requests within a session, so that LM Studio's (and any
llama.cpp-backed server's) KV-cache prefix reuse can fire. The
load-bearing change is moving the temporal context (date / time)
out of the system prompt and into a per-record prefix derived from
each record's own `Timestamp` field. Empirically (see [the
benchmark report](../history/llm-cache-bench-2026-05-20.md)) this
produces a ~25× speedup on follow-up turns for realistic
shell-agent-v2 prompt sizes.

---

## 1. Problem

LM Studio's OpenAI-compatible `/v1/chat/completions` endpoint, like
the underlying `llama-server`, reuses KV cache for the longest
byte-identical prefix between the new request and the previously
cached one. When the prefix matches all the way through the system
block, the server skips re-tokenising and re-attending to those
tokens — a several-second saving on prompts in the 5K-token range.

shell-agent-v2's current message assembly defeats this. Inside
`chat.BuildSystemPrompt` (`internal/chat/chat.go:128`) the system
prompt is built as:

```
[base systemPrompt]
[+ System Rules block if present]
[+ Current date and time: 2026-05-20 12:34:56.789]  ← rebuilt every call
[+ Yesterday: 2026-05-19]                            ← rebuilt every call
[+ Location: …]
[+ sandbox guidance if enabled]
[+ Global Memory block]
[+ Session Memory block]
[+ Findings block]
```

Two of those lines change on every turn:

1. **Current date and time** — milliseconds precision; differs on
   every request.
2. **Yesterday** — derived from the current time; differs whenever
   midnight rolls over (rare).

Because the volatile timestamp lives **in the middle** of the
system prompt, the byte prefix differs from the previous request
starting around that line. Everything after it — sandbox guidance,
all three memory blocks, the full system prompt content — gets
re-tokenised and re-attended every turn. KV cache reuse never
fires for the system block.

For prompts around 5K tokens, the empirical measurement
([llm-cache-bench-2026-05-20.md](../history/llm-cache-bench-2026-05-20.md)
T7) shows this costs **~6.5 s of prompt-processing time every
turn**. With a stable system prompt (T8) the same hardware takes
**~250 ms** after the first turn — a 25× saving.

User-visible symptom: when the user finishes typing and hits SEND,
the spinner takes 6-10 s before the assistant begins responding,
even on substantive but otherwise short follow-up turns. Token
generation itself is fast; prompt processing dominates the
perceived latency.

---

## 2. Goals

1. **System prompt byte-stable across consecutive requests within
   a session.** Same memory state → same bytes. (Memory state
   changes are out of scope for Phase 1 — see §9.)
2. **Conversation history byte-stable.** Each historical record
   renders to the same bytes on every turn.
3. **Per-turn temporal context still reaches the model.** The
   information "the user is asking right now at time T" must remain
   in the prompt — otherwise the model can't answer relative date
   queries ("what happened yesterday?") correctly.
4. **No record format change, no migration.** `chat.json` schema
   stays as-is.
5. **No external dependency on `cache_prompt` or other extension
   parameters.** The benchmark confirms LM Studio caches
   automatically for sufficiently large prefixes; the explicit
   parameter is unnecessary and risks being rejected by other
   OpenAI-compatible servers.

Non-goals:

- **Vertex AI Gemini optimisation.** Vertex uses an explicit
  context-caching API (per-call `cache_id`) with its own pricing.
  Out of scope for this ADR. Separate work item if the
  cost / latency picture warrants it.
- **Memory-block stability** (Phase 2). Memory blocks change
  whenever `extractMemories` extracts a new fact. We measure the
  effect first (§6) before deciding whether to also move memory
  into the volatile area or to throttle extraction.
- **Tool descriptor stability.** Tool list shifts when sandbox
  becomes available, data is loaded, etc. Mostly stable within a
  session for a given activity; deferred.
- **Streaming vs non-streaming.** Streaming is already off
  (ADR-0015 §1) for a separate reason (Gemini Thought-leak); this
  ADR doesn't touch that.

---

## 3. Design

### 3.1 Where temporal context lives

**Move** `buildTemporalContext()` output **out of the system
prompt** and **into a per-record prefix** rendered at
message-build time from each record's stored `Timestamp`.

Concretely:

- `chat.BuildSystemPrompt` stops emitting `Current date and time`
  and `Yesterday` lines.
- A new helper, `chat.RenderRecordTemporalPrefix(ts time.Time)
  string`, returns the line that previously sat in the system
  prompt — but rendered from a specific timestamp rather than
  `time.Now()`.
- `contextbuild` (the message assembler) renders each `user`-role
  record's content as:

  ```
  [Time: 2026-05-20 12:34:56 (Tuesday) (UTC+09:00) — Yesterday: 2026-05-19 (Monday)]

  <stored content>
  ```

  The prefix is short (~20 tokens), human-readable, and parsed
  from the record's `Timestamp` field — so it's
  byte-deterministic on every replay.

The `Timestamp` field already exists on `memory.Record` and is
populated when the record is created. We're just reading it in a
new place.

### 3.2 Why the per-record approach (not "prepend to the latest user
message only")

The naïve fix — prepend temporal context only to the *most recent*
user message at request time — re-introduces the same cache-miss
problem one position later in the conversation. To see why,
consider turn 2:

- Turn 1 sent `[system, "Time: T1\n\n" + u1]`.
- Turn 2 sends `[system, ??? + u1, "Time: T2\n\n" + u2]`.

If the historical `u1` in turn 2 lacks the `"Time: T1\n\n"` prefix
(because it's no longer the latest), the server's cache hits up to
the end of the system block, then misses at `u1`. Prefix reuse
falls apart at the same point that today's `BuildSystemPrompt`
falls apart, just one record later.

Rendering temporal prefix from the *record's own Timestamp* —
**not** from `time.Now()` — keeps every historical user message's
rendering byte-stable across turns. Cache reuse extends all the
way through the conversation history. Only the new tokens for the
just-typed user message need processing.

### 3.3 What stays in the system prompt

After the change, `BuildSystemPrompt` emits:

```
[base systemPrompt]
[+ System Rules block]
[+ Location: … if set]                ← stable per session
[+ sandbox guidance if enabled]       ← stable per session (toggle is rare)
[+ Global Memory block]               ← changes when extraction fires (Phase 2)
[+ Session Memory block]              ← changes when extraction fires (Phase 2)
[+ Findings block]                    ← changes when promote-finding fires
```

This is stable across consecutive requests *as long as memory
state doesn't change between them*. Phase 1 leaves memory in the
system prompt because:

1. We don't yet know empirically how often memory blocks actually
   change between user turns (extraction may produce zero new
   facts on most turns).
2. Memory in the system prompt is the established attention
   pattern in the codebase — moving it changes the model's
   foregrounding of those facts and warrants its own measurement
   pass.

§6 specifies the Phase 2 measurement we'll add to inform the next
move.

### 3.4 Render path

```
agent.buildMessagesV2(ctx, budget)
  ├─ systemPrompt := chat.BuildSystemPrompt(...)
  │     (no temporal lines)
  └─ contextbuild.Build(ctx, session, cache, BuildOptions{
       SystemPrompt: systemPrompt,
       RenderUserContent: func(rec memory.Record) string {
           return chat.RenderRecordTemporalPrefix(rec.Timestamp) +
                  "\n\n" + rec.Content
       },
       ...
     })
```

`contextbuild` already iterates records to produce the `llm.Message`
array. The new `RenderUserContent` option (or equivalent in the
existing API) wraps each user-role record's content with the
temporal prefix. Tool-result and assistant records are unaffected
(they already carry timestamps embedded in their structured fields
or in the tool output text itself).

### 3.5 Token cost

Adding a ~20-token prefix to every user record costs:

- ~20 tokens × N user messages per session
- For a 50-turn session: ~1000 extra tokens
- Compared to a typical session context (5K-50K tokens), this
  is <2% overhead

Negligible against the 25× wall-clock saving.

---

## 4. Edge cases

1. **Records with empty timestamps.** Defensive: the renderer
   checks `ts.IsZero()` and skips the prefix line in that case.
   Should never happen for normally-created records, but session
   imports from older bundles or test fixtures might lack
   timestamps.

2. **Records with very old timestamps.** The model sees "Time: T1
   (some date months ago)" before a historical user message. This
   is semantically correct — the user said this at that time.
   When the user references "yesterday" in a turn-N message, the
   prefix on that turn-N record reflects the current time, so the
   model can resolve it correctly.

3. **Tool-result content (role=tool).** No temporal prefix added.
   Tool results aren't "user input"; their timing isn't part of
   the conversational reference frame.

4. **Assistant content (role=assistant).** No temporal prefix.
   Same reasoning.

5. **System Rules edits mid-session.** A one-time invalidation —
   the system prompt changes once, the next request misses cache,
   subsequent requests hit again. Acceptable.

6. **Memory extraction fires between turns.** Phase-1 territory:
   if the extraction adds a new fact, the Global / Session /
   Findings block in the system prompt grows by a few lines. The
   server-side cache hits up to that block, then misses on the
   new fact lines. Net effect: cache hit on the base prompt + rules
   + location + sandbox guidance, miss on memory + history. Still
   better than the current "miss on everything past the
   timestamp". Phase 2 will measure whether this residual miss
   matters in practice.

7. **First turn of a session.** No previous cache; cold call.
   Same as today. The optimisation kicks in from turn 2 onwards.

8. **Profile switch mid-session.** A `/profile <name>` switch
   rebuilds the backend client and resets server-side state on
   reconnect. Cache invalidation expected. Acceptable — `/profile`
   is a rare explicit action.

9. **Model swap on the LM Studio side.** If the user changes the
   loaded model in LM Studio, cache is invalidated. Out of our
   control.

10. **MLX-backed or sliding-window models.** The benchmark
    references note these silently fall back to full
    re-processing regardless of prefix stability. shell-agent-v2's
    optimisation is best-effort: on unsupported model families,
    the user sees no speedup but no regression either.

---

## 5. Rejected alternatives

### 5.1 Send `cache_prompt: true` in the request body

Rejected. The user reported being burned by an extra parameter
that the server rejected outright. The benchmark T5 shows current
LM Studio accepts the parameter but the cache fires automatically
either way (T1 vs T5 measure the same). Adding the parameter is
zero-benefit, non-zero-risk: a future LM Studio version or a
different OpenAI-compatible server might 4xx on the extra field.

### 5.2 Prepend temporal only to the latest user message

Rejected (§3.2). Re-creates the cache-miss problem one position
later in the conversation. Per-record rendering is strictly
better.

### 5.3 Insert a synthetic "context" system message right before
the latest user message

Rejected. Most OpenAI-compatible servers accept only one system
message at the start of the array; behaviour with multiple
system messages is implementation-defined. Risk of subtle prompt
interpretation differences across backends.

### 5.4 Persist the temporal context into the record's content at
record-creation time

Rejected. Mixing presentation (rendered temporal prefix) into
storage (record.Content) makes the record format harder to
evolve and impossible to render differently for different model
backends. Keep storage clean; render at message-build time.

### 5.5 Drop temporal context entirely

Rejected. The model legitimately needs to know "what is today" to
resolve relative date queries. The system tools include
`resolve-date` for complex cases but the inline hint is what
covers the common case ("yesterday", "tomorrow", weekday math).

### 5.6 Cache memory output at the agent layer

Considered for Phase 2. The idea: the agent re-renders
`FormatForPrompt` each turn even when memory state didn't change,
so identical state still might produce slightly different bytes
(e.g. fact ordering jitter). If we cache the rendered string and
only re-render when extraction signals a real change, even the
case where extraction extracted *nothing* on a turn would be
guaranteed byte-stable. Worth measuring whether this is needed
(Phase 2).

---

## 6. Measurement / validation

Phase 1 lands with two pre-existing measurement aids and one new
test:

### 6.1 Bench harness (already in tree)

`app/cmd/llm-cache-bench/` measures wall-clock per request across
controlled scenarios. The 2026-05-20 run validated Phase 1's
hypothesis ([llm-cache-bench-2026-05-20.md](../history/llm-cache-bench-2026-05-20.md))
and should be re-run post-implementation to confirm the production
code path behaves the same as the harness.

### 6.2 Phase 2 memory-stability test (already run)

Scenario T9 in the bench harness measured the residual cost of
memory volatility before this ADR was approved. The result
(see [llm-cache-bench-2026-05-20.md §T9](../history/llm-cache-bench-2026-05-20.md#t9))
is roughly **+200 ms per memory mutation** when the rest of the
prompt is cached. With typical extraction adding 0-3 facts per
turn, worst-case residual penalty is ~600 ms — well under the
6-second pain that Phase 1 fixes.

This directly informs §9 below: **Phase 2 is deferred** rather
than scheduled. The implementation complexity of memory-render
caching or memory relocation doesn't pay for itself at this
magnitude. Re-evaluate if users report the residual latency is
bothersome, or if extraction starts firing more facts per turn
than expected.

### 6.3 Go tests (new)

- `TestBuildSystemPrompt_NoTemporal` — confirms the system prompt
  emitted by the new `BuildSystemPrompt` contains no `Current
  date and time:` and no `Yesterday:` lines.
- `TestRenderRecordTemporalPrefix_ByteStable` — given the same
  `time.Time`, the renderer produces the same bytes.
- `TestContextbuild_TemporalPrefixOnUserRecords` — given a session
  with mixed user / assistant / tool records, only user records
  get the temporal prefix in the assembled message array.

### 6.4 Production telemetry (deferred — Phase 3)

Adding a `prompt_processing_ms` field to the `agent:activity`
event would let us watch the actual production speedup. LM Studio
doesn't return `cached_tokens` in usage (verified empirically), so
we rely on wall-clock measurement of the LLM round. Out of scope
for this ADR's first commit — track separately if user demand
warrants.

---

## 7. Compatibility

### Non-breaking

- `chat.json` records unchanged. Stored records carry only their
  natural content; the temporal prefix is render-time.
- `BuildSystemPrompt` callers continue to receive a system prompt
  string — same type, semantically equivalent for non-cache
  purposes.
- Wails bindings unchanged. No new events, no new endpoints.

### Backwards observation

A session opened in v0.12.x looks identical to one opened in
v0.13.0 from the user's perspective. The only behaviour change is
faster turns on local LLMs that support prefix caching.

### Forward observation

If a future change wants to render record content differently
(e.g. for a different backend's tokeniser), the
`RenderRecordTemporalPrefix` hook generalises cleanly: pass a
different renderer to `contextbuild.Build`.

---

## 8. Phasing

Suggested commits:

1. `refactor(chat): factor out temporal context renderer`
   — pure code move: extract `buildTemporalContext` body into a
   new `RenderRecordTemporalPrefix(ts time.Time) string`.
2. `feat(chat): remove temporal from BuildSystemPrompt`
   — drop the `Current date and time` / `Yesterday` lines from
   `BuildSystemPrompt`. System prompt is now stable across
   identical-memory turns.
3. `feat(contextbuild): inject temporal prefix into user records`
   — `BuildOptions` grows a per-user-record render hook;
   `contextbuild.Build` calls it for each user record's content.
4. `feat(agent): wire temporal renderer into buildMessagesV2`
   — agent layer connects steps 1-3.
5. `test(chat,contextbuild): byte-stability invariants`
   — the three tests from §6.3.
6. `docs: ADR-0017 status update + CHANGELOG v0.13.0 +
   architecture note`
   — ADR Status → Implemented; CHANGELOG entry; brief addition
   to architecture.md describing the prefix-stability principle.

Each commit is independently buildable and tested.

---

## 9. Out of scope

- **Memory-block volatility (Phase 2) — deferred.** The bench
  measurement (§6.2 / [T9](../history/llm-cache-bench-2026-05-20.md))
  shows the residual penalty is modest (~200 ms per memory
  mutation), so the implementation complexity of memory caching
  or relocation isn't justified yet. Re-evaluate if users surface
  the latency, or if extraction starts adding more facts per turn
  than the current ~0-3.
- Vertex AI context caching API integration.
- LM Studio `cache_prompt: true` or other extension parameters
  (§5.1).
- Streaming responses (ADR-0015 §1 rejection still stands).
- Tool descriptor ordering stability.

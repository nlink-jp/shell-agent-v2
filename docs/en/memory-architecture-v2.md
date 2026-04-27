# Memory Architecture v2 — Design Document

> Date: 2026-04-27
> Status: Draft for review
> Scope: `internal/memory/`, `internal/chat/BuildMessages*`, `internal/agent/agent.go` (compaction calls)

## 1. Problem Summary

The current memory system conflates two separate concerns:

1. **Storage** — what the application remembers about a session.
2. **LLM context** — what is sent to the model on a given call.

Today these are the same thing. `Session.Records` carries a `Tier`
field (`hot`/`warm`/`cold`); compaction *mutates* records by replacing
old hot records with a single warm summary. The original details are
discarded.

Consequences:

- **Lossy storage.** Once compacted, a tool result or assistant turn
  can never be recovered at full fidelity, even though disk is cheap
  and the user may want it back later.
- **One-size context.** A single global `HotTokenLimit` (mitigated in
  v0.1.2 by per-backend values, but the underlying coupling remains)
  determines how much fits. Vertex with a 1M+ window and local gemma
  with ~16K cannot share a session optimally — one is starved or the
  other is over-compacted.
- **Compaction failures lose data.** If the summarizer call fails
  partway through, you can end up with no useful state at all
  (cf. the v0.1.1 Vertex-400 incident — root cause was that compaction
  emptied the conversation entirely).
- **No replay or alternate views.** Switching backends mid-session
  cannot retroactively expand the older summary back into raw turns.

## 2. Goals

- Records are **immutable** and full-fidelity. Storage never
  destructively summarizes.
- The LLM context for each call is **derived** from records + active
  backend's budget — *built fresh*, not preserved across calls.
- Older portions outside the budget are condensed via **cached
  summaries**. Cache miss is recomputable; storage of record stays
  authoritative.
- **Backward compatible** with existing session files (legacy `tier`
  field, legacy warm-summary records).
- Per-backend context shaping naturally falls out — same session,
  different `Build()` outputs for Local vs Vertex.

## 3. Non-Goals

- No new long-term retention policy (cold/warm semantics are dropped
  from the active path; archival is a separate concern).
- Not a vector store, not RAG. Records are still ordered append.
- The pinned-memory and findings stores are unchanged.

## 4. Design Overview

```
                                         ┌─────────────────────────┐
  Session                                 │   ContextBuilder        │
  ┌──────────────────────┐                │                         │
  │ Records (immutable)  │ ─────────────▶ │  Build(session, budget) │
  │   user / assistant   │                │      ↓                  │
  │   tool / report      │                │  walk newest→oldest     │
  │   summary (legacy)   │                │  while budget holds:    │
  │   ...                │                │     include raw record  │
  └──────────────────────┘                │  for the older tail:    │
                                          │     getOrSummarize()    │
  Summary cache                           │      ↓                  │
  ┌──────────────────────┐ ◀────────────▶ │  return llm.Message[]   │
  │ keyed by             │                └─────────────────────────┘
  │ (range, summarizer)  │
  │  -> cached text      │
  └──────────────────────┘
```

Three components:

1. **Session.Records** — append-only log. New records are added
   verbatim. Tier and the `summary` role are preserved as a
   compatibility shim but never written by new code.
2. **SummaryCache** — keyed by a *content-stable hash* of the record
   range plus the summarizer model identifier. Entries never overwrite
   records. Stored alongside the session (`summaries.json`).
3. **ContextBuilder** — pure function from `(session, budget,
   summarizer)` to `[]llm.Message`. Called every time the agent loop
   needs to call the LLM.

## 5. Data Model

### 5.1 Record (mostly unchanged)

```go
type Record struct {
    Timestamp  time.Time  // unique within a session
    Role       string     // user | assistant | tool | report | summary(legacy)
    Content    string     // full fidelity
    ToolCallID string
    ToolName   string
    ObjectIDs  []string   // refs to objstore
    // Tier and SummaryRange remain for legacy load only.
}
```

`Tier` becomes vestigial. New code does not read or write it; legacy
sessions keep their values until next save (which writes the field
unset, JSON-omitted via `omitempty`).

### 5.2 Session

```go
type Session struct {
    ID      string
    Title   string
    Records []Record
    // Summaries lives in a separate file; not serialized inline.
}
```

### 5.3 Summary cache

```go
type SummaryEntry struct {
    RangeKey       string    // content-stable hash (see §6.2)
    SummarizerID   string    // backend + model that produced it
    FromTimestamp  time.Time
    ToTimestamp    time.Time
    RecordCount    int
    Summary        string
    CreatedAt      time.Time
}

type SummaryCache struct {
    Entries []SummaryEntry  // ordered by CreatedAt asc; LRU/FIFO eviction
}
```

Stored at `sessions/<id>/summaries.json` to keep `chat.json` lean.

## 6. ContextBuilder Algorithm

### 6.1 High level

```
Build(session, opts) -> []Message:
    msgs = [system_with_temporal + pinned + findings]
    sysTokens = estimate(msgs)

    budget = opts.MaxContextTokens - sysTokens - opts.OutputReserve

    acc = []                       // newest → oldest, prepended
    splitIdx = len(records)        // first record index *not* included
    used = 0

    for i = len(records)-1; i >= 0; i--:
        r = records[i]
        if r.Role == "summary": continue  // legacy summaries handled below
        rendered = renderForLLM(r, opts)  // applies tool-result truncation
        t = estimate(rendered)
        if used + t > budget && len(acc) > 0:
            splitIdx = i + 1
            break
        acc = [rendered] + acc
        used += t
        splitIdx = i

    older = records[:splitIdx]
    if non-trivial(older):
        summary = getOrCreateSummary(older, opts.SummarizerID)
        rendered = renderSummaryWithRange(summary, older[0].Timestamp, older[-1].Timestamp, len(older))
        msgs += [{role: "summary", content: rendered}]

    msgs += acc
    return msgs
```

Invariants:

- **Always at least one raw record.** Same protection as v0.1.1
  compaction fix — guards against the empty-`contents` Vertex 400.
- **Per-record tool-result truncation.** Applied at render time, never
  at storage time. Different backends can render different truncation
  levels.
- **Stable order.** Raw records keep their original order; the summary
  is inserted right after the system block, before raw records.
- **Time-range metadata on summaries.** Every summary message rendered
  into the LLM context carries an explicit time range. See §6.6.
- **Temporal annotation everywhere.** Every channel that delivers
  information to the LLM (raw records, summaries, pinned, findings)
  carries a time marker so the model can reason temporally. See §6.5.

### 6.5 Temporal annotation: cross-cutting principle

The LLM should be able to reason about *when* anything in the
delivered context occurred. The summary range header (§6.6) is one
instance; the same principle applies to every information channel
that flows into the prompt.

| Channel | What time info | How it is rendered |
|---------|---------------|--------------------|
| Top-of-prompt temporal context | "now" | already present (`buildTemporalContext`): today's date, day-of-week, yesterday |
| Raw record (user/assistant/tool/report) | when the record was created | timestamp marker prepended (see §6.5.1) |
| Cached summary of older tail | range covered | range header (see §6.6) |
| Legacy `Role=summary` record | range covered | range header from `SummaryRange` |
| Pinned memory | when the fact was learned | `(learned 2026-04-15)` suffix per fact |
| Findings | when discovered | existing `created_label` retained, surfaced in prompt |

#### 6.5.1 Raw record timestamps

A timestamp marker is prepended to a record's rendered content under
either of these conditions:

- The record is the first one after a gap of more than 30 minutes
  from the previous record (avoids clutter for tightly-spaced turns
  while still giving the model a "session resumed at …" anchor).
- The record is a tool result, report, or any role whose timing has
  domain meaning (the model often needs to know *when* a query ran).

Format (English; localized variants in JA doc):

```
[2026-04-27 14:32 JST]
<content>
```

The marker is added by the rendering layer, not stored in the
record's `Content`. Tokens spent on these markers are counted toward
the budget.

#### 6.5.2 Pinned memory and findings

- `pinned.FormatForPrompt()` already records the date a fact was
  learned (Pinned struct has `LearnedAt`). Render output gains a
  `(learned 2026-04-15)` suffix per line so the model can weigh
  recency.
- `findings.FormatForPrompt()` already includes `CreatedLabel`. Verify
  it is preserved through the prompt formatter (currently OK, but
  make this an explicit invariant).

### 6.6 Summary rendering with time range

The cached `SummaryEntry` stores the LLM-generated text only. The
time range is added at render time (so the same cache entry stays
reusable even if formatting evolves).

Format (English; localized variant for `ja` where applicable):

```
[Summary of N earlier turns — from 2026-04-25 14:32 to 2026-04-27 09:18 JST]
<summary text>
```

Rationale:

- The model frequently needs to answer "when did we discuss X?" or to
  decide whether a piece of older context is recent enough to act on.
- Without an explicit range, summarized content reads as
  timeless/recent, which biases the LLM toward treating stale events
  as current.
- The temporal context already injected at the top of the system
  block (today's date / yesterday) covers the *now*; the summary's
  range covers *when the older content was*. Both are needed.

Implementation notes:

- Timezone uses the user's local zone (same as `buildTemporalContext`).
- For *legacy* `Role=summary` records (from pre-v2 destructive
  compaction), use the existing `SummaryRange` field (`From`/`To`)
  to render the same header — old data already has the range
  available.
- If a future feature merges multiple legacy/cached summaries into a
  single older slot, render each as its own header+body block in
  chronological order rather than concatenating bodies.

### 6.2 Range key for summary cache

Goal: same range + same summarizer => same key, even across runs.
Different summarizer model => different key (gemma summaries are not
interchangeable with gemini summaries).

```
key = sha256(
    summarizer_id || "|" ||
    record_count  || "|" ||
    first.timestamp.UnixNano || "|" ||
    last.timestamp.UnixNano  || "|" ||
    sha256(concat(record contents in range))
)
```

Including a content hash means an unexpected record mutation (e.g. a
manual edit, future redaction feature) invalidates the cache. The
range bounds alone are insufficient.

### 6.3 getOrCreateSummary

```
getOrCreateSummary(records, summarizerID):
    key = computeKey(records, summarizerID)
    if cache.has(key): return cache.get(key).Summary
    summary = summarize(records)            // LLM call
    cache.put(key, {key, summarizerID, ..., summary})
    saveCache()
    return summary
```

Failure mode: if the summarizer call fails, return an empty summary
and proceed. The LLM still has a non-empty `acc` (invariant 1) so the
request will not be rejected. Operator sees an error log.

### 6.4 Eviction

Cache grows as the user has more conversations. Default policy:

- Cap at 64 entries per session (configurable).
- Evict by `CreatedAt` ascending when over cap.

This is generous: even a long session rarely produces more than a
handful of distinct ranges in normal use.

## 7. Migration

### 7.1 Reading legacy sessions

Existing sessions have records with `Tier=warm` and `Role=summary`
that were produced by destructive compaction. ContextBuilder treats
these as **opaque pre-summarized records**:

- During the walk, a `Role=summary` record is skipped from the raw
  inclusion path but is *seeded* into the summary slot if the older
  tail covers the same time range.
- More precisely: when `older` is non-empty and the legacy summary's
  `SummaryRange` is contained within `older`, append the legacy
  summary text to the cache-derived summary.

This keeps old sessions readable without forcing a re-summarization
pass.

### 7.2 Writing

New code never produces `Tier=warm` or `Role=summary` records.
`compactIfOverBudget` and `compactMemoryIfNeeded` become no-ops (and
are eventually deleted).

### 7.3 File layout

- Existing: `sessions/<id>/chat.json`
- New: `sessions/<id>/summaries.json` (created on first cache write)

Loading code tolerates the new file's absence (empty cache).

## 8. Per-Backend Behaviour

The `opts` passed to `Build()` come from the active backend:

```
opts = BuildOptions{
    MaxContextTokens:   cfg.ContextBudgetFor(backend).MaxContextTokens,
    MaxToolResultTokens: cfg.ContextBudgetFor(backend).MaxToolResultTokens,
    SummarizerID:       activeBackend.ID(),
    OutputReserve:      4096,  // typical
}
```

- **Vertex** (~1M tokens): the loop usually never breaks. The summary
  branch is dormant. Tool results render at full fidelity.
- **Local gemma** (~16K-32K): only the most recent few records are
  raw; older portion is summary. Tool-result truncation aggressive.
- **Switching mid-session**: the next call rebuilds with the new
  budget. Cache may have entries from the previous summarizer; they
  are filtered by `summarizer_id` so a switch may temporarily miss
  cache and trigger a fresh summarization.

## 9. Verification Plan

### 9.1 Unit tests (`memory` and new `context` package)

- `Build_FitsInBudget`: total token count ≤ budget for various inputs.
- `Build_AlwaysIncludesRecent`: with a single huge record and a tiny
  budget, the recent record is included raw (Vertex 400 regression).
- `Build_OlderFolds`: enough records exceed budget → summary inserted
  in the right slot, raw records in correct order.
- `SummaryCache_KeyStability`: same range + summarizer → same key
  across separate runs and processes.
- `SummaryCache_ContentMutationInvalidates`: changing a record's
  content within a range produces a different key.
- `Build_LegacySummaryRecordsRespected`: records with `Role=summary`
  contribute their text to the older slot.
- `Build_SummaryHasTimeRangeHeader`: rendered summary message starts
  with the `[Summary of N earlier turns — from … to …]` header for
  both freshly-summarized and legacy paths.
- `Build_RawRecordTimestampMarker`: a record after a >30min gap (and
  any tool/report record) is rendered with the `[YYYY-MM-DD HH:MM TZ]`
  prefix; tightly-clustered turns omit it.
- `PinnedFormatForPrompt_HasLearnedDate`: pinned facts with a
  `LearnedAt` render the date suffix.
- `FindingsFormatForPrompt_HasCreatedLabel`: findings render their
  date label.

### 9.2 Integration tests (`agent`)

- Same session, two backends: `Build()` with Vertex budget produces
  more raw turns than `Build()` with Local budget.
- Long session with cache hit: second `Build()` does not invoke the
  summarizer (assert via mock backend call count).

### 9.3 Manual / staging checks

- Existing v0.1.x session files load and render in the chat view
  unchanged.
- Switching mid-session refreshes the LLM context appropriately on
  next turn.
- Summarizer failure path: agent loop completes without error, log
  shows the failure, response is generated from the (now empty
  summary + raw recent records).

### 9.4 Performance budget

- `Build()` itself must be O(n) in records and complete in <50ms for
  a 1000-record session (no I/O for cache hit; one I/O for miss is
  acceptable).
- Token estimation per record cached on the record (lazy field) to
  avoid recomputing.

## 10. Phased Rollout

| Phase | Scope | Behaviour change |
|-------|-------|------------------|
| 1 | New `internal/context` package with `ContextBuilder`, summary cache, and tests. No agent integration. | None — code dormant. |
| 2 | Agent uses `ContextBuilder` behind a config flag (`memory.use_v2: false` default). Existing destructive compaction still active when off. | Opt-in for testing. |
| 3 | Default to v2. Destructive compaction calls become no-ops. Legacy summary records still readable. | All new sessions are append-only. |
| 4 | Remove `Tier` writes; remove legacy compaction code; tighten Record JSON. | Clean codebase, schema unchanged for old files. |

Each phase is its own PR/release.

## 11. Risks and Open Questions

### Risks

- **Cache drift.** If a future feature edits records (redaction,
  user-driven message delete), cache invalidation must catch it.
  Mitigated by content-hash keying.
- **Summary quality.** A bad summary degrades model performance more
  than truncation would. Track per-summary length / quality signal in
  cache for diagnostics.
- **Storage growth.** A long-lived session with many tool calls can
  reach megabytes. Acceptable today (disk is cheap) but a future
  archival/eviction policy is plausible.

### Open questions

1. **Summary granularity** — one summary covering the entire older
   tail vs sliding chunks of N records each? Single summary is simpler
   and matches v1's behaviour. Per-chunk gives finer cache reuse when
   the boundary moves. *Recommendation: start with single summary.*
2. **Summarizer model selection** — always use the active backend, or
   always a designated cheap model for summarization? *Recommendation:
   active backend for now; revisit if cost/latency matter.*
3. **Cache file format** — JSON like everything else, or a small
   key-value store? *Recommendation: JSON, consistent with the rest.*
4. **What to do if pinned-memory or findings change mid-session** —
   they affect the system block but not the cache. No action needed;
   they re-render every call already.
5. **Do we ever rewrite legacy summary records back to raw?** No
   automatic path. A future "rehydrate session" tool could expose
   this if useful.

## 12. Dependencies and Touchpoints

- `internal/memory` — Record / Session unchanged in interface; tier
  becomes vestigial.
- `internal/chat/BuildMessages*` — replaced by ContextBuilder. Guard
  tags / pinned / findings logic moves into the builder.
- `internal/agent/agent.go` — `compactIfOverBudget` /
  `compactMemoryIfNeeded` deleted in phase 4. The build call site
  (currently `BuildMessagesWithBudget`) calls `ContextBuilder.Build`.
- `internal/llm` — no change.
- `internal/config` — already has per-backend budget; the only
  addition is `SummarizerID` plumbing (derived from active backend
  name + model).

## 13. Summary

Records become the single source of truth, immutable and append-only.
LLM context is derived per-call by `ContextBuilder`, sized to the
active backend's budget, with older portions condensed via cached
summaries that are content-keyed for safe reuse.

The migration is staged so each phase can be tested independently and
reverted via the config flag in phase 2.

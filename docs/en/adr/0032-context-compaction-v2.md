# ADR-0032: Context compaction v2 (tiered summary + anchor preservation + lifecycle-driven topic drop)

- Status: Accepted
- Deciders: magi
- Related: ADR-0031 (memory entry lifecycle — this ADR's topic-drop signal depends on it), `docs/en/history/memory-architecture-v2.md` (the contextbuild model that this ADR extends), `docs/en/reference/memory-model.md` (schema).

## 1. Context

### 1.1 Symptom and unfinished business from ADR-0031

ADR-0031 §1.2 identified three reinforcing amplifiers behind the "long sessions re-anchor to early topics" symptom. ADR-0031 addressed (1) — the system-prompt accumulation — by introducing the memory entry lifecycle. (2) — assistant-turn extraction drift — is mitigated as a side effect via Jaccard consolidation. This ADR closes the loop on (3) — **summary anchoring**.

The conversation-tail summarizer (`agent.go:889`) instructs the LLM to *"Summarize the following conversation segment concisely. Preserve key facts, decisions, and context."* As a session grows, the summary preserves early-session anchors verbatim while transient content gets compressed away. The summary block is then injected between the system prompt and the recent raw records — a position with strong attention weight. The result: even with ADR-0031 in force, an old topic can re-enter the LLM's view through the summary and pull attention back.

### 1.2 What is wrong with the current summarizer

A read of `internal/contextbuild/builder.go` and `agent.go:883-897` shows:

1. **Single-tier compression.** All records outside the raw-records window are folded into one summary block. As the session grows, the span being summarized grows without bound; the summary itself does not get re-summarized when it would exceed its share of the budget.
2. **Range-keyed cache.** The summary cache key is the slice of record indices the summary covers (`ComputeRangeKey` in `cache.go`). Every new user turn shifts the range and forces regeneration even if no content actually changed.
3. **"Preserve key facts" prompt instruction.** This is the load-bearing pathology. The LLM treats early decisions as load-bearing and keeps them in every regenerated summary — exactly the early-anchor reinforcement we are trying to eliminate.
4. **No topic awareness.** The summary doesn't know that the conversation moved on. Topic A from turn 5 lives in the summary forever, unchanged in salience versus topic Z from turn 60.
5. **No anchor distinction.** "Key fact" and "anchor moment" are conflated. A user's stated preference ("I prefer Go") and a transient comment ("the table looks fine") get the same preservation pressure from the prompt.

### 1.3 Why this is the right next step

ADR-0031 produced two things this ADR can reuse:

- **A lifecycle state for every Session Memory entry.** When an entry is dormant or archived, ADR-0031 already concluded that the topic is no longer load-bearing for the current conversation. Compaction can use this same signal: records associated with a dormant / archived topic do not need verbatim summarization.
- **A Global Memory store partitioned by category.** `preference` and `decision` entries are the durable, anchored ones. A record that gave rise to one of those facts is the anchor itself; preserving its verbatim text is unambiguously the right call.

These two signals let compaction be **lifecycle-driven** rather than relying on the summarizer LLM's judgment — which has been the source of the drift problem.

## 2. Decision

Restructure context compaction along five axes.

### 2.1 Two-tier summary

Replace the single summary block with two tiers between the system prompt and the raw records:

```
[System Prompt]                       ← ADR-0031 lifecycle-filtered memory
[Far Summary]   (older session, ~5%)  ← topic bullets, anchors lifted out
[Near Summary]  (mid session, ~15%)   ← topic bullets, anchors lifted out
[Anchored Records]  (verbatim)        ← decision/preference origin turns
[Raw Records]   (recent, ~80%)
```

Token budget allocation (defaults; tunable under `ContextBudget.*`):

- Raw records: 80% of the available budget (after OutputReserve and SystemPrompt).
- Near summary chunk: targets ~15% of available budget. The records that flow into it are the next-oldest chunk past the raw window, summarized once.
- Far summary chunk: targets ~5%. Everything older than the near window.
- Anchored records: extracted from anywhere in the near/far span (see §2.2) and rendered verbatim **between** the summaries and the raw records.

When the far tier itself exceeds its budget (a very long session), re-compaction kicks in: the older portion of the far tier is folded back into a meta-summary of "what this conversation has been about over its lifetime", producing a single-line synopsis.

### 2.2 Anchor preservation via Global Memory cross-reference

An **anchor record** is one that the lexical Jaccard score against a `decision` or `preference` Global Memory entry's `Fact` text crosses the `AnchorJaccardThreshold` (default `0.4`).

Anchor detection uses the existing `memory.JaccardScore` / `TokenSet` helpers from ADR-0031 — no new lexical machinery. The check runs at compaction time over the union of all Global Memory entries' fact texts, so it picks up decisions / preferences that have just been pinned.

Anchor records are **rendered verbatim** in their own block, just before the raw records. They are *also* present in their summary's input (so the summary may still mention them in passing), but their canonical form for the LLM is the verbatim block.

This is the §Q2-A approach from the design discussion: anchors are derived as a side effect of extractMemories already routing decisions / preferences to Global Memory. There is **no new tool** for the LLM to call (`mark_anchor` was considered and rejected — local LLM tool-call reliability is too uncertain).

### 2.3 Lifecycle-driven dead-topic drop

A **dead record** is one whose lexical Jaccard against the `Fact` of *any* `dormant` or `archived` Session Memory entry crosses `DeadTopicJaccardThreshold` (default `0.4`), AND which does not also match any `fresh` / `active` Session Memory entry.

Dead records are **dropped entirely from the summary input** — not even mentioned as a topic bullet. The summary block emits a count of dropped records as a single elision marker (`[N dead-topic turns suppressed]`).

This is the strongest synergy with ADR-0031: the same "this topic is no longer relevant" signal that takes a fact out of the system prompt also takes it out of the conversation summary. The two ADRs reinforce rather than fight each other.

Dead records are *still* present in `session.Records` on disk — the drop is per-compaction, not destructive.

### 2.4 Content-hash cache key

Replace the range-based cache key (`internal/contextbuild/cache.go:ComputeRangeKey`) with a content-hash key:

```
key = sha256(
    SummarizerID
    || sha256(concat(record.Role + record.Content for each input record))
    || sha256(sorted(deadTopicFingerprints))
    || sha256(sorted(anchorRecordIndices))
    || tier  // "near" or "far"
)
```

This makes the cache:

- **Stable across turn additions**: a new turn that doesn't change a tier's input keeps that tier's cache hit live.
- **Invalidates on relevant lifecycle change**: when a Session Memory entry transitions to dormant (and triggers new dead-topic drops), the cache key for affected tiers changes and the summary regenerates with the new drops applied.
- **Per-tier**: each tier has its own cache slot, so churn in one doesn't invalidate the other.

The on-disk cache format (`summaries.json`) accumulates `SummaryEntry` rows keyed by this hash. Old (range-keyed) entries are read with their old key and rewritten with the new content-hash key on first hit.

### 2.5 New summarizer prompt

The current prompt is replaced. The new design has two complementary instructions per tier:

```
Summarize the following conversation segment as a list of topic bullets,
one line per distinct topic discussed. Format:
  - <topic>: <one-line summary of what was said about it>

Important rules:
- The user's preferences, decisions, and key facts are already preserved
  verbatim in a separate block. Do NOT repeat them; just list the topic
  they belong to.
- Discussions that did not lead anywhere should still be listed but
  marked: "- <topic>: discussed but not concluded".
- Dead-end / dropped topics have already been removed from this segment;
  do not invent topic bullets for content that is not here.
- Be terse. Aim for 1-3 sentences per bullet at most.
- Use the same language as the conversation.
```

This drops the *"Preserve key facts, decisions, and context"* line that was the load-bearing source of early-anchor reinforcement. The summarizer is no longer responsible for preserving anchors (the anchored records block does that), so its only job is condensation.

## 3. Consequences

### 3.1 Behavioral

- Old topics no longer re-anchor the LLM via the summary block. Either the topic is decision/preference-anchored (in which case the verbatim record is in the prompt for clear context, but presented as historical) or it's dead (in which case it's elided entirely).
- Session Memory dormant / archived state has a meaningful second effect — it drops not just the entry from the system prompt but also any conversation-tail mention from the summary.
- The summary block reads as a high-level "what was discussed" rather than "what was said". This is closer to how a human takes notes and is easier for the LLM to position itself relative to.
- Anchored records being a separate block makes "what did the user decide" answerable without scanning a paragraph of summary prose.
- Very long sessions (hundreds of turns) become viable without context window saturation, because the far tier re-compacts when it exceeds its budget.

### 3.2 Backward compatibility

- On first run after upgrade, `summaries.json` cache files contain range-keyed entries. They are still loaded; their entries are simply orphaned (won't match any new content-hash key) and will be garbage-collected by the existing FIFO cap. No migration needed.
- `agent.go:889`'s summarizer closure is replaced with the new prompt. Sessions in flight on disk are unaffected — the next user turn will compact under the new rules.
- ADR-0031 lifecycle state must be present in Session Memory entries for dead-topic drop to fire. Pre-ADR-0031 sessions (where Session Memory entries have legacy default `state=active`) skip dead-topic drop until lifecycle decay starts producing dormant entries. Safe degradation: in that case behaviour falls back to "no drops, just two-tier summarisation" which is still a net improvement.

### 3.3 Test impact

- `internal/contextbuild/builder_test.go`: existing tests for budget walking, summary inclusion, cache hit/miss remain valid. Add new tests for the two-tier split, anchor extraction, dead-topic drop, and content-hash cache stability across turn additions.
- `internal/contextbuild/cache_test.go`: adds a backward-compat test (legacy range-keyed entries load without error, simply do not match).
- Agent-level test: a session whose Records reference a dormant session-memory topic produces a summary with the dropped count noted and no verbatim mention of that topic.

### 3.4 Config knobs

New knobs under `ContextBudget` (extends, does not replace, the existing fields):

```json
{
  "ContextBudget": {
    "FarSummaryShare":           0.05,
    "NearSummaryShare":          0.15,
    "AnchorJaccardThreshold":    0.4,
    "DeadTopicJaccardThreshold": 0.4
  }
}
```

`MaxContextTokens`, `MaxToolResultTokens`, `OutputReserve` are unchanged in shape and meaning.

The remaining 80% of the budget (after `FarSummaryShare + NearSummaryShare`) is used for raw records, anchored records, and the system prompt; anchored records are accounted against the raw share since they share the same "verbatim" rendering path.

### 3.5 Removed code paths

- `assembleSummary`'s single-block path is replaced with `assembleNearSummary` and `assembleFarSummary`. The signature of `Build` does not change; the result still has `Messages`, `TotalTokens`, etc.
- The "no summarizer → drop older tail silently" branch is removed. With two tiers it becomes ambiguous which tier to drop from; instead, when no summarizer is provided, an elision marker counting dropped records is emitted in place of the summary blocks.

## 4. Implementation

### 4.1 Anchor + dead-topic detection helpers

Add to `internal/memory/lifecycle.go`:

```go
// LookupTokenSet returns a TokenSet ready for repeated comparison.
// Memoised by the caller — Build() builds it once per anchor source.
//
// (Already provided by TokenSet; this is just a label.)

// AnchorRecord reports whether a record matches any of the supplied
// anchor texts at Jaccard ≥ threshold. anchorTokenSets is precomputed
// once per Build to avoid re-tokenising every Global Memory entry per
// record.
func AnchorRecord(content string, anchorTokenSets []map[string]struct{}, threshold float64) bool

// DeadTopicRecord reports whether a record matches any dormant /
// archived session-memory fact at Jaccard ≥ threshold AND does not
// also match any active / fresh session-memory fact at the same
// threshold. The second clause is the safety net: a record that
// references both a live topic and a dead one is kept.
func DeadTopicRecord(
    content string,
    deadTokenSets, liveTokenSets []map[string]struct{},
    threshold float64,
) bool
```

Both are pure functions. Tests live next to the existing lifecycle tests.

### 4.2 Builder restructure

`internal/contextbuild/builder.go`:

- Extract the "walk newest → oldest, fill raw budget" loop into `selectRawWindow`.
- Add `partitionForTiers` that takes the older-than-raw records and splits them by token-count into `nearInput` and `farInput` per the share knobs.
- Add `liftAnchors` that scans the union of nearInput + farInput, identifies anchor records via §4.1, removes them from the summary inputs, and returns them as a separate slice for verbatim rendering.
- Add `dropDeadTopics` that scans the remaining summary inputs and removes dead records, returning the dropped count.
- Replace `assembleSummary` with `assembleTier(name, input, droppedCount, cache, opts) (block string, fromCache bool)`. Called twice.
- The final assembly order: system → far summary → near summary → anchored records → raw records.

### 4.3 Cache restructure

`internal/contextbuild/cache.go`:

- Replace `ComputeRangeKey` with `ComputeContentKey(records, deadFingerprints, anchorIndices, summarizerID, tier)`.
- `Get` and `Put` accept the new key shape; the on-disk schema gains a `kind: "content_v2"` field for forward-compat.
- Legacy range-keyed entries are read but ignored on hit (treat as miss). They are dropped on the next save.

### 4.4 Summarizer

`internal/agent/agent.go`:

- Replace the `summarize` closure's system prompt with §2.5's text.
- The closure now optionally takes a `tier` hint and language hint (reused from the existing `detectUserLanguageHint`). The detect-language behaviour is unchanged.

### 4.5 Wiring in Build options

`BuildOptions` gains:

- `AnchorSources []string`  — typically `Fact` strings of every `decision` / `preference` Global Memory entry.
- `DeadTopicSources []string` — Fact strings of every dormant / archived Session Memory entry.
- `LiveTopicSources []string` — Fact strings of every fresh / active Session Memory entry.
- `FarSummaryShare`, `NearSummaryShare`, `AnchorJaccardThreshold`, `DeadTopicJaccardThreshold` (mirror the config).

`agent.buildMessagesV2` populates these from the live stores. The agent layer does the type conversions; the memory layer does not depend on contextbuild.

## 5. Out of scope

- **Findings.** Same rationale as ADR-0031: different access pattern, defer.
- **Recall tool / memory router.** ADR-0033 / ADR-0034 territory. Once compaction stops re-anchoring on dead topics, the question "how do we get a dormant fact back in front of the LLM when needed" becomes well-formed.
- **Vector-based topic similarity.** Lexical Jaccard is sufficient for the anchor / dead-topic gates given the relatively short fact texts and the same no-external-dependency principle ADR-0031 followed.
- **User-facing summary rendering.** The compacted summary remains a backend-only artifact; the chat UI continues to render raw Records.
- **Streaming compaction.** Re-compaction of the far tier runs synchronously per Build call. A future ADR could move it to a background goroutine if latency becomes an issue, but the current default budgets keep far-tier regeneration rare.
- **Anchor flag on Record schema.** Considered as Option Q2-C and rejected. Cross-referencing via lexical Jaccard against Global Memory's Fact text avoids a Record schema change and keeps the anchor signal derivable from existing data (so reconstructing it after the fact, e.g. on import, just works).

## 6. Manual smoke checklist

1. **Two-tier rendering**: in a 30-turn session, verify app.log shows two summary blocks emitted (`tier=near` and `tier=far`) and the assembled system → far summary → near summary → anchored → raw order in the LLM transcript.
2. **Anchor preservation**: state a clear preference ("I prefer Go over Python") early in a session, run 25 more turns, verify the original preference turn appears verbatim in the anchored-records block — not just paraphrased in a summary bullet.
3. **Dead-topic drop**: discuss topic A for 5 turns, switch to topic B for 15 turns (long enough for topic A's session memory to fall dormant), verify the next summary regeneration emits `[N dead-topic turns suppressed]` and does not contain topic A's nouns.
4. **Cache stability across turn additions**: in a 20-turn session, observe `buildMessagesV2: ... cache_hit=true` on a turn where nothing changed in the summarised span. Compare to the old behaviour where every turn was a cache miss.
5. **Cache invalidation on lifecycle change**: take a session whose topic A is active. Force topic A to dormant (e.g. by toggling `DecayRate=0.5` temporarily). On the next turn, verify the summary regenerates (`cache_hit=false`) with topic A's content dropped.
6. **Long-session re-compaction**: in a 200-turn session, verify the far tier itself stays within `FarSummaryShare * budget` — confirm `app.log` shows a recompaction line when crossing the threshold.
7. **Legacy cache load**: copy a `summaries.json` from a pre-ADR-0032 build into a session, restart, verify it loads without error and a subsequent compaction writes new content-hash-keyed entries (legacy entries unreferenced and eventually dropped under FIFO).
8. **No summarizer fallback**: temporarily configure a backend that cannot summarise (or pass `nil` Summarize), verify the build emits an elision marker in place of summary tiers and does not crash.

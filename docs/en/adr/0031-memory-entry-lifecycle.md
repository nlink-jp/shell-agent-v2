# ADR-0031: Memory entry lifecycle (states, relevance decay, touch refresh, consolidation)

- Status: Accepted
- Deciders: magi
- Related: `docs/en/reference/memory-model.md` (schema), ADR-0028 (provenance trim that this builds on), ADR-0019 (remember-fact tool). Successor ADRs that depend on this: ADR-0032 (context compaction v2, planned), ADR-0033 (recall tool / memory router, planned).

## 1. Context

### 1.1 Symptom

In long sessions (typically beyond ~30 turns) the agent's situational awareness degrades: it fails to acknowledge what was just said and suddenly re-anchors to an earlier topic from the session. The working hypothesis — confirmed by walking the relevant code paths — is that the system prompt, which is rebuilt every turn from full-text dumps of `GlobalMemoryStore`, `SessionMemoryStore`, and the findings `Store`, increasingly fixates the model on stale facts as the session grows.

### 1.2 Three amplifiers

A read of `internal/chat/chat.go:196`, `internal/memory/session_memory.go`, `internal/agent/agent_extract.go`, and `internal/contextbuild/builder.go` identified three reinforcing mechanisms:

1. **System-prompt accumulation.** Each turn injects up to ~48 KiB of bullet-listed facts (three stores × 16 KiB cap each). No decay, no relevance ranking. The list is FIFO-capped at 100/50/100 entries respectively; until the cap is reached every extracted fact persists for the lifetime of the session and reappears on every request.
2. **Assistant-turn extraction drift.** `extractMemories` (`agent_extract.go:41`) reads the last four *non-tool* records — including assistant turns. A one-time off-topic excursion by the assistant gets extracted as `fact`/`context` and reinjected on every subsequent turn, creating a self-reinforcing memory of the very drift the model is meant to resist.
3. **Summary anchoring.** The conversation-tail summarizer prompt (`agent.go:889`) instructs *"Preserve key facts, decisions, and context."* As the conversation grows, the summary preserves early-session anchors and emits them between the system block and recent records — a position with strong attention weight.

This ADR addresses **(1)** — system-prompt accumulation. (2) is partially mitigated as a side effect (consolidation collapses near-duplicates from assistant drift); the deeper fix lives in a separate ADR. (3) is the subject of ADR-0032 (context compaction v2), planned as the immediate follow-on.

### 1.3 What is missing today

Walking the `GlobalMemoryStore` / `SessionMemoryStore` APIs, the memory model has:

- Creation via `Add` (+ FIFO eviction past cap)
- Deletion via `Delete` / `DeleteByFacts`
- Exact-`Fact` dedup at add time

It does **not** have:

- Per-entry state distinguishing "still load-bearing" from "stale"
- Any notion of relevance or aging
- Reference tracking (was this fact mentioned again? did the LLM use it?)
- Near-duplicate consolidation
- An eviction policy beyond cap-hit FIFO
- An audit trail for state transitions or evictions

FIFO at 50/100 is a safety valve against unbounded growth; it is not quality management.

## 2. Decision

Introduce a per-entry lifecycle to **Global Memory and Session Memory**. Findings is out of scope here (see §5). The lifecycle has four states:

```
fresh ──┬─→ active ──┬─→ dormant ──→ archived ──→ (evicted)
        │            │
        └── touch ───┘
```

- **fresh** — created within the last `FreshTurns` turns (default 3). Relevance = 1.0. Always rendered in the system prompt.
- **active** — relevance ≥ `ActiveThreshold` (default 0.4). Rendered in the system prompt.
- **dormant** — relevance fell below `ActiveThreshold` but is above `ArchiveThreshold` (default 0.1). **Not rendered** in the system prompt. Still eligible for touch.
- **archived** — relevance ≤ `ArchiveThreshold`. Not rendered. Not eligible for extractor exposure. First in line for eviction when cap pressure exists.

`State` is *derived* from `Relevance` at every load/save and after every touch/decay event; it is materialised on the entry to drive the UI badge and audit log, not to serve as a separate authoritative source of truth.

### 2.1 Relevance + decay

Each entry carries a `Relevance` float in `[0.0, 1.0]`:

- Created at `1.0`.
- After every completed user turn, `Relevance *= DecayRate` (default `0.93`). With the default an entry decays from 1.0 to ~0.4 (the active/dormant boundary) over ~12 turns of disuse, and reaches the archive threshold (~0.1) after ~32 turns.
- A touch resets `Relevance` to `1.0` and updates `LastTouchedAt` / `LastTouchedTurn`.
- `TouchCount` increments on every touch; not used for state derivation in v1 but recorded for audit and future consolidation heuristics.

Decay is **per-turn**, not per-wall-clock. Sessions paused overnight should not lose memory; sessions actively burning through 80 turns should age memory aggressively. Wall-clock decay was considered and rejected (see §5).

### 2.2 Touch (reference detection)

Two paths, unioned:

1. **Lexical fallback** (always-on, near-zero cost). After each user turn is appended, compute a token-set Jaccard between the turn's content and each entry's `Fact`. Any entry scoring ≥ `TouchJaccardThreshold` (default 0.3) is touched. The threshold is lower than `findings.DedupJaccardThreshold = 0.5` because false-positive touches are far cheaper than false-negative aging — keeping an entry warm when it was not actually referenced is much less harmful than letting a still-relevant entry go dormant.
2. **Extractor-derived touch** (primary signal, no extra LLM call). `extractMemories` already shows the LLM the last four turns + the existing-facts list (`agent_extract.go:117-126`). Extend its prompt to additionally emit a `touched:` line listing fact-text fingerprints referenced by the conversation tail. Parse it and refresh the corresponding entries.

The extractor path can mark facts the lexical path missed (paraphrase, semantic reference); the lexical path is the safety net when the extractor returns nothing useful.

### 2.3 Consolidation (near-duplicate merge)

`Add` currently dedups by exact `Fact` match. Replace with Jaccard-based consolidation:

- For each new entry, scan existing entries; if any has Jaccard ≥ `ConsolidationJaccardThreshold` (default 0.5, matching findings) against the new `Fact`, treat the new entry as a **touch** on the existing one rather than appending.
- Preserve the existing `Fact` text (the older, established phrasing). Increment `TouchCount`, reset `Relevance` to `1.0`.
- This breaks the assistant-drift self-reinforcement loop described in §1.2(2): variant rewordings of the same drift fact collapse into one entry that ages out, instead of N near-duplicate entries that each survive 32 turns and pile up in the system prompt.

### 2.4 Eviction policy

Replace cap-hit FIFO with **lowest-relevance-first**:

- When `len(Entries) > MaxEntries`, evict the entry with the lowest `Relevance`. Ties broken by oldest `LastTouchedAt`.
- An old-but-still-touched entry survives over a recently-added-but-untouched one — the correct ordering for memory quality.
- `archived` entries are preferred eviction targets even if their `LastTouchedAt` is more recent than an `active` entry (state takes precedence over recency for the eviction decision).

### 2.5 System prompt injection

`FormatForPrompt` filters out `dormant` and `archived` entries. The 16 KiB budget remains as a defence-in-depth ceiling, but with lifecycle in place it will rarely be the binding constraint.

The injected output is unchanged in format (no `[fresh]` / `[active]` tags surfaced to the LLM — see §2.7).

### 2.6 Audit log

Every state transition, touch, consolidation, and eviction is logged at `Info` level via the existing `internal/logger` package:

```
memory: session-memory entry "User wants Q1 sales analysis…" fresh→active (relevance 0.79)
memory: session-memory entry "Three datasets loaded" active→dormant (relevance 0.39, last touched turn 12, now turn 27)
memory: session-memory evicted "Spurious tangent about CI…" (relevance 0.08, state archived, last touched turn 4)
memory: session-memory touched "User wants Q1 sales analysis" (relevance 0.62 → 1.0, source: extractor)
memory: session-memory consolidated new entry into existing "User has three datasets loaded" (Jaccard 0.71, source: assistant_turn)
```

This makes *"why did this fact disappear / why is the LLM still anchored to that old thing"* answerable from the log — surfaced as a debug pain point in the lifecycle discussion.

### 2.7 What the LLM and UI see

- **LLM**: no schema change visible. `FormatForPrompt` output drops dormant/archived bullets; format is otherwise identical. No `[fresh]` / `[active]` annotations leak to the model.
- **UI**: the existing Memory sidebar gets a small state badge per row (`fresh` / `active` / `dormant` / `archived`) and a relevance hint (a thin bar at the right edge of the row). Archived entries are collapsed under a `Show archived (N)` disclosure. This is the only UI delta — no list re-ordering, no new tabs.

State and relevance are not editable from the UI in v1 (they are mechanically derived). The existing delete affordance still works on any state.

## 3. Consequences

### 3.1 Behavioural

- Long sessions no longer accumulate a ~48 KiB pile of bullets in the system prompt — only `fresh` + `active` survive injection. Expected: significant attenuation of the "old-topic re-anchor" symptom that motivated this ADR.
- Assistant-drift facts collapse via consolidation rather than persisting as N near-duplicate entries.
- Eviction quality improves: stable facts that get touched survive; one-off noise ages out and is reclaimed first under cap pressure.
- A fact that genuinely matters but happens not to be referenced for ~30 turns can fall dormant. It is **not** deleted — `dormant` and `archived` entries remain on disk and remain searchable from the sidebar. The follow-on ADRs (recall tool / memory router) are where dormant facts re-enter the LLM's view on demand.

### 3.2 Disk schema

Adds five fields to `GlobalMemoryEntry` and `SessionMemoryEntry`:

```go
Relevance       float64   `json:"relevance,omitempty"`
LastTouchedAt   time.Time `json:"last_touched_at,omitempty"`
LastTouchedTurn int       `json:"last_touched_turn,omitempty"`
TouchCount      int       `json:"touch_count,omitempty"`
State           string    `json:"state,omitempty"` // derived; persisted for UI consumers
```

`omitempty` keeps the on-disk size of fresh entries negligible.

### 3.3 Backward compatibility

Legacy entries (loaded from an existing `global_memory.json` or `session_memory.json` written by a pre-lifecycle build) lack all five fields. On `Load` we silently fill them:

- `Relevance == 0` (zero value) is recognised as "legacy" and replaced with `1.0`.
- `LastTouchedAt` and `LastTouchedTurn` default to `CreatedAt` and `0` respectively.
- `TouchCount = 0`.
- `State` is recomputed from `Relevance` (legacy entries enter as `active` — fresh window depends on turn count which is per-session, not load-time).

No migration code, no version bump on the file. `Save` writes the new fields populated; if a user downgrades, `json.Unmarshal` ignores unknown keys (the stores do not use `DisallowUnknownFields`).

This silent-fill approach matches ADR-0028's pattern.

### 3.4 Test impact

Existing `Add` / `Delete` / `FormatForPrompt` tests pass as-is with the legacy-fill behaviour.

New tests:

- Decay over N turns lands an entry in `dormant`, then `archived`.
- Touch resets relevance to 1.0; lexical and extractor paths both exercised.
- Consolidation: adding a near-duplicate increments `TouchCount` on the existing entry, does not append.
- Eviction: lowest-relevance evicts first, `archived` takes priority over `active` even when more recently touched.
- Legacy file (no lifecycle fields) loads, behaves correctly, persists with new fields on next `Save`.
- `FormatForPrompt` excludes `dormant` and `archived` entries verbatim from output.

### 3.5 Config knobs (with defaults)

All thresholds are surfaced as `Memory.Lifecycle.*` in the existing JSON config so power users can tune without recompile, but defaults are sensible and most users never touch them:

```json
{
  "Memory": {
    "Lifecycle": {
      "DecayRate": 0.93,
      "FreshTurns": 3,
      "ActiveThreshold": 0.4,
      "ArchiveThreshold": 0.1,
      "TouchJaccardThreshold": 0.3,
      "ConsolidationJaccardThreshold": 0.5
    }
  }
}
```

## 4. Implementation

### 4.1 Memory packages

- `app/internal/memory/lifecycle.go` (new). Pure functions: `DecayedRelevance(r, rate)`, `DeriveState(r, freshUntilTurn, currentTurn, thresholds)`, `JaccardScore(a, b)`, `ConsolidationMatch(entries, newFact, threshold) -> (index, ok)`. State derivation lives in one place; entry stores call these.
- `app/internal/memory/global_memory.go`:
  - Extend `GlobalMemoryEntry` with the five fields from §3.2.
  - Add `legacyFill(entry *GlobalMemoryEntry)` invoked from `Load` for each entry that has `Relevance == 0`.
  - Replace `Add` dedup logic to use `ConsolidationMatch`; the merge path bumps `TouchCount`, resets `Relevance`, and emits an audit log line.
  - Add `Touch(matchFn func(GlobalMemoryEntry) bool, currentTurn int, source string) int` returning the number touched. Audit-logged per touched entry.
  - Add `DecayAll(currentTurn int)` that multiplies relevance for every non-`fresh` entry, recomputes state, and emits audit log lines on transitions.
  - Replace FIFO eviction inside `Add` with `evictLowestRelevance` helper that respects state priority (archived → dormant → active → fresh).
  - Update `FormatForPrompt` to skip entries whose derived state is `dormant` or `archived`.
- `app/internal/memory/session_memory.go` — mirror the same changes.

### 4.2 Agent loop hooks

- `app/internal/agent/agent.go`: after each user turn append and before the LLM call (i.e., before `buildMessagesV2`), invoke `globalMemory.DecayAll(currentTurn)` and `sessionMemory.DecayAll(currentTurn)`. Decay is O(n) over entries; running it before injection guarantees `FormatForPrompt` sees freshly-computed state.
- `app/internal/agent/agent.go`: after the user turn append, invoke lexical touch:
  ```go
  pred := lifecycle.LexicalTouchPredicate(userContent, cfg.Memory.Lifecycle.TouchJaccardThreshold)
  globalMemory.Touch(pred, currentTurn, "lexical_user_turn")
  sessionMemory.Touch(pred, currentTurn, "lexical_user_turn")
  ```
- `app/internal/agent/agent_extract.go`:
  - Extend the extractor system prompt to additionally emit a `touched:` line of fact-text fingerprints (a short hash or first-N-tokens) referenced by the conversation tail.
  - Parse the `touched:` line; for each fingerprint, locate the matching entry by re-fingerprinting on the store side and call `Touch(matchFn, currentTurn, "extractor")`.
  - When `Add` returns the consolidated case (existing entry touched, no new append), no extra agent-side log needed — the store emits its own.

### 4.3 Bindings + frontend

- `app/bindings.go` — `GetGlobalMemories` / `GetSessionMemories` DTOs gain `state`, `relevance`, `last_touched_at`, `touch_count`. Existing fields unchanged. Order remains insertion order; UI does any visual grouping.
- `app/frontend/src/types.ts` — mirror DTO additions.
- `app/frontend/src/` — sidebar row gets a state badge (`fresh` / `active` / `dormant` / `archived`) and a thin relevance bar; `Show archived (N)` collapsible group. No layout overhaul.

### 4.4 Events

The existing `global_memory:updated` / `session_memory:updated` events fire after `DecayAll` if any entry's `state` flipped, so the sidebar redraws without polling. The agent loop sends these events from the same hook that calls `DecayAll`, not from inside the store (keeps the store package free of Wails dependencies).

## 5. Out of scope

- **Findings.** Different access pattern — data-analysis discoveries are typically referenced explicitly by the user, tagged for filter/search, and bounded per-session by `load-data` semantics. The cost/benefit of adding lifecycle is unclear here; defer until either symptoms appear in Findings specifically, or until ADR-0032 reveals a strong reason to unify.
- **Context compaction v2.** Tail-summarisation reform (anchor preservation, multi-tier summary, dead-topic drop) is ADR-0032 — the natural next step and the third amplifier from §1.2.
- **Recall tool / memory router.** Surfacing `dormant` / `archived` entries back into the LLM's view via an on-demand fetch or a parallel scene-keyed selector is ADR-0033 / ADR-0034 territory. The lifecycle work here is the prerequisite — once entries can be classified as not-currently-relevant, the question of how to recall them becomes well-formed.
- **Per-fact embeddings / vector search.** Out of scope for this lifecycle work; touch detection is lexical-plus-extractor, which keeps the no-external-dependency property of the project intact.
- **Wall-clock decay.** Considered and rejected: the user pain point is per-turn drift inside an active session, not multi-day staleness. Revisit if cross-session Global Memory hygiene becomes an issue.
- **Editing relevance / state from the UI.** v1 mechanics-only; users delete or keep, the system manages the in-between. Adding editing surface area now would invite ad-hoc tuning that defeats the audit-driven debugging story.

## 6. Manual smoke checklist

1. Start a fresh session. Have a 4-turn exchange about topic A; verify the relevant session-memory bullets appear in `FormatForPrompt` output (via app.log or sidebar badges = `fresh`).
2. Switch topics to B. After ~10 turns of B without referencing A, verify the topic-A bullets fall to `dormant` (sidebar badge changes; app.log emits `active→dormant`) and disappear from the system prompt.
3. Bring topic A back up in a single user turn. Verify the dormant entry gets touched, returns to `active`, and reappears in the system prompt on the next request.
4. Trigger consolidation: get the assistant to re-state a near-equivalent fact ("the user is analysing Q1 sales" vs "User wants Q1 analysis"). Verify the second extraction is logged as `consolidated`, `TouchCount` increments on the existing entry, and no new entry is appended.
5. Push past the session-memory cap (50 entries). Verify eviction targets `archived` entries first, then lowest-relevance `dormant`, never an active entry — and that the eviction line in app.log identifies the chosen entry by fact text.
6. Load a pre-lifecycle `session_memory.json` (or fabricate one by writing an entry without the new fields). Verify it loads, displays as `active` with `relevance` 1.0, and saves with the new fields populated.
7. Toggle `Memory.Lifecycle.DecayRate` to 0.5 in config, restart, verify decay proceeds visibly faster within ~3 turns.
